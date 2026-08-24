package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

type localAtomicSceneIntentRequest struct {
	SceneID string          `json:"scene_id"`
	Intent  json.RawMessage `json:"intent"`
}

// postLocalAtomicSceneIntent is the local equivalent of ZabGate's atomic
// relay. The wrapper is accepted only on Orion's loopback embedded-local
// server, and the nested intent is passed byte-for-byte to the same handler.
// No local route manufactures an attestation or bypasses Blue digest checks.
func postLocalAtomicSceneIntent(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		var envelope localAtomicSceneIntentRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxSceneIntentBytes+1)).Decode(&envelope); err != nil || envelope.SceneID == "" || len(envelope.Intent) == 0 || len(envelope.Intent) > maxSceneIntentBytes {
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", Reason: "MALFORMED_INTENT"})
			return
		}
		var intent sceneIntentRequest
		if err := json.Unmarshal(envelope.Intent, &intent); err != nil || intent.StreamID != envelope.SceneID {
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", IntentID: intent.IntentID, Reason: "MALFORMED_INTENT"})
			return
		}
		clone := r.Clone(r.Context())
		clone.Body = io.NopCloser(bytes.NewReader(envelope.Intent))
		postSceneIntent(deps)(w, clone)
	})
}
