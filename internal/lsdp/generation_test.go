package lsdp

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

type generationMirrorSpy struct{ calls int }

func (s *generationMirrorSpy) Forward(runtime.SubscriberMsg) { s.calls++ }

func TestGenerationMirrorSingleWriterHandoff(t *testing.T) {
	registry := &GenerationWires{}
	entry := &generationWire{owner: "preview"}
	previewSpy, onAirSpy := &generationMirrorSpy{}, &generationMirrorSpy{}
	preview := &ownedGenerationMirror{registry: registry, entry: entry, owner: "preview", inner: previewSpy}
	onAir := &ownedGenerationMirror{registry: registry, entry: entry, owner: "on-air", inner: onAirSpy}

	preview.Forward(nil)
	onAir.Forward(nil)
	if previewSpy.calls != 1 || onAirSpy.calls != 0 {
		t.Fatalf("preview must be the only generation writer before take: preview=%d on-air=%d", previewSpy.calls, onAirSpy.calls)
	}

	registry.mu.Lock()
	entry.owner = "on-air"
	registry.mu.Unlock()
	preview.Forward(nil)
	onAir.Forward(nil)
	if previewSpy.calls != 1 || onAirSpy.calls != 1 {
		t.Fatalf("on-air must be the only generation writer after take: preview=%d on-air=%d", previewSpy.calls, onAirSpy.calls)
	}
}

func TestGenerationRegistryEvictsOldVersions(t *testing.T) {
	registry := NewGenerationWires(slog.Default(), nil)
	for i := 0; i < maxGenerationWires+3; i++ {
		registry.MirrorForLSML("scene", fmt.Sprintf("v-%02d", i), "preview", nil)
	}
	if got := len(registry.wires); got != maxGenerationWires {
		t.Fatalf("generation wire count=%d, want %d", got, maxGenerationWires)
	}
	if registry.wires[generationKey("scene", "v-00")] != nil {
		t.Fatal("oldest abandoned generation was not evicted")
	}
	if registry.wires[generationKey("scene", fmt.Sprintf("v-%02d", maxGenerationWires+2))] == nil {
		t.Fatal("newest generation must be retained")
	}
}

func TestGenerationHandlerRequiresExactIdentity(t *testing.T) {
	registry := &GenerationWires{wires: make(map[string]*generationWire)}
	for _, target := range []string{
		"/api/v1/show/generation.lsdp",
		"/api/v1/show/generation.lsdp?scene_id=scene-1",
		"/api/v1/show/generation.lsdp?scene_id=scene-1&v=sha256:missing",
	} {
		req := httptest.NewRequest("GET", target, nil)
		res := httptest.NewRecorder()
		registry.Handler().ServeHTTP(res, req)
		if res.Code != 404 {
			t.Fatalf("%s: status=%d, want 404", target, res.Code)
		}
	}
}
