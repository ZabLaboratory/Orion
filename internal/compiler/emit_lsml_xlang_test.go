package compiler

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// xlangGolden is the committed cross-language fixture: the canonical
// bytes + content hash that the REAL TS @lumencast/compiler
// (hashBundle() + canonicalize()) produces for a bundle whose text node
// carries the `&`/`<`/`>` HTML-escape trap. It was generated once by
// running @lumencast/compiler against the equivalent layout (see
// testdata/xlang_golden.json `_comment`).
type xlangGolden struct {
	SceneID     string `json:"scene_id"`
	TextValue   string `json:"text_value"`
	BindingPath string `json:"binding_path"`
	Canonical   string `json:"canonical"`
	Hash        string `json:"hash"`
}

func loadXlangGolden(t *testing.T) xlangGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/xlang_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g xlangGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return g
}

// TestEmitLSML_CrossLanguageHashParity is the C4 gate Vigil requires
// (issue #22 acceptance #3): a bundle produced by TS
// (@lumencast/compiler hashBundle) hashes byte-identically to Orion's
// recomputed lsml.HashBundle. This is what makes adopt-on-verify a
// real byte-match (ADR 007 §C.4) rather than a fragile coincidence.
//
// The case deliberately exercises a string containing `&`, `<`, `>` —
// the exact trap the merged HTML-escape parity fix corrected. Go's
// encoding/json escapes those to &/</> by default; the TS
// JSON.stringify does not. lumencast-go's marshalString disables HTML
// escaping (SetEscapeHTML(false)) so the canonical bytes — and thus the
// content hash — stay identical across SDKs. Without that fix this test
// fails on the canonical-bytes assertion.
func TestEmitLSML_CrossLanguageHashParity(t *testing.T) {
	g := loadXlangGolden(t)

	// The Orion layout equivalent to the bundle the TS producer emitted.
	root := LayoutNode{
		Kind: "frame",
		ID:   "root",
		Children: []LayoutNode{
			{
				Kind: "text",
				ID:   "title",
				Props: map[string]json.RawMessage{
					"value": json.RawMessage(mustQuote(t, g.TextValue)),
				},
				Bindings: map[string]string{"value": g.BindingPath},
			},
		},
	}

	bundle, version, canon, err := EmitLSML(g.SceneID, root, nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	// 1) The canonical bytes Orion hashes must be byte-identical to the
	//    TS canonical form. This is the assertion the HTML-escape fix
	//    makes pass: if Go re-escaped `&`/`<`/`>`, these would differ.
	if string(canon) != g.Canonical {
		t.Fatalf("canonical bytes drift between Go and TS:\n Go: %s\n TS: %s", canon, g.Canonical)
	}

	// 2) The content hash Orion computes must equal the TS-produced hash.
	//    This is the value adopt-on-verify compares against the
	//    Canvas-supplied lsml_bundle_hash (ADR 007 §C.4).
	if version != g.Hash {
		t.Fatalf("Orion hash %q != TS golden hash %q", version, g.Hash)
	}

	// 3) Sanity: re-hashing the sealed bundle via lsml.HashBundle (the
	//    same function the parity fix lives in) reproduces the hash —
	//    the path adopt-on-verify actually exercises.
	hexHash, _, err := lsml.HashBundle(bundle)
	if err != nil {
		t.Fatalf("HashBundle: %v", err)
	}
	if "sha256:"+hexHash != g.Hash {
		t.Fatalf("lsml.HashBundle %q != TS golden hash %q", "sha256:"+hexHash, g.Hash)
	}

	// Guard the fixture itself carries the trap, so a future edit that
	// drops the HTML specials doesn't silently weaken the test.
	if !strings.ContainsAny(g.TextValue, "&<>") {
		t.Fatalf("golden text_value %q must contain &/<>/ — the HTML-escape trap", g.TextValue)
	}
}

// mustQuote JSON-encodes a string into a raw JSON literal (`"..."`),
// matching how Canvas would store a static text prop.
func mustQuote(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	return string(raw)
}
