package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// The opt-in harness owns a disposable Rust receiver; no active installation is touched.
func TestRealEditableAuthoringNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_EDITABLE_NATIVE_ADDRESS")
	if address == "" {
		t.Skip("requires disposable native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := lsdpreception.NewProducer(ctx, reception)
	defer producer.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := lsdpreception.NewHub(producer, logger)
	preview := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), hub.Lane("preview"), logger)
	preview.SetEditableAirWire(hub)
	defer preview.Close()
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: logger, Config: config.Config{Profile: config.ProfileEmbeddedLocal}, Preview: preview, SceneIntent: &SceneIntentDeps{}, NativeLSDPFlush: producer.Flush})
	base := map[string]any{"lsml": "1.2", "scene_id": "native-editable-proof", "scene_version": "sha256:" + strings.Repeat("0", 64), "layout": map[string]any{"kind": "shape", "bind": map[string]any{"width": "__editable.node.width"}}, "defaults": map[string]any{"__editable.node.width": json.Number("10")}}
	version, err := canonical.Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	base["scene_version"] = version
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": base["scene_id"], "scene_version": version, "render_mode": "source-only", "lsml_bundle": base}, "operator")
	if opened.Code != http.StatusOK {
		t.Fatalf("open: %d %s", opened.Code, opened.Body.String())
	}
	read := func() map[string]any {
		t.Helper()
		value, err := reception.Read(ctx, "solar/preview")
		if err != nil {
			t.Fatal(err)
		}
		document, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("native document missing: %T", value)
		}
		return document
	}
	before := read()
	immutable := before["x-solar-authoring"].(map[string]any)["lsml_bundle"]
	patched := editableAPIRequest(t, mux, http.MethodPut, map[string]any{"scene_id": base["scene_id"], "base_edit_seq": 0, "edit_seq": 1, "patches": []map[string]any{{"path": "__editable.node.width", "value": 42}}}, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body.String())
	}
	after := read()
	if after["defaults"].(map[string]any)["__editable.node.width"] != float64(42) {
		t.Fatal("editable mutation did not cross native LSDP")
	}
	a, _ := json.Marshal(immutable)
	b, _ := json.Marshal(after["x-solar-authoring"].(map[string]any)["lsml_bundle"])
	if string(a) != string(b) {
		t.Fatal("live mutation altered immutable base")
	}
	if _, err = preview.PromoteEditable(base["scene_id"].(string), "preview"); err != nil {
		t.Fatal(err)
	}
	if err = producer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	generations, err := reception.Read(ctx, "solar/generations")
	if err != nil {
		t.Fatal(err)
	}
	generation := generations.(map[string]any)[lsdpreception.GenerationKey(base["scene_id"].(string), version)].(map[string]any)
	if generation["x-solar-authoring"] == nil || generation["defaults"].(map[string]any)["__editable.node.width"] != float64(42) {
		t.Fatal("promoted generation lost native authoring delivery or mutation")
	}
}
