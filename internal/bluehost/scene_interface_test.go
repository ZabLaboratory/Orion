package bluehost

import (
	"strings"
	"testing"
)

func TestHostSceneInterface_SourceAuthorityCopiesAndLifetime(t *testing.T) {
	h := NewHost()
	if err := h.TakeStatic("scene-live", "live-v1"); err != nil {
		t.Fatal(err)
	}
	if err := h.PreparePreviewStatic("scene-preview", "preview-v1"); err != nil {
		t.Fatal(err)
	}
	h.SetBundle(SlotOnAir, []byte(`{"operator_inputs":[{"path":"legacy","type":"text"}]}`))
	if !strings.Contains(string(h.OperatorInputs(SlotOnAir)), "legacy") {
		t.Fatal("legacy interface lost")
	}
	source := []byte(`{"lsml":"1.1","operator_inputs":[{"path":"league","type":"text","writable_by":["operator"]}]}`)
	h.SetSceneInterface(SlotOnAir, source)
	source[0] = 'x'
	inputs, ok := h.SceneOperatorInputs("scene-live", "live-v1")
	if !ok || !strings.Contains(string(inputs), `"writable_by"`) || !strings.Contains(string(inputs), "league") {
		t.Fatalf("source interface: %s %v", inputs, ok)
	}
	inputs[0] = 'x'
	if got := h.OperatorInputs(SlotOnAir); got[0] != '[' {
		t.Fatal("caller mutated live declaration")
	}
	for _, address := range [][2]string{{"", "live-v1"}, {"scene-live", ""}, {"scene-preview", "live-v1"}, {"scene-live", "old"}} {
		if _, ok := h.SceneOperatorInputs(address[0], address[1]); ok {
			t.Fatalf("wrong address resolved: %v", address)
		}
	}
	h.SetSceneInterface(SlotPreview, []byte(`{"lsml":"1.1"}`))
	if got := string(h.OperatorInputs(SlotPreview)); got != "[]" {
		t.Fatalf("empty Preview: %s", got)
	}
	h.SetSceneInterface(SlotOnAir, []byte(`{"lsml":"1.1"}`))
	if got := string(h.OperatorInputs(SlotOnAir)); got != "[]" {
		t.Fatalf("legacy controls leaked: %s", got)
	}
	if err := h.TakeStatic("scene-next", "next-v1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.SceneOperatorInputs("scene-live", "live-v1"); ok {
		t.Fatal("old scene interface still addressable")
	}
	if got := string(h.OperatorInputs(SlotOnAir)); got != "[]" {
		t.Fatalf("interface survived scene switch: %s", got)
	}
}
