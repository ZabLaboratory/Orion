package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestEncode_SetsTypeAndV(t *testing.T) {
	out, err := Encode(Snapshot{
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
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "snapshot" || int(got["v"].(float64)) != 1 {
		t.Fatalf("envelope wrong: %s", out)
	}
}

func TestEncode_SubscribedWriterAck(t *testing.T) {
	out, err := Encode(Subscribed{Mode: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"type":"subscribed","v":1,"mode":"writer"}` {
		t.Fatalf("unexpected subscribed ack: %s", out)
	}
}

func TestDecode_DispatchesByType(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"type":"subscribe","v":1,"since_sequence":null}`, "subscribe"},
		{`{"type":"input","v":1,"path":"score.team_a","value":15}`, "input"},
		{`{"type":"unsubscribe","v":1}`, "unsubscribe"},
		{`{"type":"ping","v":1,"nonce":"abc"}`, "ping"},
	}
	for _, c := range cases {
		got, err := Decode([]byte(c.raw))
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		switch c.want {
		case "subscribe":
			if _, ok := got.(*Subscribe); !ok {
				t.Errorf("want Subscribe, got %T", got)
			}
		case "input":
			m := got.(*Input)
			if m.Path != "score.team_a" {
				t.Errorf("path=%q", m.Path)
			}
		case "unsubscribe":
			if _, ok := got.(*Unsubscribe); !ok {
				t.Errorf("want Unsubscribe, got %T", got)
			}
		case "ping":
			if got.(*Ping).Nonce != "abc" {
				t.Errorf("nonce=%q", got.(*Ping).Nonce)
			}
		}
	}
}

func TestDecode_VersionMismatch(t *testing.T) {
	_, err := Decode([]byte(`{"type":"subscribe","v":99}`))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("want ErrVersionMismatch, got %v", err)
	}
}

func TestDecode_UnknownType(t *testing.T) {
	_, err := Decode([]byte(`{"type":"yolo","v":1}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func TestEncode_NoHTMLEscaping(t *testing.T) {
	// Paths may legally contain `<channel>` (placeholder syntax in
	// declared bindings); we must not rewrite to <.
	out, err := Encode(Delta{
		SceneID:  "s",
		Sequence: 1,
		Patches: []Patch{
			{Path: "__inputs.platform.<channel>.last_chat", Value: json.RawMessage(`"hi"`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`<channel>`)) {
		t.Fatalf("HTML-escaped path leaked into wire: %s", out)
	}
}

func TestEncode_NoTrailingNewline(t *testing.T) {
	out, err := Encode(Pong{Nonce: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[len(out)-1] == '\n' {
		t.Fatalf("trailing newline present: %q", out)
	}
}

func TestRoundTrip_Subscribe_NullSince(t *testing.T) {
	raw := []byte(`{"type":"subscribe","v":1,"since_sequence":null}`)
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := got.(*Subscribe)
	if m.SinceSequence != nil {
		t.Fatalf("since_sequence should be nil, got %v", *m.SinceSequence)
	}
}

func TestRoundTrip_Subscribe_SinceSequenceSet(t *testing.T) {
	raw := []byte(`{"type":"subscribe","v":1,"since_sequence":42}`)
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := got.(*Subscribe)
	if m.SinceSequence == nil || *m.SinceSequence != 42 {
		t.Fatalf("since_sequence=%v", m.SinceSequence)
	}
}
