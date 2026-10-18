package lsml_test

import (
	"encoding/json"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

func TestValidate_Minimal(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:abc",
		Layout:       json.RawMessage(`{"kind":"text","bind":{"value":"x"}}`),
	}
	rep := lsml.Validate(b)
	if len(rep.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", rep.Errors)
	}
}

func TestValidate_ImageRequiresAlt(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:abc",
		Layout: json.RawMessage(`{
			"kind": "frame",
			"size": {"w":100,"h":100},
			"children": [
				{"kind":"image","bind":{"src":"team.logo"},"size":{"w":10,"h":10}}
			]
		}`),
	}
	rep := lsml.Validate(b)
	if len(rep.Errors) == 0 {
		t.Fatal("missing alt should error")
	}
}

func TestValidate_AnimateDiscipline(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:abc",
		Layout: json.RawMessage(`{
			"kind": "frame",
			"animate": { "width": 200 }
		}`),
	}
	rep := lsml.Validate(b)
	if len(rep.Errors) == 0 {
		t.Fatal("animating width should error")
	}
}

func TestValidate_UnknownPrimitive(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:abc",
		Layout:       json.RawMessage(`{"kind":"unicorn"}`),
	}
	rep := lsml.Validate(b)
	if len(rep.Errors) == 0 {
		t.Fatal("unknown primitive should error")
	}
}

func TestValidate_RejectsBadVersion(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "2.0",
		SceneID:      "main",
		SceneVersion: "sha256:abc",
		Layout:       json.RawMessage(`{"kind":"text","bind":{"value":"x"}}`),
	}
	rep := lsml.Validate(b)
	if len(rep.Errors) == 0 {
		t.Fatal("LSML 2.0 should error")
	}
}

func TestHashBundle_Stable(t *testing.T) {
	b := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:placeholder",
		Layout:       json.RawMessage(`{"kind":"text","bind":{"value":"x"}}`),
		Defaults:     map[string]any{"x": "Hello"},
	}
	h1, _, err := lsml.HashBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := lsml.HashBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hex length = %d, want 64", len(h1))
	}
}

func TestHashBundle_IndependentOfSceneVersion(t *testing.T) {
	b1 := &lsml.Bundle{
		LSML:         "1.0",
		SceneID:      "main",
		SceneVersion: "sha256:aaa",
		Layout:       json.RawMessage(`{"kind":"text","bind":{"value":"x"}}`),
	}
	b2 := *b1
	b2.SceneVersion = "sha256:bbb"
	h1, _, _ := lsml.HashBundle(b1)
	h2, _, _ := lsml.HashBundle(&b2)
	if h1 != h2 {
		t.Fatalf("scene_version influences hash: %s vs %s", h1, h2)
	}
}
