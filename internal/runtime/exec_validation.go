package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Validation-mode inertia of the effect seam (ADR 003 §3.2.1 / risk B10,
// issue #87). This is the STRUCTURAL half of the scene-validation gate:
// when a scene runs in validation mode, NO effect touches the world. That
// is a property of the effect-execution SEAM itself, not an opt-in flag
// each effect remembers to honour — the rot B10 forbids.
//
// How the inertia is structural, not per-effect:
//
//   - Every world-touching async-effect op (`http.request`, `db.query`,
//     `source.read`) is dispatched through execNode. In validation mode
//     execNode routes EVERY such op through ONE path — validationEffect —
//     BEFORE the op's real closure runs. The op's I/O closure (the only
//     code that opens a socket / a pgx connection / reads a live source)
//     is NEVER invoked. There is no way for an op to reach the world: the
//     seam decides, not the op.
//   - An effect added later cannot leak: ValidateValidationModeCoverage is
//     INTROSPECTIVE — it reflects the ops SetEffects ACTUALLY registers
//     (registeredWorldEffectOps installs SetEffects on a throwaway scene)
//     and fails the harness for any of them without a declared
//     validation-mode synthetic response. The world-op set derives from the
//     single worldEffectRegistrations table SetEffects installs from, so a
//     new op that forgets to declare its behaviour fails the harness rather
//     than silently opening a socket — there is no second list to forget.
//   - `animation.play` is NOT a world-touching op (its "effect" is a
//     scene-state write rendered off the delta pipe); in validation mode
//     it completes through its existing duration fallback (the timer wheel
//     resumes the parked `completed` continuation), capturing the attempt.
//   - `variable.set` / `print` / `gate` write only THIS clone instance's
//     own state through the (capturing) effector — never the world.

// worldEffectOps is the set of exec ops that perform EXTERNAL I/O (open a
// socket, a DB connection, or read a live source). It is DERIVED from
// worldEffectRegistrations (exec_effects.go) — the SAME table SetEffects
// installs from — so it can never drift from what SetEffects actually
// registers. Every entry MUST have a declared validation-mode synthetic
// response in validationSyntheticResult; the guard reflects the live
// registry of a Scene after SetEffects and fails on any op that does not,
// so a phase-N world op added to that table without a validation-mode
// declaration fails the harness, not a live egress.
var worldEffectOps = func() map[string]struct{} {
	m := make(map[string]struct{}, len(worldEffectRegistrations))
	for _, r := range worldEffectRegistrations {
		m[r.op] = struct{}{}
	}
	return m
}()

// EnumerateWorldEffects returns the world-touching op names in sorted
// order — the registry the guard test (criterion 14) enumerates.
func EnumerateWorldEffects() []string {
	out := make([]string, 0, len(worldEffectOps))
	for op := range worldEffectOps {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// validationSyntheticResult returns the declared-shape synthetic value a
// world-touching op binds in validation mode — the same shape its real
// completion would carry, so the continuation walks `then`/`completed`
// exactly as live (proving termination through the success path), never
// the world. A new world op MUST extend this switch; the guard test fails
// the harness otherwise.
func validationSyntheticResult(op string) (json.RawMessage, bool) {
	switch op {
	case OpHTTPRequest:
		// {status, body} — finishEffect binds <node>.status / <node>.response.
		return json.RawMessage(`{"status":200,"body":null}`), true
	case OpDBQuery:
		// QueryResult — finishEffect binds <node>.rows/.count/.elapsed_ms.
		return json.RawMessage(`{"rows":[],"count":0,"elapsed_ms":0}`), true
	default:
		return nil, false
	}
}

// ValidateValidationModeCoverage is the structural guard (B10, criterion
// 14): every world-touching op that SetEffects registers MUST declare a
// validation-mode synthetic response. It is INTROSPECTIVE — it installs
// SetEffects on a throwaway scene and reflects the ops it actually
// registered (registeredWorldEffectOps) rather than trusting a
// hand-maintained list. A new world op added to worldEffectRegistrations
// (the single table SetEffects installs from) without a synthetic
// declaration therefore fails this guard automatically — there is no second
// list to forget. Returns an error naming the first undeclared op so the
// harness fails the campaign on it, so an effect added later cannot silently
// leak to a real egress in validation mode.
func ValidateValidationModeCoverage() error {
	for _, op := range registeredWorldEffectOps() {
		if _, ok := validationSyntheticResult(op); !ok {
			return fmt.Errorf("validation-mode coverage: world effect %q declares no synthetic response (B10)", op)
		}
	}
	return nil
}

// SetValidationMode puts the scene into validation mode (B10). Pre-Run
// only; the validation harness sets it on a fresh clone before firing any
// entrypoint. It also installs the harness-driven clock so the timer wheel
// resolves without real waits. Idempotent.
func (s *Scene) SetValidationMode() {
	s.validationMode = true
	s.clock = newValidationClock()
}

// SeedValidationLeaf writes a state leaf on a validation-mode clone before
// an entrypoint fires (the on-event canonical fixture). Validation mode
// only — a guard so it can never mutate a live scene. Scene goroutine is
// not running during a campaign, so this is a plain intra-goroutine write.
func (s *Scene) SeedValidationLeaf(path string, value json.RawMessage) {
	if !s.validationMode {
		return
	}
	if s.state.Set(path, value) {
		s.pending[path] = struct{}{}
	}
}

// validationEffect resolves a world-touching op in validation mode: it
// records the attempt and synchronously resumes the node's continuation
// with the declared-shape synthetic result — NEVER running the op's I/O
// closure. Scene goroutine only.
//
// The op still parks its `completed`/`then` continuation through the
// normal effectOutcome machinery (so latent semantics are identical to
// live); validationEffect simply delivers the synthetic completion
// immediately on the same goroutine instead of submitting a worker job.
func (s *Scene) validationEffect(node *ExecNode) execOpOutcome {
	s.recordEffectAttempt(node.Op, node.ID)
	value, ok := validationSyntheticResult(node.Op)
	if !ok {
		// Should be unreachable — the guard runs before any campaign — but
		// fail-closed: resume to the error port rather than the world.
		value = nil
	}
	key := s.nextWakeKey()
	return execOpOutcome{
		park:    true,
		parkKey: key,
		resume:  ExecTarget{Node: node.ID, Port: effectCompletePort},
		start: func() {
			// Deliver synchronously, intra-goroutine — no socket, no pgx,
			// no worker pool. The synthetic envelope walks `then`.
			s.resumeParkedWith(key, map[string]json.RawMessage{
				effectEnvKey(node.ID): mustEffectEnvelope(value),
			})
		},
	}
}

// validationFinish handles the synthetic completion re-entry of a world
// op in validation mode: it binds the declared-shape output pins (the SAME
// binding the live op's finishEffect does) and walks `then`. The op need
// not be registered (SetEffects uninstalled) — the seam owns the finish.
func (s *Scene) validationFinish(t *execTask, node *ExecNode) execOpOutcome {
	return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
		switch node.Op {
		case OpHTTPRequest:
			var out struct {
				Status int             `json:"status"`
				Body   json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal(value, &out); err == nil {
				env[node.ID+".status"] = json.RawMessage(strconv.Itoa(out.Status))
				env[node.ID+".response"] = out.Body
			}
		case OpDBQuery:
			env[node.ID+".rows"] = json.RawMessage(`[]`)
			env[node.ID+".count"] = json.RawMessage(`0`)
			env[node.ID+".elapsed_ms"] = json.RawMessage(`0`)
		}
	})
}

// mustEffectEnvelope marshals a success envelope carrying value.
func mustEffectEnvelope(value json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(effectEnvelope{Value: value})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// --- per-campaign report capture (ADR 003 §3.2.1) ---------------------

// validationCapture accumulates the per-entrypoint observations of one
// fired entrypoint: leaves written, effects attempted, and the exec nodes
// the task actually entered (coverage — informative). It is owned by the
// scene goroutine while the entrypoint runs; the harness reads it after the
// task drains. A small mutex guards it only so a defensive concurrent read
// (none today) stays race-clean under -race.
type validationCapture struct {
	mu            sync.Mutex
	leavesWritten map[string]struct{}
	effectsTried  []EffectAttempt
	nodesCovered  map[string]struct{}
}

// EffectAttempt is one attempted effect in validation mode — listed in the
// report so the author sees every call the scene would make live.
type EffectAttempt struct {
	Op   string `json:"op"`
	Node string `json:"node"`
}

func newValidationCapture() *validationCapture {
	return &validationCapture{
		leavesWritten: map[string]struct{}{},
		nodesCovered:  map[string]struct{}{},
	}
}

func (c *validationCapture) recordLeaf(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.leavesWritten[path] = struct{}{}
	c.mu.Unlock()
}

func (c *validationCapture) recordNode(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.nodesCovered[id] = struct{}{}
	c.mu.Unlock()
}

func (c *validationCapture) recordEffect(op, node string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.effectsTried = append(c.effectsTried, EffectAttempt{Op: op, Node: node})
	c.mu.Unlock()
}

func (c *validationCapture) snapshot() (leaves []string, effects []EffectAttempt, covered []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	leaves = sortedKeys(c.leavesWritten)
	covered = sortedKeys(c.nodesCovered)
	effects = append([]EffectAttempt{}, c.effectsTried...)
	return
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recordEffectAttempt is called by validationEffect (and is a no-op
// outside a campaign — validation == nil).
func (s *Scene) recordEffectAttempt(op, node string) {
	s.validation.recordEffect(op, node)
}

// EntrypointResult is the per-entrypoint outcome of a validation run.
// One of these is recorded per (blueprint, entrypoint) fired by the
// campaign; the scene passes iff every result has Pass == true.
type EntrypointResult struct {
	Entrypoint    string          `json:"entrypoint"`
	Kind          string          `json:"kind,omitempty"`
	Event         string          `json:"event,omitempty"`
	Pass          bool            `json:"pass"`
	FailReason    string          `json:"fail_reason,omitempty"`
	Steps         uint64          `json:"steps"`
	WallMS        json.RawMessage `json:"wall_ms"`
	LeavesWritten []string        `json:"leaves_written"`
	EffectsTried  []EffectAttempt `json:"effects_attempted"`
	NodesCovered  []string        `json:"exec_node_coverage"`
}

// ValidationBudget bounds one entrypoint's proof (ADR §3.2.1, defaults
// 5 s wall / 1 M steps, env-tunable). The budget bounds the PROOF, never
// the engine: a divergent `while` crosses it, fails validation, and never
// reaches air — the language loses nothing. A non-positive bound disables
// that dimension.
type ValidationBudget struct {
	MaxSteps uint64
	MaxWall  time.Duration
}

// DefaultValidationBudget is ADR §3.2.1's default.
var DefaultValidationBudget = ValidationBudget{
	MaxSteps: 1_000_000,
	MaxWall:  5 * time.Second,
}

// validationClock is the synchronous, harness-driven time source the
// validation driver runs the timer wheel on. The driver advances it to
// the next armed deadline whenever the runnable queue empties but parked
// timer continuations remain — so `delay` / `animation.play`'s duration
// fallback resolve WITHOUT a real wait, deterministically. It is never
// the live clock (the harness installs it on the clone only).
type validationClock struct {
	now time.Time
	c   chan time.Time
}

func newValidationClock() *validationClock {
	return &validationClock{now: time.Unix(0, 0), c: make(chan time.Time, 1)}
}

func (c *validationClock) Now() time.Time { return c.now }

func (c *validationClock) NewTimer(time.Duration) Timer { return &validationTimer{c: c} }

type validationTimer struct{ c *validationClock }

func (t *validationTimer) C() <-chan time.Time { return t.c.c }
func (t *validationTimer) Stop()               {}
func (t *validationTimer) Reset(time.Duration) {}

// RunValidationEntrypoint fires one entrypoint on this (validation-mode
// clone) scene and drives it to completion ENTIRELY on the calling
// goroutine — no scene goroutine, no real waits, no sockets. It returns
// the per-entrypoint result (ADR §3.2.1). Pass == false on: budget
// exceeded (divergent logic), a panic, or an unknown entrypoint.
//
// The driver is the goroutine-free analogue of Scene.Run's exec branch:
// step the head task; when the queue empties, fire any due timers
// (advancing the harness clock to the next deadline); stop when nothing
// runnable and nothing parked remains, or a budget is crossed. Single
// goroutine, so -race is trivially clean and a divergent `while` is caught
// by the budget rather than spun forever on a real goroutine.
func (s *Scene) RunValidationEntrypoint(entry string, env map[string]json.RawMessage, budget ValidationBudget) EntrypointResult {
	res := EntrypointResult{Entrypoint: entry}
	if len(s.execProgs) == 0 {
		res.FailReason = "no exec program"
		res.WallMS = floatToJSON(0)
		return res
	}
	ref, ok := s.resolveEntry(entry)
	if !ok {
		res.FailReason = "unknown entrypoint"
		res.WallMS = floatToJSON(0)
		return res
	}
	// The report names the entrypoint by its program-LOCAL id; the
	// blueprint key is already carried separately on the blueprintReport
	// (issue #105: the namespaced key is an index/firing concern, the
	// report keeps the author-facing local id).
	res.Entrypoint = strings.TrimPrefix(entry, ref.prog.BlueprintKey+"/")
	res.Kind, res.Event = ref.entry.Kind, ref.entry.Event

	s.validation = newValidationCapture()
	clk, _ := s.clock.(*validationClock)

	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			res.Pass = false
			res.FailReason = fmt.Sprintf("panic: %v", r)
		}
	}()

	s.enqueueFireEnv(entry, env)

	var steps uint64
	for {
		if len(s.execQueue) == 0 {
			// Nothing runnable: resolve due/parked timers if any, else done.
			if s.wheel.len() == 0 {
				break // task graph fully drained — success path
			}
			if clk != nil {
				// Jump to the earliest deadline and fire it — no real wait.
				next := s.wheel.peek().deadline
				if next.After(clk.now) {
					clk.now = next
				}
			}
			s.fireDueTimers()
			if len(s.execQueue) == 0 {
				// A spurious/empty wheel fire produced no runnable work and
				// no remaining timers — drained.
				if s.wheel.len() == 0 {
					break
				}
				// Defensive: a timer that resumed nothing but left entries.
				continue
			}
			continue
		}
		t := s.execQueue[0]
		s.execQueue = s.execQueue[1:]
		for len(t.frames) > 0 {
			s.stepTask(t)
			steps++
			if stepBudgetExceeded(steps, budget.MaxSteps) {
				res.Steps = steps
				res.FailReason = fmt.Sprintf("step budget exceeded (%d steps)", budget.MaxSteps)
				res.WallMS = floatToJSON(float64(time.Since(start)) / float64(time.Millisecond))
				res.fill(s.validation)
				return res
			}
			if budget.MaxWall > 0 && steps%execTimeCheckEvery == 0 && time.Since(start) >= budget.MaxWall {
				res.Steps = steps
				res.FailReason = fmt.Sprintf("wall budget exceeded (%s)", budget.MaxWall)
				res.WallMS = floatToJSON(float64(time.Since(start)) / float64(time.Millisecond))
				res.fill(s.validation)
				return res
			}
		}
	}

	res.Pass = true
	res.Steps = steps
	res.WallMS = floatToJSON(float64(time.Since(start)) / float64(time.Millisecond))
	res.fill(s.validation)
	return res
}

func (r *EntrypointResult) fill(c *validationCapture) {
	leaves, effects, covered := c.snapshot()
	r.LeavesWritten, r.EffectsTried, r.NodesCovered = leaves, effects, covered
}

// stepBudgetExceeded reports whether the task crossed the per-entrypoint
// step budget. The budget bounds the PROOF, never the engine (ADR §3.2.1):
// a divergent `while` crosses it and fails validation — it never reaches
// air. A non-positive budget disables the step check (wall-clock only).
func stepBudgetExceeded(steps, budget uint64) bool {
	return budget > 0 && steps >= budget
}

// floatToJSON renders a float deterministically (used by the report).
// IEEE-754 -0 renders as "0" (strconv with 'g' folds -0 to 0 here, but we
// normalise defensively so a -0 wall/step never appears in a report).
func floatToJSON(f float64) json.RawMessage {
	if f == 0 {
		f = 0 // fold -0 → +0
	}
	return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64))
}
