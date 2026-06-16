package runtime

import (
	"encoding/json"
	"sort"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Event-targeted simulate (ADR 015 §3.3): a restricted FIRING MODE of the
// validation harness, not a second engine. `Validate` sweeps every
// CanonicalEventFixtures × onTickFrames to prove air-eligibility of a
// pushed scene; `Simulate` instead fires the entrypoints the caller's
// synthetic event selects, with the caller's payload, and returns the SAME
// ValidationReport / EntrypointResult. Both share cloneValidationScene
// (validation-mode clone, inert effect seam, harness clock) and
// RunValidationEntrypoint (same interpreter, same budget) — anti-drift
// (R5 / ADR 003 R6). No persistence, no roster reload, no record: the
// endpoint returns the report verbatim.

// SyntheticEvent is the caller-supplied event a simulate run fires against.
// Topic is a quasar.* canonical type (e.g. "chat"); Payload is the value
// seeded into the event leaf the matched on-event body reads. A nil/empty
// payload falls back to CanonicalEventFixtures[topic] at the harness.
type SyntheticEvent struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Simulate fires the entrypoints selected by the synthetic event (or the
// explicit `entrypoints` allow-list, by program-local id) across every
// blueprint, each in its own validation-mode clone, and returns the
// resulting ValidationReport. The selection rule (ADR 015 §3.2 default):
//
//   - on-event entries whose Event topic equals the synthetic topic — fed
//     the supplied payload (or the canonical fixture for the topic);
//   - on-start entries — always fired (the scene's init path);
//
// When `entrypoints` is non-empty it overrides the rule: only entries
// whose program-local id is in the set are fired (on-event ones still
// seeded with the payload, others fired bare). An empty plan for a
// blueprint yields a blueprint report with no entrypoints — not a failure.
//
// Simulate NEVER touches the live show, opens a socket, writes a record,
// or reloads the roster: every clone is validation-mode (B10), driven
// synchronously by RunValidationEntrypoint.
func (h *Harness) Simulate(graph *compiler.Graph, bundle *compiler.RenderBundle, progs []*ExecProgram, ev SyntheticEvent, entrypoints []string) ValidationReport {
	rep := ValidationReport{
		HarnessVersion: HarnessVersion,
		Status:         StatusValidated,
		Budget: budgetReport{
			MaxSteps:  h.budget.MaxSteps,
			MaxWallMS: h.budget.MaxWall.Milliseconds(),
		},
	}

	payload := ev.Payload
	if len(payload) == 0 {
		payload = fixtureForEvent(ev.Topic)
	}

	var only map[string]struct{}
	if len(entrypoints) > 0 {
		only = make(map[string]struct{}, len(entrypoints))
		for _, k := range entrypoints {
			only[k] = struct{}{}
		}
	}

	sorted := append([]*ExecProgram{}, progs...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].BlueprintKey < sorted[j].BlueprintKey
	})

	for _, prog := range sorted {
		br := h.simulateBlueprint(graph, bundle, prog, ev.Topic, payload, only)
		for _, er := range br.Entrypoints {
			if !er.Pass {
				rep.Status = StatusFailed
			}
		}
		rep.Blueprints = append(rep.Blueprints, br)
	}
	return rep
}

// simulateBlueprint fires the selected entrypoints of one blueprint, each
// in a FRESH validation-mode clone (one entry's mutations never leak into
// the next), mirroring validateBlueprint's isolation.
func (h *Harness) simulateBlueprint(graph *compiler.Graph, bundle *compiler.RenderBundle, prog *ExecProgram, topic string, payload json.RawMessage, only map[string]struct{}) blueprintReport {
	br := blueprintReport{BlueprintKey: prog.BlueprintKey}
	for _, plan := range planSimulateEntrypoints(prog, topic, payload, only) {
		scene := h.cloneValidationScene(graph, bundle, prog)
		if plan.seedPath != "" {
			scene.SeedValidationLeaf(plan.seedPath, plan.seedValue)
		}
		res := scene.RunValidationEntrypoint(plan.entry, plan.env, h.budget)
		scene.cancel()
		br.Entrypoints = append(br.Entrypoints, res)
	}
	return br
}

// planSimulateEntrypoints selects the firings for a simulate run. Without
// an explicit allow-list: every on-event entry matching the topic (seeded
// the payload) plus every on-start entry (fired bare). With an allow-list:
// only entries whose program-local id is in `only` — on-event ones still
// seeded with the payload at their own event leaf, the rest fired bare.
// Deterministic order (sorted local keys), mirroring planEntrypoints.
func planSimulateEntrypoints(prog *ExecProgram, topic string, payload json.RawMessage, only map[string]struct{}) []entrypointPlan {
	keys := make([]string, 0, len(prog.Entrypoints))
	for k := range prog.Entrypoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var plans []entrypointPlan
	for _, k := range keys {
		e := prog.Entrypoints[k]
		nk := entryKey(prog.BlueprintKey, k)

		selected := false
		if only != nil {
			_, selected = only[k]
		} else {
			selected = e.Kind == EntryOnStart ||
				(e.Kind == EntryOnEvent && e.Event == topic)
		}
		if !selected {
			continue
		}

		switch e.Kind {
		case EntryOnEvent:
			plans = append(plans, entrypointPlan{
				entry:     nk,
				seedPath:  eventsPrefix + e.Event,
				seedValue: payload,
			})
		default:
			plans = append(plans, entrypointPlan{entry: nk})
		}
	}
	return plans
}
