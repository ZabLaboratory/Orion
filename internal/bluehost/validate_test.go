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

// TestValidateProgram_AcceptsWhenCapabilityServed proves the accept side:
// against the real Zab registry, a program requiring a capability that
// registry serves is reported servable.
func TestValidateProgram_AcceptsWhenCapabilityServed(t *testing.T) {
	if err := bluehost.ValidateProgram(httpRequiresFixture(t), providers.Registry(), providers.Policy(true)); err != nil {
		t.Fatalf("expected servable, got %+v", err)
	}
}

// TestValidateProgram_RefusesUnknownCapability proves the refuse side —
// the class of failure a clean Blue compile can never catch, because only
// Orion knows its own provider registry (§6.3).
func TestValidateProgram_RefusesUnknownCapability(t *testing.T) {
	err := bluehost.ValidateProgram(unknownCapabilityFixture(t), providers.Registry(), providers.Policy(true))
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

// TestValidateProgram_NeverTouchesHostSlots proves the hard constraint:
// ValidateProgram runs against a throwaway runtime of its own — a real
// Host's preview/on-air slots are untouched (still unloaded) whether the
// verdict is a pass or a refusal.
func TestValidateProgram_NeverTouchesHostSlots(t *testing.T) {
	host := bluehost.NewHost()

	if err := bluehost.ValidateProgram(httpRequiresFixture(t), providers.Registry(), providers.Policy(true)); err != nil {
		t.Fatalf("expected servable, got %+v", err)
	}
	if err := bluehost.ValidateProgram(unknownCapabilityFixture(t), providers.Registry(), providers.Policy(true)); err == nil {
		t.Fatal("expected refusal for an unserved capability, got nil")
	}

	if digest := host.Digest(bluehost.SlotPreview); digest != "" {
		t.Fatalf("SlotPreview unexpectedly occupied: %q", digest)
	}
	if digest := host.Digest(bluehost.SlotOnAir); digest != "" {
		t.Fatalf("SlotOnAir unexpectedly occupied: %q", digest)
	}
}
