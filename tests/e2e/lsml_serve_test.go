//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// seedLSMLScene compiles a representative scene, emits its LSML bundle,
// and persists the pushed version with the LSML columns populated (the
// dual|lsdp mode shape). Returns the LSML content address and the
// stored bundle bytes for byte-equal assertions.
func seedLSMLScene(t *testing.T, st *store.Store, sceneID uuid.UUID) (string, []byte) {
	t.Helper()
	ctx := context.Background()

	if _, err := st.CreateScene(ctx, sceneID, "lsml-scene"); err != nil {
		t.Fatal(err)
	}

	fetcher := &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {
				Version: "v1",
				Root: compiler.LayoutNode{
					Kind: "stack",
					ID:   "root",
					Children: []compiler.LayoutNode{
						{Kind: "text", ID: "title", Bindings: map[string]string{"text": "score.home"}},
					},
				},
			},
		},
		blueprints: map[string]*compiler.BlueprintGraph{
			"bp-1": {
				ID: "bp-1",
				Nodes: []compiler.BlueprintNode{
					{ID: "out.x", Compute: "core.input", OutputAt: "score.home"},
				},
			},
		},
		manifest: compiler.ComputeManifest{
			"core.input": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}

	_, bundle, version, err := compiler.Compile(ctx, sceneID.String(),
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, fetcher)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// C2: emit the LSML bundle the same way the push handler does in
	// dual|lsdp mode.
	lsmlBundle, lsmlHash, _, err := compiler.EmitLSML(
		sceneID.String(), bundle.Root, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if err != nil {
		t.Fatalf("emit lsml: %v", err)
	}
	lsmlBytes, err := json.Marshal(lsmlBundle)
	if err != nil {
		t.Fatalf("marshal lsml: %v", err)
	}

	defID := uuid.New()
	if err := st.InsertDefinition(ctx, store.SceneDefinition{
		ID: defID, SceneID: sceneID, DefinitionVersion: 1,
		CanvasVersion: "v1", BlueBlueprintID: "bp-1",
		ComponentsJSON: json.RawMessage(`[]`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err = st.Tx(ctx, func(tx pgx.Tx) error {
		gjson, _ := json.Marshal(struct{}{})
		bjson, _ := json.Marshal(bundle)
		pv := store.ScenePushedVersion{
			SceneID:        sceneID,
			SceneVersion:   version,
			DefinitionID:   defID,
			GraphJSON:      gjson,
			BundleJSON:     bjson,
			LSMLBundleJSON: lsmlBytes,
			LSMLBundleHash: &lsmlHash,
			CreatedAt:      time.Now(),
		}
		if err := st.InsertPushedVersion(ctx, tx, pv); err != nil {
			return err
		}
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &version)
	})
	if err != nil {
		t.Fatal(err)
	}

	return lsmlHash, lsmlBytes
}

// Acceptance #1 + #2 (store half): a pushed version persists its LSML
// bundle keyed by the LSML content address, and a by-hash read returns
// the exact bytes.
func TestE2E_LSMLPersistServeRoundTrip(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()

	lsmlHash, want := seedLSMLScene(t, st, sceneID)

	got, err := st.GetLSMLBundleByHash(context.Background(), sceneID, lsmlHash)
	if err != nil {
		t.Fatalf("get lsml by hash: %v", err)
	}
	// Postgres jsonb is not byte-preserving; compare the canonical
	// re-marshalled forms instead so the round-trip is semantic.
	if !jsonEqual(t, got, want) {
		t.Fatalf("lsml bytes mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// Acceptance #2 (content-addressing): a GET for a hash that was never
// persisted resolves to ErrNotFound (→ 404 at the API layer).
func TestE2E_LSMLUnknownHashIsNotFound(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()

	seedLSMLScene(t, st, sceneID)

	_, err := st.GetLSMLBundleByHash(context.Background(), sceneID,
		"sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown hash, got %v", err)
	}
}

// Acceptance #3 (bespoke serve unchanged): even with LSML persisted, the
// bespoke render-bundle artefact is still readable by scene_version and
// is a DISTINCT byte stream from the LSML bundle.
func TestE2E_BespokeServeUnchangedWithLSML(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()

	lsmlHash, lsmlBytes := seedLSMLScene(t, st, sceneID)

	scene, err := st.GetScene(context.Background(), sceneID)
	if err != nil {
		t.Fatal(err)
	}
	if scene.LatestPushedVersion == nil {
		t.Fatal("scene has no latest pushed version")
	}

	pv, err := st.GetLatestPushedVersion(context.Background(), sceneID)
	if err != nil {
		t.Fatalf("get bespoke pushed version: %v", err)
	}
	if len(pv.BundleJSON) == 0 {
		t.Fatal("bespoke bundle bytes are empty")
	}
	// The bespoke scene_version and the LSML content address are two
	// distinct addresses for two artefacts (ADR 007 §C.4 — collapse is
	// C4, not C2). They MUST differ here.
	if pv.SceneVersion == lsmlHash {
		t.Fatalf("bespoke scene_version unexpectedly equals lsml hash: %s", lsmlHash)
	}
	if jsonEqual(t, pv.BundleJSON, lsmlBytes) {
		t.Fatal("bespoke RenderBundle bytes must differ from the LSML bundle bytes")
	}
}

// jsonEqual reports whether two raw JSON byte streams are semantically
// equal (key-order and whitespace insensitive) — Postgres jsonb storage
// reorders keys, so a raw bytes.Equal would be too strict.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return string(ca) == string(cb)
}
