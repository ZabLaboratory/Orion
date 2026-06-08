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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// isMigrationIdempotentError reports whether an applyMigration error is
// benign because the schema objects already exist.
// Postgres codes: 42P07 (duplicate_table), 42701 (duplicate_column).
func isMigrationIdempotentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "42P07") ||
		strings.Contains(msg, "42701")
}

// requireDB opens a store against ORION_E2E_DATABASE_URL, applies migrations
// (tolerating idempotent duplicate errors), and registers a cleanup.
func requireDB(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("ORION_E2E_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORION_E2E_DATABASE_URL not set; skipping e2e")
	}
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	// Apply each migration independently; absorb idempotent errors.
	for _, path := range []string{
		"../../../migrations/0001_init.sql",
		"../../../migrations/0002_lsml_bundle.sql",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		stripped := stripGooseMarkers(string(raw))
		if _, err := st.Pool().Exec(context.Background(), stripped); err != nil {
			if !isMigrationIdempotentError(err) {
				t.Fatalf("apply migration %s: %v", path, err)
			}
			// Benign: schema already exists.
		}
	}
	return st
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
func newServer(t *testing.T, st *store.Store) *httptest.Server {
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
