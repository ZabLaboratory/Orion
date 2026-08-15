// effect_overlay.go wires `core.overlay-app.set@1` to the real stream-level
// overlay-app wire effector (internal/lsdp.Wire.EmitOverlayApp) — the ENGINE-
// B-PARITY-ORION host mirror walker.go's fireLocalSideEffect defers to this
// package for. Its doc comment is explicit: "This portable core has no host
// mirror/effector to write into ... the write lands in a reserved namespaced
// ctx.variables bag instead". Unlike the 4 opcodes of full right (effects.go,
// NewEffectHandlers), the portable core NEVER calls back into a host-injected
// StartOptions.EffectHandlers entry for this opcode — walker.go's dispatch
// (`case "core.animation.play@1", "core.show.emit@1", "core.overlay-app.set@1":
// return w.fireLocalSideEffect(...)`) is unconditional, so an EffectHandlers
// map entry keyed "core.overlay-app.set@1" would simply never be called. The
// only surfaced signal is the reserved bag riding StepResult.Variables
// (runtime.go); dispatchOverlayAppSet reads it after every
// Step/Tick/Call/WritePlatformEvent/Resolve.
//
// `core.animation.play@1` and `core.show.emit@1` share the exact same bag
// mechanism but are deliberately OUT of scope here: no production stream-rule
// consumes them through Engine B yet, and wiring an effector nobody calls
// would be unverifiable dead code (ORION-OVERLAY-EFFECTOR-STREAM-RULE-PROOF).
package bluehost

import (
	"sort"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// overlayAppSetBag is the reserved ctx.variables key walker.go's
// fireLocalSideEffect writes every core.overlay-app.set@1 firing under:
// "__" + strings.TrimPrefix(strings.TrimSuffix(opcodeID, "@1"), "core.").
const overlayAppSetBag = "__overlay-app.set"

// OverlayAppMirror is the minimal seam this package needs onto the LSDP
// wire's stream-level overlay-app control mirror. internal/lsdp.Wire
// satisfies it directly (its EmitOverlayApp method); internal/runtime.
// MirrorRegistry declares the identical method for Engine A's Show. A narrow
// local interface — rather than importing internal/runtime, which would also
// drag in MirrorFor/SetActive/Drop/EmitRoster/EmitSlotAssignment this package
// never uses, and internal/runtime already imports internal/bluehost
// (bluewire does, transitively) — keeps this package's dependency surface
// exactly what it needs, matching EffectDeps' own dependency-minimal fields
// (effects.go).
type OverlayAppMirror interface {
	EmitOverlayApp(appID string, running, onAir *bool)
}

// SetOverlayMirror wires the real stream-level overlay-app wire effector
// core.overlay-app.set@1 forwards to. Normally antenneWire
// (cmd/orion/main.go), the SAME lsdp.Wire Engine A's Show.mirrors drives.
// nil (unset, or the antenne LSDP wire never built — bespoke mode) drops
// every core.overlay-app.set@1 firing: the reserved ctx.variables bag
// walker.go still writes stays the only observable trace, the same
// fail-closed posture SetHTTPEffects' unconfigured egress/runner already
// applies.
func (h *Host) SetOverlayMirror(m OverlayAppMirror) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.overlayMirror = m
}

// dispatchOverlayAppSet forwards every core.overlay-app.set@1 record found in
// variables (a Step/Tick/Call/WritePlatformEvent/Resolve's
// StepResult.Variables) to h.overlayMirror.
//
// Preview NEVER touches the real antenna wire — same posture NewEffectHandlers
// already applies to http/db/service.call (effects.go: "blueruntime.Preview
// NEVER dials the network or the DB"). A preview instance's firings still
// land in the bag (walker.go writes it unconditionally); they are just never
// forwarded to the wire from here.
//
// variables is a clone of the instance's ENTIRE accumulated ctx.variables —
// cumulative across the instance's whole lifetime, not a per-step delta
// (runtime.go's `Variables: cloneMap(instance.variables)` on every
// Step/Tick/Call/WritePlatformEvent/Resolve) — so every previously-fired
// node's record reappears on every subsequent call. lumencast-go's
// server.Server.SetOverlayApps has no dedup of its own (it unconditionally
// fans out the full show-level overlay_apps snapshot to every live
// subscriber on every call), so re-dispatching an unchanged record here would
// re-broadcast forever on every Tick. entry.overlaySeen (host.go, one map per
// Host slot ENTRY — replaced wholesale on Prepare/Take, never explicitly
// invalidated) is the edge-detector: a node id is only (re-)dispatched when
// its resolved (app_id, running, on_air) digest changed since the last
// dispatch this same instance produced.
//
// instance identifies which entry produced variables, so a result racing a
// concurrent Take/Release that has already superseded slot's entry is
// dropped rather than corrupting the new entry's dedup state or emitting a
// stale instance's intent onto the wire — the same "drop, do not project a
// stale result" posture bluewire.Bridge's sequenceFor applies one layer up.
func (h *Host) dispatchOverlayAppSet(slot Slot, instance *blueruntime.InstanceHandle, variables map[string]any) {
	if modeFor(slot) != blueruntime.Execute {
		return
	}
	h.mu.Lock()
	mirror := h.overlayMirror
	h.mu.Unlock()
	if mirror == nil {
		return
	}
	bag, _ := variables[overlayAppSetBag].(map[string]any)
	if len(bag) == 0 {
		return
	}

	nodeIDs := make([]string, 0, len(bag))
	for nodeID := range bag {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs) // deterministic dispatch order, same posture as bluewire's sorted patch paths

	for _, nodeID := range nodeIDs {
		record, _ := bag[nodeID].(map[string]any)
		appID, running, onAir, ok := overlayAppSetRecordFields(record)
		if !ok {
			continue
		}
		digest := overlayAppSetDigest(appID, running, onAir)

		h.mu.Lock()
		e, live := h.slots[slot]
		if !live || e.instance != instance {
			h.mu.Unlock()
			continue
		}
		if e.overlaySeen != nil && e.overlaySeen[nodeID] == digest {
			h.mu.Unlock()
			continue
		}
		if e.overlaySeen == nil {
			e.overlaySeen = map[string]string{}
		}
		e.overlaySeen[nodeID] = digest
		h.mu.Unlock()

		mirror.EmitOverlayApp(appID, running, onAir)
	}
}

// overlayAppSetRecordFields resolves app_id/running/on_air from a
// fireLocalSideEffect record. Both `inputs` (gatherDataInputs — a wired
// connection or a compiler-folded literal, walker.go) and `config` (the raw
// authored node config) are checked, inputs taking precedence — the same
// wired-then-literal precedence Engine A's pull* helpers apply
// (internal/runtime/exec_overlay_app.go), so a hand-built low-level
// blue.program.v1 fixture that bakes app_id/running/on_air straight onto
// node.config (bypassing a compiler's literal-folding into a data port, the
// shape internal/runtime's own A/B parity harness uses for this exact
// opcode) resolves identically to a compiled one. app_id is required (same
// as Engine A's execOverlayAppSet); running/on_air both absent is also a
// no-op — EmitOverlayApp itself already treats that as "nothing to record"
// (overlay_mirror.go) — skip it here too rather than manufacture a digest
// for a null-op record.
func overlayAppSetRecordFields(record map[string]any) (appID string, running, onAir *bool, ok bool) {
	if record == nil {
		return "", nil, nil, false
	}
	inputs, _ := record["inputs"].(map[string]any)
	config, _ := record["config"].(map[string]any)

	appID = stringField(inputs, "app_id")
	if appID == "" {
		appID = stringField(config, "app_id")
	}
	if appID == "" {
		return "", nil, nil, false
	}

	running = boolPtrField(inputs, "running")
	if running == nil {
		running = boolPtrField(config, "running")
	}
	onAir = boolPtrField(inputs, "on_air")
	if onAir == nil {
		onAir = boolPtrField(config, "on_air")
	}
	if running == nil && onAir == nil {
		return "", nil, nil, false
	}
	return appID, running, onAir, true
}

// boolPtrField reads an OPTIONAL boolean field: nil when absent or not a
// bool. The walker.go inputs/config maps carry decoded JSON values, so a
// present `true`/`false` always decodes to a native Go bool.
func boolPtrField(m map[string]any, key string) *bool {
	v, ok := m[key].(bool)
	if !ok {
		return nil
	}
	return &v
}

// overlayAppSetDigest encodes (appID, running, onAir) into a short
// comparable string: "?" per dimension = unset (untouched), "T"/"F" = the
// resolved value. Cheap deliberately — this runs once per overlay-app.set
// node per Step/Tick, not a hot allocation-sensitive path.
func overlayAppSetDigest(appID string, running, onAir *bool) string {
	return appID + "|" + boolPtrDigest(running) + boolPtrDigest(onAir)
}

func boolPtrDigest(v *bool) string {
	if v == nil {
		return "?"
	}
	if *v {
		return "T"
	}
	return "F"
}
