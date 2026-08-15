// Package bluehost is Orion's thin adapter over github.com/ZabLaboratory/Blue/runtime/go
// (ADR-BLUE-012 §4.3/§4.4/§6.6): it owns the preview/on-air instance
// lifecycle and isolation, translating Orion's broadcast vocabulary
// (preview/on-air) to the runtime's portable Mode (Preview/Execute).
// The runtime package it wraps is intentionally host-neutral — it opens no
// socket and holds no DB pool of its own; every Zab-specific capability
// (providers, and since ENGINE-B-PARITY-ORION the 4 opcodes-of-full-right
// EffectHandlers: core.http.request@1/core.http-request@1/core.db.query@1/
// core.service.call@1,
// see effects.go) rides through what a caller passes to Prepare/Take.
package bluehost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Slot names the two isolated instance roles a Host manages. Per §4.4
// step 6 and invariant §5.10, no on-air effect is ever armed before a
// take is committed — Preview and OnAir are always distinct instances,
// never the same handle re-tagged.
type Slot string

const (
	SlotPreview Slot = "preview"
	SlotOnAir   Slot = "on-air"
)

var (
	ErrAlreadyLoaded = errors.New("bluehost: slot already holds a running instance")
	ErrNotLoaded     = errors.New("bluehost: slot holds no instance")
)

// entry pairs a running instance with the program handle it was started
// from, so Step/Stop never need the caller to keep the handle around.
type entry struct {
	instance   *blueruntime.InstanceHandle
	digest     string            // scene_digest / program identity this slot is serving
	bundle     []byte            // optional LSML render-bundle bytes for this slot, set via SetBundle
	awaitTypes map[string]string // compiler-declared operator.await value types

	// overlaySeen dedupes core.overlay-app.set@1 dispatch (effect_overlay.go)
	// against StepResult.Variables' cumulative bag: keyed by node id, valued
	// by a digest of the last (app_id, running, on_air) actually forwarded
	// to overlayMirror. Scoped to the entry (not the Host or the Slot) so a
	// fresh instance from Prepare/Take starts with a clean slate for free —
	// the old entry, and its stale seen-set, is simply discarded, never
	// explicitly invalidated.
	overlaySeen map[string]string

	triggers []TriggerDecl // declared core.operator.on-call@1 entrypoints (operator rail, #335)
	awaits   []AwaitDecl   // declared core.operator.await-value@1 suspend points (operator rail, #335)
}

// TriggerDecl is one declared `core.operator.on-call@1` entrypoint of a
// prepared program (ORION-OPERATOR-RAIL-ENGINE-B, #335). CallID is exactly
// the id `Host.Call` expects. The operator rail (internal/api) reads this
// BEFORE calling Call: `blueruntime.Runtime.Call` silently no-ops on an
// unmatched call id instead of erroring (walker.go's
// runEntrypointsWithOutputs — a "call" entrypoint that never matches simply
// contributes nothing to the loop, err stays nil), so a Host caller wanting
// active-only "fail closed, never silent" semantics (ADR 008 parity) MUST
// pre-validate the id itself; HasTrigger below is that pre-check.
type TriggerDecl struct {
	CallID string
	UI     json.RawMessage
}

// AwaitDecl is one declared `core.operator.await-value@1` suspend point —
// name, value type and UI hint, read statically from the program at
// Prepare/Take time (same technique as awaitTypesInProgram). This is the
// DECLARED set, not the currently-ARMED one: unlike Engine A's
// listPendingAwaits (internal/runtime/exec_operator.go), blueruntime keeps
// its live pendingAwaits registry unexported (runtime.go's own Resolve
// reads instance.pendingAwaits directly) and exposes no public accessor for
// it — there is no Engine B source for "is this specific await currently
// parked right now". Callers must not present this list as a live-prompt
// feed without accounting for that gap (see cockpit.go's appendEngineBScene
// doc, which deliberately does NOT emit this facet for that reason).
type AwaitDecl struct {
	AwaitName string
	ValueType string
	UI        json.RawMessage
}

// DeclaredContracts returns slot's loaded program's statically-declared
// operator surface (triggers + awaits) — nil, nil when the slot holds no
// instance.
func (h *Host) DeclaredContracts(slot Slot) (triggers []TriggerDecl, awaits []AwaitDecl) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return nil, nil
	}
	return e.triggers, e.awaits
}

// HasTrigger reports whether callID is a declared on-call entrypoint of
// slot's loaded program. See TriggerDecl's doc for why this pre-check is
// load-bearing, not cosmetic.
func (h *Host) HasTrigger(slot Slot, callID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return false
	}
	for _, t := range e.triggers {
		if t.CallID == callID {
			return true
		}
	}
	return false
}

// declaredContracts parses a blue.program.v1 document for its declared
// on-call triggers (top-level `entrypoints[]` items with `kind:"call"`,
// UI hint read off the config of the node they target) and await-value
// suspend points (`nodes[]` items with `opcode:"core.operator.await-value@1"`,
// mirroring awaitTypesInProgram's walk but retaining the UI hint too).
// Written as a sibling of awaitTypesInProgram rather than a refactor of it:
// awaitTypesInProgram already backs the tested Resolve type-check path and
// is left untouched. Returns nil, nil on an undecodable program (same
// fail-soft posture as awaitTypesInProgram).
func declaredContracts(program []byte) (triggers []TriggerDecl, awaits []AwaitDecl) {
	decoder := json.NewDecoder(bytes.NewReader(program))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, nil
	}

	nodeConfig := map[string]map[string]any{}
	nodes, _ := document["nodes"].([]any)
	for _, rawNode := range nodes {
		node, _ := rawNode.(map[string]any)
		if node == nil {
			continue
		}
		id, _ := node["id"].(string)
		cfg, _ := node["config"].(map[string]any)
		if id != "" {
			nodeConfig[id] = cfg
		}
	}

	entrypoints, _ := document["entrypoints"].([]any)
	for _, rawEntry := range entrypoints {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil || entry["kind"] != "call" {
			continue
		}
		callID, _ := entry["id"].(string)
		if callID == "" {
			continue
		}
		nodeID, _ := entry["node_id"].(string)
		triggers = append(triggers, TriggerDecl{
			CallID: callID,
			UI:     rawConfigValue(nodeConfig[nodeID], "ui"),
		})
	}

	for _, rawNode := range nodes {
		node, _ := rawNode.(map[string]any)
		if node == nil || node["opcode"] != "core.operator.await-value@1" {
			continue
		}
		name, _ := node["id"].(string)
		config, _ := node["config"].(map[string]any)
		if configured, ok := config["await_name"].(string); ok && configured != "" {
			name = configured
		}
		if name == "" {
			continue
		}
		valueType, _ := config["value_type"].(string)
		awaits = append(awaits, AwaitDecl{
			AwaitName: name,
			ValueType: valueType,
			UI:        rawConfigValue(config, "ui"),
		})
	}

	sort.Slice(triggers, func(i, j int) bool { return triggers[i].CallID < triggers[j].CallID })
	sort.Slice(awaits, func(i, j int) bool { return awaits[i].AwaitName < awaits[j].AwaitName })
	return triggers, awaits
}

// rawConfigValue re-marshals cfg[key] to json.RawMessage, or nil when cfg is
// nil, key is absent, or re-marshalling fails.
func rawConfigValue(cfg map[string]any, key string) json.RawMessage {
	if cfg == nil {
		return nil
	}
	v, ok := cfg[key]
	if !ok {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// Host owns exactly one preview and one on-air instance at a time, per
// §4.4: "Orion canonique charge blue.program.v1 et héberge une instance
// preview Blue isolée en mémoire" / "prépare une instance on-air
// canonique depuis la même référence". A second Prepare/Take on an
// already-loaded slot is refused (ErrAlreadyLoaded) — the caller must
// Release first, keeping "no on-air effect before commit" structurally
// true: there is never a moment with two live on-air instances.
type Host struct {
	mu sync.Mutex

	// runtimeMu serializes calls into Blue's stateful InstanceHandle. It is
	// deliberately separate from mu: direct EffectHandlers may perform I/O,
	// so slot/configuration state must not remain locked while the runtime is
	// executing a host-provided handler.
	runtimeMu sync.Mutex
	runtime   *blueruntime.Runtime
	slots     map[Slot]*entry

	// httpEgress/httpRunner wire the async invocation/completion protocol
	// (Blue PR #313, runtime/go effects.go/runtime.go: StepResult.
	// Invocations + Runtime.Complete) to a real outbound HTTP executor for
	// `core.http.request` invocations — see effect_http.go. Both nil by
	// default: HTTP invocations receive an explicit provider-unavailable
	// completion, while unsupported capabilities remain pending. Set once via
	// SetHTTPEffects.
	httpEgress *effects.EgressPolicy
	httpRunner *effects.Runner
	logger     *slog.Logger

	// overlayMirror is the real core.overlay-app.set@1 effector
	// (effect_overlay.go), set once via SetOverlayMirror. nil by default:
	// every firing still lands in the reserved ctx.variables bag (walker.go),
	// but dispatchOverlayAppSet drops it instead of reaching the wire — the
	// same unwired-seam posture httpEgress/httpRunner apply above.
	overlayMirror OverlayAppMirror
}

// NewHost builds an empty Host. One Host per Orion process — it is the
// entire "cycle de vie d'instances longues de blue-runtime-go" §4.3
// grants Orion.
func NewHost() *Host {
	return &Host{
		runtime: blueruntime.NewRuntime(),
		slots:   map[Slot]*entry{},
		logger:  slog.Default(),
	}
}

// modeFor translates Orion's broadcast vocabulary to the portable ABI's
// Mode, per runtime.go's own comment: "Host vocabulary such as 'on-air'
// is intentionally translated to Execute outside this package."
func modeFor(slot Slot) blueruntime.Mode {
	if slot == SlotOnAir {
		return blueruntime.Execute
	}
	return blueruntime.Preview
}

// Prepare loads program and starts a fresh instance in slot, isolated
// from whatever the other slot is running. providers are copied by the
// portable runtime at admission (never read from a DB/catalogue) — the
// caller builds them from Orion's adapters, never from Prism or a client
// payload (§4.4 invariant: no mutable payload initializes an instance).
func (h *Host) Prepare(slot Slot, instanceID, digest string, program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy, effectHandlers map[string]blueruntime.EffectFunc) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.slots[slot]; exists {
		return fmt.Errorf("%w: %s", ErrAlreadyLoaded, slot)
	}

	handle, err := h.runtime.Load(program)
	if err != nil {
		return fmt.Errorf("bluehost: load %s: %w", slot, err)
	}

	instance, err := h.runtime.Start(handle, blueruntime.StartOptions{
		InstanceID:     instanceID,
		Mode:           modeFor(slot),
		Providers:      providers,
		Policy:         policy,
		EffectHandlers: effectHandlers,
	})
	if err != nil {
		return fmt.Errorf("bluehost: start %s: %w", slot, err)
	}

	triggers, awaits := declaredContracts(program)
	h.slots[slot] = &entry{instance: instance, digest: digest, awaitTypes: awaitTypesInProgram(program), triggers: triggers, awaits: awaits}
	return nil
}

// Digest reports the scene_digest the slot is currently serving, or ""
// if the slot is empty — used to short-circuit a redundant re-Prepare on
// the same digest (idempotent re-push, §4.4 does not require it but it
// avoids tearing down a healthy instance for a no-op push).
func (h *Host) Digest(slot Slot) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return ""
	}
	return e.digest
}

// SetBundle attaches the content-addressed LSML render-bundle bytes to
// slot's current entry, so a caller (the GET render-bundle route) can
// serve back exactly what Prepare/Take last loaded without a second
// Canvas fetch or any Store dependency. No-op if slot is empty — a
// caller races Release only at its own risk, same as every other Host
// method.
func (h *Host) SetBundle(slot Slot, bundle []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.slots[slot]; ok {
		e.bundle = bundle
	}
}

// Bundle returns the LSML render-bundle bytes SetBundle last attached to
// slot, or nil if none was set (or the slot is empty).
func (h *Host) Bundle(slot Slot) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return nil
	}
	return e.bundle
}

// Dispatch delivers an inbound event to the slot's instance.
func (h *Host) Dispatch(slot Slot, data []byte) (blueruntime.Receipt, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.Receipt{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	receipt, err := h.runtime.Dispatch(instance, data)
	h.runtimeMu.Unlock()
	return receipt, err
}

// Step advances the slot's instance by one deterministic transition. Any
// `core.effect.invoke@1` invocation the transition emitted
// (StepResult.Invocations) is handed to dispatchInvocations AFTER the
// runtime lock is released — Step itself never blocks on network I/O; the
// real HTTP call (when wired via SetHTTPEffects) runs on the worker pool
// and reports back through Runtime.Complete on its own goroutine.
func (h *Host) Step(slot Slot) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	result, err := h.runtime.Step(instance)
	h.runtimeMu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return result, nil
}

// Release stops and forgets the slot's instance. Safe to call on an
// empty slot (no-op) so a take/retake sequence never needs its own
// existence check.
func (h *Host) Release(slot Slot, reason string) error {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	instance := e.instance
	delete(h.slots, slot)
	h.mu.Unlock()

	h.runtimeMu.Lock()
	err := h.runtime.Stop(instance, reason)
	h.runtimeMu.Unlock()
	return err
}

// Take prepares a brand-new on-air instance from program and, once it
// starts cleanly, atomically releases whatever on-air instance preceded
// it and commits the new one. Per §4.4/§6.8, the default behaviour is no
// state transfer: the new instance starts cold from the same
// ResolvedSceneRef, never seeded from the outgoing instance's variables.
// A failure before the new instance starts leaves the previous on-air
// instance (if any) completely untouched — the atomicity boundary §4.4
// promises ("un échec avant commit ne modifie pas l'active").
func (h *Host) Take(instanceID, digest string, program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy, effectHandlers map[string]blueruntime.EffectFunc) error {
	h.mu.Lock()
	handle, err := h.runtime.Load(program)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("bluehost: load on-air: %w", err)
	}
	instance, err := h.runtime.Start(handle, blueruntime.StartOptions{
		InstanceID:     instanceID,
		Mode:           blueruntime.Execute,
		Providers:      providers,
		Policy:         policy,
		EffectHandlers: effectHandlers,
	})
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("bluehost: start on-air: %w", err)
	}

	triggers, awaits := declaredContracts(program)
	previous := h.slots[SlotOnAir]
	h.slots[SlotOnAir] = &entry{instance: instance, digest: digest, awaitTypes: awaitTypesInProgram(program), triggers: triggers, awaits: awaits}
	h.mu.Unlock()

	if previous != nil {
		// Compensation for the outgoing instance happens AFTER the new one
		// is committed — a Stop failure here is logged by the caller, never
		// allowed to roll back the take that already succeeded (§4.4: "un
		// échec après commit produit un état typé et une compensation").
		h.runtimeMu.Lock()
		err := h.runtime.Stop(previous.instance, "superseded-by-take")
		h.runtimeMu.Unlock()
		return err
	}
	return nil
}

// Tick advances slot's instance virtual clock by deltaSeconds, firing
// `core.event.on-tick@1` (ENGINE-B-PARITY-ORION entrypoint genre #1) and
// every `core.flow.delay@1` continuation now due. Mirrors Engine A's
// injected-clock contract (internal/runtime/exec_timer.go) — the caller
// (a periodic ticker, same cadence as the antenne scene loop) is
// responsible for calling this at a steady rate; the Host itself owns no
// clock or goroutine of its own.
func (h *Host) Tick(slot Slot, deltaSeconds float64) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	result, err := h.runtime.Tick(instance, deltaSeconds)
	h.runtimeMu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return result, nil
}

// Call fires `core.operator.on-call@1` (entrypoint genre #3) on slot's
// instance, addressed by callID — the Engine B analogue of Orion's
// `POST /operator/call/{entrypoint_id}` (Blue ADR 008 §3.2), same
// active-only routing as Engine A: a slot with no prepared instance
// returns ErrNotLoaded, never a silent no-op.
func (h *Host) Call(slot Slot, callID string, payload any) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	result, err := h.runtime.Call(instance, callID, payload)
	h.runtimeMu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return result, nil
}

// WritePlatformEvent fires `core.event.on-platform-event@1` (entrypoint
// genre #2) by writing payload onto leaf — the canonical
// `__inputs.platform.<platform>.<channel>.last_<event_type>` string,
// byte-identical to Orion's platformEventEntryLeaf (Engine A,
// internal/runtime/exec_on_platform_test.go convention) — so the SAME
// inbound platform event (Quasar/Twitch) arms Engine A and Engine B
// identically.
func (h *Host) WritePlatformEvent(slot Slot, leaf string, payload any) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	result, err := h.runtime.WritePlatformEvent(instance, leaf, payload)
	h.runtimeMu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return result, nil
}

// Resolve resumes one parked `core.operator.await-value@1` continuation
// on slot's instance — the Engine B analogue of Orion's
// `POST /operator/resolve/{await_name}` (Blue ADR 008 §3.3).
func (h *Host) Resolve(slot Slot, awaitName string, value any) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	awaitType := e.awaitTypes[awaitName]
	h.mu.Unlock()

	// Blue's portable Resolve ABI carries the value, while the compiler's
	// await node carries the static value_type. Enforce that admission at the
	// Host boundary so a fractional or otherwise malformed operator value
	// cannot resume a continuation that Engine A would keep parked.
	if awaitType != "" && !awaitValueMatchesType(value, awaitType) {
		return blueruntime.StepResult{}, &blueruntime.Error{
			SchemaVersion: blueruntime.ErrorSchema,
			Code:          "AWAIT_TYPE_MISMATCH",
			Stage:         "resolve",
			Message:       fmt.Sprintf("await %q rejects value for %s", awaitName, awaitType),
		}
	}

	h.runtimeMu.Lock()
	result, err := h.runtime.Resolve(instance, awaitName, value)
	h.runtimeMu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return result, nil
}

// awaitTypesInProgram extracts only compiler-authored await metadata. It is
// intentionally read at Prepare/Take time and never inferred from a resolve
// payload; a missing or unknown type leaves refinement to the compiler while
// the Host still rejects all known primitive mismatches fail-closed.
func awaitTypesInProgram(program []byte) map[string]string {
	decoder := json.NewDecoder(bytes.NewReader(program))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil
	}
	nodes, _ := document["nodes"].([]any)
	result := make(map[string]string)
	for _, rawNode := range nodes {
		node, _ := rawNode.(map[string]any)
		if node == nil || node["opcode"] != "core.operator.await-value@1" {
			continue
		}
		name, _ := node["id"].(string)
		config, _ := node["config"].(map[string]any)
		if configured, ok := config["await_name"].(string); ok && configured != "" {
			name = configured
		}
		valueType, _ := config["value_type"].(string)
		if name != "" && valueType != "" {
			result[name] = valueType
		}
	}
	return result
}

func awaitValueMatchesType(value any, valueType string) bool {
	encoded, err := json.Marshal(value)
	if err != nil || !json.Valid(encoded) {
		return false
	}
	switch valueType {
	case "core.primitive.string":
		var typed string
		return json.Unmarshal(encoded, &typed) == nil
	case "core.primitive.boolean":
		var typed bool
		return json.Unmarshal(encoded, &typed) == nil
	case "core.primitive.float":
		var typed float64
		return json.Unmarshal(encoded, &typed) == nil
	case "core.primitive.integer":
		var typed float64
		if json.Unmarshal(encoded, &typed) != nil || math.IsNaN(typed) || math.IsInf(typed, 0) {
			return false
		}
		return typed == math.Trunc(typed)
	default:
		return true
	}
}

// Complete reports a provider's outcome for a `core.effect.invoke@1`
// invocation Step/Tick/Call/WritePlatformEvent previously emitted
// (StepResult.Invocations) — the async admission-protocol completion
// path, distinct from the 4 synchronous EffectHandlers opcodes of full
// right wired at Prepare/Take.
func (h *Host) Complete(slot Slot, data []byte) (blueruntime.Receipt, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.Receipt{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	h.mu.Lock()
	current, ok := h.slots[slot]
	if !ok || current.instance != instance {
		h.mu.Unlock()
		h.runtimeMu.Unlock()
		return blueruntime.Receipt{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	h.mu.Unlock()
	receipt, err := h.runtime.Complete(instance, data)
	h.runtimeMu.Unlock()
	return receipt, err
}
