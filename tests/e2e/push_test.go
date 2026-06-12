//go:build e2e

// Package e2e exercises Orion against a live Postgres + stubbed
// upstream services. Run with:
//
//	docker compose -f deploy/compose.yaml up -d orion-postgres
//	ORION_E2E_DATABASE_URL=postgres://orion:CHANGEME@localhost:5447/orion?sslmode=disable \
//	  go test -tags e2e ./tests/e2e/...
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// requireDB returns a freshly migrated database in its OWN ephemeral Postgres
// schema, so each test (and each re-run against a shared/persistent CI
// Postgres) migrates into a guaranteed-clean namespace. See the e2e isolation
// note in tests/e2e/contract/helpers_test.go for the root cause (the prod
// migrations are strict bare CREATE TABLE; a re-apply collides on the implicit
// pg_type row-type with SQLSTATE 23505). The schema is dropped on cleanup.
func requireDB(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("ORION_E2E_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORION_E2E_DATABASE_URL not set; skipping e2e")
	}

	schema := "e2e_" + stripDashes(uuid.NewString())
	provisionSchema(t, dsn, schema)

	st, err := store.Open(context.Background(), dsnWithSearchPath(dsn, schema))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	// Run the migration inline so the test-runner doesn't need goose
	// installed.
	if err := applyMigration(context.Background(), st.Pool()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool) error {
	// Apply every migration in order so the e2e schema matches prod.
	// requireDB guarantees a clean, isolated schema, so the strict prod
	// CREATE TABLE statements (no IF NOT EXISTS) apply exactly once and a
	// duplicate error here is a REAL failure — no idempotency tolerance.
	for _, path := range []string{
		"../../migrations/0001_init.sql",
		"../../migrations/0002_lsml_bundle.sql",
		"../../migrations/0003_scene_validations.sql",
		"../../migrations/0004_show_state.sql",
	} {
		migration, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Strip goose markers so we can exec the raw SQL.
		stripped := stripGoose(string(migration))
		if _, err := pool.Exec(ctx, stripped); err != nil {
			return fmt.Errorf("apply %s: %w", path, err)
		}
	}
	return nil
}

// provisionSchema creates the ephemeral schema on a throwaway connection and
// registers its DROP on cleanup.
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
// DSN so every pooled connection inherits the isolated schema.
func dsnWithSearchPath(dsn, schema string) string {
	sep := "?"
	if indexOf(dsn, "?") >= 0 {
		sep = "&"
	}
	return dsn + sep + "options=" + url.QueryEscape("-c search_path="+schema)
}

// stripDashes removes '-' so a UUID is a bare Postgres identifier fragment.
func stripDashes(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		if c != '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

func stripGoose(s string) string {
	// goose Up section only — drop everything after `-- +goose Down`.
	for _, marker := range []string{"-- +goose Down"} {
		if idx := indexOf(s, marker); idx >= 0 {
			s = s[:idx]
		}
	}
	// Remove `-- +goose ...` lines.
	out := ""
	for _, line := range splitLines(s) {
		if hasPrefix(line, "-- +goose") {
			continue
		}
		out += line + "\n"
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// stubFetcher is a pre-canned fetcher backed by maps the test seeds.
type stubFetcher struct {
	layouts    map[string]*compiler.CanvasLayout
	blueprints map[string]*compiler.BlueprintGraph
	manifest   compiler.ComputeManifest
}

func (f *stubFetcher) FetchCanvasLayout(_ context.Context, v string) (*compiler.CanvasLayout, error) {
	if l, ok := f.layouts[v]; ok {
		return l, nil
	}
	return nil, fmt.Errorf("stub: layout %s missing", v)
}
func (f *stubFetcher) FetchBlueprint(_ context.Context, id string) (*compiler.BlueprintGraph, error) {
	if b, ok := f.blueprints[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("stub: blueprint %s missing", id)
}
func (f *stubFetcher) FetchComponent(_ context.Context, ref compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, fmt.Errorf("stub: components not seeded")
}
func (f *stubFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return f.manifest, nil
}

// Criterion 1: push compiles, advances latest_pushed_version.
func TestE2E_PushAdvancesPointer(t *testing.T) {
	st := requireDB(t)

	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "test-scene"); err != nil {
		t.Fatal(err)
	}

	fetcher := &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {
				Version: "v1",
				Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
			},
		},
		blueprints: map[string]*compiler.BlueprintGraph{
			"bp-1": {
				ID: "bp-1",
				Nodes: []compiler.BlueprintNode{
					// core.input declares its leaf path in config.name (ADR 004
					// §7.2). Replaces the removed BlueprintNode.OutputAt field.
					{
						ID:      "out.x",
						Compute: "core.input",
						Config:  map[string]json.RawMessage{"name": json.RawMessage(`"score"`)},
					},
				},
			},
		},
		manifest: compiler.ComputeManifest{
			"core.input": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}

	graph, bundle, version, err := compiler.Compile(context.Background(), sceneID.String(),
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, fetcher)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	defID := uuid.New()
	def := store.SceneDefinition{
		ID: defID, SceneID: sceneID, DefinitionVersion: 1,
		CanvasVersion: "v1", BlueBlueprintID: "bp-1",
		ComponentsJSON: json.RawMessage(`[]`),
		CreatedAt:      time.Now(),
	}
	if err := st.InsertDefinition(context.Background(), def); err != nil {
		t.Fatal(err)
	}

	err = st.Tx(context.Background(), func(tx pgx.Tx) error {
		gjson, _ := json.Marshal(graph)
		bjson, _ := json.Marshal(bundle)
		pv := store.ScenePushedVersion{
			SceneID:      sceneID,
			SceneVersion: version,
			DefinitionID: defID,
			GraphJSON:    gjson, BundleJSON: bjson,
			CreatedAt: time.Now(),
		}
		if err := st.InsertPushedVersion(context.Background(), tx, pv); err != nil {
			return err
		}
		return st.SetLatestPushedVersion(context.Background(), tx, sceneID, &version)
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.GetScene(context.Background(), sceneID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatestPushedVersion == nil || *got.LatestPushedVersion != version {
		t.Fatalf("pointer not advanced: %+v", got)
	}
}
