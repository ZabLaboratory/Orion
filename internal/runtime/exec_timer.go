package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The timer wheel + `delay` + cancellation (ADR 003 §3.1.3/§3.1.4,
// issue #83). One time.Timer-driven channel per scene, consumed inside
// the scene loop's select — the wheel, the parked map and every resume
// stay on the scene goroutine: single-writer by construction.
//
// Doctrine (§1.1): no authoring cap, no kill, no amputation. The B8
// cap (parkTask, exec.go) is SEAM back-pressure: beyond it the NEW
// park is shed and counted — a task already parked or running is never
// touched. Cancellation (§3.1.4) is the one sanctioned task drop:
// switch-away / re-push / archive / shutdown cancel all live tasks of
// the affected scene version, by ADR decision — that is lifecycle
// semantics, not an engine cap.

// wheelEntry is one armed timer: a deadline plus the wake key of the
// continuation it resumes. seq breaks deadline ties in park order, so
// the firing order is fully deterministic — never map-iteration order.
type wheelEntry struct {
	deadline time.Time
	seq      uint64
	key      string
}

// wheelHeap is a binary min-heap over (deadline, seq). Scene-goroutine
// only. Same shape as scene.go's topoQueue; entries instead of ints.
type wheelHeap struct{ h []wheelEntry }

func (w *wheelHeap) len() int          { return len(w.h) }
func (w *wheelHeap) peek() *wheelEntry { return &w.h[0] }
func (w *wheelHeap) clear()            { w.h = w.h[:0] }

func wheelLess(a, b wheelEntry) bool {
	if !a.deadline.Equal(b.deadline) {
		return a.deadline.Before(b.deadline)
	}
	return a.seq < b.seq
}

func (w *wheelHeap) push(e wheelEntry) {
	w.h = append(w.h, e)
	i := len(w.h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !wheelLess(w.h[i], w.h[parent]) {
			break
		}
		w.h[parent], w.h[i] = w.h[i], w.h[parent]
		i = parent
	}
}

func (w *wheelHeap) pop() wheelEntry {
	root := w.h[0]
	n := len(w.h) - 1
	w.h[0] = w.h[n]
	w.h = w.h[:n]
	i := 0
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		small := l
		if r := l + 1; r < n && wheelLess(w.h[r], w.h[l]) {
			small = r
		}
		if !wheelLess(w.h[small], w.h[i]) {
			break
		}
		w.h[i], w.h[small] = w.h[small], w.h[i]
		i = small
	}
	return root
}

// SetClock injects the time source (fake clock in tests). Pre-Run only.
func (s *Scene) SetClock(c Clock) {
	if c != nil {
		s.clock = c
	}
}

// SetExecParkCap overrides the B8 per-scene cap on parked
// timers/continuations. Pre-Run only. n <= 0 disables the cap.
func (s *Scene) SetExecParkCap(n int) { s.execParkCap = n }

// nextWakeKey mints a version-stamped wake key (ADR 003 §3.1.4 /
// §3.1.3): scene version + cancellation epoch + a per-scene sequence.
// A resume carrying a stamp from a cancelled epoch (or another
// version) is dropped as stale in resumeParked — it can never resume
// anything. Phase 3 (B-syswrite) adds the token stamp and the
// authenticated delivery path on top of this format; the version/epoch
// check stays as the inner gate.
func (s *Scene) nextWakeKey() string {
	s.execWakeSeq++
	return fmt.Sprintf("wk|%s|%d|%d", s.graph.SceneVersion, s.execEpoch, s.execWakeSeq)
}

// parseWakeStamp extracts the version/epoch stamp of a wake key.
// ok=false for unstamped keys (synthetic test ops) — those skip the
// staleness gate and rely on the parked-map lookup alone.
func parseWakeStamp(key string) (version string, epoch uint64, ok bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 || parts[0] != "wk" {
		return "", 0, false
	}
	e, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return parts[1], e, true
}

// wheelAdd arms a timer entry for an already-parked continuation and
// (re)schedules the underlying Timer. Scene goroutine only.
func (s *Scene) wheelAdd(key string, deadline time.Time) {
	s.execWheelSeq++
	s.wheel.push(wheelEntry{deadline: deadline, seq: s.execWheelSeq, key: key})
	s.reportWheel()
	s.rearmWheel()
}

// rearmWheel points the scene's single Timer at the earliest deadline
// (or disarms it when the wheel is empty). The timer is created
// lazily: a scene that never delays never allocates one, and the nil
// wheelC channel blocks forever in the select — zero cost.
func (s *Scene) rearmWheel() {
	if s.wheel.len() == 0 {
		if s.wheelTimer != nil {
			s.wheelTimer.Stop()
		}
		return
	}
	d := s.wheel.peek().deadline.Sub(s.clock.Now())
	if d < 0 {
		d = 0
	}
	if s.wheelTimer == nil {
		s.wheelTimer = s.clock.NewTimer(d)
		s.wheelC = s.wheelTimer.C()
		return
	}
	s.wheelTimer.Reset(d)
}

// fireDueTimers resumes every continuation whose deadline has passed,
// in strict (deadline, park-order) sequence — deterministic by the
// heap, never by map iteration — then re-arms for the next deadline.
// Runs on the scene goroutine, triggered by the wheel channel case of
// the Run select. A spurious wake (entry purged by cancellation) finds
// nothing due and simply re-arms.
func (s *Scene) fireDueTimers() {
	now := s.clock.Now()
	fired := false
	for s.wheel.len() > 0 && !s.wheel.peek().deadline.After(now) {
		e := s.wheel.pop()
		fired = true
		s.resumeParked(e.key)
	}
	if fired {
		s.reportWheel()
	}
	s.rearmWheel()
}

func (s *Scene) reportWheel() {
	if s.execMetrics != nil {
		s.execMetrics.ExecTimerWheelSize(s.id, s.wheel.len())
	}
}

// CancelExec requests cancellation of all live exec tasks of this
// scene instance (ADR 003 §3.1.4: switch-away / re-push / archive /
// shutdown). Safe from any goroutine: the request travels a dedicated
// 1-buffered channel (coalescing — a pending cancel absorbs repeats)
// so it is never lost to a full inbox; the actual teardown runs on the
// scene goroutine (cancelExecTasks).
func (s *Scene) CancelExec() {
	select {
	case s.execCancelCh <- struct{}{}:
	default: // a cancel is already pending — coalesce
	}
}

// cancelExecTasks drops every live task of this scene version: the
// runnable queue, all parked continuations, and every timer entry. The
// epoch bump is what version-stamps the cut: any in-flight resume
// minted before the cancellation carries the old epoch and is dropped
// as stale on arrival (resumeParked), counted, resuming nothing.
// Scene goroutine only.
func (s *Scene) cancelExecTasks() {
	dropped := len(s.execQueue) + len(s.execParked)
	s.execEpoch++
	s.execQueue = nil
	clear(s.execParked)
	// Invalidate every operator await of this scene version (ADR 008
	// invariant 7, Orion #209): the parked continuations above are gone, so
	// the awaits that referenced them must go too — a resolve arriving after
	// the cut finds no registry entry and the route answers 410. The epoch
	// bump also stales any wake key the resolve might still carry.
	clear(s.pendingAwaits)
	s.wheel.clear()
	if s.wheelTimer != nil {
		s.wheelTimer.Stop()
	}
	s.reportParked()
	s.reportWheel()
	if dropped > 0 {
		s.logger.Info("exec tasks cancelled (scene-version lifecycle)",
			"dropped", dropped, "epoch", s.execEpoch)
	}
}

// --- the `delay` exec op (latent, ADR 003 §3.1.3) ---------------------

// maxDelaySeconds guards the float→Duration conversion: anything that
// would overflow int64 nanoseconds pins to the maximum representable
// duration instead of wrapping negative (which would fire a huge
// authored delay immediately).
const maxDelaySeconds = float64(math.MaxInt64) / float64(time.Second)

// durationFromSeconds maps the authored `seconds` value to a wheel
// duration. Non-positive input — including negative values and
// IEEE-754 `-0`, which is == 0 and fails `> 0` — yields 0: the delay
// still parks (latent semantics preserved: the chain forks and the
// surrounding task continues) and the timer fires immediately. Defined,
// tested, no panic, no infinite wait.
func durationFromSeconds(secs float64) time.Duration {
	if !(secs > 0) { // negative, -0, 0, NaN
		return 0
	}
	if secs >= maxDelaySeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(secs * float64(time.Second))
}

// execDelay implements `delay`: park the chain's continuation under a
// version-stamped wake key on the timer wheel; at the deadline the
// continuation resumes on the scene goroutine. Park=fork (issue #82
// semantics): the surrounding task — an enclosing loop or sequence —
// CONTINUES while the suspended chain waits for the deadline.
func execDelay(s *Scene, t *execTask, node *ExecNode, _ string) execOpOutcome {
	resume, ok := node.next("then", "completed")
	if !ok {
		// Nothing wired after the delay: the chain ends here; there
		// is no continuation to park.
		return execOpOutcome{}
	}
	secs := s.pullFloat(t, node, "seconds", 0)
	return execOpOutcome{
		park:     true,
		parkKey:  s.nextWakeKey(),
		resume:   resume,
		timer:    true,
		deadline: s.clock.Now().Add(durationFromSeconds(secs)),
	}
}

// pullFloat reads a numeric data input as float64 (delay's fractional
// `seconds`). Unwired/invalid → def.
func (s *Scene) pullFloat(t *execTask, node *ExecNode, port string, def float64) float64 {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return def
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		s.logger.Warn("exec: data input not a number", "node", node.ID, "port", port, "value", string(raw))
		return def
	}
	return f
}
