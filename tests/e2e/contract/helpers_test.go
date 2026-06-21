//go:build e2e

// Package contract contains Probe-authored API-level contract tests for
// Orion. It lives in a sub-package of tests/e2e so it compiles independently
// of the Forge-authored tests in the parent package (tests/e2e/push_test.go,
// lsml_serve_test.go) which currently reference the removed BlueprintNode.OutputAt
// field and fail to compile under -tags e2e. Isolating here lets the contract
// tests run in CI immediately.
//
// This is a bloquant Forge defect signalled to Forge — see Probe report.
package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
	"github.com/ZabLaboratory/Orion/internal/ws"
)

// ─────────────────────────────────────────────────────────────────────────────
// DB helpers
// ─────────────────────────────────────────────────────────────────────────────
//
// e2e DB isolation (Keeper, keeper/orion-e2e-migration-idempotent).
//
// The CI `e2e (Postgres)` job runs the WHOLE tests/e2e/... tree against ONE
// shared Postgres. The prod migrations use bare `CREATE TABLE` (no IF NOT
// EXISTS — they must stay strict), and every `CREATE TABLE` also mints an
// implicit composite row-type in pg_catalog.pg_type. A second apply against an
// already-migrated DB therefore collides on pg_type_typname_nsp_index and
// surfaces as a UNIQUE violation (SQLSTATE 23505) BEFORE the friendlier
// duplicate_table (42P07) path — a code the old idempotency allowlist did not
// absorb, reddening main and skipping deploy.
//
// Rather than keep widening the allowlist (which only masks the crosstalk and
// would re-bite on the next object that surfaces a new SQLSTATE), each
// requireDB now provisions its OWN ephemeral Postgres schema and bakes it into
// the connection search_path, so every test migrates against a guaranteed
// CLEAN namespace. No idempotency tolerance is needed; a re-run against the
// same shared DB is just another fresh schema. The schema is dropped on
// cleanup so a long-lived/persistent DB does not accumulate.

// requireDB opens a store bound to a fresh, isolated Postgres schema, applies
// every migration into it, and drops the schema on cleanup.
func requireDB(t *testing.T) store.Store {
	t.Helper()
	dsn := os.Getenv("ORION_E2E_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORION_E2E_DATABASE_URL not set; skipping e2e")
	}

	schema := newSchemaName()
	provisionSchema(t, dsn, schema)

	st, err := store.Open(context.Background(), dsnWithSearchPath(dsn, schema))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	for _, path := range []string{
		"../../../migrations/0001_init.sql",
		"../../../migrations/0002_lsml_bundle.sql",
		"../../../migrations/0003_scene_validations.sql",
		"../../../migrations/0004_show_state.sql",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		stripped := stripGooseMarkers(string(raw))
		if _, err := st.Pool().Exec(context.Background(), stripped); err != nil {
			t.Fatalf("apply migration %s: %v", path, err)
		}
	}
	return st
}

// newSchemaName returns a Postgres-identifier-safe, collision-free schema name.
func newSchemaName() string {
	return "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// provisionSchema creates the ephemeral schema on a throwaway connection and
// registers its DROP on cleanup. Using a one-shot pgx.Conn (not the store
// pool) avoids any search_path chicken-and-egg with the schema being created.
func provisionSchema(t *testing.T, dsn, schema string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for schema bootstrap: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("create schema %s: %v", schema, err)
	}
	_ = conn.Close(ctx)

	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		_, _ = c.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		_ = c.Close(ctx)
	})
}

// dsnWithSearchPath bakes a libpq `options=-c search_path=<schema>` into the
// DSN so EVERY pooled connection inherits the isolated schema (a per-connection
// SET would not survive pgxpool multiplexing).
func dsnWithSearchPath(dsn, schema string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "options=" + url.QueryEscape("-c search_path="+schema)
}

// stripGooseMarkers removes goose directives and the Down section.
func stripGooseMarkers(s string) string {
	if idx := strings.Index(s, "-- +goose Down"); idx >= 0 {
		s = s[:idx]
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "-- +goose") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP server helpers
// ─────────────────────────────────────────────────────────────────────────────

// newServer wires a minimal PublicDeps and returns an httptest.Server.
// Caller must call ts.Close().
func newServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	t.Cleanup(show.Stop)

	// A zero-value ws.Server is safe to register: RegisterPublic stores
	// the method values but never invokes them for push/status routes.
	wsSrv := &ws.Server{Show: show, Logger: logger, Metrics: metrics}
	deps := api.PublicDeps{
		Logger:  logger,
		Metrics: metrics,
		Config: config.Config{
			PushTimeout: 15 * time.Second,
			LSDPMode:    config.LSDPModeBespoke,
		},
		Show:     show,
		Store:    st,
		Fetcher:  newStubFetcher(),
		WSServer: wsSrv,
	}
	mux := http.NewServeMux()
	api.RegisterPublic(mux, deps)
	return httptest.NewServer(mux)
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub fetcher (minimal scene for compile-through tests)
// ─────────────────────────────────────────────────────────────────────────────

type stubFetcher struct {
	layouts    map[string]*compiler.CanvasLayout
	blueprints map[string]*compiler.BlueprintGraph
	manifest   compiler.ComputeManifest
}

func newStubFetcher() *stubFetcher {
	return &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {
				Version: "v1",
				Root: compiler.LayoutNode{
					Kind: "stack",
					ID:   "root",
					Children: []compiler.LayoutNode{
						{Kind: "text", ID: "score", Bindings: map[string]string{"text": "score.home"}},
					},
				},
			},
		},
		blueprints: map[string]*compiler.BlueprintGraph{
			"bp-probe": {
				ID: "bp-probe",
				Nodes: []compiler.BlueprintNode{
					// core.input@1 declares its leaf path in config.name (ADR 004 §7.2).
					{
						ID:      "out.score",
						Compute: "core.input@1",
						Config:  map[string]json.RawMessage{"name": json.RawMessage(`"score.home"`)},
					},
				},
			},
		},
		manifest: compiler.ComputeManifest{
			"core.input@1": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
}

func (f *stubFetcher) FetchCanvasLayout(_ context.Context, v string) (*compiler.CanvasLayout, error) {
	if l, ok := f.layouts[v]; ok {
		return l, nil
	}
	return nil, fmt.Errorf("stub: layout %q missing", v)
}
func (f *stubFetcher) FetchBlueprint(_ context.Context, id string) (*compiler.BlueprintGraph, error) {
	if b, ok := f.blueprints[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("stub: blueprint %q missing", id)
}
func (f *stubFetcher) FetchBlueprintGraph(_ context.Context, id string, version int) (*compiler.ResolvedBlueprintGraph, error) {
	return nil, fmt.Errorf("stub: blueprint graph %q@%d not seeded", id, version)
}
func (f *stubFetcher) FetchComponent(_ context.Context, _ compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, fmt.Errorf("stub: components not seeded")
}
func (f *stubFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return f.manifest, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Default push envelope
// ─────────────────────────────────────────────────────────────────────────────

func defaultEnvelope() compiler.PushEnvelope {
	return compiler.PushEnvelope{
		CanvasVersion:   "v1",
		BlueBlueprintID: "bp-probe",
	}
}

// freshID generates a new UUID for test isolation.
func freshID() uuid.UUID { return uuid.New() }
