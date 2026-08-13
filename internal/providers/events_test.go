package providers

import (
	"encoding/json"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

func TestBuildEvent_ParsesAsCanonicalEnvelope(t *testing.T) {
	data, err := BuildEvent(
		"evt-1", "quasar.twitch.channel-42", "quasar.twitch.chat-message", "corr-1",
		7, 1734000000000,
		map[string]any{"message": "gg", "count": json.Number("3")},
	)
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	if _, err := blueruntime.ParseEvent(data); err != nil {
		t.Fatalf("ParseEvent(BuildEvent(...)): %v", err)
	}
}

func TestBuildEvent_TamperedPayloadFailsDigest(t *testing.T) {
	data, err := BuildEvent("evt-1", "quasar.twitch.channel-42", "quasar.twitch.chat-message", "corr-1", 7, 1734000000000, map[string]any{"message": "gg"})
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	envelope["payload"] = map[string]any{"message": "tampered"}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := blueruntime.ParseEvent(tampered); err == nil {
		t.Fatal("expected ParseEvent to reject a tampered payload")
	}
}
