package runtime

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// `overlay-app.set` executor (ADR 016 Prism §3.2, issue #283, conformance
// core.overlay-app.set@1).
//
// These tests prove the executor reads `app_id` + the OPTIONAL `running` /
// `on_air`, forwards the desired state through the Show's overlay-mirror seam,
// fires `then` (no error pin, construction-safe like show.emit), and that an
// absent dimension is passed as nil (unchanged) while an unwired seam never
// halts the chain.

// overlayCapture records the seam invocation under a mutex (the seam runs on
// the scene goroutine; the test reads from its own — guarded for -race).
type overlayCapture struct {
	mu             sync.Mutex
	calls          int
	appID          string
	running, onAir *bool
}

func (c *overlayCapture) set(appID string, running, onAir *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.appID, c.running, c.onAir = appID, running, onAir
}

func (c *overlayCapture) snapshot() (int, string, *bool, *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.appID, c.running, c.onAir
}

// overlaySetProg fires `on-start` → overlay-app.set(cfg) → variable.set marking
// `__vars.bp.done = 1`, so a test can wait on `done` to know the op ran and
// fell through to `then`. cfg carries app_id / running / on_air as literals.
func overlaySetProg(cfg map[string]json.RawMessage) *ExecProgram {
	mark := varSet("done", "done", nil, nil)
	mark.Config["value"] = raw(`1`)
	set := &ExecNode{
		ID:     "set",
		Op:     OpOverlayAppSet,
		Config: cfg,
		Next:   map[string]ExecTarget{"then": {Node: "done"}},
	}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{"set": set, "done": mark},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "set"}, Kind: EntryOnStart},
		},
	}
}

func TestOverlayAppSet_EmitsMirrorState(t *testing.T) {
	cap := &overlayCapture{}
	sc := execScene(t, "overlay-emit", overlaySetProg(map[string]json.RawMessage{
		"app_id":  raw(`"app-1"`),
		"running": raw(`true`),
		"on_air":  raw(`false`),
	}))
	sc.SetOverlayAppSetter(cap.set)
	startScene(t, sc)
	mustFire(t, sc, "start")

	// The op ran and fell through to `then`.
	waitForState(t, sc, "__vars.bp.done", "1", 2*time.Second)

	calls, appID, running, onAir := cap.snapshot()
	if calls != 1 {
		t.Fatalf("seam calls = %d, want 1", calls)
	}
	if appID != "app-1" {
		t.Fatalf("app_id = %q, want app-1", appID)
	}
	if running == nil || *running != true {
		t.Fatalf("running = %v, want *true", running)
	}
	if onAir == nil || *onAir != false {
		t.Fatalf("on_air = %v, want *false", onAir)
	}
}

// A set carrying only `on_air` leaves `running` unchanged — the seam receives
// nil for the untouched dimension (partial update, ADR 016 §3.2).
func TestOverlayAppSet_OptionalDimensionsPassNil(t *testing.T) {
	cap := &overlayCapture{}
	sc := execScene(t, "overlay-partial", overlaySetProg(map[string]json.RawMessage{
		"app_id": raw(`"app-2"`),
		"on_air": raw(`true`),
	}))
	sc.SetOverlayAppSetter(cap.set)
	startScene(t, sc)
	mustFire(t, sc, "start")
	waitForState(t, sc, "__vars.bp.done", "1", 2*time.Second)

	_, appID, running, onAir := cap.snapshot()
	if appID != "app-2" {
		t.Fatalf("app_id = %q, want app-2", appID)
	}
	if running != nil {
		t.Fatalf("running = %v, want nil (unchanged)", *running)
	}
	if onAir == nil || *onAir != true {
		t.Fatalf("on_air = %v, want *true", onAir)
	}
}

// An unwired seam (no SetOverlayAppSetter) never halts the chain — `then`
// fires, the emission is a no-op (construction-safe, no error pin).
func TestOverlayAppSet_NilSeamConstructionSafe(t *testing.T) {
	sc := execScene(t, "overlay-nilseam", overlaySetProg(map[string]json.RawMessage{
		"app_id":  raw(`"app-3"`),
		"running": raw(`true`),
	}))
	// No SetOverlayAppSetter → s.overlayAppSet nil.
	startScene(t, sc)
	mustFire(t, sc, "start")
	waitForState(t, sc, "__vars.bp.done", "1", 2*time.Second)
}

// A missing `app_id` skips the emission but STILL fires `then` (authoring gap
// surfaced at validation, never a runtime crash).
func TestOverlayAppSet_MissingAppIDSkipsButFiresThen(t *testing.T) {
	cap := &overlayCapture{}
	sc := execScene(t, "overlay-noappid", overlaySetProg(map[string]json.RawMessage{
		"running": raw(`true`),
	}))
	sc.SetOverlayAppSetter(cap.set)
	startScene(t, sc)
	mustFire(t, sc, "start")
	waitForState(t, sc, "__vars.bp.done", "1", 2*time.Second)

	if calls, _, _, _ := cap.snapshot(); calls != 0 {
		t.Fatalf("seam calls = %d, want 0 (skipped on empty app_id)", calls)
	}
}
