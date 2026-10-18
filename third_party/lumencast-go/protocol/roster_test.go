package protocol

import (
	"testing"
)

// TestEncodeSceneRoster pins the on-wire shape of the scene_roster
// frame against the contract mirrored by @lumencast/protocol
// (Lumencast/lumencast-js#88) : { v, type, entries[], ts? }, no seq.
func TestEncodeSceneRoster(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "populated",
			in: SceneRoster{
				Entries: []RosterEntry{
					{SceneID: "main-stage", SceneVersion: "sha256:abc"},
					{SceneID: "intermission", SceneVersion: "sha256:def"},
				},
			},
			want: `{"v":1,"type":"scene_roster","entries":[{"scene_id":"main-stage","scene_version":"sha256:abc"},{"scene_id":"intermission","scene_version":"sha256:def"}]}`,
		},
		{
			// entries is required and MUST encode as [] — never null.
			name: "empty entries render as array",
			in:   SceneRoster{},
			want: `{"v":1,"type":"scene_roster","entries":[]}`,
		},
		{
			name: "with ts",
			in: SceneRoster{
				Entries: []RosterEntry{{SceneID: "s", SceneVersion: "sha256:0"}},
				TS:      "2026-07-02T10:00:00Z",
			},
			want: `{"v":1,"type":"scene_roster","entries":[{"scene_id":"s","scene_version":"sha256:0"}],"ts":"2026-07-02T10:00:00Z"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("encode mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestSceneRosterRoundTrip proves a server-emitted roster decodes back
// to the same typed frame — the parity check a runtime relies on.
func TestSceneRosterRoundTrip(t *testing.T) {
	orig := SceneRoster{
		Entries: []RosterEntry{
			{SceneID: "main-stage", SceneVersion: "sha256:abc"},
		},
		TS: "2026-07-02T10:00:00Z",
	}
	raw, err := Encode(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := msg.(*SceneRoster)
	if !ok {
		t.Fatalf("decoded %T, want *SceneRoster", msg)
	}
	if got.V != Version || got.Type != TypeSceneRoster {
		t.Fatalf("envelope not stamped: v=%d type=%q", got.V, got.Type)
	}
	if len(got.Entries) != 1 || got.Entries[0].SceneID != "main-stage" ||
		got.Entries[0].SceneVersion != "sha256:abc" {
		t.Fatalf("entries round-trip mismatch: %+v", got.Entries)
	}
	if got.TS != orig.TS {
		t.Fatalf("ts round-trip mismatch: %q", got.TS)
	}
}
