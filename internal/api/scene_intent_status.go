package api

import (
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"net/http"
)

// hostStatusResponse is the new path's read equivalent of legacy's
// `GET /api/v1/show` (show summary): which scene_digest, if any, each
// bluehost.Host slot currently carries. First read-route migration of
// #15's route-by-route plan (Refs #331) — additive, registered beside
// GET /api/v1/show, not replacing it yet.
type hostStatusResponse struct {
	Preview hostSlotStatus `json:"preview"`
	OnAir   hostSlotStatus `json:"on_air"`
}

type hostSlotStatus struct {
	SceneDigest       string `json:"scene_digest,omitempty"`
	ArtifactSetDigest string `json:"artifact_set_digest,omitempty"`
	Loaded            bool   `json:"loaded"`

	// Projection is the identity of the most recently forwarded LSDP
	// delta for this slot's bridge, if any has forwarded yet — the SAME
	// correlation_id/render_revision stamped on the wire (bluewire.Bridge.
	// LastForwarded). Read-only, stateless (in-memory only, never
	// persisted, lost on restart like every other field here), and purely
	// a polling convenience: an external observer (Refs B3-R6-17-PULSAR)
	// can already read this identity off a live LSDP WS subscription
	// today without this field existing at all. Orion never waits on
	// anyone reading it, and this field never reflects anything about
	// whether the projection actually reached the antenna — that
	// confirmation is PGM, observed later and elsewhere (ADR-BLUE-012
	// §4.4/B17). Omitted entirely when no bridge is running or none has
	// forwarded yet.
	Projection *hostSlotProjection `json:"projection,omitempty"`
}

type hostSlotProjection struct {
	Sequence       uint64 `json:"sequence"`
	RenderRevision string `json:"render_revision,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
}

func getHostStatus(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		previewDigest := deps.Host.Digest(bluehost.SlotPreview)
		onAirDigest := deps.Host.Digest(bluehost.SlotOnAir)
		writeJSON(w, http.StatusOK, hostStatusResponse{
			Preview: hostSlotStatus{
				SceneDigest: previewDigest, ArtifactSetDigest: deps.Host.ArtifactSetDigest(bluehost.SlotPreview), Loaded: previewDigest != "",
				Projection: lastProjection(deps.Bridges, bluehost.SlotPreview),
			},
			OnAir: hostSlotStatus{
				SceneDigest: onAirDigest, ArtifactSetDigest: deps.Host.ArtifactSetDigest(bluehost.SlotOnAir), Loaded: onAirDigest != "",
				Projection: lastProjection(deps.Bridges, bluehost.SlotOnAir),
			},
		})
	})
}

// lastProjection reads slot's running bridge (if any) for its most recently
// forwarded identity. A nil registry, no running bridge, or a bridge that
// has never forwarded all return nil — never a zero-value/invented
// identity standing in for "unknown".
func lastProjection(bridges *bluewire.Registry, slot bluehost.Slot) *hostSlotProjection {
	if bridges == nil {
		return nil
	}
	bridge := bridges.Current(slot)
	if bridge == nil {
		return nil
	}
	identity, ok := bridge.LastForwarded()
	if !ok {
		return nil
	}
	return &hostSlotProjection{
		Sequence:       identity.Sequence,
		RenderRevision: identity.RenderRevision,
		CorrelationID:  identity.CorrelationID,
	}
}
