package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// fakeAirValidator records the triple it was asked about and returns a
// canned verdict, so a test can prove the gate consulted IT (not the store).
type fakeAirValidator struct {
	verdict   bool
	err       error
	calledID  uuid.UUID
	calledVer string
	calls     int
}

func (f *fakeAirValidator) IsVersionValidated(_ context.Context, sceneID uuid.UUID, sceneVersion, _ string) (bool, error) {
	f.calls++
	f.calledID = sceneID
	f.calledVer = sceneVersion
	return f.verdict, f.err
}

// TestAirValidator_NilFallsBackToStore proves RC-A5 §3 parity at the seam:
// with PublicDeps.AirValidator unset (the antenne default), deps.airValidator()
// resolves to the store-backed adapter — the gate reads the DB row exactly as
// before #247. No mirror, no behaviour change.
func TestAirValidator_NilFallsBackToStore(t *testing.T) {
	deps := PublicDeps{} // AirValidator nil, Store nil
	av := deps.airValidator()
	sav, ok := av.(storeAirValidator)
	if !ok {
		t.Fatalf("antenne default validator = %T, want storeAirValidator", av)
	}
	if sav.st != deps.Store {
		t.Fatal("storeAirValidator must wrap deps.Store")
	}
}

// TestAirValidator_WiredIsUsed proves the embedded-local path: when an
// AirValidator is wired (the mirror), deps.airValidator() returns it and the
// gate consults it instead of the store (#247).
func TestAirValidator_WiredIsUsed(t *testing.T) {
	fake := &fakeAirValidator{verdict: true}
	deps := PublicDeps{AirValidator: fake}
	if got := deps.airValidator(); got != AirValidator(fake) {
		t.Fatalf("airValidator() = %v, want the wired fake", got)
	}
}

// TestIsAirEligible_RoutesThroughWiredValidator proves the gate's
// isAirEligible consults the wired validator (mirror) and returns its
// verdict — the embedded-local routing end to end at the gate seam.
func TestIsAirEligible_RoutesThroughWiredValidator(t *testing.T) {
	id := uuid.New()
	ver := "sha256:abc"
	fake := &fakeAirValidator{verdict: true}
	deps := PublicDeps{AirValidator: fake}

	ok, err := isAirEligible(context.Background(), deps, id, ver)
	if err != nil {
		t.Fatalf("isAirEligible: %v", err)
	}
	if !ok {
		t.Fatal("gate must return the wired validator's verdict (true)")
	}
	if fake.calls != 1 || fake.calledID != id || fake.calledVer != ver {
		t.Fatalf("gate did not consult the wired validator with the request triple: %+v", fake)
	}
}

// TestIsAirEligible_FailClosedOnValidatorError proves the gate propagates a
// validator error (fail-closed) regardless of the source (RC-A5 §2). A mirror
// read error must refuse, not air.
func TestIsAirEligible_FailClosedOnValidatorError(t *testing.T) {
	fake := &fakeAirValidator{verdict: false, err: context.DeadlineExceeded}
	deps := PublicDeps{AirValidator: fake}
	ok, err := isAirEligible(context.Background(), deps, uuid.New(), "sha256:x")
	if err == nil {
		t.Fatal("a validator error must propagate (fail-closed)")
	}
	if ok {
		t.Fatal("eligibility must be false on a validator error")
	}
}
