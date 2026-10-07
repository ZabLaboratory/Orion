package lsdpreception

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/providers"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

// Opt-in CEF proof seam. Uses the complete real scene, never saves its mutation.
func TestRealNativeBlueAnimation(t *testing.T) {
	address, path, phase := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_LSML_PATH"), os.Getenv("ORION_TEST_ANIMATION_PHASE")
	if address == "" || path == "" || phase == "" {
		t.Skip("requires real native receiver, source and animation phase")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var document map[string]any
	if phase == "seed" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		children := document["layout"].(map[string]any)["children"].([]any)
		panel := children[1].(map[string]any)["children"].([]any)[1].(map[string]any)["children"].([]any)[0].(map[string]any)
		panel["id"] = "cef-animation-panel"
		asset := func(start, end map[string]any) any {
			start["at"] = 0
			end["at"] = 1
			return map[string]any{"target": "cef-animation-panel", "keyframes": map[string]any{"duration_ms": 120, "easing": "linear", "steps": []any{start, end}}}
		}
		document["animations"] = map[string]any{"hide": asset(map[string]any{"opacity": 1}, map[string]any{"opacity": 0}), "reveal": asset(map[string]any{"opacity": 0}, map[string]any{"opacity": 1}), "move": asset(map[string]any{"translateX": 0}, map[string]any{"translateX": 40})}
	} else {
		raw, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": "solar/program"})
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct{ State map[string]any }
		if err = json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		document = snapshot.State
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	reception, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := NewProducer(ctx, reception)
	defer producer.Close()
	lane := NewHub(producer, slog.Default()).Lane("program")
	mirror := lane.MirrorForLSML(document["scene_id"].(string), document["scene_version"].(string), raw).(*Mirror)
	lane.SetActive(document["scene_id"].(string))
	if err = producer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if phase != "seed" {
		host := bluehost.NewHost()
		host.SetSceneMutationSink(func(slot bluehost.Slot, ops []map[string]any) error {
			if slot != bluehost.SlotOnAir {
				t.Fatal("wrong lane")
			}
			return mirror.ApplyLSML(ops)
		})
		program := nativeEffectProgram(t, "core.animation", "play", map[string]any{"animation_id": phase})
		if err = host.Take("animation-"+phase, document["scene_id"].(string), document["scene_version"].(string), program, providers.Registry(), providers.Policy(false), nil); err != nil {
			t.Fatal(err)
		}
		defer host.Release(bluehost.SlotOnAir, "proof-end")
		if _, err = host.Step(bluehost.SlotOnAir); err != nil {
			t.Fatal(err)
		}
		if err = producer.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("REAL_BLUE_ANIMATION phase=%s native_ack=true source_saved=false", phase)
}
