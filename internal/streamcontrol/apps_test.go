package streamcontrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOverlayChild(t *testing.T) {
	if os.Getenv("ORION_OVERLAY_CHILD") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}
func TestConfiguredOverlayLaunchIsIdempotentAndStopsOwnedProcess(t *testing.T) {
	t.Setenv("ORION_OVERLAY_CHILD", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "apps.json")
	raw, _ := json.Marshal(map[string]Command{"marker": {Executable: executable, Args: []string{"-test.run=^TestOverlayChild$"}}})
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	apps, err := NewApps(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer apps.Close()
	ctx := context.Background()
	if err := apps.Set(ctx, "marker", true); err != nil {
		t.Fatal(err)
	}
	first := apps.Status()["marker"]
	if !first.Running || first.PID == 0 {
		t.Fatalf("not launched: %+v", first)
	}
	if err := apps.Set(ctx, "marker", true); err != nil {
		t.Fatal(err)
	}
	if apps.Status()["marker"].PID != first.PID {
		t.Fatal("duplicate process")
	}
	if err := apps.Set(ctx, "marker", false); err != nil {
		t.Fatal(err)
	}
	if apps.Status()["marker"].Running {
		t.Fatal("owned process left running")
	}
	if err := apps.Set(ctx, "unknown", true); err == nil {
		t.Fatal("unconfigured executable admitted")
	}
}
