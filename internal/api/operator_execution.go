package api

import (
	"errors"
	"net/http"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// Orion reports the portable runtime's dispatch, never a business verdict on
// a Canvas-validated Blue. Invocations may complete after this HTTP response.
func writeOperatorExecution(w http.ResponseWriter, code int, status string, result blueruntime.StepResult) {
	invocations := make([]map[string]any, 0, len(result.Invocations))
	for _, invocation := range result.Invocations {
		invocations = append(invocations, map[string]any{
			"invocation_id": invocation["invocation_id"], "capability": invocation["capability"],
			"operation": invocation["operation"],
		})
	}
	writeJSON(w, code, map[string]any{
		"status": status,
		"execution": map[string]any{
			"phase": "dispatch-returned", "runtime_status": result.Status,
			"runtime_sequence": result.RuntimeSequence, "event_id": result.EventID,
			"invocation_count": len(result.Invocations),
			"invocations":      invocations,
		},
	})
}

// Preserve the runtime's public typed error. Unexpected host errors retain the
// existing generic envelope; no payload or provider response is echoed here.
func writeOperatorExecutionError(w http.ResponseWriter, err error, fallback string) {
	if errors.Is(err, bluehost.ErrSceneChanging) {
		writeOperatorError(w, http.StatusConflict, "SCENE_CHANGING", "execution is blocked during scene replacement")
		return
	}
	var runtimeErr *blueruntime.Error
	if errors.As(err, &runtimeErr) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "BLUE_EXECUTION_FAILED", "message": fallback,
			"runtime_error": runtimeErr,
		})
		return
	}
	writeOperatorError(w, http.StatusInternalServerError, "INTERNAL", fallback)
}
