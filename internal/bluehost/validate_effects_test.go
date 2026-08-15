package bluehost

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// TestValidateProgram_StartAloneNeverDialsEvenWithLiveExecuteHandlers is C2's
// central security proof (Bastion veto, SCENE-VALIDATION-GATE-C2-ORION): a
// submitted program whose on-start entrypoint fires core.http.request@1 must
// never actually dial through a bare Load -> Start -> Stop sequence with no
// Step in between — exactly what ValidateProgram does, and all it does.
//
// Deliberately proven in the SINGLE MOST ADVERSARIAL configuration possible —
// mode=Execute, with REAL, live EffectHandlers whose egress policy
// allowlists the spy host — precisely so the guarantee does NOT rest on
// ValidateProgram's own additional (belt-and-suspenders) Preview +
// EffectDeps{} hardening: it rests on the structural fact that Start alone
// never executes a program's on-start entrypoint. That fact is otherwise
// only visible by reading runtime.go's pendingStart field, which Step alone
// consumes — TestEffectHandlers_ExecuteDispatchesRealHTTP (effects_test.go)
// needs its OWN two explicit rt.Step calls just to observe a single dispatch
// at all, on the exact same fixture used here.
//
// This is the class of failure Bastion's veto named precisely: Orion's
// service-token-bearing egress firing an authenticated SSRF against the Zab
// mesh from a bare POST to a validation endpoint, if a submitted program's
// on-start were ever allowed to run for real.
func TestValidateProgram_StartAloneNeverDialsEvenWithLiveExecuteHandlers(t *testing.T) {
	dialed := false
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dialed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer spy.Close()

	u, err := url.Parse(spy.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Live egress policy that WOULD allow this exact host through, and
	// live EffectHandlers built in mode=Execute (never the synthetic-preview
	// branch) — the worst case a caller could construct.
	egress := effects.NewEgressPolicy([]string{u.Hostname()}, true).InsecureAllowPrivateForTest()
	handlers := NewEffectHandlers(EffectDeps{Egress: egress}, blueruntime.Execute)
	program := buildHTTPRequestProgram(t, spy.URL)

	rt := blueruntime.NewRuntime()
	handle, err := rt.Load(program)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	instance, err := rt.Start(handle, blueruntime.StartOptions{
		InstanceID:     "adversarial-worst-case",
		Mode:           blueruntime.Execute,
		EffectHandlers: handlers,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := rt.Stop(instance, "test: prove no dial without an explicit Step"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if dialed {
		t.Fatal("SECURITY: Start (with no Step call in between) dialed the spy server — on-start executed without an explicit Step")
	}
}
