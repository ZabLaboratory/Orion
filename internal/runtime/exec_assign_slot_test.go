package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

const assignRouteJSON = `{"service":"zabcam","route_id":"zabcam.slots.assign","method":"PUT",` +
	`"path_template":"/cam/api/v1/streams/{stream_id}/slots/{slot_ref}",` +
	`"params":["stream_id","slot_ref"],"token_paths":["zabcam.slots.assign"]}`

// assignSlotProgram builds a one-node assign-slot program with the route
// baked (compiler output) and slot_ref/peer_label as config-defaulted DATA
// inputs (the author's inline literals), binding ok/error downstream.
func assignSlotProgram(routeJSON, slotRef, peerLabel string) *ExecProgram {
	node := &ExecNode{
		ID: "assign", Op: OpAssignSlot,
		Config: map[string]json.RawMessage{
			bakedRouteConfigKey: raw(routeJSON),
			"slot_ref":          mustJSONString(slotRef),
			"peer_label":        mustJSONString(peerLabel),
		},
		Next: map[string]ExecTarget{
			"then":  {Node: "set.ok"},
			"error": {Node: "set.err"},
		},
	}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"assign":  node,
			"set.ok":  setFromPin("set.ok", "ok", "assign", "ok", nil),
			"set.err": setFromPin("set.err", "err", "assign", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "assign"}}},
	}
}

// captureAssigner records the (slot_ref, peer_label) the mirror seam is fired
// with — the proof the LSDP delta carries the right binding.
type captureAssigner struct {
	mu   sync.Mutex
	hits [][2]string
}

func (c *captureAssigner) fn(slotRef, peerLabel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits = append(c.hits, [2]string{slotRef, peerLabel})
}

func (c *captureAssigner) snapshot() [][2]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][2]string(nil), c.hits...)
}

// TestAssignSlot_UpsertsThenMirrors is the conformance-named proof (RC4): an
// assign-slot fire (a) upserts ZabCam with a runtime-built path + scoped token
// + {peer_label} body, then (b) fires the stream-level mirror seam with
// slot_ref→peer_label — the LSDP delta's binding — and binds ok=true.
func TestAssignSlot_UpsertsThenMirrors(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "scoped-bearer"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	prog := assignSlotProgram(assignRouteJSON, "cam-left", "alice")
	sc := effectsScene(t, "assign-ok", prog, eff)
	mirrorSeam := &captureAssigner{}
	sc.SetSlotAssigner(mirrorSeam.fn)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.ok", `true`, 2*time.Second)

	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	// stream_id from the runtime scene context ("live"), slot_ref from input.
	if gotPath != "/cam/api/v1/streams/live/slots/cam-left" {
		t.Errorf("path = %q, want /cam/api/v1/streams/live/slots/cam-left", gotPath)
	}
	if gotBody != `{"peer_label":"alice"}` {
		t.Errorf("body = %q, want {\"peer_label\":\"alice\"}", gotBody)
	}
	if gotAuth != "Bearer scoped-bearer" {
		t.Errorf("authorization = %q, want the scoped bearer", gotAuth)
	}
	// GAP (Bastion): the token is minted for EXACTLY the route's token_paths,
	// so the future ZabCam X-Authenticated-Paths enforcement bites on a tight
	// scope (no broad /cam grant).
	if asked := minter.lastAsked(); len(asked) != 1 || asked[0] != "zabcam.slots.assign" {
		t.Errorf("minted token_paths = %v, want [zabcam.slots.assign]", asked)
	}
	// The mirror seam fired with the binding the LSDP delta carries.
	hits := mirrorSeam.snapshot()
	if len(hits) != 1 || hits[0] != [2]string{"cam-left", "alice"} {
		t.Fatalf("mirror seam hits = %v, want [[cam-left alice]]", hits)
	}
}

// TestAssignSlot_RejectedUpsertNoMirror (ADR R2): a non-2xx ZabCam response
// binds ok=false and emits NO mirror — the LSDP cache never diverges from the
// durable authority.
func TestAssignSlot_RejectedUpsertNoMirror(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	prog := assignSlotProgram(assignRouteJSON, "cam-left", "alice")
	sc := effectsScene(t, "assign-rejected", prog, eff)
	mirrorSeam := &captureAssigner{}
	sc.SetSlotAssigner(mirrorSeam.fn)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.ok", `false`, 2*time.Second)

	if hits := mirrorSeam.snapshot(); len(hits) != 0 {
		t.Fatalf("mirror seam fired on a rejected upsert (%v) — LSDP must not diverge", hits)
	}
}

// TestAssignSlot_AntiInjectionTemplate (RC6): a slot_ref carrying path
// separators / traversal cannot escape its segment — the runtime escapes it,
// the only structural slashes are the curated template's own.
func TestAssignSlot_AntiInjectionTemplate(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	prog := assignSlotProgram(assignRouteJSON, "../../rooms/evil/delete", "alice")
	sc := effectsScene(t, "assign-inject", prog, eff)
	sc.SetSlotAssigner((&captureAssigner{}).fn)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.ok", `true`, 2*time.Second)

	want := "/cam/api/v1/streams/live/slots/..%2F..%2Frooms%2Fevil%2Fdelete"
	if gotPath != want {
		t.Errorf("escaped path = %q, want %q (slot_ref must stay one segment)", gotPath, want)
	}
}

// TestAssignSlot_BudgetExceededNoUpsertNoMirror (R3): an assign-slot over the
// per-stream egress budget fails closed to `error`, never hits ZabCam, never
// mirrors.
func TestAssignSlot_BudgetExceededNoUpsertNoMirror(t *testing.T) {
	srv, hits := countingServiceServer(t)

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:       newTestRunner(t),
		ServiceCall:  effects.NewServiceCallClient(srv.URL, minter.mint, nil),
		EgressBudget: effects.NewStreamEgressLimiter(1, 10),
	}
	prog := assignSlotProgram(assignRouteJSON, "cam-left", "alice")
	sc := effectsScene(t, "assign-budget", prog, eff)
	mirrorSeam := &captureAssigner{}
	sc.SetSlotAssigner(mirrorSeam.fn)
	startScene(t, sc)

	mustFire(t, sc, "e") // allowed
	mustFire(t, sc, "e") // denied
	waitForState(t, sc, "__vars.bp.err", `"EGRESS_BUDGET_EXCEEDED"`, 2*time.Second)
	waitFor(t, "one allowed upsert reaches ZabCam", func() bool { return hits.Load() == 1 })

	time.Sleep(50 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("ZabCam hits = %d, want exactly 1 (budget cap)", got)
	}
	// Exactly one mirror (for the allowed call); the denied one never mirrored.
	if got := len(mirrorSeam.snapshot()); got != 1 {
		t.Fatalf("mirror seam fired %d times, want 1 (denied call must not mirror)", got)
	}
}
