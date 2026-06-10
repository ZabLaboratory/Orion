package obs

import "testing"

// TestInboxDroppedMetric pins the wire name and the seam method of the
// inbox drop counter (ADR 003 §3.3 E2, issue #84):
// `orion_inbox_dropped_total{scene_id}` increments once per refused
// scene write, via the InboxMetrics interface the adapters consume.
// Asserted through the REGISTRY (what the scrape endpoint serves), not
// the struct field, so the registered wire name is part of the proof.
func TestInboxDroppedMetric(t *testing.T) {
	m := NewMetrics()

	m.InboxDropped("scene-1")
	m.InboxDropped("scene-1")
	m.InboxDropped("scene-2")

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "orion_inbox_dropped_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "scene_id" {
					got[l.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("orion_inbox_dropped_total not exposed on the registry")
	}
	if got["scene-1"] != 2 {
		t.Fatalf("scene-1 drops = %v, want 2", got["scene-1"])
	}
	if got["scene-2"] != 1 {
		t.Fatalf("scene-2 drops = %v, want 1", got["scene-2"])
	}
}
