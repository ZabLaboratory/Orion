package bluehost

import blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

const runtimeOutputBag = "__outputs__"

// normalizeRuntimeOutputs keeps the host boundary compatible with both
// Engine B runtimes: newer runtimes expose graph-output sinks in Outputs,
// while older ones only expose the same values in Variables[__outputs__].
// The bridge must receive the normalized map or a successful operator call
// can still produce no LSML patch.
func normalizeRuntimeOutputs(result blueruntime.StepResult) blueruntime.StepResult {
	bag, ok := result.Variables[runtimeOutputBag].(map[string]any)
	if !ok || len(bag) == 0 {
		return result
	}
	if result.Outputs == nil {
		result.Outputs = map[string]any{}
	}
	for name, value := range bag {
		result.Outputs[name] = value
	}
	return result
}
