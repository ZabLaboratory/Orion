package bluehost

import (
	"context"
	"encoding/json"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/streamcontrol"
)

func TestRealMarkerRuleLaunchAndLSMLIntent(t *testing.T) {
	executable, address, capture := os.Getenv("ORION_TEST_MARKER_EXE"), os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_MARKER_CAPTURE")
	if executable == "" || address == "" || capture == "" {
		t.Skip("requires actual Marker executable, native receiver and explicit capture path")
	}
	// Marker owns its real Electron window/capture implementation. No image fixture.
	t.Setenv("MARKER_SMOKE", "1")
	t.Setenv("MARKER_SMOKE_OUT", capture)
	t.Setenv("MARKER_DEMO_CAPTURE", "1")
	root := os.Getenv("ORION_TEST_MARKER_ROOT")
	raw, _ := json.Marshal(map[string]streamcontrol.Command{markerAppID: {Executable: executable, Args: []string{root}, Directory: root}})
	directory := t.TempDir()
	manifest := filepath.Join(directory, "apps.json")
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := streamcontrol.Open(filepath.Join(directory, "control.lsml"))
	if err != nil {
		t.Fatal(err)
	}
	apps, err := streamcontrol.NewApps(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer apps.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := lsdpreception.NewProducer(ctx, reception)
	defer producer.Close()
	hub := lsdpreception.NewHub(producer, slog.Default())
	control := streamcontrol.NewController(store, apps, hub.Lane("program"))
	plane := NewRulePlane(nil, nil, EffectDeps{OverlayMirror: control}, 60, nil)
	defer plane.Stop()
	plane.SetStore(store)
	program := markerOverlayProgram(t)
	if err := plane.Promote("marker-rule", "marker-native-proof", program); err != nil {
		t.Fatal(err)
	}
	activated := time.Now()
	if _, err := plane.Call("marker-rule", "marker_overlay_on", nil); err != nil {
		t.Fatal(err)
	}
	if err := control.Error(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	status := control.Status()[markerAppID]
	if !status.Running || status.PID == 0 || !store.Snapshot().Apps[markerAppID].Running {
		t.Fatalf("enabled intent without process: %+v", status)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, err := os.Stat(capture); err == nil && info.Size() > 1000 && !info.ModTime().Before(activated) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real Marker capture missing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	file, err := os.Open(capture)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if image.Bounds().Dx() < 400 || image.Bounds().Dy() < 200 {
		t.Fatal("Marker surface incomplete")
	}
	if _, err := plane.Call("marker-rule", "marker_overlay_off", nil); err != nil {
		t.Fatal(err)
	}
	if err := producer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if control.Status()[markerAppID].Running || store.Snapshot().Apps[markerAppID].Running {
		t.Fatal("Marker OFF left process/intent on")
	}
	// Reopening Orion's LSML restores the same rule without an upstream compile.
	restored, err := streamcontrol.Open(filepath.Join(directory, "control.lsml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot().Rules) != 1 || restored.Snapshot().Apps[markerAppID].Running {
		t.Fatal("restart intent differs")
	}
	t.Logf("REAL_MARKER_PROOF pid=%d capture=%s pixels=%dx%d native_ack=true persistent_rule=true off=true", status.PID, capture, image.Bounds().Dx(), image.Bounds().Dy())
}
