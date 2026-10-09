package lsdp

import "testing"

func TestNativeSurfaceIncludesAllAuthoredBindingGroups(t *testing.T) {
	surface := RenderSurface("draft", []byte(`{"layout":{"kind":"frame","bind":{"value":"text"},"bindStyle":{"fill":"colour"},"bindUniversal":{"width":"width"},"bindAnimate":{"transform.translate":"position"},"children":[{"bindStyle":{"weight":"weight"}}],"template":{"bind":{"src":"image"}}}}`), nil)
	for _, path := range []string{"text", "colour", "width", "position", "weight", "image"} {
		if !surface(path) {
			t.Fatalf("declared %s was filtered", path)
		}
	}
	if surface("internal.compute") {
		t.Fatal("unbound compute must remain filtered")
	}
}
