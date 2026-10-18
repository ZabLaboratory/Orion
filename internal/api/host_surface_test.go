package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

func TestHostSurfaceReportsUnwiredEffectsAndHostExtensions(t *testing.T) {
	host := bluehost.NewHost()
	host.SetShowEmitSink(func(string, any) {})
	host.SetSceneMutationSink(func(bluehost.Slot, []map[string]any) error { return nil })
	r := httptest.NewRecorder()
	getHostSurface(PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}})(r, httptest.NewRequest("GET", "/api/v1/runtime/host-surface", nil))
	var body struct {
		Providers []struct{ Capability, Health string }
		Effects   []struct {
			Opcode    string
			Available bool
		}
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Providers) != 8 || len(body.Effects) != 8 {
		t.Fatalf("incomplete surface: %s", r.Body.String())
	}
	for _, provider := range body.Providers {
		if provider.Capability == "core.db.query" && provider.Health != "unavailable" {
			t.Fatal("DB falsely advertised as configured")
		}
	}
	found := false
	for _, effect := range body.Effects {
		if effect.Opcode == "zabcam.assign-slot@1" {
			found = true
		}
		if effect.Opcode == "core.animation.play@1" && !effect.Available {
			t.Fatal("animation missing")
		}
	}
	if !found {
		t.Fatal("host extension omitted")
	}
}
