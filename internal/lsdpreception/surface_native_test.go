package lsdpreception

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/providers"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

func TestRealNativeBlueStructuralMutation(t *testing.T) {
	address, path := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_LSML_PATH")
	if address == "" || path == "" {
		t.Skip("requires real native receiver and original LSML")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	id, version := document["scene_id"].(string), document["scene_version"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProducer(ctx, r)
	defer p.Close()
	hub := NewHub(p, slog.Default())
	lane := hub.Lane("preview")
	mirror := lane.MirrorForLSML(id, version, source).(*Mirror)
	lane.SetActive(id)
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		p.Replace("solar/preview", document)
		if err := p.Flush(ctx); err != nil {
			t.Error(err)
		}
	}()
	raw := nativeEffectProgram(t, "core.lsml", "mutate", map[string]any{"operations": []any{map[string]any{"op": "add", "path": "/layout/children/-", "value": map[string]any{"id": "native-blue-added", "kind": "frame", "background": "#808080", "size": map[string]any{"w": json.Number("40"), "h": json.Number("40")}}}}})
	host := bluehost.NewHost()
	host.SetSceneMutationSink(func(slot bluehost.Slot, operations []map[string]any) error {
		if slot != bluehost.SlotPreview {
			t.Fatal("crossed Program lane")
		}
		return mirror.ApplyLSML(operations)
	})
	if err := host.PreparePreview("structural-blue", id, version, raw, providers.Registry(), providers.Policy(false), nil); err != nil {
		t.Fatal(err)
	}
	defer host.Release(bluehost.SlotPreview, "proof-end")
	if _, err := host.Step(bluehost.SlotPreview); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	state, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": "solar/preview"})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ State map[string]any }
	if err := json.Unmarshal(state, &snapshot); err != nil {
		t.Fatal(err)
	}
	children := snapshot.State["layout"].(map[string]any)["children"].([]any)
	if children[len(children)-1].(map[string]any)["id"] != "native-blue-added" || snapshot.State["scene_version"] != version {
		t.Fatal("Blue structural mutation absent or identity changed")
	}
}

func nativeEffectProgram(t *testing.T, capability, operation string, request map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile("../bluespike/testdata/02-http-requires.program.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var program map[string]any
	if err := decoder.Decode(&program); err != nil {
		t.Fatal(err)
	}
	for _, row := range program["effects"].([]any) {
		effect := row.(map[string]any)
		effect["capability"] = capability
		effect["operation"] = operation
	}
	for _, row := range program["requires"].([]any) {
		requirement := row.(map[string]any)
		requirement["capability"] = capability
		requirement["operation"] = operation
	}
	for _, row := range program["data_literals"].([]any) {
		literal := row.(map[string]any)
		if literal["node_id"] == "http-call" {
			literal["value"] = request
		}
	}
	delete(program, "program_digest")
	program["program_digest"], err = canonical.Digest(program)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
