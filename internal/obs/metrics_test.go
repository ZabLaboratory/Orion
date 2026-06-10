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

// TestTaskCPUSecondsMetric pins the wire name and labels of the B7
// aggregate-CPU counter (ADR 003 §3.1.6, issue #89):
// `orion_task_cpu_seconds_total{scene_id, scene_version}` accumulates
// the seconds the ExecCPUSeconds seam reports, per scene-version.
// Asserted through the REGISTRY so the registered wire name — what
// the alert rule will reference — is part of the proof.
func TestTaskCPUSecondsMetric(t *testing.T) {
	m := NewMetrics()

	m.ExecCPUSeconds("scene-1", "sha256:v1", 0.25)
	m.ExecCPUSeconds("scene-1", "sha256:v1", 0.5)
	m.ExecCPUSeconds("scene-1", "sha256:v2", 1.0)

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "orion_task_cpu_seconds_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			var sceneID, version string
			for _, l := range metric.GetLabel() {
				switch l.GetName() {
				case "scene_id":
					sceneID = l.GetValue()
				case "scene_version":
					version = l.GetValue()
				}
			}
			got[sceneID+"|"+version] = metric.GetCounter().GetValue()
		}
	}
	if len(got) == 0 {
		t.Fatal("orion_task_cpu_seconds_total not exposed on the registry")
	}
	if got["scene-1|sha256:v1"] != 0.75 {
		t.Fatalf("v1 cpu = %v, want 0.75 (accumulated)", got["scene-1|sha256:v1"])
	}
	if got["scene-1|sha256:v2"] != 1.0 {
		t.Fatalf("v2 cpu = %v, want 1.0 (separate scene_version bucket)", got["scene-1|sha256:v2"])
	}
}
