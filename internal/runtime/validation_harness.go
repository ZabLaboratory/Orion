package runtime

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// The scene-validation harness (ADR 003 §3.2, issue #87): the capstone of
// safety that reconciles "unbounded engine" with "safe antenna". It runs a
// validation CAMPAIGN against a pushed (scene_id, scene_version) in an
// isolated, validation-mode clone of the scene — the SAME interpreter as
// live (no separate semantics to drift), the effect seam structurally
// inert (B10), every entrypoint fired and proven to terminate within
// budget. The scene is air-eligible iff EVERY entrypoint of EVERY blueprint
// passes; a budget crossing (a divergent `while`) is a FAILED validation
// and never reaches air — the language loses nothing (§1.1).

// HarnessVersion identifies the harness semantics. A change bumps it so
// the fleet re-validates (records are keyed by it). Bump when the set of
// entrypoint kinds, budgets-as-contract, or pass/fail rules change.
const HarnessVersion = "1"

// CanonicalEventFixtures maps each quasar.* canonical event type to the
// fixture leaf payload the harness feeds an on-event entrypoint that reacts
// to it (ADR §3.2.1: "each quasar.* node fed the canonical fixture for its
// event type"). The shared cross-repo fixture (criterion 9 / Quasar) is the
// source of truth for the exact bytes; the harness only needs a
// representative non-empty value of each type so the reactive cone fires.
// An on-event topic not in this map is fed a null fixture (the event still
// fires; the author's logic decides what to read).
var CanonicalEventFixtures = map[string]json.RawMessage{
	"chat":         json.RawMessage(`{"user":"_fixture","text":"hello","channel":"_fixture"}`),
	"follow":       json.RawMessage(`{"user":"_fixture"}`),
	"subscribe":    json.RawMessage(`{"user":"_fixture","tier":1,"months":1}`),
	"cheer":        json.RawMessage(`{"user":"_fixture","bits":100}`),
	"raid":         json.RawMessage(`{"user":"_fixture","viewers":10}`),
	"ban":          json.RawMessage(`{"user":"_fixture"}`),
}

// onTickFrames is how many on-tick frames the harness fires per on-tick
// entrypoint (ADR §3.2.1: "on-tick × N frames"). Bounded — the per-frame
// task must itself terminate within budget; N proves the per-frame logic
// is repeatable, not that an infinite stream of frames terminates (that is
// the live-observability concern, §3.2.2).
const onTickFrames = 8

// ExecProgramsFromGraph deserializes the compiled exec layer carried by a
// graph artefact (graph.ExecPrograms — one raw JSON ExecProgram per
// blueprint). An empty/absent set is a pure-dataflow scene (no exec logic
// to prove). A malformed entry is returned as an error so the campaign
// fails LOUDLY rather than silently airing an unproven scene (fail-loud,
// ADR §2). R9: the compiler emits no programs yet, so this returns an
// empty slice for every prod artefact today.
func ExecProgramsFromGraph(graph *compiler.Graph) ([]*ExecProgram, error) {
	if len(graph.ExecPrograms) == 0 {
		return nil, nil
	}
	out := make([]*ExecProgram, 0, len(graph.ExecPrograms))
	for i, raw := range graph.ExecPrograms {
		var p ExecProgram
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &execProgramDecodeError{index: i, err: err}
		}
		out = append(out, &p)
	}
	return out, nil
}

type execProgramDecodeError struct {
	index int
	err   error
}

func (e *execProgramDecodeError) Error() string {
	return "exec program decode (index " + strconv.Itoa(e.index) + "): " + e.err.Error()
}

// ValidationStatus is the campaign verdict.
type ValidationStatus string

const (
	StatusValidated ValidationStatus = "validated"
	StatusFailed    ValidationStatus = "failed"
)

// ValidationReport is the per-(scene_id, scene_version) campaign report
// persisted as JSONB. Status is "validated" iff every entrypoint passed.
type ValidationReport struct {
	HarnessVersion string             `json:"harness_version"`
	Status         ValidationStatus   `json:"status"`
	Budget         budgetReport       `json:"budget"`
	Blueprints     []blueprintReport  `json:"blueprints"`
}

type budgetReport struct {
	MaxSteps uint64 `json:"max_steps"`
	MaxWallMS int64 `json:"max_wall_ms"`
}

type blueprintReport struct {
	BlueprintKey string             `json:"blueprint_key"`
	Entrypoints  []EntrypointResult `json:"entrypoints"`
}

// Harness runs validation campaigns. Stateless beyond its dependencies —
// one is wired at boot and shared. It NEVER touches the live show: every
// campaign builds its own isolated clone scenes.
type Harness struct {
	registry *ComputeRegistry
	logger   *slog.Logger
	budget   ValidationBudget
}

// NewHarness builds the harness. A zero budget falls back to the ADR
// default (5 s / 1 M steps).
func NewHarness(registry *ComputeRegistry, logger *slog.Logger, budget ValidationBudget) *Harness {
	if budget.MaxSteps == 0 && budget.MaxWall == 0 {
		budget = DefaultValidationBudget
	}
	return &Harness{
		registry: registry,
		logger:   logger.With("component", "validation"),
		budget:   budget,
	}
}

// Validate runs the campaign for a scene's artefacts and the set of exec
// programs (one per blueprint). It returns the report. progs may be empty:
// a scene with no exec logic trivially validates (status=validated, no
// entrypoints) — pushing a pure-dataflow scene must not require exec work.
//
// Each program runs in its OWN validation-mode clone scene (private state,
// no subscribers, inert effect seam). The same compute registry and the
// same interpreter as live drive it — there is no separate validation
// semantics to drift from production (R6 mitigation).
func (h *Harness) Validate(graph *compiler.Graph, bundle *compiler.RenderBundle, progs []*ExecProgram) ValidationReport {
	rep := ValidationReport{
		HarnessVersion: HarnessVersion,
		Status:         StatusValidated,
		Budget: budgetReport{
			MaxSteps:  h.budget.MaxSteps,
			MaxWallMS: h.budget.MaxWall.Milliseconds(),
		},
	}

	// Sort programs by blueprint key for a deterministic report order.
	sorted := append([]*ExecProgram{}, progs...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].BlueprintKey < sorted[j].BlueprintKey
	})

	for _, prog := range sorted {
		br := h.validateBlueprint(graph, bundle, prog)
		for _, er := range br.Entrypoints {
			if !er.Pass {
				rep.Status = StatusFailed
			}
		}
		rep.Blueprints = append(rep.Blueprints, br)
	}
	return rep
}

// validateBlueprint runs every entrypoint of one blueprint, each in a
// FRESH clone scene (so one entrypoint's state mutations never leak into
// the next entrypoint's proof — each fires from declared defaults).
func (h *Harness) validateBlueprint(graph *compiler.Graph, bundle *compiler.RenderBundle, prog *ExecProgram) blueprintReport {
	br := blueprintReport{BlueprintKey: prog.BlueprintKey}

	for _, plan := range planEntrypoints(prog) {
		scene := h.cloneValidationScene(graph, bundle, prog)
		// Seed the on-event canonical fixture into the clone's state so the
		// fired body observes it on its data pulls (the live on-event path
		// reacts to a write at __events.<event>; the harness mirrors that
		// pre-state without a subscriber).
		if plan.seedPath != "" {
			scene.SeedValidationLeaf(plan.seedPath, plan.seedValue)
		}
		// The clone's goroutine is NEVER started (the harness drives it
		// synchronously), so there is no Stop() to call: cancel the bound
		// context to release the watcher resources without waiting on a
		// `done` that Run would have closed.
		res := scene.RunValidationEntrypoint(plan.entry, plan.env, h.budget)
		scene.cancel()
		br.Entrypoints = append(br.Entrypoints, res)
	}
	return br
}

// cloneValidationScene builds an isolated validation-mode clone: a private
// graph + bundle copy, validation mode set (inert effect seam + harness
// clock), the exec program installed. The scene goroutine is NEVER
// started — the harness drives it synchronously via RunValidationEntrypoint
// (single goroutine, -race clean, divergence caught by budget not by a
// spinning goroutine).
func (h *Harness) cloneValidationScene(graph *compiler.Graph, bundle *compiler.RenderBundle, prog *ExecProgram) *Scene {
	gcopy := *graph
	bcopy := *bundle
	scene := NewScene(graph.SceneID, &gcopy, &bcopy, h.registry, h.logger)
	scene.SetValidationMode()
	scene.InstallExec(prog)
	return scene
}

// entrypointPlan is one entrypoint firing the campaign performs, with the
// task-env bindings it seeds (on-tick delta) and an optional state leaf to
// pre-seed before firing (on-event canonical fixture).
type entrypointPlan struct {
	entry     string
	env       map[string]json.RawMessage
	seedPath  string
	seedValue json.RawMessage
}

// planEntrypoints enumerates EVERY entrypoint firing of a blueprint
// (ADR §3.2.1): on-start once; on-tick × N frames (delta bound); each
// on-event topic fed its canonical fixture; and explicitly-keyed entries
// (Kind == "") fired once so operator/test-only entries are still proven.
// Deterministic order (sorted entry keys, then frame index).
func planEntrypoints(prog *ExecProgram) []entrypointPlan {
	keys := make([]string, 0, len(prog.Entrypoints))
	for k := range prog.Entrypoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var plans []entrypointPlan
	for _, k := range keys {
		e := prog.Entrypoints[k]
		// The clone's trigger index is keyed by the namespaced trigger
		// key (issue #105 — InstallExec namespaces every entry
		// `<blueprint_key>/<entry_id>`); fire through the same key so the
		// resolution in RunValidationEntrypoint matches.
		nk := entryKey(prog.BlueprintKey, k)
		switch e.Kind {
		case EntryOnTick:
			// N frames, each with a representative delta_seconds bound
			// under the event node id (the live on-tick binding).
			for frame := 0; frame < onTickFrames; frame++ {
				var env map[string]json.RawMessage
				if e.Node != "" {
					env = map[string]json.RawMessage{
						e.Node + ".delta_seconds": json.RawMessage("0.016"),
					}
				}
				plans = append(plans, entrypointPlan{entry: nk, env: env})
			}
		case EntryOnEvent:
			// Feed the on-event topic its canonical fixture by pre-seeding
			// the event leaf the entrypoint's body reads — the entrypoint
			// fire itself carries no env (the value lives in state).
			plans = append(plans, entrypointPlan{
				entry:     nk,
				seedPath:  eventsPrefix + e.Event,
				seedValue: fixtureForEvent(e.Event),
			})
		case EntryOnPlatformEvent:
			// Feed the platform entry by pre-seeding the FULL platform leaf
			// it observes (e.Event already holds the canonical
			// `__inputs.platform.*` path — no prefix to prepend, unlike
			// on-event). The fire carries no env; the payload lives in state.
			plans = append(plans, entrypointPlan{
				entry:     nk,
				seedPath:  e.Event,
				seedValue: json.RawMessage(`null`),
			})
		default:
			// on-start and explicitly-fired entries: one firing.
			plans = append(plans, entrypointPlan{entry: nk})
		}
	}
	return plans
}

// fixtureForEvent returns the canonical fixture for an on-event topic, or
// null when none is declared (the event still fires).
func fixtureForEvent(event string) json.RawMessage {
	if v, ok := CanonicalEventFixtures[event]; ok {
		return v
	}
	return json.RawMessage(`null`)
}

// validationBudgetFromEnv reads the env-tunable budget
// (ORION_VALIDATION_MAX_STEPS / ORION_VALIDATION_MAX_WALL_S), falling back
// to the ADR default. Parsing lives in config; this is the typed bridge.
func ValidationBudgetFrom(maxSteps uint64, maxWall time.Duration) ValidationBudget {
	b := DefaultValidationBudget
	if maxSteps > 0 {
		b.MaxSteps = maxSteps
	}
	if maxWall > 0 {
		b.MaxWall = maxWall
	}
	return b
}
