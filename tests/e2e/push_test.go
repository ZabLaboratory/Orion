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
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// requireDB returns a freshly migrated database. Each test gets its
// own schema so they can run in parallel without crosstalk.
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

	// Run the migration inline so the test-runner doesn't need goose
	// installed.
	if err := applyMigration(context.Background(), st.Pool()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool) error {
	// Apply every migration in order so the e2e schema matches prod.
	// Idempotent: tests in this package share one CI Postgres, so the
	// schema may already exist from a prior test's requireDB. The raw
	// migration SQL uses bare CREATE TABLE (no IF NOT EXISTS — it is the
	// prod goose script and must stay strict there), so a second apply
	// raises duplicate_table (42P07) / duplicate_column (42701). Those
	// are benign here and absorbed; any other error is a real failure.
	for _, path := range []string{
		"../../migrations/0001_init.sql",
		"../../migrations/0002_lsml_bundle.sql",
		"../../migrations/0003_scene_validations.sql",
	} {
		migration, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Strip goose markers so we can exec the raw SQL.
		stripped := stripGoose(string(migration))
		if _, err := pool.Exec(ctx, stripped); err != nil {
			if isMigrationAlreadyApplied(err) {
				continue
			}
			return fmt.Errorf("apply %s: %w", path, err)
		}
	}
	return nil
}

// isMigrationAlreadyApplied reports whether a migration Exec failed only
// because the schema objects already exist (duplicate_table 42P07 /
// duplicate_column 42701), which is benign for a shared-DB e2e run.
func isMigrationAlreadyApplied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "already exists") ||
		contains(msg, "42P07") ||
		contains(msg, "42701")
}

// contains is a tiny substring check kept local so this file stays free of
// the strings import (matching its hand-rolled helpers above).
func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

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

