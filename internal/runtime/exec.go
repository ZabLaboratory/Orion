package runtime

import (
	"encoding/json"
	"fmt"
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

// ExecEntry is one event entrypoint: the target the event node's exec
// out pin is wired to. Phase-1 trigger nodes (`on-start`, `on-tick`,
// `on-event`) compile down to entries; firing one creates a task.
type ExecEntry struct {
	Target ExecTarget `json:"target"`
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
	// (`orion_parked_tasks`). Issue #83 (B8) adds the cap + the
	// timer wheel that feeds `orion_timer_wheel_size`.
	ExecParkedTasks(sceneID string, n int)
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
)

// InstallExec attaches the exec program. Must be called before Run
// (like SetMirror): the scene goroutine is the only reader afterwards.
// The program itself must stay immutable — it may be shared between
// instances.
func (s *Scene) InstallExec(p *ExecProgram) {
	if p != nil {
		for _, n := range p.Nodes {
			n.seqTargets = sequenceTargets(n)
		}
	}
	s.execProg = p
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
func (s *Scene) enqueueFire(entry string) {
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
	t.pushNode(e.Target)
	s.execQueue = append(s.execQueue, t)
}

// parkTask registers a parked continuation under its wake key.
// Scene goroutine only.
func (s *Scene) parkTask(key string, t *execTask) {
	if _, dup := s.execParked[key]; dup {
		s.logger.Error("exec park: duplicate wake key — continuation dropped", "key", key)
		return
	}
	s.execParked[key] = t
	s.reportParked()
}

// resumeParked re-enqueues the continuation parked under key. Scene
// goroutine only; cross-goroutine resumes travel the inbox
// (InputMsg.ResumeExec) — the seam issue #83's timer wheel and the
// phase-3 authenticated completion path (B-syswrite: version- and
// token-stamped wake keys) build on.
func (s *Scene) resumeParked(key string) {
	t, ok := s.execParked[key]
	if !ok {
		// Stale/unknown wake key: dropped, logged — resumes nothing.
		s.logger.Warn("exec resume for unknown wake key", "key", key)
		return
	}
	delete(s.execParked, key)
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
