package bluehost

import (
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

func TestNormalizeRuntimeOutputsPromotesGraphOutputBag(t *testing.T) {
	result := normalizeRuntimeOutputs(blueruntime.StepResult{
		Outputs: map[string]any{"existing": "kept"},
		Variables: map[string]any{
			runtimeOutputBag: map[string]any{"scene.league": "LEC"},
		},
	})
	if result.Outputs["existing"] != "kept" {
		t.Fatalf("existing runtime output was lost: %#v", result.Outputs)
	}
	if result.Outputs["scene.league"] != "LEC" {
		t.Fatalf("reserved graph output was not promoted: %#v", result.Outputs)
	}
}

func TestNormalizeRuntimeOutputsIsNoopWithoutOutputBag(t *testing.T) {
	result := normalizeRuntimeOutputs(blueruntime.StepResult{
		Outputs:   map[string]any{"existing": "kept"},
		Variables: map[string]any{"other": true},
	})
	if len(result.Outputs) != 1 || result.Outputs["existing"] != "kept" {
		t.Fatalf("unexpected normalization without output bag: %#v", result.Outputs)
	}
}
