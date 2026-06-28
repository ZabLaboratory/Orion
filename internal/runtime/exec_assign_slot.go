package runtime

import (
	"context"
	"encoding/json"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// OpAssignSlot is the runtime op the Blue node `zabcam.assign-slot@1`
// lowers to (ADR Blue 009 §3.3). It is a DOUBLE-effect node, at the
// traversal:
//
//  1. Durable egress (curated, ADR Blue 002) — an upsert to ZabCam
//     `PUT /cam/streams/{stream_id}/slots/{slot_ref}` `{peer_label}` via the
//     registry route `zabcam.slots.assign`, baked by the compiler under
//     `__route`. Identical egress machinery as `service.call`: registry-gated,
//     path built from the curated template (stream_id from the runtime scene
//     context, slot_ref escaped), token scoped to the route's token_paths,
//     caller Authorization NEVER forwarded, charged against the per-stream R3
//     budget. ZabCam is the durable authority.
//  2. Stream-level mirror + LSDP delta — ONLY on a 2xx upsert, the runtime
//     records `slot_ref → peer_label` at stream level (survives scene
//     switches) and emits an LSDP delta re-keying the slot so Solar switches
//     the `meet.peer` instantly. The LSDP state is a derived cache (ADR R2):
//     a failed egress yields ok:false and NO mirror — no divergence toward an
//     unpersisted state.
//
// Inputs (DATA): `slot_ref` (string), `peer_label` (string). Outputs: `ok`
// (bool), `error` (string) — the egress-effect continuation shape (then /
// error), like the other curated effects.
const OpAssignSlot = "assign-slot"

// internal task-env slots carrying the resolved inputs across the park, so
// the success continuation mirrors the SAME slot_ref/peer_label the egress
// upserted (re-pulling at finish could observe a since-changed reactive
// input). The `__` prefix is runtime-internal, never an authored pin.
func assignSlotRefKey(nodeID string) string   { return nodeID + ".__slotref" }
func assignPeerLabelKey(nodeID string) string { return nodeID + ".__peerlabel" }

// execAssignSlot is the `assign-slot` op. See OpAssignSlot.
func execAssignSlot(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			bindAssignSlotResult(s, node.ID, env, value)
		})
	}

	route, ok := bakedRouteOf(node)
	if !ok {
		// The compiler ALWAYS bakes `__route` on a valid push (an undeclared
		// route is rejected at compile, EGRESS_ROUTE_NOT_DECLARED). A missing
		// bake on air is a structural backstop — fail to `error`, never the
		// world, never a guess.
		return serviceCallError(s, node, "EGRESS_ROUTE_NOT_BAKED")
	}

	// Per-stream egress budget (R3): same gate as service.call. Over budget ⇒
	// fail closed to `error`, never an egress, never a mirror.
	if e := s.effects; e != nil && !e.EgressBudget.Allow(s.egressStreamKey()) {
		if e.Metrics != nil {
			e.Metrics.EgressBudgetExceeded(s.id)
		}
		return serviceCallError(s, node, "EGRESS_BUDGET_EXCEEDED")
	}

	slotRef := pullString(s, t, node, "slot_ref")
	peerLabel := pullString(s, t, node, "peer_label")
	// stream_id is the runtime scene context — NEVER authored data (RC6). The
	// path is built from the curated template; both params are escapeSegment'd
	// by BuildPath so neither can add a segment, traverse, or smuggle a query.
	values := map[string]string{
		"stream_id": s.egressStreamKey(),
		"slot_ref":  slotRef,
	}
	payload, _ := json.Marshal(struct {
		PeerLabel string `json:"peer_label"`
	}{PeerLabel: peerLabel})
	path, err := effects.BuildPath(route.PathTemplate, route.Params, values)
	if err != nil {
		return serviceCallError(s, node, "EGRESS_PARAM_MISSING: "+err.Error())
	}

	// Stash the resolved inputs for the success continuation (the mirror keys
	// off the SAME values the egress upserted).
	t.env[assignSlotRefKey(node.ID)] = mustJSONString(slotRef)
	t.env[assignPeerLabelKey(node.ID)] = mustJSONString(peerLabel)

	timeout := s.effectTimeout(t, node)
	e := s.effects
	key := s.nextWakeKey()
	run := func(ctx context.Context) effects.Result {
		if e == nil || e.ServiceCall == nil {
			return effects.Result{Err: "SERVICE_CALL_UNCONFIGURED"}
		}
		res, err := e.ServiceCall.Call(ctx, route.Method, path, route.TokenPaths, payload)
		if err != nil {
			return effects.Result{Err: "SERVICE_CALL_FAILED: " + err.Error()}
		}
		out, err := json.Marshal(res)
		if err != nil {
			return effects.Result{Err: "SERVICE_CALL_ENCODE: " + err.Error()}
		}
		return effects.Result{Value: out}
	}
	return s.effectOutcome(node, key, run, timeout)
}

// bindAssignSlotResult maps the egress result onto `ok`/`error` and, on a
// 2xx upsert ONLY, fires the stream-level mirror + LSDP delta. Runs in
// finishEffect on the scene goroutine (single-writer), so the mirror seam is
// invoked in firing order. A non-2xx upsert binds ok:false and an error
// string and emits NO mirror (ADR R2 — LSDP never diverges from ZabCam).
func bindAssignSlotResult(s *Scene, nodeID string, env map[string]json.RawMessage, value json.RawMessage) {
	slotRef := envString(env, assignSlotRefKey(nodeID))
	peerLabel := envString(env, assignPeerLabelKey(nodeID))
	delete(env, assignSlotRefKey(nodeID))
	delete(env, assignPeerLabelKey(nodeID))

	var out effects.ServiceCallResult
	if err := json.Unmarshal(value, &out); err != nil {
		env[nodeID+".ok"] = json.RawMessage("false")
		env[nodeID+".error"] = mustJSONString("ASSIGN_RESULT_DECODE")
		return
	}
	if out.Status >= 200 && out.Status < 300 {
		env[nodeID+".ok"] = json.RawMessage("true")
		// Derived cache: mirror + LSDP delta only after the durable upsert.
		if s.assignSlot != nil && slotRef != "" {
			s.assignSlot(slotRef, peerLabel)
		}
		return
	}
	env[nodeID+".ok"] = json.RawMessage("false")
	env[nodeID+".error"] = mustJSONString("ASSIGN_REJECTED")
}

// envString reads a JSON-string env slot, or "" when absent / not a string.
func envString(env map[string]json.RawMessage, key string) string {
	raw, ok := env[key]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}
