package bluehost

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
)

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
// DECLARED set, not the currently-ARMED one: it says nothing about whether
// this specific await is parked right now. blueruntime/go now exposes the
// live registry too (Runtime.PendingAwaitNames, #344 — names only, no
// value_type/UI), surfaced here as Host.PendingAwaitNames; a caller joins
// the two BY AwaitName (see cockpit.go's appendEngineBScene, which does
// exactly that). Never present a declared entry as a live prompt on its
// own — an unarmed one is exactly the case that must NOT reach the
// operator (see PendingAwaitNames's doc).
type AwaitDecl struct {
	AwaitName string
	ValueType string
	UI        json.RawMessage
}

type programMetadata struct {
	triggers   []TriggerDecl
	awaits     []AwaitDecl
	awaitTypes map[string]string
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

// PendingAwaitNames reports the await_name of every `core.operator.
// await-value@1` suspend point currently ARMED on slot's instance — the
// LIVE registry (blueruntime.Runtime.PendingAwaitNames, #344), sorted, not
// the DECLARED set DeclaredContracts returns (a declared-but-unreached
// await never appears here). Nil when the slot holds no instance or
// nothing is armed.
//
// Routed through runtimeMu, not mu: it reads the same instance state
// Call/Resolve/Step mutate, and must observe a consistent snapshot rather
// than racing a concurrent transition (a name reported here could
// otherwise already be resolved by the time a caller acts on it — an
// unavoidable TOCTOU the caller must still tolerate, but this at least
// avoids a torn read of the registry itself).
func (h *Host) PendingAwaitNames(slot Slot) []string {
	h.mu.Lock()
	e, ok := h.slots[slot]
	h.mu.Unlock()
	if !ok || e.instance == nil { // nil instance: static occupation (#398), no await can arm
		return nil
	}

	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	return h.runtime.PendingAwaitNames(e.instance)
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
