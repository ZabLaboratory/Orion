package lsdp

import "testing"

func TestBoundLeafSetBootstrapStateUsesNeutralValues(t *testing.T) {
	set := boundLeafSet{exact: map[string]struct{}{
		"pl.L0.name":  {},
		"pl.L0.score": {},
	}}
	state := set.bootstrapState()
	if len(state) != 2 {
		t.Fatalf("bootstrap state length = %d, want 2", len(state))
	}
	for path, value := range state {
		if value != nil {
			t.Fatalf("bootstrap state[%q] = %#v, want nil", path, value)
		}
	}
}
