package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEncodeServerFrames(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "snapshot",
			in: Snapshot{
				Seq:          1,
				SceneID:      "main-stage",
				SceneVersion: "sha256:abc",
				State: map[string]json.RawMessage{
					"show.title": json.RawMessage(`"Hello"`),
				},
			},
			want: `{"v":1,"type":"snapshot","seq":1,"scene_id":"main-stage","scene_version":"sha256:abc","state":{"show.title":"Hello"}}`,
		},
		{
			name: "delta",
			in: Delta{
				Seq: 42,
				Patches: []Patch{
					{Path: "players.0.score", Value: json.RawMessage(`7`)},
				},
			},
			want: `{"v":1,"type":"delta","seq":42,"patches":[{"path":"players.0.score","value":7}]}`,
		},
		{
			name: "scene_changed",
			in: SceneChanged{
				Seq:          100,
				SceneID:      "intermission",
				SceneVersion: "sha256:def",
			},
			want: `{"v":1,"type":"scene_changed","seq":100,"scene_id":"intermission","scene_version":"sha256:def"}`,
		},
		{
			name: "error",
			in: Error{
				Seq:         50,
				Code:        string(CodeWriteForbidden),
				Message:     "viewer cannot send input",
				Recoverable: false,
			},
			want: `{"v":1,"type":"error","seq":50,"code":"WRITE_FORBIDDEN","message":"viewer cannot send input","recoverable":false}`,
		},
		{
			name: "pong",
			in:   Pong{},
			want: `{"v":1,"type":"pong"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("\nwant: %s\n got: %s", tc.want, got)
			}
		})
	}
}

func TestEncodeStampsEnvelope(t *testing.T) {
	// Caller leaves V/Type zero — Encode MUST stamp them.
	raw, err := Encode(Delta{Seq: 1, Patches: []Patch{{Path: "x", Value: json.RawMessage(`1`)}}})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.V != Version || env.Type != TypeDelta {
		t.Fatalf("envelope not stamped: %+v", env)
	}
}

func TestDecodeClientFrames(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{
			name: "subscribe live",
			raw:  `{"v":1,"type":"subscribe","token":"tok"}`,
			want: &Subscribe{V: 1, Type: TypeSubscribe, Token: "tok"},
		},
		{
			name: "subscribe test",
			raw:  `{"v":1,"type":"subscribe","token":"tok","scene":"s","session":"sess"}`,
			want: &Subscribe{V: 1, Type: TypeSubscribe, Token: "tok", Scene: "s", Session: "sess"},
		},
		{
			name: "input",
			raw:  `{"v":1,"type":"input","patches":[{"path":"__inputs.title","value":"hi"}]}`,
			want: &Input{V: 1, Type: TypeInput, Patches: []Patch{{Path: "__inputs.title", Value: json.RawMessage(`"hi"`)}}},
		},
		{
			name: "ping",
			raw:  `{"v":1,"type":"ping"}`,
			want: &Ping{V: 1, Type: TypePing},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("\nwant: %s\n got: %s", wantJSON, gotJSON)
			}
		})
	}
}

func TestDecodeRejectsBadVersion(t *testing.T) {
	_, err := Decode([]byte(`{"v":2,"type":"subscribe","token":"x"}`))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("want ErrVersionMismatch, got %v", err)
	}
}

func TestDecodeRejectsUnknownType(t *testing.T) {
	_, err := Decode([]byte(`{"v":1,"type":"banana"}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func TestDecodeServerFrames(t *testing.T) {
	raw := `{"v":1,"type":"snapshot","seq":1,"scene_id":"x","scene_version":"sha256:y","state":{}}`
	msg, err := DecodeServer([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	snap, ok := msg.(*Snapshot)
	if !ok {
		t.Fatalf("got %T, want *Snapshot", msg)
	}
	if snap.SceneID != "x" || snap.Seq != 1 {
		t.Fatalf("unexpected: %+v", snap)
	}
}

func TestRoundTripPreservesRawValues(t *testing.T) {
	// Ensures we don't lose fidelity on null vs missing, integer vs
	// float, etc.
	original := `{"v":1,"type":"delta","seq":2,"patches":[{"path":"a","value":null},{"path":"b","value":3.14},{"path":"c","value":[1,2,3]}]}`
	msg, err := DecodeServer([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	d := msg.(*Delta)
	out, err := Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Fatalf("\nwant: %s\n got: %s", original, out)
	}
}

func TestEncodeNoHTMLEscape(t *testing.T) {
	// Paths with `<`, `>`, `&` MUST survive on the wire.
	d := Delta{Seq: 1, Patches: []Patch{{Path: "a<b>&c", Value: json.RawMessage(`"x"`)}}}
	out, err := Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "a<b>&c") {
		t.Fatalf("HTML chars escaped: %s", out)
	}
}

func TestSequenceTracker_ServerEmit(t *testing.T) {
	s := NewSequenceTracker()
	if got := s.NextServer(); got != 1 {
		t.Fatalf("first NextServer = %d, want 1", got)
	}
	if got := s.NextServer(); got != 2 {
		t.Fatalf("second NextServer = %d, want 2", got)
	}
	s.Reset()
	if got := s.NextServer(); got != 1 {
		t.Fatalf("after reset NextServer = %d, want 1", got)
	}
}

func TestSequenceTracker_ObserveServer(t *testing.T) {
	s := NewSequenceTracker()

	// First frame must be seq=1.
	if skip, err := s.ObserveServer(1); err != nil || skip {
		t.Fatalf("seq=1 first: skip=%v err=%v", skip, err)
	}
	// Contiguous.
	if skip, err := s.ObserveServer(2); err != nil || skip {
		t.Fatalf("seq=2: skip=%v err=%v", skip, err)
	}
	// Replay.
	if skip, err := s.ObserveServer(2); err != nil || !skip {
		t.Fatalf("replay seq=2: skip=%v err=%v", skip, err)
	}
	// Gap.
	if _, err := s.ObserveServer(5); !errors.Is(err, ErrGap) {
		t.Fatalf("seq=5: want ErrGap, got %v", err)
	}
}

func TestSequenceTracker_AcceptsAnyPositiveStart(t *testing.T) {
	// LSDP/1.1 §18.1.1 — fresh tracker accepts any positive seq as
	// the baseline (per-scene seq, late-joining subscribers may see
	// snapshot at seq > 1). Only seq=0 is rejected.
	s := NewSequenceTracker()
	if skip, err := s.ObserveServer(42); err != nil || skip {
		t.Fatalf("first frame at 42: want OK, got skip=%v err=%v", skip, err)
	}
	if skip, err := s.ObserveServer(43); err != nil || skip {
		t.Fatalf("subsequent +1: want OK, got skip=%v err=%v", skip, err)
	}
	// seq=0 is still invalid.
	s2 := NewSequenceTracker()
	if _, err := s2.ObserveServer(0); !errors.Is(err, ErrInvalidSeqStart) {
		t.Fatalf("seq=0: want ErrInvalidSeqStart, got %v", err)
	}
}

func TestSequenceTracker_SceneChangedReset(t *testing.T) {
	// After SceneChanged, the runtime calls ObserveSnapshot to rebase
	// to the new scene's snapshot seq (typically 1, but per §18.1.1
	// it can be any positive value).
	s := NewSequenceTracker()
	_, _ = s.ObserveServer(1)
	_, _ = s.ObserveServer(2)
	_, _ = s.ObserveServer(3)
	if err := s.ObserveSnapshot(1); err != nil {
		t.Fatalf("rebase to 1: %v", err)
	}
	// Subsequent deltas continue from there.
	if skip, err := s.ObserveServer(2); err != nil || skip {
		t.Fatalf("seq=2 after rebase: skip=%v err=%v", skip, err)
	}
}

func TestLeafPath_Validate(t *testing.T) {
	good := []string{
		"a",
		"a.b.c",
		"players.0.name",
		"__inputs.title",
		"__inputs.platform.twitch.last_chat",
	}
	for _, p := range good {
		if err := LeafPath(p).Validate(); err != nil {
			t.Errorf("%q: unexpected error %v", p, err)
		}
	}
	bad := []string{
		"",
		".a",
		"a.",
		"a..b",
		"a-b",
		"a.b c",
		"{x}.y", // template form rejected by Validate
	}
	for _, p := range bad {
		if err := LeafPath(p).Validate(); err == nil {
			t.Errorf("%q: expected error", p)
		}
	}
}

func TestLeafPath_ValidateTemplate(t *testing.T) {
	if err := LeafPath("{player}.score").ValidateTemplate(); err != nil {
		t.Fatalf("template: %v", err)
	}
	if err := LeafPath("{}").ValidateTemplate(); err == nil {
		t.Fatal("empty scope should fail")
	}
	if err := LeafPath("{x-y}").ValidateTemplate(); err == nil {
		t.Fatal("invalid scope should fail")
	}
}

func TestLeafPath_Substitute(t *testing.T) {
	got, err := LeafPath("{player}.score").Substitute(map[string]string{"player": "players.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "players.0.score" {
		t.Fatalf("got %q", got)
	}

	if _, err := LeafPath("{missing}.x").Substitute(map[string]string{}); err == nil {
		t.Fatal("missing scope should fail")
	}
	if _, err := LeafPath("{a.score").Substitute(map[string]string{}); err == nil {
		t.Fatal("unterminated should fail")
	}
}

func TestLeafPath_IsReserved(t *testing.T) {
	cases := map[string]bool{
		"__inputs":           true,
		"__inputs.title":     true,
		"__system.adapter.x": true,
		"__test.input":       true,
		"__schema.x":         true,
		"foo":                false,
		"_partial":           false,
		"__custom":           false, // not in the reserved set
	}
	for p, want := range cases {
		if got := LeafPath(p).IsReserved(); got != want {
			t.Errorf("%q: got %v want %v", p, got, want)
		}
	}
}

func TestLeafPath_HasPrefix(t *testing.T) {
	p := LeafPath("players.0.score")
	if !p.HasPrefix("players") {
		t.Fail()
	}
	if !p.HasPrefix("players.0") {
		t.Fail()
	}
	if p.HasPrefix("playerstats") {
		t.Fail()
	}
}

func TestErrorCode_Recoverable(t *testing.T) {
	cases := map[ErrorCode]bool{
		CodeWriteForbidden:     true,
		CodeBundleFetchFailed:  true,
		CodeVersionGap:         true,
		CodeUnknownPath:        true,
		CodeInvalidValue:       true,
		CodeRateLimit:          true,
		CodeAuthDenied:         false,
		CodeSceneNotFound:      false,
		CodeBundleIncompatible: false,
		CodeVersionMismatch:    false,
		CodeTestSessionExpired: false,
	}
	for code, want := range cases {
		if got := code.Recoverable(false); got != want {
			t.Errorf("%s: got %v want %v", code, got, want)
		}
	}
	// CodeInternal honours the supplied default.
	if !CodeInternal.Recoverable(true) {
		t.Fatal("internal recoverable=true not honoured")
	}
	if CodeInternal.Recoverable(false) {
		t.Fatal("internal recoverable=false not honoured")
	}
}

func TestRole_Validity(t *testing.T) {
	if !RoleOperator.IsValid() || !RoleViewer.IsValid() ||
		!RoleService.IsValid() || !RoleTest.IsValid() {
		t.Fatal("known roles must be valid")
	}
	if Role("admin").IsValid() {
		t.Fatal("unknown role must not be valid")
	}
}

// ──────────────────────────────────────────────────────────────────────
// LSDP/1.1 — additive frame surface.
// ──────────────────────────────────────────────────────────────────────

func TestSubProtocolNegotiation(t *testing.T) {
	if SubProtocol != "lsdp.v1" {
		t.Fatalf("1.0 subprotocol drift: %s", SubProtocol)
	}
	if SubProtocolV1_1 != "lsdp.v1.1" {
		t.Fatalf("1.1 subprotocol drift: %s", SubProtocolV1_1)
	}
	if len(SubProtocols) != 2 || SubProtocols[0] != SubProtocolV1_1 || SubProtocols[1] != SubProtocol {
		t.Fatalf("preference order broken: %v", SubProtocols)
	}
}

func TestSubscribeWithSinceSequence_RoundTrip(t *testing.T) {
	in := Subscribe{Token: "t", SinceSequence: 12345}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"type":"subscribe","token":"t","since_sequence":12345}`
	if string(raw) != want {
		t.Fatalf("\nwant: %s\n got: %s", want, raw)
	}
	msg, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := msg.(*Subscribe)
	if !ok {
		t.Fatalf("decoded type %T", msg)
	}
	if got.SinceSequence != 12345 {
		t.Fatalf("since_sequence not round-tripped: %d", got.SinceSequence)
	}
}

func TestSubscribeWithoutSinceSequence_OmitsField(t *testing.T) {
	// 1.0-compatible : zero SinceSequence MUST omit on the wire.
	raw, err := Encode(Subscribe{Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "since_sequence") {
		t.Fatalf("zero since_sequence leaked: %s", raw)
	}
}

func TestInputWithClientMsgID_RoundTrip(t *testing.T) {
	in := Input{
		Patches:     []Patch{{Path: "x", Value: json.RawMessage(`1`)}},
		ClientMsgID: "ui-9f3a",
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"type":"input","patches":[{"path":"x","value":1}],"client_msg_id":"ui-9f3a"}`
	if string(raw) != want {
		t.Fatalf("\nwant: %s\n got: %s", want, raw)
	}
	msg, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*Input)
	if got.ClientMsgID != "ui-9f3a" {
		t.Fatalf("client_msg_id not round-tripped: %q", got.ClientMsgID)
	}
}

func TestPingPongNonce_RoundTrip(t *testing.T) {
	rawPing, _ := Encode(Ping{Nonce: "probe-7a2c"})
	if !strings.Contains(string(rawPing), `"nonce":"probe-7a2c"`) {
		t.Fatalf("ping nonce missing on wire: %s", rawPing)
	}
	msg, err := Decode(rawPing)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.(*Ping).Nonce; got != "probe-7a2c" {
		t.Fatalf("ping nonce not round-tripped: %q", got)
	}

	rawPong, _ := Encode(Pong{Nonce: "probe-7a2c"})
	if !strings.Contains(string(rawPong), `"nonce":"probe-7a2c"`) {
		t.Fatalf("pong nonce missing on wire: %s", rawPong)
	}
	msg, err = DecodeServer(rawPong)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.(*Pong).Nonce; got != "probe-7a2c" {
		t.Fatalf("pong nonce not round-tripped: %q", got)
	}

	// 1.0-compat : empty nonce omitted from wire.
	rawBare, _ := Encode(Pong{})
	if string(rawBare) != `{"v":1,"type":"pong"}` {
		t.Fatalf("bare pong leaked nonce: %s", rawBare)
	}
}

func TestUnsubscribe_RoundTrip(t *testing.T) {
	raw, err := Encode(Unsubscribe{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"type":"unsubscribe"}`
	if string(raw) != want {
		t.Fatalf("\nwant: %s\n got: %s", want, raw)
	}
	msg, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(*Unsubscribe); !ok {
		t.Fatalf("decoded type %T (want *Unsubscribe)", msg)
	}
}

func TestDeltaWithCauseAndTransition_RoundTrip(t *testing.T) {
	in := Delta{
		Seq: 7,
		Patches: []Patch{
			{
				Path:  "score",
				Value: json.RawMessage(`42`),
				Transition: &TransitionSpec{
					Kind:       "tween",
					DurationMs: 500,
					Easing:     "ease-out",
				},
			},
		},
		Cause: &Cause{Source: "operator:alice", InputID: "ui-9f3a"},
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"transition":{"kind":"tween"`) {
		t.Fatalf("transition not encoded: %s", raw)
	}
	if !strings.Contains(string(raw), `"cause":{"source":"operator:alice"`) {
		t.Fatalf("cause not encoded: %s", raw)
	}
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*Delta)
	if got.Cause == nil || got.Cause.InputID != "ui-9f3a" {
		t.Fatalf("cause.input_id not round-tripped: %+v", got.Cause)
	}
	if got.Patches[0].Transition == nil || got.Patches[0].Transition.Kind != "tween" {
		t.Fatalf("transition not round-tripped: %+v", got.Patches[0].Transition)
	}
}

func TestDeltaWithProjectionMetadata_RoundTrip(t *testing.T) {
	in := Delta{
		Seq: 7,
		Patches: []Patch{
			{
				Path:  "score",
				Value: json.RawMessage(`42`),
			},
		},
		Cause:             &Cause{Source: "operator:alice", InputID: "ui-9f3a"},
		SchemaVersion:     "schema-v1",
		SceneDigest:       "scene-abc",
		RuntimeInstanceID: "rt-123",
		Target:            "preview",
		RenderRevision:    "rev-456",
		CorrelationID:     "corr-789",
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"schema_version":"schema-v1"`) {
		t.Fatalf("schema_version not encoded: %s", raw)
	}
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*Delta)
	if got.SchemaVersion != "schema-v1" || got.SceneDigest != "scene-abc" || got.RuntimeInstanceID != "rt-123" || got.Target != "preview" || got.RenderRevision != "rev-456" || got.CorrelationID != "corr-789" {
		t.Fatalf("projection metadata not round-tripped: %+v", got)
	}
}

func TestSnapshotWithProjectionMetadata_RoundTrip(t *testing.T) {
	in := Snapshot{
		Seq:          3,
		SceneID:      "main-stage",
		SceneVersion: "sha256:abc",
		State: map[string]json.RawMessage{
			"show.title": json.RawMessage(`"Hello"`),
		},
		SchemaVersion:     "schema-v1",
		SceneDigest:       "scene-abc",
		RuntimeInstanceID: "rt-123",
		Target:            "preview",
		RenderRevision:    "rev-456",
		CorrelationID:     "corr-789",
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"schema_version":"schema-v1"`) {
		t.Fatalf("schema_version not encoded: %s", raw)
	}
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*Snapshot)
	if got.SchemaVersion != "schema-v1" || got.SceneDigest != "scene-abc" || got.RuntimeInstanceID != "rt-123" || got.Target != "preview" || got.RenderRevision != "rev-456" || got.CorrelationID != "corr-789" {
		t.Fatalf("projection metadata not round-tripped: %+v", got)
	}
}

func TestSnapshotWithoutProjectionMetadata_OmitsFields(t *testing.T) {
	in := Snapshot{
		Seq:          1,
		SceneID:      "main-stage",
		SceneVersion: "sha256:abc",
		State:        map[string]json.RawMessage{},
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "schema_version") || strings.Contains(string(raw), "scene_digest") || strings.Contains(string(raw), "correlation_id") {
		t.Fatalf("unset projection metadata must be omitted, not fabricated: %s", raw)
	}
}

func TestSceneChangedWithTransition_RoundTrip(t *testing.T) {
	in := SceneChanged{
		Seq:          100,
		SceneID:      "scene-b",
		SceneVersion: "sha256:b0",
		FromSceneID:  "scene-a",
		Transition:   &SceneTransition{Kind: "crossfade", DurationMs: 600},
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"from_scene_id":"scene-a"`) {
		t.Fatalf("from_scene_id missing: %s", raw)
	}
	if !strings.Contains(string(raw), `"transition":{"kind":"crossfade","duration_ms":600}`) {
		t.Fatalf("transition missing or wrong: %s", raw)
	}
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*SceneChanged)
	if got.FromSceneID != "scene-a" {
		t.Fatalf("from_scene_id not round-tripped: %q", got.FromSceneID)
	}
	if got.Transition == nil || got.Transition.Kind != "crossfade" || got.Transition.DurationMs != 600 {
		t.Fatalf("transition not round-tripped: %+v", got.Transition)
	}
}

func TestBackwardCompat_1_0_BundlesStillEncode(t *testing.T) {
	// A pure-1.0 caller (no 1.1 fields populated) MUST produce the
	// exact same bytes as before. Regression check.
	in := Subscribe{Token: "t"}
	raw, _ := Encode(in)
	want := `{"v":1,"type":"subscribe","token":"t"}`
	if string(raw) != want {
		t.Fatalf("1.0 subscribe shape changed:\nwant: %s\n got: %s", want, raw)
	}

	in2 := Delta{Seq: 1, Patches: []Patch{{Path: "x", Value: json.RawMessage(`1`)}}}
	raw, _ = Encode(in2)
	want = `{"v":1,"type":"delta","seq":1,"patches":[{"path":"x","value":1}]}`
	if string(raw) != want {
		t.Fatalf("1.0 delta shape changed:\nwant: %s\n got: %s", want, raw)
	}
}

func TestForwardCompat_1_0Server_Ignores_1_1_Fields(t *testing.T) {
	// A 1.0 client receiving a 1.1 frame with optional fields SHOULD
	// decode it cleanly (the additional fields are tolerated by Go's
	// stdlib JSON decoder by default — they go into nil pointers).
	raw := []byte(`{"v":1,"type":"delta","seq":1,"patches":[{"path":"x","value":1}],"cause":{"source":"adapter:http_poll"},"schema_version":"schema-v1","scene_digest":"scene-abc","runtime_instance_id":"rt-123","target":"preview","render_revision":"rev-456","correlation_id":"corr-789"}`)
	msg, err := DecodeServer(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.(*Delta)
	if got.Seq != 1 || len(got.Patches) != 1 {
		t.Fatal("1.0-only fields lost")
	}
	if got.Cause == nil || got.Cause.Source != "adapter:http_poll" {
		t.Fatalf("optional cause not decoded: %+v", got.Cause)
	}
}

// Bundle-shape preservation : ensure the bytes JSON consumers see for
// 1.0 frames have not drifted because we added omitempty fields to
// every struct.
func TestNoUnintendedFieldLeak(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{Snapshot{Seq: 1, SceneID: "s", SceneVersion: "sha256:0", State: map[string]json.RawMessage{}},
			`{"v":1,"type":"snapshot","seq":1,"scene_id":"s","scene_version":"sha256:0","state":{}}`},
		{Pong{}, `{"v":1,"type":"pong"}`},
		{Ping{}, `{"v":1,"type":"ping"}`},
		{Subscribe{Token: "t"}, `{"v":1,"type":"subscribe","token":"t"}`},
		{Input{Patches: []Patch{{Path: "x", Value: json.RawMessage(`1`)}}},
			`{"v":1,"type":"input","patches":[{"path":"x","value":1}]}`},
		{SceneChanged{Seq: 1, SceneID: "s", SceneVersion: "sha256:0"},
			`{"v":1,"type":"scene_changed","seq":1,"scene_id":"s","scene_version":"sha256:0"}`},
	}
	for _, tc := range cases {
		raw, err := Encode(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("\nwant: %s\n got: %s", tc.want, raw)
		}
	}
}
