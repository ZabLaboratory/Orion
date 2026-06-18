package compiler

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// xlangMultiGolden is the committed cross-language fixture for the
// N-blueprint LSML bundle (ADR 001 criterion 7, extending issue #18). The
// blueprint dimension surfaces in the LSML bundle solely through the layout
// bindings, which now carry the "<key>.<leaf>" path form — so the golden is a
// layout binding two text nodes to two distinct blueprint keys.
//
// TsCanonical / TsHash are the values the REAL TS @lumencast/compiler must
// produce; they are PLACEHOLDERS (empty) until regenerated TS-side and
// committed (R2 gate). The parity assertion is skipped-with-failure-marker
// while empty so CI cannot go green on an un-cross-checked hash.
type xlangMultiGolden struct {
	SceneID      string `json:"scene_id"`
	ScoreBinding string `json:"score_binding"`
	TimerBinding string `json:"timer_binding"`
	TsCanonical  string `json:"ts_canonical"`
	TsHash       string `json:"ts_hash"`
}

func loadXlangMultiGolden(t *testing.T) xlangMultiGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/xlang_golden_multiblueprint.json")
	if err != nil {
		t.Fatalf("read multi-blueprint golden: %v", err)
	}
	var g xlangMultiGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse multi-blueprint golden: %v", err)
	}
	return g
}

// multiBlueprintLSMLRoot is the layout whose two text nodes bind to two
// distinct blueprint keys (score / timer) — the LSML projection of an
// N-blueprint scene. This is the bundle the TS side must hash identically.
func multiBlueprintLSMLRoot(g xlangMultiGolden) LayoutNode {
	return LayoutNode{
		Kind: "frame",
		ID:   "root",
		Children: []LayoutNode{
			{
				Kind:     "text",
				ID:       "score",
				Bindings: map[string]string{"value": g.ScoreBinding},
			},
			{
				Kind:     "text",
				ID:       "timer",
				Bindings: map[string]string{"value": g.TimerBinding},
			},
		},
	}
}

// TestEmitLSML_CrossLanguageHashParity_MultiBlueprint is the criterion-7 gate:
// the N-blueprint LSML bundle must hash byte-identically across Go and TS.
//
// HONESTY NOTE: this machine has no TS toolchain, so the TS-side hash cannot
// be minted here. The fixture carries empty TS fields until Conduit/Canvas
// regenerate them with the real @lumencast/compiler. While empty the test
// HARD-FAILS on the parity assertion (it must not pass on a Go-only value).
// The Go-side self-consistency + determinism is asserted unconditionally so
// the wiring is exercised now.
func TestEmitLSML_CrossLanguageHashParity_MultiBlueprint(t *testing.T) {
	g := loadXlangMultiGolden(t)
	root := multiBlueprintLSMLRoot(g)

	bundle, version, canon, err := EmitLSML(g.SceneID, root, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	// Go self-consistency: re-hashing the sealed bundle reproduces `version`.
	hexHash, _, err := lsml.HashBundle(bundle)
	if err != nil {
		t.Fatalf("HashBundle: %v", err)
	}
	if "sha256:"+hexHash != version {
		t.Fatalf("Go self-inconsistency: EmitLSML %q != HashBundle %q", version, "sha256:"+hexHash)
	}

	// The "<key>.<leaf>" binding paths must survive into the canonical bytes
	// — that is the only place the N-blueprint dimension lives in the LSML.
	if !strings.Contains(string(canon), g.ScoreBinding) || !strings.Contains(string(canon), g.TimerBinding) {
		t.Fatalf("keyed binding paths absent from canonical LSML: %s", canon)
	}

	// Cross-language parity: REQUIRED before merge (R2). Fail loudly while the
	// TS golden is a placeholder so CI cannot go green un-cross-checked.
	if g.TsHash == "" || g.TsCanonical == "" {
		t.Fatal("multi-blueprint xlang golden has placeholder TS fields — " +
			"Conduit/Canvas must regenerate ts_hash/ts_canonical with the real " +
			"@lumencast/compiler and commit them before merge (ADR 001 criterion 7 / R2)")
	}
	if string(canon) != g.TsCanonical {
		t.Fatalf("canonical bytes drift Go vs TS:\n Go: %s\n TS: %s", canon, g.TsCanonical)
	}
	if version != g.TsHash {
		t.Fatalf("Orion hash %q != TS golden hash %q", version, g.TsHash)
	}
}
