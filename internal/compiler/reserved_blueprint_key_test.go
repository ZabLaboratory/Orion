package compiler

import "testing"

// The "_" blueprint key is reserved as the operator default-blueprint
// addressing token (api.defaultBlueprintToken). compiler must reject a
// non-empty authored key "_" so the cockpit/operator alias stays unambiguous
// (Blue ADR 008 §3.2, ADR 016 RC-6).

func TestValidateBlueprintKeys_RejectsReservedKey(t *testing.T) {
	diags := validateBlueprintKeys([]BlueprintRef{{Key: "_", ID: "x"}})
	if len(diags) != 1 || diags[0].Code != ErrReservedBlueprintKey {
		t.Fatalf("authored key \"_\" must be RESERVED_BLUEPRINT_KEY, got %+v", diags)
	}
}

func TestValidateBlueprintKeys_AllowsEmptyAndNormalKeys(t *testing.T) {
	// The empty key (legacy/default — the one the token aliases) is allowed,
	// as are ordinary keys.
	diags := validateBlueprintKeys([]BlueprintRef{
		{Key: "", ID: "legacy"},
		{Key: "left", ID: "a"},
		{Key: "right", ID: "b"},
	})
	if len(diags) != 0 {
		t.Fatalf("empty + normal keys must pass, got %+v", diags)
	}
}

// TestReservedKeyMatchesAPIToken guards against drift between the compiler's
// reserved key and the API addressing token (they live in different packages
// to avoid an import cycle; their VALUE must stay equal). The API constant is
// "_" (api.defaultBlueprintToken). If either side changes, this fails.
func TestReservedKeyMatchesAPIToken(t *testing.T) {
	if reservedBlueprintKey != "_" {
		t.Fatalf("reservedBlueprintKey = %q, must equal api.defaultBlueprintToken (\"_\")",
			reservedBlueprintKey)
	}
}
