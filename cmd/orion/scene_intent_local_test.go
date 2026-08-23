package main

import (
	"log/slog"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/config"
)

func TestWireSceneIntent_EmbeddedLocalUsesSignedCapsulesWithoutWorkload(t *testing.T) {
	dir := t.TempDir()
	trustPath := writeCanvasTrust(t, dir)
	deps, err := wireSceneIntent(config.Config{
		Profile:             config.ProfileEmbeddedLocal,
		CanvasTrustPath:     trustPath,
		CanvasLocatorPrefix: "scenes/",
		OwnerID:             "owner-1",
		TenantID:            "tenant-1",
	}, slog.Default(), bluehost.EffectDeps{})
	if err != nil {
		t.Fatalf("wireSceneIntent embedded-local: %v", err)
	}
	if deps == nil || !deps.EmbeddedLocal {
		t.Fatalf("expected embedded-local scene-intent deps, got %+v", deps)
	}
	if deps.Workload != nil {
		t.Fatal("embedded-local must not require a remote workload client")
	}
}
