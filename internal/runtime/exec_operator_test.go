package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

// Operator runtime tests (Orion #209, Blue ADR 008 §3.2/§3.3). These prove
// the engine-side behaviour through a REAL Scene: on-call fires its `then`
// with the payload bound; await-value suspends and resumes on resolve;
// value_type is checked; and a switch-away (CancelExec, invariant 7)
// invalidates a live await so a late resolve is Gone.

// waitForPendingAwait blocks until the scene publishes an await under
// (blueprintKey, awaitName), or fails on timeout.
func waitForPendingAwait(t *testing.T, sc *Scene, blueprintKey, awaitName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, pa := range sc.PendingAwaits(blueprintKey) {
			if pa.AwaitName == awaitName {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("await %s/%s never appeared in pending list", blueprintKey, awaitName)
}

// onCallProgram: an on-call entry whose `then` runs a print that copies the
// bound payload into __vars.bp.got (via variable.set reading the entry's
// payload data-out pin).
func operatorAwaitProgram() *ExecProgram {
	// reached marks completion; got captures the resolved value.
	reached := varSet("reached", "reached", nil, nil)
	reached.Config["value"] = raw(`true`)
	got := varSet("got", "got",
		[]ExecDataInput{{Port: "value", From: "op", FromPort: "value"}},
		map[string]ExecTarget{"then": {Node: "reached"}})
	await := &ExecNode{ID: "op", Op: OpOperatorAwait,
		Config: map[string]json.RawMessage{
			"await_name": raw(`"pick"`), "value_type": raw(`"core.primitive.integer"`)},
		Next: map[string]ExecTarget{"then": {Node: "got"}}}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{"op": await, "got": got, "reached": reached},
		Entrypoints:  map[string]ExecEntry{"e": {Target: ExecTarget{Node: "op"}}},
	}
}

func TestOperator_AwaitSuspendsAndResumes(t *testing.T) {
	sc := execScene(t, "op-await", operatorAwaitProgram())
	startScene(t, sc)
	mustFire(t, sc, "e")

	// The chain reaches the await and suspends — published, not completed.
	waitForPendingAwait(t, sc, "bp", "pick", 2*time.Second)
	if _, ok := sc.state.Get("__vars.bp.reached"); ok {
		t.Fatal("chain completed before resolve — await did not suspend")
	}

	// Resolve with a valid integer → continuation resumes, value bound.
	if err := sc.ResolveAwait("bp", "pick", raw(`42`)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	waitForState(t, sc, "__vars.bp.got", `42`, 2*time.Second)
	waitForState(t, sc, "__vars.bp.reached", `true`, 2*time.Second)

	// The await is gone from the registry after resolve.
	if len(sc.PendingAwaits("bp")) != 0 {
		t.Fatal("await still pending after resolve")
	}
}

func TestOperator_ResolveTypeMismatchRejected(t *testing.T) {
	sc := execScene(t, "op-await-type", operatorAwaitProgram())
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitForPendingAwait(t, sc, "bp", "pick", 2*time.Second)

	// value_type is core.primitive.integer; a fractional number is invalid.
	if err := sc.ResolveAwait("bp", "pick", raw(`3.5`)); err != ErrAwaitTypeMismatch {
		t.Fatalf("got %v, want ErrAwaitTypeMismatch", err)
	}
	// The await survives a rejected resolve (the continuation stays parked).
	if len(sc.PendingAwaits("bp")) != 1 {
		t.Fatal("await dropped by a rejected resolve")
	}
	// A subsequent valid resolve still works.
	if err := sc.ResolveAwait("bp", "pick", raw(`7`)); err != nil {
		t.Fatalf("valid resolve after mismatch: %v", err)
	}
	waitForState(t, sc, "__vars.bp.reached", `true`, 2*time.Second)
}

func TestOperator_ResolveUnknownIsGone(t *testing.T) {
	sc := execScene(t, "op-await-unknown", operatorAwaitProgram())
	startScene(t, sc)
	// No await armed yet.
	if err := sc.ResolveAwait("bp", "pick", raw(`1`)); err != ErrAwaitUnknown {
		t.Fatalf("got %v, want ErrAwaitUnknown", err)
	}
}

// TestOperator_AwaitInvalidatedByCancelExec proves ADR 008 invariant 7: a
// switch-away cancels the parked continuation and clears the await, so a
// late resolve is Gone (errAwaitUnknown → 410).
func TestOperator_AwaitInvalidatedByCancelExec(t *testing.T) {
	sc := execScene(t, "op-await-cancel", operatorAwaitProgram())
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitForPendingAwait(t, sc, "bp", "pick", 2*time.Second)

	// Deactivation: cancel all live exec of this scene version.
	sc.CancelExec()

	// The await must disappear, and a late resolve must be Gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sc.PendingAwaits("bp")) != 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if len(sc.PendingAwaits("bp")) != 0 {
		t.Fatal("await survived CancelExec (invariant 7 violated)")
	}
	if err := sc.ResolveAwait("bp", "pick", raw(`1`)); err != ErrAwaitUnknown {
		t.Fatalf("late resolve after cancel: got %v, want ErrAwaitUnknown", err)
	}
	// And the chain never completed.
	if _, ok := sc.state.Get("__vars.bp.reached"); ok {
		t.Fatal("cancelled chain still completed")
	}
}

// TestOperator_OnCallFiresThenWithPayload proves on-call: an explicit
// operator dispatch fires the `then` chain with the request payload bound
// under the on-call node's `payload` data-out pin.
func TestOperator_OnCallFiresThenWithPayload(t *testing.T) {
	got := varSet("got", "got",
		[]ExecDataInput{{Port: "value", From: "call", FromPort: "payload"}}, nil)
	onCall := ExecEntry{Kind: EntryOnCall, Node: "call", Target: ExecTarget{Node: "got"}}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{"got": got},
		Entrypoints:  map[string]ExecEntry{"call": onCall},
	}
	sc := execScene(t, "op-oncall", prog)
	startScene(t, sc)

	if !sc.HasOnCallEntry("call") {
		t.Fatal("on-call entry not armed")
	}
	if !sc.FireOnCall("call", raw(`{"x":1}`)) {
		t.Fatal("FireOnCall inbox full")
	}
	waitForState(t, sc, "__vars.bp.got", `{"x":1}`, 2*time.Second)
}
