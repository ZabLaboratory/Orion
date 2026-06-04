package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// representativeLayout builds a Canvas tree exercising the full mapping:
// static props (spread at node top level), dynamic bindings (→ bind),
// transitions (→ animate), nested children, and the 8-primitive +
// instance catalog. It mirrors the shape `@lumencast/compiler` consumes.
func representativeLayout() LayoutNode {
	return LayoutNode{
		Kind: "frame",
		ID:   "root",
		Props: map[string]json.RawMessage{
			"size": json.RawMessage(`{"w":1920,"h":1080}`),
			"bg":   json.RawMessage(`"#0b0b0f"`),
		},
		Children: []LayoutNode{
			{
				Kind: "stack",
				ID:   "col",
				Props: map[string]json.RawMessage{
					"direction": json.RawMessage(`"vertical"`),
					"gap":       json.RawMessage(`24`),
				},
				Children: []LayoutNode{
					{
						Kind: "text",
						ID:   "title",
						Props: map[string]json.RawMessage{
							"size":   json.RawMessage(`132`),
							"weight": json.RawMessage(`900`),
						},
						Bindings: map[string]string{
							"value": "scene.title",
						},
						Transitions: map[string]json.RawMessage{
							"opacity": json.RawMessage(`{"from":0,"to":1,"duration":400}`),
						},
					},
					{
						Kind: "image",
						ID:   "logo",
						Props: map[string]json.RawMessage{
							"alt":  json.RawMessage(`"team logo"`),
							"size": json.RawMessage(`{"w":256,"h":256}`),
						},
						Bindings: map[string]string{
							"src": "team.logo_url",
						},
					},
				},
			},
		},
	}
}

func representativeInputs() []OperatorInput {
	min := 0.0
	max := 100.0
	return []OperatorInput{
		{
			Path:       "scene.title",
			Label:      "Title",
			Type:       "string",
			WritableBy: []string{"operator"},
			MaxLength:  intp(80),
		},
		{
			Path:       "score.value",
			Label:      "Score",
			Type:       "number",
			WritableBy: []string{"operator"},
			Min:        &min,
			Max:        &max,
			Group:      "scoreboard",
		},
	}
}

func intp(i int) *int { return &i }

// Acceptance #1 — the emitter produces a valid LSML 1.1 bundle from a
// Canvas+Blue+components input. We assert the structural shape that
// @lumencast/compiler.compileBundle() requires: lsml == "1.1", a
// scene_id, a scene_version, and a layout tree over the known primitive
// catalog with the static-props-vs-bind split.
func TestEmitLSML_ValidBundleShape(t *testing.T) {
	bundle, version, _, err := EmitLSML(
		"scene-1",
		representativeLayout(),
		representativeInputs(),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	if bundle.LSML != "1.1" {
		t.Fatalf("lsml = %q, want \"1.1\"", bundle.LSML)
	}
	if bundle.SceneID != "scene-1" {
		t.Fatalf("scene_id = %q, want scene-1", bundle.SceneID)
	}
	if bundle.SceneVersion != version {
		t.Fatalf("bundle.scene_version %q != returned version %q", bundle.SceneVersion, version)
	}
	if !strings.HasPrefix(version, "sha256:") {
		t.Fatalf("scene_version = %q, want sha256: prefix", version)
	}

	// Layout must be a primitive tree the catalog recognises.
	var root map[string]any
	if err := json.Unmarshal(bundle.Layout, &root); err != nil {
		t.Fatalf("layout not JSON: %v", err)
	}
	assertKnownPrimitiveTree(t, "layout", root)

	// Static-props-vs-bind split: the root frame's static "size" sits
	// at top level, the text's dynamic "value" sits under "bind".
	if _, ok := root["size"]; !ok {
		t.Fatal("root static prop 'size' not spread at node top level")
	}
	if _, ok := root["props"]; ok {
		t.Fatal("LSML nodes must not carry a 'props' wrapper key")
	}
	col := firstChild(t, root)
	title := firstChild(t, col)
	bind, ok := title["bind"].(map[string]any)
	if !ok || bind["value"] != "scene.title" {
		t.Fatalf("text dynamic binding not mapped to bind.value: %v", title["bind"])
	}
	if _, ok := title["animate"]; !ok {
		t.Fatal("text transition not mapped to animate")
	}

	// operator_inputs survive with the LSML shape.
	if len(bundle.OperatorInputs) != 2 {
		t.Fatalf("operator_inputs = %d, want 2", len(bundle.OperatorInputs))
	}
	score := bundle.OperatorInputs[1]
	if score.Path != "score.value" || score.Constraints["min"] != 0.0 || score.Constraints["max"] != 100.0 {
		t.Fatalf("operator input constraints not folded: %+v", score)
	}
}

// Acceptance #2 — lsml.HashBundle is deterministic and the emitted
// scene_version equals that hash (prefixed sha256:).
func TestEmitLSML_DeterministicHash(t *testing.T) {
	b1, v1, canon1, err := EmitLSML("scene-1", representativeLayout(), representativeInputs(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b2, v2, canon2, err := EmitLSML("scene-1", representativeLayout(), representativeInputs(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("scene_version not deterministic: %s vs %s", v1, v2)
	}
	if string(canon1) != string(canon2) {
		t.Fatal("canonical bytes not deterministic across calls")
	}

	// scene_version must equal lsml.HashBundle of the emitted bundle.
	hexHash, _, err := lsml.HashBundle(b1)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != "sha256:"+hexHash {
		t.Fatalf("scene_version %q != sha256:%s (HashBundle)", v1, hexHash)
	}
	// HashBundle ignores scene_version, so both bundles hash equal even
	// though their scene_version field is already filled.
	h2, _, _ := lsml.HashBundle(b2)
	if hexHash != h2 {
		t.Fatalf("HashBundle differs across identical emits: %s vs %s", hexHash, h2)
	}
}

// The operator-authored animation tree rides through opaque on the root
// node and is byte-preserved (Orion never parses it — ADR 007 §C.1).
func TestEmitLSML_AnimationsRideOpaque(t *testing.T) {
	anim := json.RawMessage(`[{"id":"a","kind":"sequence","children":[]}]`)
	bundle, _, _, err := EmitLSML("scene-1", representativeLayout(), nil, nil, anim)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(bundle.Layout, &root); err != nil {
		t.Fatal(err)
	}
	got, ok := root["animations"]
	if !ok {
		t.Fatal("animations not attached to root node")
	}
	gotBytes, _ := json.Marshal(got)
	var want any
	_ = json.Unmarshal(anim, &want)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("animations drifted\n got:  %s\n want: %s", gotBytes, wantBytes)
	}
}

// When no animations are authored, the field is absent (back-compat with
// consumers that don't expect it).
func TestEmitLSML_NoAnimationsOmitsField(t *testing.T) {
	bundle, _, _, err := EmitLSML("scene-1", representativeLayout(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle.Layout), `"animations"`) {
		t.Fatalf("animations key leaked when none authored: %s", bundle.Layout)
	}
}

// External adapters declared on the layout map into the LSML
// external_adapters block as opaque entries (ADR 007 §C.1, §9).
func TestEmitLSML_ExternalAdapters(t *testing.T) {
	hz := 5.0
	adapters := []ExternalAdapter{
		{Key: "poll-scores", Label: "Scores", Kind: "http-poll", TargetPaths: []string{"score.value"}, FrequencyHz: &hz, URL: "https://x/scores"},
	}
	bundle, _, _, err := EmitLSML("scene-1", representativeLayout(), nil, adapters, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.ExternalAdapters) != 1 {
		t.Fatalf("external_adapters = %d, want 1", len(bundle.ExternalAdapters))
	}
	var decoded map[string]any
	if err := json.Unmarshal(bundle.ExternalAdapters[0], &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["kind"] != "http-poll" || decoded["key"] != "poll-scores" {
		t.Fatalf("adapter not preserved: %v", decoded)
	}
}

// --- helpers ---

func assertKnownPrimitiveTree(t *testing.T, path string, node map[string]any) {
	t.Helper()
	kind, _ := node["kind"].(string)
	if kind == "" {
		t.Fatalf("%s: node missing kind", path)
	}
	switch kind {
	case "stack", "grid", "frame", "text", "image", "shape", "media", "repeat", "instance":
		// known LSML 1.1 catalog (1.0 eight + 1.1 instance).
	default:
		t.Fatalf("%s: unknown primitive %q", path, kind)
	}
	if children, ok := node["children"].([]any); ok {
		for i, c := range children {
			cm, ok := c.(map[string]any)
			if !ok {
				t.Fatalf("%s.children[%d]: not an object", path, i)
			}
			assertKnownPrimitiveTree(t, path+".children", cm)
		}
	}
}

func firstChild(t *testing.T, node map[string]any) map[string]any {
	t.Helper()
	children, ok := node["children"].([]any)
	if !ok || len(children) == 0 {
		t.Fatalf("node %v has no children", node["kind"])
	}
	cm, ok := children[0].(map[string]any)
	if !ok {
		t.Fatal("first child is not an object")
	}
	return cm
}
