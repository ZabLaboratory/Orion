// External test package: internal/providers imports internal/bluehost
// (events.go, for *bluehost.Host) — an in-package `package bluehost` test
// importing internal/providers (to exercise the REAL registry, the whole
// point of ValidateProgram) would be an import cycle. Every symbol this
// file needs (ValidateProgram, NewHost, Slot constants, Host.Digest) is
// already exported.
package bluehost_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// httpRequiresFixture reads the SAME program fixture
// internal/api/scene_intent_providers_test.go's httpRequiresProgram(t)
// reads (Refs #332): a blue.program.v1 declaring `requires` on
// core.http.request v1/request — a capability internal/providers.Registry()
// actually serves.
func httpRequiresFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/02-http-requires.program.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// unknownCapabilityFixture takes the same fixture and renames the ONE
// capability its requires/effects entries declare to something
// internal/providers.Registry() does not serve — the exact "compiles fine,
// declares a capability Orion doesn't serve" case ValidateProgram exists to
// catch. Both entries are renamed together, keeping the fixture's own
// requires/effects cross-reference (blueruntime's effectTypesMatch)
// internally consistent. program_digest IS content-verified by
// blueruntime.ParseProgram (PROGRAM_DIGEST_MISMATCH otherwise), so it is
// recomputed with internal/canonical — the same shared LSML canonicalization
// blueruntime verifies on receipt (see that package's doc comment).
func unknownCapabilityFixture(t *testing.T) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(httpRequiresFixture(t)))
	decoder.UseNumber() // canonical.Bytes requires json.Number, not float64
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	requires, _ := doc["requires"].([]any)
	effects, _ := doc["effects"].([]any)
	if len(requires) != 1 || len(effects) != 1 {
		t.Fatalf("fixture shape drifted: requires=%d effects=%d", len(requires), len(effects))
	}
	requires[0].(map[string]any)["capability"] = "core.unknown-capability"
	effects[0].(map[string]any)["capability"] = "core.unknown-capability"
	delete(doc, "program_digest")
	digest, err := canonical.Digest(doc)
	if err != nil {
		t.Fatalf("recompute program_digest: %v", err)
	}
	doc["program_digest"] = digest
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal mutated fixture: %v", err)
	}
	return out
}

// dualEffectTightBudgetFixture takes httpRequiresFixture, clones its single
// core.effect.invoke@1 node/effect into a SECOND one chained right after
// the first (same dispatch, same servable capability), and lowers
// budgets.max_effects_per_dispatch to 1 — a program that admits cleanly
// (its one `requires` capability, core.http.request, IS served) but blows
// its own declared execution budget on the SECOND invocation of the SAME
// dispatch (walker.go: `effectsFired > maxEffectsPerDispatch`). This is the
// deterministic, minimal-diff way to construct "admits fine, fails at
// execution" — the class of failure admission-only validation can never
// catch (porteur scope widening, SCENE-VALIDATION-GATE-C2-ORION).
//
// budgets.max_pending_effects/max_effects_per_dispatch cannot simply be set
// to 0 to force an immediate breach: blueruntime's effectBudgetsFor
// (effects.go) treats a declared value <= 0 as "unset" and falls back to
// its own default of 100 — a single invocation never breaches that. A real
// breach needs a program that actually FIRES more effects in one dispatch
// than a validly-lowered (positive) budget allows.
func dualEffectTightBudgetFixture(t *testing.T) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(httpRequiresFixture(t)))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	nodes, _ := doc["nodes"].([]any)
	effectsArr, _ := doc["effects"].([]any)
	execEdges, _ := doc["exec_edges"].([]any)
	dataLiterals, _ := doc["data_literals"].([]any)
	budgets, _ := doc["budgets"].(map[string]any)
	if len(effectsArr) != 1 || budgets == nil {
		t.Fatalf("fixture shape drifted: effects=%d budgets_present=%v", len(effectsArr), budgets != nil)
	}

	firstEffect, ok := effectsArr[0].(map[string]any)
	if !ok {
		t.Fatal("fixture shape drifted: effects[0] is not an object")
	}
	secondEffect := make(map[string]any, len(firstEffect))
	for k, v := range firstEffect {
		secondEffect[k] = v
	}
	secondEffect["id"] = "http-call-2"

	doc["nodes"] = append(nodes, map[string]any{
		"id": "http-call-2", "opcode": "core.effect.invoke@1",
		"config": map[string]any{"effect": "http-call-2"},
	})
	doc["exec_edges"] = append(execEdges, map[string]any{
		"from_node": "http-call", "from_port": "then",
		"to_node": "http-call-2", "to_port": "exec_in", "sequence": json.Number("0"),
	})
	doc["data_literals"] = append(dataLiterals, map[string]any{
		"node_id": "http-call-2", "port": "request", "value": map[string]any{},
	})
	doc["effects"] = append(effectsArr, secondEffect)
	budgets["max_effects_per_dispatch"] = json.Number("1")

	delete(doc, "program_digest")
	digest, err := canonical.Digest(doc)
	if err != nil {
		t.Fatalf("recompute program_digest: %v", err)
	}
	doc["program_digest"] = digest
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal mutated fixture: %v", err)
	}
	return out
}

// TestValidateProgram_AcceptsWhenCapabilityServed proves the accept side:
// against the real Zab registry, a program requiring a capability that
// registry serves is admitted AND runs for real to a clean settlement.
func TestValidateProgram_AcceptsWhenCapabilityServed(t *testing.T) {
	if err := bluehost.ValidateProgram(httpRequiresFixture(t), providers.Registry(), providers.Policy(true), 0, 0); err != nil {
		t.Fatalf("expected servable, got %+v", err)
	}
}

// TestValidateProgram_RefusesUnknownCapability proves the refuse-at-
// admission side — the class of failure a clean Blue compile can never
// catch, because only Orion knows its own provider registry (§6.3).
func TestValidateProgram_RefusesUnknownCapability(t *testing.T) {
	err := bluehost.ValidateProgram(unknownCapabilityFixture(t), providers.Registry(), providers.Policy(true), 0, 0)
	if err == nil {
		t.Fatal("expected refusal for an unserved capability, got nil (servable)")
	}
	if err.Code != "CAPABILITY_UNAVAILABLE" {
		t.Fatalf("expected code=CAPABILITY_UNAVAILABLE, got code=%q stage=%q message=%q", err.Code, err.Stage, err.Message)
	}
	if err.Stage != "start" {
		t.Fatalf("expected stage=start, got %q", err.Stage)
	}
}

// TestValidateProgram_RefusesExecutionFailure proves the NEW value of the
// porteur's scope widening: a program that ADMITS cleanly (its declared
// capability is served) still gets refused, with the real reason, when it
// fails at actual execution — here, blowing its own declared
// max_effects_per_dispatch budget on the second effect invocation of the
// same dispatch.
func TestValidateProgram_RefusesExecutionFailure(t *testing.T) {
	err := bluehost.ValidateProgram(dualEffectTightBudgetFixture(t), providers.Registry(), providers.Policy(true), 0, 0)
	if err == nil {
		t.Fatal("expected refusal for a program that fails its own declared execution budget, got nil (servable)")
	}
	if err.Code != "PROGRAM_BUDGET_INVALID" {
		t.Fatalf("expected code=PROGRAM_BUDGET_INVALID, got code=%q stage=%q message=%q", err.Code, err.Stage, err.Message)
	}
	if err.Stage != "step" {
		t.Fatalf("expected stage=step (an execution-time failure, not admission), got %q", err.Stage)
	}
}

// TestValidateProgram_NeverTouchesHostSlots proves the hard constraint:
// ValidateProgram runs against a throwaway runtime of its own — a real
// Host's preview/on-air slots are untouched (still unloaded) across an
// admitted-and-executed pass, an admission refusal, and an execution
// refusal alike.
func TestValidateProgram_NeverTouchesHostSlots(t *testing.T) {
	host := bluehost.NewHost()

	if err := bluehost.ValidateProgram(httpRequiresFixture(t), providers.Registry(), providers.Policy(true), 0, 0); err != nil {
		t.Fatalf("expected servable, got %+v", err)
	}
	if err := bluehost.ValidateProgram(unknownCapabilityFixture(t), providers.Registry(), providers.Policy(true), 0, 0); err == nil {
		t.Fatal("expected refusal for an unserved capability, got nil")
	}
	if err := bluehost.ValidateProgram(dualEffectTightBudgetFixture(t), providers.Registry(), providers.Policy(true), 0, 0); err == nil {
		t.Fatal("expected refusal for a program that fails its own declared execution budget, got nil")
	}

	if digest := host.Digest(bluehost.SlotPreview); digest != "" {
		t.Fatalf("SlotPreview unexpectedly occupied: %q", digest)
	}
	if digest := host.Digest(bluehost.SlotOnAir); digest != "" {
		t.Fatalf("SlotOnAir unexpectedly occupied: %q", digest)
	}
}
