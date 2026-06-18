package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// silentDeps returns PublicDeps with a no-op logger — enough to exercise
// the pure C4 adopt-on-verify helper without a DB or HTTP server.
func silentDeps() PublicDeps {
	return PublicDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// representativeBundle is a small but non-trivial compiled render tree
// that EmitLSML can seal. It deliberately carries a string with HTML
// special characters (`&`, `<`, `>`) so the hash we recompute is the
// exact value the HTML-escape parity fix targets (ADR 007 §C.4).
//
// AuthoringRoot mirrors Root here: this fixture uses no authoring-only
// vocab (`value` is read identically in both vocabs), so the authoring
// and lowered trees coincide. The C4 path hashes AuthoringRoot (the LSML
// bundle is authoring-vocab, ADR 007 §9.6), so the fixture must set it —
// a real compile fills it with the pre-lowering `expanded` tree
// (compile.go). Leaving it zero would emit an empty-tree LSML and break
// the adopt-on-verify match (the wiring this test guards).
func representativeBundle() *compiler.RenderBundle {
	root := compiler.LayoutNode{
		Kind: "frame",
		ID:   "root",
		Children: []compiler.LayoutNode{
			{
				Kind: "text",
				ID:   "title",
				Props: map[string]json.RawMessage{
					// The HTML-escape trap: `&`/`<`/`>` must survive
					// byte-identically across the TS and Go serializers.
					"value": json.RawMessage(`"A & B < C > D"`),
				},
				Bindings: map[string]string{"value": "scene.title"},
			},
		},
	}
	return &compiler.RenderBundle{
		Root:          root,
		AuthoringRoot: root,
	}
}

// orionHashOf returns the LSML content address Orion computes for a
// bundle, the same way the push handler does in dual|lsdp mode: from the
// AUTHORING tree (ADR 007 §9.6 / §C.4), NOT the lowered render Root.
func orionHashOf(t *testing.T, sceneID string, bundle *compiler.RenderBundle) string {
	t.Helper()
	_, hash, _, err := compiler.EmitLSML(sceneID, bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	return hash
}

// Acceptance #1 — byte-match: Orion adopts the Canvas hash as the
// scene identity. The returned scene_version equals the LSML content
// address AND the persisted row is realigned (pv.SceneVersion ==
// pv.LSMLBundleHash == the adopted hash), so the C2 serve at
// /lsml-bundle?v={scene_version} resolves on the unified identity.
func TestPersistLSML_ByteMatchAdoptsCanvasHash(t *testing.T) {
	deps := silentDeps()
	sceneID := uuid.New()
	bundle := representativeBundle()
	legacyVersion := "sha256:legacy-graph-bundle-mint"

	canvasHash := orionHashOf(t, sceneID.String(), bundle)

	pv := store.ScenePushedVersion{SceneID: sceneID, SceneVersion: legacyVersion}
	got := persistLSMLAndMaybeAdopt(deps, sceneID, legacyVersion, canvasHash, bundle, &pv)

	if got != canvasHash {
		t.Fatalf("adopted scene_version = %q, want the Canvas hash %q", got, canvasHash)
	}
	if pv.SceneVersion != canvasHash {
		t.Fatalf("pv.SceneVersion = %q, want realigned to %q", pv.SceneVersion, canvasHash)
	}
	if pv.LSMLBundleHash == nil || *pv.LSMLBundleHash != canvasHash {
		t.Fatalf("pv.LSMLBundleHash = %v, want %q (addresses must collapse)", pv.LSMLBundleHash, canvasHash)
	}
	if len(pv.LSMLBundleJSON) == 0 {
		t.Fatal("LSML bundle bytes must be persisted on adoption")
	}
}

// Acceptance #2 — mismatch: Orion mints legacy + warns, never adopts,
// never fails. The LSML bundle is still persisted under Orion's own
// hash (so the C2 serve still works for the LSML address), but the
// scene_version stays the legacy mint (the addresses do NOT collapse).
func TestPersistLSML_MismatchKeepsLegacyMint(t *testing.T) {
	deps := silentDeps()
	sceneID := uuid.New()
	bundle := representativeBundle()
	legacyVersion := "sha256:legacy-graph-bundle-mint"

	orionHash := orionHashOf(t, sceneID.String(), bundle)
	canvasHash := "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	if canvasHash == orionHash {
		t.Fatal("test setup: canvasHash must differ from the real Orion hash")
	}

	pv := store.ScenePushedVersion{SceneID: sceneID, SceneVersion: legacyVersion}
	got := persistLSMLAndMaybeAdopt(deps, sceneID, legacyVersion, canvasHash, bundle, &pv)

	if got != legacyVersion {
		t.Fatalf("scene_version = %q, want legacy mint %q on mismatch", got, legacyVersion)
	}
	if pv.SceneVersion != legacyVersion {
		t.Fatalf("pv.SceneVersion = %q, want legacy mint preserved", pv.SceneVersion)
	}
	// The LSML bundle is still persisted (under Orion's own hash) so the
	// C2 LSML serve works even when identity did not collapse.
	if pv.LSMLBundleHash == nil || *pv.LSMLBundleHash != orionHash {
		t.Fatalf("pv.LSMLBundleHash = %v, want Orion's own hash %q", pv.LSMLBundleHash, orionHash)
	}
}

// Absent Canvas hash (old/legacy Canvas): persist the LSML bundle for
// the C2 serve, but do NOT collapse identity — scene_version stays the
// legacy mint. No warning (absence is normal, not drift).
func TestPersistLSML_AbsentHashKeepsLegacyMint(t *testing.T) {
	deps := silentDeps()
	sceneID := uuid.New()
	bundle := representativeBundle()
	legacyVersion := "sha256:legacy-graph-bundle-mint"

	pv := store.ScenePushedVersion{SceneID: sceneID, SceneVersion: legacyVersion}
	got := persistLSMLAndMaybeAdopt(deps, sceneID, legacyVersion, "", bundle, &pv)

	if got != legacyVersion {
		t.Fatalf("scene_version = %q, want legacy mint %q when no Canvas hash supplied", got, legacyVersion)
	}
	if pv.SceneVersion != legacyVersion {
		t.Fatalf("pv.SceneVersion = %q, want legacy mint preserved", pv.SceneVersion)
	}
	if pv.LSMLBundleHash == nil {
		t.Fatal("LSML bundle must still be persisted for the C2 serve")
	}
}

// The adopted hash is exactly what the C2 GetLSMLBundleByHash lookup
// keys on: after adoption pv.SceneVersion == *pv.LSMLBundleHash, which
// is what makes /lsml-bundle?v={scene_version} resolve (acceptance #1,
// addresses collapsed).
func TestPersistLSML_AdoptedAddressesAreIdentical(t *testing.T) {
	deps := silentDeps()
	sceneID := uuid.New()
	bundle := representativeBundle()

	canvasHash := orionHashOf(t, sceneID.String(), bundle)
	pv := store.ScenePushedVersion{SceneID: sceneID, SceneVersion: "sha256:legacy"}
	_ = persistLSMLAndMaybeAdopt(deps, sceneID, "sha256:legacy", canvasHash, bundle, &pv)

	if pv.LSMLBundleHash == nil || pv.SceneVersion != *pv.LSMLBundleHash {
		t.Fatalf("post-adopt scene_version %q must equal LSML hash %v (collapsed address)",
			pv.SceneVersion, pv.LSMLBundleHash)
	}
}
