package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// This file is the exec-layer half of the hybrid scheduler decided by
// ADR 003 §3.1 (issue #82, phase 1): the program model (what an exec
// chain looks like at runtime), the per-scene task scheduling (fire /
// shed / park / resume), and the effect seam. The interpreter itself
// (continuation frames + step function) lives in exec_interpreter.go.
//
// Doctrine (ADR 003 §1.1, non-negotiable): the exec layer NEVER kills,
// skips or bounds a running task. Time-slicing exists to keep the scene
// loop fair (re-enqueue, resume exactly where it stopped); the B5
// budget back-pressures NEW fires only. Boundedness is an authoring
// concern proven by the phase-4 validation gate, never an engine cap.

// Exec op names — the runtime-canonical vocabulary of the interpreter.
// The compiler partition (ADR 003 §3.1.2, a separate phase-1 issue)
// maps Blue manifest ids (e.g. `core.flow.branch@1`) onto these ops
// when it starts emitting ExecPrograms; keeping the runtime vocabulary
// independent of wire ids means the interpreter does not have to guess
// Blue's qualified names before that issue lands.
const (
	OpBranch      = "branch"
	OpSequence    = "sequence"
	OpGate        = "gate"
	OpForLoop     = "for-loop"
	OpForEach     = "for-each"
	OpWhile       = "while"
	OpVariableSet = "variable.set"
	OpPrint       = "print"
	// OpDelay is the latent timer node (issue #83): parks the chain's
	// continuation on the scene's timer wheel (exec_timer.go).
	OpDelay = "delay"
)

// ExecTarget addresses one end of an exec edge: a node plus the exec
// input pin the edge lands on (UE-faithful — `gate` distinguishes
// `enter`/`open`/`close`/`toggle`).
type ExecTarget struct {
	Node string `json:"node"`
	Port string `json:"port,omitempty"`
}

// ExecDataInput is one inbound DATA edge on an exec node: the value the
// node pulls on demand (branch.condition, variable.set.value, loop
// bounds). From/FromPort address the producer — FromPort matters when
// the producer is another exec node's data out (a loop's `index` /
// `element` pin, bound in the task environment).
type ExecDataInput struct {
	Port     string `json:"port"`
	From     string `json:"from"`
	FromPort string `json:"from_port,omitempty"`
}

// ExecNode is one node of an exec chain.
type ExecNode struct {
	ID     string                     `json:"id"`
	Op     string                     `json:"op"`
	Config map[string]json.RawMessage `json:"config,omitempty"`
	Data   []ExecDataInput            `json:"data,omitempty"`
	// Next maps each exec OUT pin to its target ("then", "true",
	// "false", "body", "completed", "exit", "then_0"…). At most one
	// target per pin (Blue validates exec out-pins single-wired).
	Next map[string]ExecTarget `json:"next,omitempty"`

	// seqTargets is the precomputed `then_0..then_N` order for
	// sequence nodes (installExec). The interpreter never derives
	// sibling order from Next's map iteration — determinism is the
	// whole point of the continuation design (ADR 003 §3.1.7).
	seqTargets []ExecTarget
}

// next returns the first wired target among the given pin names.
func (n *ExecNode) next(names ...string) (ExecTarget, bool) {
	for _, name := range names {
		if t, ok := n.Next[name]; ok {
			return t, true
		}
	}
	return ExecTarget{}, false
}

// Entry kinds — the trigger vocabulary (ADR 003 §3.1.3, issue #83).
// The runtime wires each kind to its firing source: `on-start` fires
// when the scene becomes live and on each test-session open;
// `on-tick` fires per global tick (tick.go) with `delta_seconds`
// bound; `on-event` fires on a write to `__events.<event>`.
const (
	EntryOnStart = "on-start"
	EntryOnTick  = "on-tick"
	EntryOnEvent = "on-event"
)

// ExecEntry is one event entrypoint: the target the event node's exec
// out pin is wired to. Phase-1 trigger nodes (`on-start`, `on-tick`,
// `on-event`) compile down to entries; firing one creates a task.
type ExecEntry struct {
	Target ExecTarget `json:"target"`
	// Kind selects the runtime trigger ("" = fired explicitly via
	// FireExec only — tests, future operator dispatch).
	Kind string `json:"kind,omitempty"`
	// Event names the `__events.<event>` topic an on-event entry
	// listens to.
	Event string `json:"event,omitempty"`
	// Node is the event node's id — the namespace its data out pins
	// (`<node>.delta_seconds` for on-tick) are bound under in the
	// task environment.
	Node string `json:"node,omitempty"`
}

// ExecProgram is a blueprint's exec layer, ready to interpret. It is
// READ-ONLY after InstallExec: two scene instances may share one
// program (live + test session) — all mutable execution state lives on
// the task and in each scene's own State, never on the program (B9).
type ExecProgram struct {
	// BlueprintKey namespaces `__vars.<key>.<name>` and the
	// `__debug.<key>.print` ring (ADR 001 §3.3 / ADR 003 §3.1.3).
	BlueprintKey string                `json:"blueprint_key"`
	Nodes        map[string]*ExecNode  `json:"nodes"`
	Entrypoints  map[string]ExecEntry  `json:"entrypoints"`
}

// Effector is the single seam through which the exec layer touches the
// world (ADR 003 §3.1.1): every effect goes through it, never through
// a concurrent state access. The default implementation (sceneEffector)
// writes the OWNING scene instance's state, on the scene goroutine —
// single-writer and `__vars` isolation (B9) hold by construction. The
// phase-4 validation harness swaps in a structurally inert
// implementation here (B10); phase-3 async effects extend it.
type Effector interface {
	// SetLeaf writes a state leaf of this scene instance and seeds
	// the dirty cone so the data layer and subscribers react.
	SetLeaf(path string, value json.RawMessage)
	// Print records a debug line (structured log + `__debug` ring).
	Print(blueprintKey, line string)
}

// sceneEffector is the live-mode Effector: intra-goroutine writes into
// the scene's own state.
type sceneEffector struct{ s *Scene }

func (e *sceneEffector) SetLeaf(path string, value json.RawMessage) {
	if e.s.state.Set(path, value) {
		e.s.pending[path] = struct{}{}
	}
	// Validation-campaign capture (issue #87): record every leaf the
	// scene would write so the report carries it. No-op outside a
	// campaign (validation == nil). This is the SAME intra-goroutine
	// write live takes — the capture is a tap, not a divergence.
	e.s.validation.recordLeaf(path)
}

// printRingSize bounds the `__debug.<key>.print` ring (ADR 003
// §3.1.3 — "ring of last N lines").
const printRingSize = 50

func (e *sceneEffector) Print(blueprintKey, line string) {
	e.s.logger.Info("blueprint print", "blueprint_key", blueprintKey, "line", line)
	leaf := "__debug." + blueprintKey + ".print"
	var ring []string
	if raw, ok := e.s.state.Get(leaf); ok {
		_ = json.Unmarshal(raw, &ring)
	}
	ring = append(ring, line)
	if len(ring) > printRingSize {
		ring = ring[len(ring)-printRingSize:]
	}
	out, err := json.Marshal(ring)
	if err != nil {
		return
	}
	e.SetLeaf(leaf, out)
}

// ExecMetrics is the observability seam the exec layer reports
// through. *obs.Metrics implements it (compile-checked where main
// wires Show.SetExecMetrics). nil = metrics disabled (tests, sessions
// not yet wired).
type ExecMetrics interface {
	// ExecEventShed counts a B5 back-pressure shed: a NEW fire
	// dropped because the per-scene concurrent-task budget is full
	// (`orion_event_shed_total`). Never a killed task.
	ExecEventShed(sceneID string)
	// ExecTaskPreempt counts a time-slice yield + re-enqueue
	// (`orion_task_preempt_total`).
	ExecTaskPreempt(sceneID string)
	// ExecParkedTasks gauges the parked-continuation count
	// (`orion_parked_tasks`).
	ExecParkedTasks(sceneID string, n int)
	// ExecTimerWheelSize gauges the armed timer entries
	// (`orion_timer_wheel_size`, issue #83).
	ExecTimerWheelSize(sceneID string, n int)
	// ExecParkDropped counts a dropped park
	// (`orion_exec_park_dropped_total`): reason "duplicate_key" is
	// the C1 ordering-anomaly counter (a wake key collided — the
	// continuation is dropped, loudly); reason "cap" is the B8
	// back-pressure shed of a NEW park when the per-scene parked cap
	// is full. Neither ever touches an already-parked or running task.
	ExecParkDropped(sceneID, reason string)
	// ExecResumeStale counts a resume dropped by the version/epoch
	// stamp check (`orion_exec_resume_stale_total`, ADR 003 §3.1.4):
	// a wake key minted before a cancellation resumes nothing.
	ExecResumeStale(sceneID string)
	// ExecResumeUnknown counts a resume whose wake key matches no
	// parked continuation (issue #86,
	// `orion_exec_completion_rejected_total{reason="unknown"}`): a
	// forged/cross-scene external report, OR the benign loser of the
	// report-vs-duration-fallback race (the winner consumed the key).
	// Either way it is dropped and resumes nothing.
	ExecResumeUnknown(sceneID string)
}

// Exec scheduling defaults (ADR 003 §3.1.3: "after a step budget —
// default 10 000 steps or 4 ms, env-tunable — the task yields").
// Per-scene overrides via SetExecSlicing / SetExecBudget pre-Run.
const (
	defaultExecSliceSteps    = 10_000
	defaultExecSliceDuration = 4 * time.Millisecond
	// defaultExecTaskBudget is the B5 per-scene concurrent exec-task
	// budget (queued + parked). Beyond it, new fires are shed with
	// `orion_event_shed_total` — running tasks are untouched.
	defaultExecTaskBudget = 1024
	// execTimeCheckEvery throttles the wall-clock check inside a
	// slice (time.Now per step would dominate small steps). The
	// step budget itself is exact and deterministic.
	execTimeCheckEvery = 256
	// defaultExecParkCap is the B8 per-scene cap on parked
	// timers/continuations (ADR 003 §3.1.6). Beyond it a NEW park is
	// shed and counted (`orion_exec_park_dropped_total{reason="cap"}`)
	// — tasks already parked or running are untouched.
	defaultExecParkCap = 1024
)

// InstallExec attaches the exec program. Must be called before Run
// (like SetMirror): the scene goroutine is the only reader afterwards.
// The program itself must stay immutable — it may be shared between
// instances.
func (s *Scene) InstallExec(p *ExecProgram) {
	s.execOnStart, s.execOnTick = nil, nil
	s.execOnEvent = nil
	if p != nil {
		for _, n := range p.Nodes {
			n.seqTargets = sequenceTargets(n)
		}
		// Trigger indexes (issue #83), in SORTED entry-key order: the
		// firing order of multiple entries of one kind must never
		// derive from map iteration.
		keys := make([]string, 0, len(p.Entrypoints))
		for k := range p.Entrypoints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch e := p.Entrypoints[k]; e.Kind {
			case EntryOnStart:
				s.execOnStart = append(s.execOnStart, k)
			case EntryOnTick:
				s.execOnTick = append(s.execOnTick, k)
			case EntryOnEvent:
				if e.Event == "" {
					continue
				}
				if s.execOnEvent == nil {
					s.execOnEvent = map[string][]string{}
				}
				s.execOnEvent[e.Event] = append(s.execOnEvent[e.Event], k)
			}
		}
	}
	s.execProg = p
}

// FireOnStart fires every `on-start` entrypoint (ADR 003 §3.1.3: the
// scene becomes live, or a test session opens). Safe from any
// goroutine — the fires travel the inbox; execOnStart is built pre-Run
// and read-only afterwards. A scene without an exec program (every
// prod scene until the phase-4 gate wires exec live) is a no-op.
func (s *Scene) FireOnStart(source string) {
	for _, k := range s.execOnStart {
		if !s.Input(InputMsg{FireExec: k, Source: source}) {
			s.logger.Warn("on-start fire dropped: inbox full", "entry", k)
		}
	}
}

// sequenceTargets extracts `then_0..then_N` in ascending index order,
// stopping at the first unwired index — a deterministic order that
// never depends on map iteration.
func sequenceTargets(n *ExecNode) []ExecTarget {
	if n.Op != OpSequence {
		return nil
	}
	var out []ExecTarget
	for i := 0; ; i++ {
		t, ok := n.Next[fmt.Sprintf("then_%d", i)]
		if !ok {
			return out
		}
		out = append(out, t)
	}
}

// SetExecMetrics installs the metrics sink. Pre-Run only.
func (s *Scene) SetExecMetrics(m ExecMetrics) { s.execMetrics = m }

// SetExecBudget overrides the B5 concurrent-task budget. Pre-Run only.
// n <= 0 disables the budget (unbounded).
func (s *Scene) SetExecBudget(n int) { s.execBudget = n }

// SetExecSlicing overrides the time-slice budgets. Pre-Run only.
// Tests pin steps low (deterministic preemption) or the duration high
// (wall-clock out of the picture).
func (s *Scene) SetExecSlicing(steps int, dur time.Duration) {
	if steps > 0 {
		s.execSliceSteps = steps
	}
	if dur > 0 {
		s.execSliceDur = dur
	}
}

// SetEffector swaps the effect seam. Pre-Run only. The phase-4
// validation harness installs its structurally inert implementation
// through this (B10).
func (s *Scene) SetEffector(e Effector) { s.effector = e }

// registerExecOp installs an additional exec op. Pre-Run only. This is
// the seam issue #83 (`delay` + timer wheel) and phase 3 (async
// effects) plug into; tests use it to exercise the park/resume path
// with a synthetic latent op.
func (s *Scene) registerExecOp(op string, fn execOpFn) {
	if s.execOps == nil {
		s.execOps = map[string]execOpFn{}
	}
	s.execOps[op] = fn
}

// FireExec requests an exec entrypoint fire. Safe from any goroutine:
// the request travels the inbox (FIFO with state writes, so the order
// between fires and inputs is the arrival order) and the budget check
// + task creation happen on the scene goroutine — single-writer holds.
// Returns false if the inbox is full.
func (s *Scene) FireExec(entry, source string) bool {
	return s.Input(InputMsg{FireExec: entry, Source: source})
}

// enqueueFire creates a task for an entrypoint fire — or sheds it
// under B5 back-pressure. Scene goroutine only.
func (s *Scene) enqueueFire(entry string) { s.enqueueFireEnv(entry, nil) }

// enqueueFireEnv is enqueueFire with event-pin bindings seeded into
// the task environment (on-tick's `<node>.delta_seconds`).
func (s *Scene) enqueueFireEnv(entry string, env map[string]json.RawMessage) {
	if s.execProg == nil {
		s.logger.Warn("exec fire on scene without exec program", "entry", entry)
		return
	}
	e, ok := s.execProg.Entrypoints[entry]
	if !ok {
		s.logger.Warn("exec fire for unknown entrypoint", "entry", entry)
		return
	}
	// B5 back-pressure (ADR 003 §3.1.6): the budget counts ALIVE
	// tasks (runnable + parked). Beyond it the NEW fire is shed and
	// counted — a running task is never killed.
	if s.execBudget > 0 && len(s.execQueue)+len(s.execParked) >= s.execBudget {
		if s.execMetrics != nil {
			s.execMetrics.ExecEventShed(s.id)
		}
		s.logger.Debug("exec fire shed (task budget full)",
			"entry", entry, "budget", s.execBudget)
		return
	}
	s.execTaskSeq++
	t := &execTask{
		id:  s.execTaskSeq,
		env: map[string]json.RawMessage{},
	}
	for k, v := range env {
		t.env[k] = v
	}
	t.pushNode(e.Target)
	s.execQueue = append(s.execQueue, t)
}

// parkTask registers a parked continuation under its wake key and
// reports whether the park was accepted. Scene goroutine only.
//
// Two drop paths, both counted on `orion_exec_park_dropped_total`:
//   - "duplicate_key" (C1, Bastion condition inherited from #82): a
//     wake-key collision is an ordering anomaly — dropped loudly and
//     observable, never silent.
//   - "cap" (B8 back-pressure, ADR 003 §3.1.6): the per-scene parked
//     cap is full, so the NEW park is shed. The task that requested
//     the park continues with its remaining frames; tasks already
//     parked or running are never touched (doctrine §1.1).
func (s *Scene) parkTask(key string, t *execTask) bool {
	if _, dup := s.execParked[key]; dup {
		if s.execMetrics != nil {
			s.execMetrics.ExecParkDropped(s.id, "duplicate_key")
		}
		s.logger.Error("exec park: duplicate wake key — continuation dropped", "key", key)
		return false
	}
	if s.execParkCap > 0 && len(s.execParked) >= s.execParkCap {
		if s.execMetrics != nil {
			s.execMetrics.ExecParkDropped(s.id, "cap")
		}
		s.logger.Warn("exec park shed (B8 parked cap full)",
			"key", key, "cap", s.execParkCap)
		return false
	}
	s.execParked[key] = t
	s.reportParked()
	return true
}

// resumeParked re-enqueues the continuation parked under key. Scene
// goroutine only; cross-goroutine resumes travel the inbox
// (InputMsg.ResumeExec); timer-wheel resumes call this directly from
// fireDueTimers. The phase-3 authenticated completion path
// (B-syswrite) adds the token stamp + role check ABOVE this gate; the
// version/epoch staleness check below stays as the inner gate.
func (s *Scene) resumeParked(key string) { s.resumeParkedWith(key, nil) }

// resumeParkedWith is resumeParked with completion bindings (phase 3,
// issue #85): env entries are merged into the parked continuation's
// environment before re-enqueue, so the resumed chain observes the
// effect's outputs through the same task-env pins as loop bindings.
// Scene goroutine only.
func (s *Scene) resumeParkedWith(key string, env map[string]json.RawMessage) {
	if ver, epoch, stamped := parseWakeStamp(key); stamped &&
		(ver != s.graph.SceneVersion || epoch != s.execEpoch) {
		// Version-stamped wake key of a cancelled epoch / another
		// version (ADR 003 §3.1.4): the in-flight result is discarded
		// on arrival — dropped, counted, resumes nothing.
		if s.execMetrics != nil {
			s.execMetrics.ExecResumeStale(s.id)
		}
		s.logger.Warn("exec resume dropped: stale wake key",
			"key", key, "epoch", s.execEpoch)
		return
	}
	t, ok := s.execParked[key]
	if !ok {
		// Unknown wake key: dropped, logged, counted — resumes nothing.
		// This is gate (4) of the external completion contract (#86)
		// AND the idempotent second resolution of the report-vs-
		// duration-fallback race (the first resolver consumed the key).
		if s.execMetrics != nil {
			s.execMetrics.ExecResumeUnknown(s.id)
		}
		s.logger.Warn("exec resume for unknown wake key", "key", key)
		return
	}
	delete(s.execParked, key)
	for k, v := range env {
		t.env[k] = v
	}
	s.execQueue = append(s.execQueue, t)
	s.reportParked()
}

func (s *Scene) reportParked() {
	if s.execMetrics != nil {
		s.execMetrics.ExecParkedTasks(s.id, len(s.execParked))
	}
}

// runExecSlice runs the head task for one time slice, then returns so
// the loop can drain the inbox, recompute the dirty cone and emit a
// delta. A task that exhausts its slice is RE-ENQUEUED AT THE TAIL —
// round-robin fairness across tasks, and never, under any condition, a
// kill: preemption changes scheduling, not semantics (ADR 003 §3.1.3).
func (s *Scene) runExecSlice() {
	if len(s.execQueue) == 0 {
		return
	}
	t := s.execQueue[0]
	s.execQueue = s.execQueue[1:]
	start := time.Now()
	for steps := 0; ; {
		if len(t.frames) == 0 {
			return // task complete
		}
		s.stepTask(t)
		steps++
		if steps >= s.execSliceSteps ||
			(steps%execTimeCheckEvery == 0 && time.Since(start) >= s.execSliceDur) {
			if len(t.frames) > 0 {
				s.execQueue = append(s.execQueue, t)
				if s.execMetrics != nil {
					s.execMetrics.ExecTaskPreempt(s.id)
				}
			}
			return
		}
	}
}
