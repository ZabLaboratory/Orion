package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// CameraSlotAssigner is the runtime seam used by Prism's editable-camera
// control. Orion implements it with the same durable operation as Blue's
// `zabcam.assign-slot@1`; the API never writes the LSDP mirror directly.
type CameraSlotAssigner interface {
	AssignCameraSlot(ctx context.Context, slotRef, peerLabel string) error
}

const (
	maxCameraSlotBody        = 32 << 10
	maxCameraSlotAssignments = 3
	maxCameraSlotField       = 128
)

type cameraSlotAssignment struct {
	SlotRef   string `json:"slot_ref"`
	PeerLabel string `json:"peer_label"`
}

type cameraSlotProjectionRequest struct {
	StreamID    string                 `json:"stream_id,omitempty"`
	Assignments []cameraSlotAssignment `json:"assignments"`
}

type cameraSlotProjectionResponse struct {
	Status      string                 `json:"status"`
	StreamID    string                 `json:"stream_id"`
	Assignments []cameraSlotAssignment `json:"assignments"`
}

// postCameraSlots applies the current editable-scene camera mapping through
// Orion's canonical slot-assignment operation. It is deliberately
// operator-gated and bounded: the payload contains no room token, URL, or
// scene data, only the three logical slots resolved by Prism. Program/Pulsar
// are not involved in this path.
func postCameraSlots(assigner CameraSlotAssigner) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if assigner == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": "CAMERA_SLOT_ASSIGNER_UNAVAILABLE",
			})
			return
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, maxCameraSlotBody+1))
		if err != nil || len(raw) > maxCameraSlotBody {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status": "rejected",
				"reason": "MALFORMED_CAMERA_SLOTS",
			})
			return
		}
		var req cameraSlotProjectionRequest
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status": "rejected",
				"reason": "MALFORMED_CAMERA_SLOTS",
			})
			return
		}
		if req.StreamID != "" && req.StreamID != "live" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status": "rejected",
				"reason": "UNKNOWN_STREAM",
			})
			return
		}
		if len(req.Assignments) > maxCameraSlotAssignments {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status": "rejected",
				"reason": "CAMERA_SLOT_LIMIT_EXCEEDED",
			})
			return
		}

		assignments := make(map[string]string, len(req.Assignments))
		for _, assignment := range req.Assignments {
			slotRef := strings.TrimSpace(assignment.SlotRef)
			peerLabel := strings.TrimSpace(assignment.PeerLabel)
			if slotRef == "" || peerLabel == "" ||
				len(slotRef) > maxCameraSlotField || len(peerLabel) > maxCameraSlotField {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status": "rejected",
					"reason": "MALFORMED_CAMERA_SLOT",
				})
				return
			}
			if _, exists := assignments[slotRef]; exists {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status": "rejected",
					"reason": "DUPLICATE_CAMERA_SLOT",
				})
				return
			}
			assignments[slotRef] = peerLabel
		}

		response := make([]cameraSlotAssignment, 0, len(assignments))
		for slotRef, peerLabel := range assignments {
			response = append(response, cameraSlotAssignment{SlotRef: slotRef, PeerLabel: peerLabel})
		}
		sort.Slice(response, func(i, j int) bool { return response[i].SlotRef < response[j].SlotRef })
		for index, assignment := range response {
			if err := assigner.AssignCameraSlot(r.Context(), assignment.SlotRef, assignment.PeerLabel); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"status":   "rejected",
					"reason":   "CAMERA_SLOT_ASSIGN_FAILED",
					"slot_ref": assignment.SlotRef,
					"assigned": index,
					"error":    err.Error(),
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, cameraSlotProjectionResponse{
			Status:      "projected",
			StreamID:    "live",
			Assignments: response,
		})
	})
}
