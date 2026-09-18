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

// CameraSlotReleaser is the inverse seam for a complete editable-scene
// projection. It is intentionally separate from CameraSlotAssigner so older
// bespoke adapters remain assignment-compatible; production Orion wires both
// methods through the same EffectDeps bundle.
type CameraSlotReleaser interface {
	ReleaseCameraSlot(ctx context.Context, slotRef string) error
}

const (
	maxCameraSlotBody        = 32 << 10
	maxCameraSlotAssignments = 3
	maxCameraSlotClearRefs   = 16
	maxCameraSlotField       = 128
)

type cameraSlotAssignment struct {
	SlotRef   string `json:"slot_ref"`
	PeerLabel string `json:"peer_label"`
}

type cameraSlotProjectionRequest struct {
	StreamID      string                 `json:"stream_id,omitempty"`
	Assignments   []cameraSlotAssignment `json:"assignments"`
	ClearSlotRefs []string               `json:"clear_slot_refs,omitempty"`
}

type cameraSlotProjectionResponse struct {
	Status      string                 `json:"status"`
	StreamID    string                 `json:"stream_id"`
	Assignments []cameraSlotAssignment `json:"assignments"`
	Cleared     []string               `json:"cleared,omitempty"`
}

// Older local scene bundles can predate the optional inverse egress route.
// A complete projection must still be able to apply its current assignments
// in that compatibility window; stale cleanup is retried on the next scene
// arm once the local route registry has been refreshed.  Assignment failures
// remain hard failures because acknowledging one would make the Preview lie
// about the durable camera authority.
func optionalReleaseRouteUnavailable(err error) bool {
	return err != nil && strings.Contains(
		err.Error(),
		"EGRESS_ROUTE_NOT_DECLARED: zabcam/zabcam.slots.release",
	)
}

// postCameraSlots applies the current editable-scene camera mapping through
// Orion's canonical slot-assignment operation. It is deliberately
// operator-gated and bounded: the payload contains no room token, URL, or
// scene data, only the current logical slots plus explicit stale refs resolved
// by Prism. Program/Pulsar are not involved in this path.
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
		if len(req.ClearSlotRefs) > maxCameraSlotClearRefs {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status": "rejected",
				"reason": "CAMERA_SLOT_CLEAR_LIMIT_EXCEEDED",
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

		clearRefs := make([]string, 0, len(req.ClearSlotRefs))
		seenClearRefs := make(map[string]struct{}, len(req.ClearSlotRefs))
		for _, rawSlotRef := range req.ClearSlotRefs {
			slotRef := strings.TrimSpace(rawSlotRef)
			if slotRef == "" || len(slotRef) > maxCameraSlotField {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status": "rejected",
					"reason": "MALFORMED_CAMERA_SLOT_CLEAR",
				})
				return
			}
			if _, exists := seenClearRefs[slotRef]; exists {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status": "rejected",
					"reason": "DUPLICATE_CAMERA_SLOT_CLEAR",
				})
				return
			}
			if _, assigned := assignments[slotRef]; assigned {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status": "rejected",
					"reason": "CAMERA_SLOT_CLEAR_ASSIGN_CONFLICT",
				})
				return
			}
			seenClearRefs[slotRef] = struct{}{}
			clearRefs = append(clearRefs, slotRef)
		}
		sort.Strings(clearRefs)

		cleared := make([]string, 0, len(clearRefs))
		if len(clearRefs) > 0 {
			releaser, ok := assigner.(CameraSlotReleaser)
			if !ok {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unavailable",
					"reason": "CAMERA_SLOT_RELEASER_UNAVAILABLE",
				})
				return
			}
			// Remove stale bindings first. This prevents a complete-scene
			// projection from briefly exposing both the old and new slot set.
			for _, slotRef := range clearRefs {
				if err := releaser.ReleaseCameraSlot(r.Context(), slotRef); err != nil {
					if optionalReleaseRouteUnavailable(err) {
						// Compatibility with a local bundle generated before the
						// inverse route was published. Do not reject the current
						// assignment set or turn a best-effort cleanup into a
						// Preview alert; the next projection retries the clear.
						continue
					}
					writeJSON(w, http.StatusBadGateway, map[string]any{
						"status":   "rejected",
						"reason":   "CAMERA_SLOT_RELEASE_FAILED",
						"slot_ref": slotRef,
						"cleared":  len(cleared),
						"error":    err.Error(),
					})
					return
				}
				cleared = append(cleared, slotRef)
			}
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
			Cleared:     cleared,
		})
	})
}
