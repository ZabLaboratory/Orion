package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// Golden-file tests anchor the wire shapes from ADR 002. Solar's
// mock-orion fixtures (criterion 16) round-trip these byte-for-byte;
// any deviation here means Solar's existing tests would fail against
// the live Orion. Adding fields is fine — `omitempty` keeps existing
// fixtures byte-stable.

// snapshotGolden is the canonical first-message-after-subscribe.
const snapshotGolden = `{"type":"snapshot","v":1,"scene_id":"scene-42","scene_version":"sha256:abc","sequence":12345,"state":{"score.team_a":14}}`

// deltaGolden is a single-patch delta with cause echoed.
const deltaGolden = `{"type":"delta","v":1,"scene_id":"scene-42","sequence":12346,"patches":[{"path":"score.team_a","value":15}],"cause":{"source":"operator:user-abc","input_id":"ui:score-up"}}`

// sceneChangedGolden is the cross-scene transition signal.
const sceneChangedGolden = `{"type":"scene_changed","v":1,"from_scene_id":"scene-42","to_scene_id":"scene-43"}`

// errorGolden is a non-recoverable error frame.
const errorGolden = `{"type":"error","v":1,"code":"WRITE_FORBIDDEN","message":"path not in scope","recoverable":false}`

func TestGolden_Snapshot(t *testing.T) {
	got, err := Encode(Snapshot{
		SceneID:      "scene-42",
		SceneVersion: "sha256:abc",
		Sequence:     12345,
		State: map[string]json.RawMessage{
			"score.team_a": json.RawMessage(`14`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, []byte(snapshotGolden)) {
		t.Fatalf("snapshot mismatch:\ngot:  %s\nwant: %s", got, snapshotGolden)
	}
}

func TestGolden_Delta(t *testing.T) {
	got, err := Encode(Delta{
		SceneID:  "scene-42",
		Sequence: 12346,
		Patches: []Patch{
			{Path: "score.team_a", Value: json.RawMessage(`15`)},
		},
		Cause: &Cause{Source: "operator:user-abc", InputID: "ui:score-up"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, []byte(deltaGolden)) {
		t.Fatalf("delta mismatch:\ngot:  %s\nwant: %s", got, deltaGolden)
	}
}

func TestGolden_SceneChanged(t *testing.T) {
	got, err := Encode(SceneChanged{
		FromSceneID: "scene-42",
		ToSceneID:   "scene-43",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, []byte(sceneChangedGolden)) {
		t.Fatalf("scene_changed mismatch:\ngot:  %s\nwant: %s", got, sceneChangedGolden)
	}
}

func TestGolden_Error(t *testing.T) {
	got, err := Encode(Error{
		Code:        CodeWriteForbidden,
		Message:     "path not in scope",
		Recoverable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, []byte(errorGolden)) {
		t.Fatalf("error mismatch:\ngot:  %s\nwant: %s", got, errorGolden)
	}
}

func TestGolden_DecodesIncomingFrames(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"subscribe-null", `{"type":"subscribe","v":1,"since_sequence":null}`},
		{"subscribe-with-cursor", `{"type":"subscribe","v":1,"since_sequence":42}`},
		{"input", `{"type":"input","v":1,"path":"match.id","value":"match-8","source":"operator:user-abc","client_msg_id":"uuid-7"}`},
		{"unsubscribe", `{"type":"unsubscribe","v":1}`},
		{"ping", `{"type":"ping","v":1,"nonce":"abc"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Decode([]byte(c.raw)); err != nil {
				t.Fatalf("decode %s: %v", c.raw, err)
			}
		})
	}
}

// TestVersionMismatch_PinsTheCanonicalErrorPath ensures bumping `v`
// yields the typed sentinel — the WS layer routes on this to send a
// VERSION_MISMATCH error frame.
func TestVersionMismatch_PinsTheCanonicalErrorPath(t *testing.T) {
	_, err := Decode([]byte(`{"type":"input","v":99,"path":"x","value":1}`))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
}

// jsonEqual compares two JSON byte slices for semantic equivalence
// (key order tolerant). The Encode path emits keys in struct field
// order so the goldens are byte-stable, but this helper exists for
// the readers' sanity in case Go ever reorders.
func jsonEqual(a, b []byte) bool {
	var ai, bi any
	if err := json.Unmarshal(a, &ai); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bi); err != nil {
		return false
	}
	ar, _ := json.Marshal(ai)
	br, _ := json.Marshal(bi)
	return bytes.Equal(ar, br)
}
