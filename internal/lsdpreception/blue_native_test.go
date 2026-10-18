package lsdpreception

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

// Execute an actual validated blue.program.v1, derived from the repository's
// minimal opcode fixture. The visual scene is always the full original LSML.
func outputProgram(t *testing.T, path string, value any) []byte {
	t.Helper()
	raw, err := os.ReadFile("../bluespike/testdata/01-minimal.program.json")
	if err != nil {
		t.Fatal(err)
	}
	var program map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&program); err != nil {
		t.Fatal(err)
	}
	data := func(name, kind string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": kind, "required": required}
	}
	exec := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	program["opcodes"] = append(program["opcodes"].([]any), map[string]any{"id": "core.output@1", "kind": "pure", "config": []any{data("name", "core.string", true)}, "inputs": []any{data("value", "core.json", true), exec("in")}, "outputs": []any{data("value", "core.json", false), exec("then")}})
	program["nodes"] = append(program["nodes"].([]any), map[string]any{"id": "mark", "opcode": "core.output@1", "config": map[string]any{"name": path}})
	program["exec_edges"] = []any{map[string]any{"from_node": "entry", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": json.Number("0")}}
	program["data_literals"] = []any{map[string]any{"node_id": "mark", "port": "value", "value": value}}
	program["state"] = map[string]any{"variables": []any{}, "outputs": []any{}}
	delete(program, "program_digest")
	// Normalize numeric literals into the canonicalizer's verified JSON model.
	raw, _ = json.Marshal(program)
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&program); err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.Digest(program)
	if err != nil {
		t.Fatal(err)
	}
	program["program_digest"] = digest
	raw, err = json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRealNativeVisual(t *testing.T) {
	phase := os.Getenv("ORION_TEST_NATIVE_PHASE")
	if phase == "" {
		t.Skip("driven by the offline CEF visual proof")
	}
	address, path := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_LSML_PATH")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err = json.Unmarshal(source, &original); err != nil {
		t.Fatal(err)
	}
	id, version := original["scene_id"].(string), original["scene_version"].(string)
	key := GenerationKey(id, version)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProducer(ctx, r)
	defer p.Close()
	h := NewHub(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if phase == "seed" || phase == "restore" {
		h.Lane("program").MirrorForLSML(id, version, source)
		h.Lane("program").SetActive(id)
		h.MirrorForLSML(id, version, "on-air", source)
	} else {
		c, e := native.Dial(ctx, address, "", nil)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := c.Exchange(ctx, map[string]any{"kind": "state.read", "target": "solar/generations"})
		c.Close()
		if e != nil {
			t.Fatal(e)
		}
		var snapshot struct{ State map[string]map[string]any }
		if e = json.Unmarshal(raw, &snapshot); e != nil {
			t.Fatal(e)
		}
		doc := snapshot.State[key]
		if doc == nil {
			t.Fatal("visual generation was not seeded")
		}
		if phase == "document" {
			doc["layout"].(map[string]any)["children"].([]any)[1].(map[string]any)["background"] = "#203248"
		}
		switch phase {
		case "patch":
			raw, _ = json.Marshal(doc)
			mirror := h.MirrorForLSML(id, version, "on-air", raw)
			host := bluehost.NewHost()
			if e = host.Take("cef-blue-instance", id, "sha256:cef-blue-test", outputProgram(t, "__lit.text.text_msxz43eh_1", "Orion + Blue + LSDP natif"), nil, nil, nil); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if err := host.Release(bluehost.SlotOnAir, "visual-test-end"); err != nil {
					t.Error(err)
				}
			}()
			bridge := bluewire.NewBridge(host, bluehost.SlotOnAir, mirror, id, "sha256:cef-blue-test", "cef-blue-instance", blueproject.TargetProgram, "local-source-only", "cef-native-proof")
			if e = bridge.TickOnce(0); e != nil {
				t.Fatal(e)
			}
		case "document":
			// Send the complete desired state to the real native state route.
			// Rust computes the structural diff, preserving unrelated generations.
			p.Replace("solar/generations", snapshot.State)
		default:
			t.Fatal("unknown visual phase")
		}
	}
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.Exchange(ctx, map[string]any{"kind": "state.read", "target": "solar/generations"})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ State map[string]map[string]any }
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	doc := snapshot.State[key]
	if doc == nil {
		t.Fatal("generation absent after native ACK")
	}
	if phase == "patch" && (doc["defaults"].(map[string]any)["__lit.text.text_msxz43eh_1"] != "Orion + Blue + LSDP natif" || doc["x-orion"].(map[string]any)["runtime_instance_id"] != "cef-blue-instance") {
		t.Fatal("actual Blue output or provenance absent after native ACK")
	}
	if phase == "document" && doc["layout"].(map[string]any)["children"].([]any)[1].(map[string]any)["background"] != "#203248" {
		t.Fatal("full-document native diff absent after ACK")
	}
	untouched, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(untouched, source) {
		t.Fatal("original LSML was modified")
	}
	t.Logf("native visual phase=%s generation=%s; original LSML unchanged", phase, key)
}
