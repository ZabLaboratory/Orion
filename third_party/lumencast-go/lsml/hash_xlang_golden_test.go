package lsml_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// Cross-language hashBundle golden — gate for ADR 007 §C4 (adopt-on-verify).
//
// These golden values are the canonical JSON + sha256 produced by the TS
// reference implementation @lumencast/compiler (canonicalize.ts, v0.2.0),
// captured empirically. The adopt-on-verify path only stays on the new
// engine if Go lsml.HashBundle agrees byte-for-byte with the TS hash for
// the SAME logical bundle; any mismatch silently falls back to legacy.
//
// Each sub-test feeds the SAME logical bundle to Go HashBundle and asserts
// the canonical bytes + hash match the TS golden.
//
//   - case_a_float   : exponential / shortest-decimal floats        -> MATCH (verified)
//   - case_b_html    : strings containing & < >                      -> MATCH (since fix)
//   - case_c_opt     : optional field absent                        -> MATCH (verified)
//
// RESOLVED DIVERGENCE (case_b_html), reported 2026-06-05, fixed in
// hash.go via marshalString (json.Encoder.SetEscapeHTML(false)):
//   Go's encoding/json escapes &,<,> as &,<,> by default
//   (SetEscapeHTML(true)); TS JSON.stringify does NOT. hash.go now disables
//   HTML escaping for both string values and object keys so the canonical
//   form is byte-identical to canonicalize.ts §3. case_b_html is now a
//   permanent regression golden locking the TS hash — any future SDK change
//   that reintroduces escaping turns this test red.

type xlangGolden struct {
	name      string
	bundle    string // raw LSML 1.1 JSON, scene_version already zeroed
	tsHash    string // sha256 from @lumencast/compiler.hashBundle
	tsCanon   string // canonical JSON string from canonicalize.ts
	expectGap bool   // true = known divergence, gate intentionally red
}

func xlangGoldenCases() []xlangGolden {
	return []xlangGolden{
		{
			name:    "case_a_float",
			bundle:  `{"lsml":"1.1","scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000","layout":{"kind":"stack"},"defaults":{"tiny":0.0000001,"exp":1.5e-10,"whole":2.0,"big":1234567890123456789}}`,
			tsHash:  "16dee731508082b869796d77d45832e1d780866259ab48f5918e12c547c94662",
			tsCanon: `{"defaults":{"big":1234567890123456800,"exp":1.5e-10,"tiny":1e-7,"whole":2},"layout":{"kind":"stack"},"lsml":"1.1","scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`,
		},
		{
			name:    "case_b_html",
			bundle:  `{"lsml":"1.1","scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000","layout":{"kind":"stack"},"metadata":{"title":"A & B <live>"}}`,
			tsHash:  "7050dd0c6c1a92a174db87b457eb66205519cd87ac583694f97c8c3fb7da097c",
			tsCanon: `{"layout":{"kind":"stack"},"lsml":"1.1","metadata":{"title":"A & B <live>"},"scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`,
		},
		{
			name:    "case_c_optional_absent",
			bundle:  `{"lsml":"1.1","scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000","layout":{"kind":"stack"}}`,
			tsHash:  "f3f9db9b4436fe3ba31794e74d5c5959f94e90360c78d783adcd71989e2bd85c",
			tsCanon: `{"layout":{"kind":"stack"},"lsml":"1.1","scene_id":"s","scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`,
		},
	}
}

func TestHashBundle_CrossLanguageGolden(t *testing.T) {
	for _, tc := range xlangGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			var b lsml.Bundle
			if err := json.Unmarshal([]byte(tc.bundle), &b); err != nil {
				t.Fatalf("unmarshal bundle: %v", err)
			}
			goHash, goCanon, err := lsml.HashBundle(&b)
			if err != nil {
				t.Fatalf("HashBundle: %v", err)
			}

			canonMatch := string(goCanon) == tc.tsCanon
			hashMatch := goHash == tc.tsHash

			if tc.expectGap {
				// Known SDK divergence. We assert it is STILL divergent so
				// the gate flags the moment the fix lands (then flip expectGap).
				if canonMatch && hashMatch {
					t.Fatalf("KNOWN DIVERGENCE RESOLVED for %s — Go and TS now agree. "+
						"Remove expectGap and treat this case as a normal golden.", tc.name)
				}
				t.Logf("known divergence (expected) — Go canon: %s | TS canon: %s", goCanon, tc.tsCanon)
				t.Logf("Go hash: %s | TS hash: %s", goHash, tc.tsHash)
				return
			}

			if !canonMatch {
				t.Errorf("canonical bytes diverge for %s\n  go: %s\n  ts: %s", tc.name, goCanon, tc.tsCanon)
			}
			if !hashMatch {
				t.Errorf("sha256 diverges for %s\n  go: %s\n  ts: %s", tc.name, goHash, tc.tsHash)
			}
		})
	}
}

// TestHashRaw_CrossLanguageGolden pins HashRaw on the SAME TS-produced
// goldens, fed as the raw bytes they are — no struct in the path. Every
// golden here is struct-representable, so this is the case where HashRaw and
// HashBundle MUST agree; the divergence HashRaw exists for is covered by
// TestHashRaw_PreservesMembersBundleDoesNotDeclare.
func TestHashRaw_CrossLanguageGolden(t *testing.T) {
	for _, tc := range xlangGoldenCases() {
		if tc.expectGap {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			rawHash, rawCanon, err := lsml.HashRaw([]byte(tc.bundle))
			if err != nil {
				t.Fatalf("HashRaw: %v", err)
			}
			if string(rawCanon) != tc.tsCanon {
				t.Errorf("canonical bytes diverge from TS\n  go: %s\n  ts: %s", rawCanon, tc.tsCanon)
			}
			if rawHash != tc.tsHash {
				t.Errorf("sha256 diverges from TS\n  go: %s\n  ts: %s", rawHash, tc.tsHash)
			}

			// And it agrees with the struct path on the inputs the struct can
			// carry — HashBundle delegates here, so a divergence would mean
			// the marshal step is not the identity it claims to be.
			var b lsml.Bundle
			if err := json.Unmarshal([]byte(tc.bundle), &b); err != nil {
				t.Fatalf("unmarshal bundle: %v", err)
			}
			bundleHash, _, err := lsml.HashBundle(&b)
			if err != nil {
				t.Fatalf("HashBundle: %v", err)
			}
			if bundleHash != rawHash {
				t.Errorf("HashBundle %s != HashRaw %s on a struct-representable bundle",
					bundleHash, rawHash)
			}
		})
	}
}

// The reason HashRaw exists. Bundle is a typed VIEW: a member it does not
// declare is dropped on unmarshal, so hashing the struct hashes something
// the sender never sent. A verifier built on HashBundle would report drift
// on every document carrying such a member — here `animations`, which real
// producers do carry — and the report would be about this struct's field
// list, not about the document.
func TestHashRaw_PreservesMembersBundleDoesNotDeclare(t *testing.T) {
	const raw = `{"lsml":"1.1","scene_id":"s",` +
		`"scene_version":"sha256:0000000000000000000000000000000000000000000000000000000000000000",` +
		`"layout":{"kind":"stack"},"animations":{"pop":{"target":"score","keyframes":[]}}}`

	rawHash, rawCanon, err := lsml.HashRaw([]byte(raw))
	if err != nil {
		t.Fatalf("HashRaw: %v", err)
	}
	if !strings.Contains(string(rawCanon), `"animations"`) {
		t.Fatalf("HashRaw dropped a member it must preserve: %s", rawCanon)
	}

	var b lsml.Bundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	structHash, structCanon, err := lsml.HashBundle(&b)
	if err != nil {
		t.Fatalf("HashBundle: %v", err)
	}
	if strings.Contains(string(structCanon), `"animations"`) {
		t.Fatal("Bundle now declares animations — this test's premise is stale, revisit it")
	}
	if structHash == rawHash {
		t.Fatal("struct and raw hashes agree despite a dropped member: the loss is undetectable")
	}
}

// A bundle with no scene_version at all still hashes: setSceneVersion only
// substitutes a member that exists. Guards the verifier against panicking on
// a malformed document instead of simply reporting a mismatch.
func TestHashRaw_ToleratesAbsentSceneVersion(t *testing.T) {
	if _, _, err := lsml.HashRaw([]byte(`{"lsml":"1.1","layout":{"kind":"stack"}}`)); err != nil {
		t.Fatalf("HashRaw on a bundle without scene_version: %v", err)
	}
}

// Malformed JSON is an error, never a hash — a verifier must not be able to
// mistake garbage for a value that simply differs.
func TestHashRaw_RejectsMalformedJSON(t *testing.T) {
	if _, _, err := lsml.HashRaw([]byte(`{"lsml":`)); err == nil {
		t.Fatal("HashRaw accepted malformed JSON")
	}
}
