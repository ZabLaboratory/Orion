package providers

import (
	"os"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// isolationProvider is a synthetic core.http.request/1/request provider
// whose operation explicitly does NOT support preview — the shape that
// exercises §5.10's isolation invariant: "un provider armé on-air ne tire
// jamais en preview". Unlike Registry()'s real httpRequestProvider (preview:
// "noop", deliberately safe in both modes), this descriptor proves the
// runtime's own mode gate (checkProviders, CAPABILITY_MODE_UNSUPPORTED) is
// actually reached through bluehost.Host.Prepare/Take with a Providers
// slice built the same way production wiring builds one.
func isolationProvider() map[string]any {
	provider := httpRequestProvider()
	operations := provider["operations"].([]any)
	operation := operations[0].(map[string]any)
	operation["preview"] = "unsupported"
	return provider
}

func httpRequiresFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/02-http-requires.program.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestIsolation_OnAirOnlyProviderNeverArmedInPreview proves the on-air/
// preview isolation invariant end to end through bluehost.Host: a provider
// operation that only supports Execute (on-air) refuses Prepare (preview)
// with the runtime's own CAPABILITY_MODE_UNSUPPORTED, while Take (on-air)
// on the exact same provider succeeds — the same Providers slice, the only
// difference is which bluehost slot (and therefore Mode) admits it.
func TestIsolation_OnAirOnlyProviderNeverArmedInPreview(t *testing.T) {
	program := httpRequiresFixture(t)
	provider := []map[string]any{isolationProvider()}

	h := bluehost.NewHost()
	if err := h.Prepare(bluehost.SlotPreview, "preview-1", "sha256:aaa", program, provider, nil); err == nil {
		t.Fatal("expected Prepare (preview mode) to be refused for a preview-unsupported provider")
	}
	if h.Digest(bluehost.SlotPreview) != "" {
		t.Fatal("a refused Prepare must not leave a loaded preview instance")
	}

	if err := h.Take("onair-1", "sha256:aaa", program, provider, nil); err != nil {
		t.Fatalf("expected Take (on-air/execute mode) to succeed for the same provider: %v", err)
	}
	if h.Digest(bluehost.SlotOnAir) != "sha256:aaa" {
		t.Fatal("expected the on-air slot to carry the taken instance")
	}
}
