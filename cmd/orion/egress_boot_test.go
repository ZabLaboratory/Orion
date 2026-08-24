package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
)

func TestLoadEgressRoutes_EmbeddedLocalUsesFrozenBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scene-bundle.json")
	bundle := compiler.SceneBundle{
		ComputeManifest: json.RawMessage(`{"egress_routes":[{"service":"zabcam","route_id":"zabcam.slots.assign","method":"PUT","path_template":"/cam/api/v1/cam/streams/{stream_id}/slots/{slot_ref}","params":["stream_id","slot_ref"],"token_paths":["zabcam.slots.assign"]}]}`),
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	routes, err := loadEgressRoutes(context.Background(), config.Config{
		Profile:         config.ProfileEmbeddedLocal,
		SceneBundlePath: path,
		ServicePaths:    []string{"zabcam.slots.assign"},
		BlueBaseURL:     "http://127.0.0.1:1/unreachable",
	}, func([]string) string {
		called = true
		return "must-not-be-used"
	})
	if err != nil {
		t.Fatalf("loadEgressRoutes: %v", err)
	}
	if called {
		t.Fatal("embedded-local bundle path unexpectedly requested a service token")
	}
	if _, ok := routes[compiler.EgressRouteKey("zabcam", "zabcam.slots.assign")]; !ok {
		t.Fatalf("frozen route missing: %#v", routes)
	}
}

func TestLoadEgressRoutes_EmbeddedLocalBundleMissFailsClosed(t *testing.T) {
	_, err := loadEgressRoutes(context.Background(), config.Config{
		Profile:         config.ProfileEmbeddedLocal,
		SceneBundlePath: filepath.Join(t.TempDir(), "missing.json"),
	}, func([]string) string { return "unused" })
	if err == nil {
		t.Fatal("missing declared local bundle must fail closed")
	}
}
