package providers

import (
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

func TestRegistry_EachProviderParsesAsCanonicalEnvelope(t *testing.T) {
	for _, provider := range Registry() {
		data, err := CanonicalBytes(provider)
		if err != nil {
			t.Fatalf("CanonicalBytes(%v): %v", provider["capability"], err)
		}
		if _, err := blueruntime.ParseProvider(data); err != nil {
			t.Fatalf("ParseProvider(%v): %v", provider["capability"], err)
		}
	}
}

func TestRegistry_CapabilitiesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, provider := range Registry() {
		capability := provider["capability"].(string)
		if seen[capability] {
			t.Fatalf("duplicate capability %q in Registry()", capability)
		}
		seen[capability] = true
	}
	for _, want := range []string{"core.http.request", "show.emit", "overlay-app"} {
		if !seen[want] {
			t.Fatalf("Registry() is missing capability %q", want)
		}
	}
}

func TestPolicy_GatesHTTPEgressOnDeploymentPosture(t *testing.T) {
	httpRequirement := map[string]any{"capability": "core.http.request"}
	otherRequirement := map[string]any{"capability": "show.emit"}

	allowed := Policy(true)
	if !allowed(httpRequirement, nil, blueruntime.Execute) {
		t.Fatal("expected core.http.request admitted when httpEgressAllowed=true")
	}
	if !allowed(otherRequirement, nil, blueruntime.Execute) {
		t.Fatal("expected show.emit admitted regardless of http egress posture")
	}

	denied := Policy(false)
	if denied(httpRequirement, nil, blueruntime.Execute) {
		t.Fatal("expected core.http.request denied when httpEgressAllowed=false")
	}
	if !denied(otherRequirement, nil, blueruntime.Execute) {
		t.Fatal("expected show.emit unaffected by http egress posture")
	}
}
