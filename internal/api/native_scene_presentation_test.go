package api

import "testing"

func TestNativePresentationAcknowledgment(t *testing.T) {
	accepted := map[string]any{"request_id": "request", "phase": "committed", "scene_id": "scene", "scene_version": "version"}
	for _, tc := range []struct {
		name, lane, field string
		value             any
		matched, failed   bool
	}{
		{name: "matching", lane: "preview", matched: true},
		{name: "other request", lane: "preview", field: "request_id", value: "other"},
		{name: "other scene", lane: "preview", field: "scene_id", value: "other"},
		{name: "other version", lane: "preview", field: "scene_version", value: "other"},
		{name: "other phase", lane: "preview", field: "phase", value: "prepared"},
		{name: "other lane", lane: "program"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ack := map[string]any{}
			for key, value := range accepted {
				ack[key] = value
			}
			if tc.field != "" {
				ack[tc.field] = tc.value
			}
			matched, err := presentationAcknowledgment(map[string]any{"solar_presentation_preview": ack}, tc.lane, "request", "commit", "committed", "scene", "version")
			if matched != tc.matched || (err != nil) != tc.failed {
				t.Fatalf("matched=%v error=%v", matched, err)
			}
		})
	}
	failed := map[string]any{"request_id": "request", "phase": "failed", "transition_phase": "prepare", "error": "rejected"}
	state := map[string]any{"solar_presentation_preview": failed}
	if matched, err := presentationAcknowledgment(state, "preview", "request", "commit", "committed", "scene", "version"); matched || err != nil {
		t.Fatal("stale phase failure was accepted")
	}
	if matched, err := presentationAcknowledgment(state, "preview", "request", "prepare", "prepared", "scene", "version"); !matched || err == nil {
		t.Fatal("matching rejection was ignored")
	}
}
