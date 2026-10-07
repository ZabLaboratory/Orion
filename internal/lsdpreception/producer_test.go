package lsdpreception

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

func TestTreeAndObjectOperations(t *testing.T) {
	if treeHash(nil) != "tree-sha256:1b16b1df538ba12dc3f97edbb85caa7050d46c148134290feba80f8236c83db9" {
		t.Fatal("null hash")
	}
	for _, item := range []struct {
		value float64
		want  string
	}{{1e-7, "1e-7"}, {1e-6, "0.000001"}, {1e20, "100000000000000000000"}, {1e21, "1e+21"}, {0, "0"}, {-0.1, "-0.1"}} {
		if got := number(item.value); got != item.want {
			t.Fatalf("number %g: %s", item.value, got)
		}
	}
	before := map[string]any{"a/b": map[string]any{"~": 1.0}}
	next, err := applyObjectOperations(before, []map[string]any{{"op": "add", "path": "/a~1b/~0", "value": nil}})
	if err != nil || next.(map[string]any)["a/b"].(map[string]any)["~"] != nil || before["a/b"].(map[string]any)["~"] != 1.0 {
		t.Fatal("atomic pointer mutation", err)
	}
}

func TestSourceDocumentPreservesLSMLAddressDistinctFromArtifactSet(t *testing.T) {
	const sourceVersion = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const artifactSet = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	raw := []byte(`{"lsml":"1.2","scene_id":"scene-1","scene_version":"` + sourceVersion + `","layout":{"type":"frame","children":[]}}`)
	document, err := sourceDocument("scene-1", artifactSet, raw)
	if err != nil || document["scene_version"] != sourceVersion || document["x-orion-artifact-set"] != artifactSet {
		t.Fatalf("native source must retain its fetchable LSML identity: %v %v", document, err)
	}
}

// This is an actual Orion producer → native Rust receiver test, using the real
// LSML and its complete geometry/assets. The harness owns an ephemeral node.
func TestRealNativeProducers(t *testing.T) {
	address, path := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_LSML_PATH")
	if address == "" || path == "" {
		t.Skip("requires actual native receiver and real LSML")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err = json.Unmarshal(source, &original); err != nil {
		t.Fatal(err)
	}
	sceneID, version := original["scene_id"].(string), original["scene_version"].(string)
	r, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	p := NewProducer(ctx, r)
	defer p.Close()
	// This integration test owns its disposable native resources.
	p.Replace("solar/program", nil)
	p.Replace("solar/preview", nil)
	p.Replace("solar/generations", map[string]any{})
	p.Replace("solar/sessions", map[string]any{})
	p.Replace("orion/state", map[string]any{})
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h := NewHub(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read := func(target string) any {
		raw, e := c.Exchange(ctx, map[string]any{"kind": "state.read", "target": target})
		if e != nil {
			t.Fatal(e)
		}
		var snapshot struct{ State any }
		if e = json.Unmarshal(raw, &snapshot); e != nil {
			t.Fatal(e)
		}
		return snapshot.State
	}
	flush := func() {
		t.Helper()
		if err := p.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	preview, program := h.Lane("preview"), h.Lane("program")
	pm := preview.MirrorForLSML(sceneID, version, source)
	if pm == nil {
		t.Fatal(p.Error())
	}
	pm.Forward(&protocol.Snapshot{SceneID: sceneID, State: map[string]json.RawMessage{}})
	preview.SetActive(sceneID)
	gm := h.MirrorForLSML(sceneID, version, "preview", source)
	flush()
	if read("solar/program") != nil {
		t.Fatal("Preview reached Program")
	}
	key := GenerationKey(sceneID, version)
	if read("solar/generations").(map[string]any)[key] == nil {
		t.Fatal("exact generation missing")
	}
	var pathKey string
	for k, v := range original["defaults"].(map[string]any) {
		if _, ok := v.(string); ok && len(k) > 11 && k[:11] == "__lit.text." {
			pathKey = k
			break
		}
	}
	if pathKey == "" {
		t.Fatal("real scene has no authored text binding")
	}
	value := json.RawMessage(`"Orion → LSDP natif"`)
	delta := &protocol.Delta{SceneID: sceneID, Patches: []protocol.Patch{{Path: pathKey, Value: value}, {Path: "__vars.unbound", Value: json.RawMessage(`{"rows":[1]}`)}}, RuntimeInstanceID: "real-producer-test", SceneDigest: "blue-runtime-digest", Target: "preview", RenderRevision: "1", CorrelationID: "native-proof"}
	pm.Forward(delta)
	gm.Forward(delta)
	flush()
	state := read("solar/preview").(map[string]any)
	if state["defaults"].(map[string]any)[pathKey] != "Orion → LSDP natif" {
		t.Fatal("Blue output not applied")
	}
	if _, ok := state["defaults"].(map[string]any)["__vars.unbound"]; ok {
		t.Fatal("unbound intermediate leaked")
	}
	if state["x-orion"].(map[string]any)["runtime_instance_id"] != "real-producer-test" {
		t.Fatal("projection provenance missing")
	}
	// A proven external edit is preserved when the next producer leaf rebases.
	external := state
	external["test_extension"] = map[string]any{"fraction": 0.0000001, "unicode": "é / ~", "nested": []any{nil, 12.5}}
	result, e := c.SendPort(ctx, "", "solar/preview", "solar.lsml/1", external)
	if e != nil || native.Completed(result) != nil {
		t.Fatal("native full-document diff", e)
	}
	pm.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: pathKey, Value: json.RawMessage(`"after external edit"`)}}})
	flush()
	if read("solar/preview").(map[string]any)["test_extension"] == nil {
		t.Fatal("concurrent LSML edit lost")
	}
	// Same-generation Take changes writer lease. A delayed Preview tick cannot
	// overwrite the physical Program generation, even if its owner string repeats.
	air := h.MirrorForLSML(sceneID, version, "on-air", source)
	live := program.MirrorForLSML(sceneID, version, source)
	program.SetActive(sceneID)
	air.Forward(delta)
	live.Forward(delta)
	flush()
	gm.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: pathKey, Value: json.RawMessage(`"stale"`)}}})
	flush()
	if read("solar/generations").(map[string]any)[key].(map[string]any)["defaults"].(map[string]any)[pathKey] == "stale" {
		t.Fatal("stale generation writer")
	}
	// Switching Preview does not retarget an immutable generation or Program.
	other := map[string]any{}
	raw, _ := json.Marshal(original)
	json.Unmarshal(raw, &other)
	other["scene_id"] = "other-preview"
	otherRaw, _ := json.Marshal(other)
	preview.MirrorForLSML("other-preview", version, otherRaw)
	preview.SetActive("other-preview")
	flush()
	if read("solar/program").(map[string]any)["scene_id"] != sceneID {
		t.Fatal("Preview switch corrupted Program")
	}
	program.EmitSlotAssignment("slot/one~", "peer-a")
	program.EmitViewerPayload(`{"rooms":[]}`)
	program.EmitOverlayApp("overlay-1", boolPointer(true), nil)
	flush()
	generation := read("solar/generations").(map[string]any)[key].(map[string]any)
	if generation["defaults"].(map[string]any)["__cam.slots.slot/one~"] != "peer-a" {
		t.Fatal("camera generation assignment missing")
	}
	if read("orion/state").(map[string]any)["overlay_running_overlay-1"] != true {
		t.Fatal("overlay control missing")
	}
	// A freshly armed physical Pulsar lane inherits current camera authority,
	// including explicit custom owner IDs, before its first frame is rendered.
	h.MirrorForLSML(sceneID, "camera-generation", "pulsar-lane-a", source)
	flush()
	cameraKey := GenerationKey(sceneID, "camera-generation")
	cameraDefaults := read("solar/generations").(map[string]any)[cameraKey].(map[string]any)["defaults"].(map[string]any)
	if cameraDefaults["__cam.slots.slot/one~"] != "peer-a" || cameraDefaults["__cam.viewer"] != `{"rooms":[]}` {
		t.Fatal("new physical generation lost current camera authority")
	}
	program.EmitSlotCleared("slot/one~")
	flush()
	if read("solar/generations").(map[string]any)[cameraKey].(map[string]any)["defaults"].(map[string]any)["__cam.slots.slot/one~"] != nil {
		t.Fatal("custom physical owner missed camera release")
	}
	// Independent test clones use their actual API session identity.
	a := h.NewSessionWireFor("session-a", sceneID, version, &compiler.RenderBundle{SourceLSML: source})
	b := h.NewSessionWireFor("session-b", sceneID, version, &compiler.RenderBundle{SourceLSML: source})
	a.Mirror().Forward(delta)
	flush()
	sessions := read("solar/sessions").(map[string]any)
	if sessions["session-a"].(map[string]any)["defaults"].(map[string]any)[pathKey] == sessions["session-b"].(map[string]any)["defaults"].(map[string]any)[pathKey] {
		t.Fatal("test session cross-talk")
	}
	a.Close()
	a.Mirror().Forward(delta)
	flush()
	if read("solar/sessions").(map[string]any)["session-a"] != nil {
		t.Fatal("closed session resurrected")
	}
	b.Close()
	flush()
	// Native subscribers live on the shared node. Abandoned API clones must
	// expire through Orion's lease/sweep instead of leaking until shutdown.
	manager := runtime.NewTestSessionManager(runtime.NewComputeRegistry(), h.logger, time.Minute)
	manager.SetSessionWires(h)
	defer manager.Close()
	defaults := map[string]json.RawMessage{}
	for k, value := range original["defaults"].(map[string]any) {
		defaults[k], _ = json.Marshal(value)
	}
	leaseID, _ := manager.Open(ctx, sceneID, &compiler.Graph{SceneID: sceneID, SceneVersion: version, Defaults: defaults}, &compiler.RenderBundle{SourceLSML: source})
	flush()
	if read("solar/sessions").(map[string]any)[leaseID] == nil {
		t.Fatal("actual API session ID not published")
	}
	if _, err := manager.Renew(leaseID); err != nil {
		t.Fatal(err)
	}
	manager.Sweep(time.Now().Add(2 * time.Minute))
	flush()
	if read("solar/sessions").(map[string]any)[leaseID] != nil {
		t.Fatal("expired native clone/resource leaked")
	}
	if _, err := manager.Renew(leaseID); err == nil {
		t.Fatal("expired session revived")
	}
	untouched, _ := os.ReadFile(path)
	if string(untouched) != string(source) {
		t.Fatal("source file mutated")
	}
	t.Logf("actual native ACKs: Blue leaves, native diff and rebase, Program/Preview switch, generation writer lease, camera/overlay and test-session isolation; scene=%s; leaf=%s", sceneID, pathKey)
}
func boolPointer(value bool) *bool { return &value }
