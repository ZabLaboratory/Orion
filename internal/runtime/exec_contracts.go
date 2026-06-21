package runtime

import (
	"encoding/json"
	"sort"
)

// Cockpit contract introspection (Orion #210, Blue ADR 008 §3.5). The
// cockpit reads ONE aggregate — GET /cockpit/contracts — to render the live
// operator UI of every active rule. The contract is DERIVED here by
// introspecting the installed exec programs + render bundle of a scene; it is
// never stored. Three facets per scene:
//
//   - params   : the scene's declared operator inputs (interface ports +
//                their UI hints — label/type/min/max/step/enum/regex).
//   - triggers : every armed `core.operator.on-call@1` entrypoint
//                (#209 — addressable via POST /operator/call).
//   - awaits   : every `core.operator.await-value@1` currently `pending`
//                (#209 registry — armed suspension points only).
//
// Scope (`scene` vs `stream`) is NOT a property read here: it is the ROLE the
// hosting scene plays in the show (ADR 009 / ADR 008 §3.5). The API layer
// stamps it — `scene` for the active scene (vanishes on a scene flip),
// `stream` for a promoted stream-level rule (survives the flip). This method
// returns the role-agnostic facets; aggregation + scope is the route's job.

// ContractParam is one interface-port facet item: a declared operator input
// of the scene, carried verbatim from the render bundle (#165 UI hints live
// inline on OperatorInput — label/type/min/max/step/enum/regex). The cockpit
// renders a control from it; `path` is the leaf the operator writes.
type ContractParam struct {
	Path       string          `json:"path"`
	Label      string          `json:"label,omitempty"`
	Type       string          `json:"type"`
	Default    json.RawMessage `json:"default,omitempty"`
	Group      string          `json:"group,omitempty"`
	OptionsSrc string          `json:"options_source,omitempty"`
	Min        *float64        `json:"min,omitempty"`
	Max        *float64        `json:"max,omitempty"`
	Step       *float64        `json:"step,omitempty"`
	MaxLength  *int            `json:"max_length,omitempty"`
	Regex      string          `json:"regex,omitempty"`
	EnumValues []string        `json:"enum_values,omitempty"`
}

// ContractTrigger is one on-call entrypoint facet item: an armed operator
// trigger the cockpit fires via POST /operator/call/{blueprint_id}/{entrypoint_id}.
// `state` is always `armed` here — an installed entrypoint is armed by
// construction (parity with awaits: Orion never emits an `idle` entry; the
// cockpit derives idle by diff if it tracks a declared superset).
type ContractTrigger struct {
	BlueprintKey string          `json:"blueprint_id"`
	EntrypointID string          `json:"entrypoint_id"`
	State        string          `json:"state"` // always "armed"
	UI           json.RawMessage `json:"ui,omitempty"`
}

// ContractAwait is one pending-await facet item — the frozen Conduit shape
// (PR #213): {blueprint_id, await_name, value_type, ui?}. `state: armed`
// is implicit by membership in this list (Orion never emits an `idle` entry,
// the cockpit derives idle by diff). Reuses PendingAwait verbatim.
type ContractAwait struct {
	BlueprintKey string          `json:"blueprint_id"`
	AwaitName    string          `json:"await_name"`
	ValueType    string          `json:"value_type"`
	State        string          `json:"state"` // always "armed"
	UI           json.RawMessage `json:"ui,omitempty"`
}

// SceneContracts is one scene's role-agnostic operator surface (params +
// triggers + awaits). The API layer wraps each facet item with the scope
// derived from the hosting scene's role.
type SceneContracts struct {
	Params   []ContractParam
	Triggers []ContractTrigger
	Awaits   []ContractAwait
}

// OperatorContracts derives this scene's cockpit contract by introspection.
// Runs on the scene goroutine via the Control seam so the pending-await
// registry is read under single-writer discipline (the install-time exec
// index and the render bundle are read-only after install, but reading them
// inside the same Control closure keeps one consistent snapshot). Returns nil
// if the scene loop is gone (inbox full / stopped) — the route treats that as
// an empty contribution.
func (s *Scene) OperatorContracts() *SceneContracts {
	reply := make(chan *SceneContracts, 1)
	if !s.Input(InputMsg{Control: func(sc *Scene) {
		reply <- sc.deriveContracts()
	}}) {
		return nil
	}
	return <-reply
}

// deriveContracts builds the three facets. Scene goroutine only.
func (s *Scene) deriveContracts() *SceneContracts {
	return &SceneContracts{
		Params:   s.contractParams(),
		Triggers: s.contractTriggers(),
		Awaits:   s.contractAwaits(),
	}
}

// contractParams reads the scene's declared operator inputs from the render
// bundle (the canonical operator-input list, ADR 003 §7.1) and maps each to a
// ContractParam. Empty when the bundle declares none.
func (s *Scene) contractParams() []ContractParam {
	if s.bundle == nil {
		return []ContractParam{}
	}
	out := make([]ContractParam, 0, len(s.bundle.OperatorInputs))
	for _, in := range s.bundle.OperatorInputs {
		out = append(out, ContractParam{
			Path:       in.Path,
			Label:      in.Label,
			Type:       in.Type,
			Default:    in.Default,
			Group:      in.Group,
			OptionsSrc: in.OptionsSrc,
			Min:        in.Min,
			Max:        in.Max,
			Step:       in.Step,
			MaxLength:  in.MaxLength,
			Regex:      in.Regex,
			EnumValues: in.EnumValues,
		})
	}
	// The bundle's OperatorInputs are already de-duplicated + ordered by the
	// compiler; sort by path for a deterministic listing regardless.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// contractTriggers walks every installed program's entrypoints and emits one
// item per armed on-call entrypoint. Deterministic order (blueprint key, then
// entrypoint id). The entrypoint id is the program-local entry key — exactly
// the {entrypoint_id} path param POST /operator/call expects.
func (s *Scene) contractTriggers() []ContractTrigger {
	out := []ContractTrigger{}
	bpKeys := make([]string, 0, len(s.execProgs))
	for k := range s.execProgs {
		bpKeys = append(bpKeys, k)
	}
	sort.Strings(bpKeys)
	for _, bp := range bpKeys {
		prog := s.execProgs[bp]
		entryIDs := make([]string, 0, len(prog.Entrypoints))
		for id := range prog.Entrypoints {
			entryIDs = append(entryIDs, id)
		}
		sort.Strings(entryIDs)
		for _, id := range entryIDs {
			e := prog.Entrypoints[id]
			if e.Kind != EntryOnCall {
				continue
			}
			var ui json.RawMessage
			if e.Node != "" {
				if n, ok := prog.Nodes[e.Node]; ok {
					ui = rawConfig(n.Config, awaitUIConfigKey)
				}
			}
			out = append(out, ContractTrigger{
				BlueprintKey: bp,
				EntrypointID: id,
				State:        "armed",
				UI:           ui,
			})
		}
	}
	return out
}

// contractAwaits projects the live pending-await registry (the #209 source of
// truth) into the cockpit await facet. Membership ⇒ armed (Conduit contract):
// Orion never emits an idle entry. Empty key = all blueprints; sorted by the
// underlying registry key for determinism (listPendingAwaits guarantees it).
func (s *Scene) contractAwaits() []ContractAwait {
	pending := s.listPendingAwaits("")
	out := make([]ContractAwait, 0, len(pending))
	for _, pa := range pending {
		out = append(out, ContractAwait{
			BlueprintKey: pa.BlueprintKey,
			AwaitName:    pa.AwaitName,
			ValueType:    pa.ValueType,
			State:        "armed",
			UI:           pa.UI,
		})
	}
	return out
}
