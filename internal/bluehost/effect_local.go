package bluehost

import (
	"context"
	"encoding/json"
	"fmt"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// runLocalInvocation implements the same local effectors for generic
// core.effect.invoke as for the dedicated show.emit/overlay-app.set opcodes.
// Preview receives a completion without mutating the Program control plane.
func (h *Host) runLocalInvocation(slot Slot, invocation map[string]any) effects.Result {
	capability, operation := stringField(invocation, "capability"), stringField(invocation, "operation")
	request, ok := invocation["request"].(map[string]any)
	if !ok {
		return effects.Result{Err: "EFFECT_REQUEST_INVALID: request must be an object"}
	}
	h.mu.Lock()
	sink, mirror, mutate, deps := h.showEmitSink, h.overlayMirror, h.sceneMutationSink, h.effectDeps
	h.mu.Unlock()
	switch {
	case capability == "core.db.query" && operation == "query", capability == "core.service.call" && operation == "call":
		config, _ := request["config"].(map[string]any)
		inputs, _ := request["inputs"].(map[string]any)
		if inputs == nil {
			inputs = request
		}
		opcode := capability + "@1"
		mode := blueruntime.Execute
		if slot != SlotOnAir {
			mode = blueruntime.Preview
		}
		value, err := NewEffectHandlers(deps, mode)[opcode](config, inputs)
		if err != nil {
			return effects.Result{Err: err.Error()}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return effects.Result{Err: err.Error()}
		}
		return effects.Result{Value: raw}
	case capability == "zabcam.slots" && (operation == "assign" || operation == "release"):
		if slot != SlotOnAir {
			return effects.Result{Err: "PREVIEW_WRITE_FORBIDDEN"}
		}
		var err error
		if operation == "assign" {
			err = deps.AssignCameraSlot(context.Background(), stringField(request, "slot_ref"), stringField(request, "peer_label"))
		} else {
			err = deps.ReleaseCameraSlot(context.Background(), stringField(request, "slot_ref"))
		}
		if err != nil {
			return effects.Result{Err: err.Error()}
		}
	case capability == "core.lsml" && operation == "mutate":
		if mutate == nil {
			return effects.Result{Err: "EFFECT_PROVIDER_UNAVAILABLE: scene mutation sink is not configured"}
		}
		raw, err := json.Marshal(request["operations"])
		var operations []map[string]any
		if err != nil || json.Unmarshal(raw, &operations) != nil || len(operations) == 0 {
			return effects.Result{Err: "EFFECT_REQUEST_INVALID: operations are required"}
		}
		if err := mutate(slot, operations); err != nil {
			return effects.Result{Err: err.Error()}
		}
	case capability == "core.animation" && operation == "play":
		if err := h.publishAnimation(slot, stringField(invocation, "invocation_id"), request); err != nil {
			return effects.Result{Err: err.Error()}
		}
	case capability == "core.show.emit" && operation == "emit":
		topic := stringField(request, "topic")
		if topic == "" {
			return effects.Result{Err: "EFFECT_REQUEST_INVALID: topic is required"}
		}
		if slot == SlotOnAir {
			if sink == nil {
				return effects.Result{Err: "EFFECT_PROVIDER_UNAVAILABLE: show event sink is not configured"}
			}
			sink(topic, request["payload"])
		}
	case capability == "core.overlay-app" && operation == "set":
		appID, running, onAir, valid := overlayAppSetRecordFields(map[string]any{"inputs": request})
		if !valid {
			return effects.Result{Err: "EFFECT_REQUEST_INVALID: app_id and a boolean running/on_air are required"}
		}
		if slot == SlotOnAir {
			if mirror == nil {
				return effects.Result{Err: "EFFECT_PROVIDER_UNAVAILABLE: overlay mirror is not configured"}
			}
			mirror.EmitOverlayApp(appID, running, onAir)
			if status, ok := mirror.(interface{ Error() error }); ok {
				if err := status.Error(); err != nil {
					return effects.Result{Err: err.Error()}
				}
			}
		}
	default:
		return effects.Result{Err: fmt.Sprintf("EFFECT_PROVIDER_UNAVAILABLE: unsupported capability/operation %s/%s", capability, operation)}
	}
	value, err := json.Marshal(map[string]any{"ok": true, "emulated": slot != SlotOnAir})
	if err != nil {
		return effects.Result{Err: "EFFECT_RESPONSE_INVALID"}
	}
	return effects.Result{Value: value}
}
