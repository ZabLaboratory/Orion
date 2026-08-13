package blueproject

import (
	"encoding/json"
	"testing"
)

func TestProject_DropsObjectValues(t *testing.T) {
	step := StepOutputs{
		RuntimeSequence: 7,
		Outputs: map[string]any{
			"title.text":   "hello",
			"score.value":  42.0,
			"visible":      true,
			"tags":         []any{"a", "b", 3.0},
			"nested.thing": map[string]any{"bad": "shape"},
			"rows":         []any{map[string]any{"bad": "row"}},
		},
	}

	p := Project(step, "sha256:abc", "instance-1", "rev-1", "corr-1", TargetPreview)

	if p.SchemaVersion != "orion.blue-solar-projection.v1" {
		t.Fatalf("unexpected schema_version: %q", p.SchemaVersion)
	}
	if p.OutputSequence != 7 {
		t.Fatalf("unexpected output_sequence: %d", p.OutputSequence)
	}
	if len(p.Patches) != 4 {
		t.Fatalf("expected 4 wire-legal patches, got %d: %+v", len(p.Patches), p.Patches)
	}
	for _, forbidden := range []string{"nested.thing", "rows"} {
		if _, ok := p.Patches[forbidden]; ok {
			t.Fatalf("expected %q to be dropped as non-scalar", forbidden)
		}
	}

	var tags []any
	if err := json.Unmarshal(p.Patches["tags"], &tags); err != nil {
		t.Fatalf("tags patch not valid JSON: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(tags))
	}
}

func TestProject_EmptyOutputsYieldsEmptyPatches(t *testing.T) {
	p := Project(StepOutputs{}, "sha256:abc", "instance-1", "rev-1", "corr-1", TargetProgram)
	if len(p.Patches) != 0 {
		t.Fatalf("expected empty patches, got %d", len(p.Patches))
	}
	if p.Target != TargetProgram {
		t.Fatalf("unexpected target: %q", p.Target)
	}
}

func TestProject_ArrayOfArraysStillLegal(t *testing.T) {
	step := StepOutputs{Outputs: map[string]any{"grid": []any{[]any{1.0, 2.0}, []any{3.0}}}}
	p := Project(step, "sha256:abc", "instance-1", "rev-1", "corr-1", TargetPreview)
	if _, ok := p.Patches["grid"]; !ok {
		t.Fatal("expected nested array-of-scalars to be wire-legal")
	}
}
