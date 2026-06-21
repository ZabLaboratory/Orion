package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// minimalBundle builds a small but representative frozen bundle: one canvas
// layout keyed by its 64-hex version, one collapsed blueprint, one pinned
// resolved graph, and a verbatim compute manifest envelope. No components
// (the canvas-chat-sponso scene declares none).
func minimalBundle() SceneBundle {
	return SceneBundle{
		CanvasLayouts: map[string]json.RawMessage{
			sixtyFourHex: json.RawMessage(`{
				"version": "` + sixtyFourHex + `",
				"root": {"kind":"box","id":"root","children":[
					{"kind":"text","id":"t","bindings":{"value":"chat.last_message"}}
				]},
				"assets": {"allowedHosts":["cdn.cyell.dev"]}
			}`),
		},
		Blueprints: map[string]json.RawMessage{
			"11111111-1111-1111-1111-111111111111": json.RawMessage(`{
				"id":"11111111-1111-1111-1111-111111111111",
				"nodes":[{"id":"n1","definition":"core.output@1","config":{"name":"title"}}],
				"edges":[]
			}`),
		},
		BlueprintGraphs: map[string]json.RawMessage{
			"22222222-2222-2222-2222-222222222222@3": json.RawMessage(`{
				"blueprint_id":"22222222-2222-2222-2222-222222222222",
				"version":3,
				"nodes":[{"id":"in","definition":"core.input@1","config":{"name":"score"}}],
				"edges":[],
				"interface":{"inputs":[{"name":"score","type":"number","required":true}],
				             "outputs":[{"name":"color","type":"string","required":true}]},
				"purity":{"is_pure":true,"is_bounded":true}
			}`),
		},
		ComputeManifest: json.RawMessage(`{
			"entries":[
				{"node_id":"core.output@1","is_pure":true,"is_bounded":true,
				 "declared_inputs":[{"name":"value"}],"declared_output_type":null,"version":1}
			],
			"count":1
		}`),
	}
}

func TestBundledFetcher_ServesFrozenLayout(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	l, err := f.FetchCanvasLayout(context.Background(), sixtyFourHex)
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != sixtyFourHex || l.Root.Kind != "box" {
		t.Fatalf("layout not served verbatim: %+v", l)
	}
	if len(l.Assets) == 0 {
		t.Fatal("opaque assets block must survive")
	}
}

func TestBundledFetcher_CollapsesBlueprintTwoCall(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	g, err := f.FetchBlueprint(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 1 || g.Nodes[0].Compute != "core.output@1" {
		t.Fatalf("collapsed blueprint mismatch: %+v", g.Nodes)
	}
}

func TestBundledFetcher_PinnedResolvedGraph(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	r, err := f.FetchBlueprintGraph(context.Background(), "22222222-2222-2222-2222-222222222222", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 3 || !r.Purity.IsPure {
		t.Fatalf("resolved graph mismatch: %+v", r)
	}
}

// A missing pinned (id,version) must fail CLOSED with ErrRefUnresolved — the
// same sentinel the HTTP path raises — so the compiler rejects the reference
// with BLUEPRINT_REF_UNRESOLVED rather than substituting another version.
func TestBundledFetcher_MissingPinnedFailsClosed(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	_, err := f.FetchBlueprintGraph(context.Background(), "22222222-2222-2222-2222-222222222222", 99)
	if !errors.Is(err, ErrRefUnresolved) {
		t.Fatalf("missing pinned version must be ErrRefUnresolved, got %v", err)
	}
}

// A bundle miss for layout/blueprint/component is a hard fetch error, never
// a silent nil.
func TestBundledFetcher_MissIsHardError(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	if _, err := f.FetchCanvasLayout(context.Background(), "deadbeef"); !errors.Is(err, ErrBundleMiss) {
		t.Fatalf("layout miss = %v, want ErrBundleMiss", err)
	}
	if _, err := f.FetchBlueprint(context.Background(), "no-such-id"); !errors.Is(err, ErrBundleMiss) {
		t.Fatalf("blueprint miss = %v, want ErrBundleMiss", err)
	}
	if _, err := f.FetchComponent(context.Background(), ComponentRef{ID: "c", Version: "1"}); !errors.Is(err, ErrBundleMiss) {
		t.Fatalf("component miss = %v, want ErrBundleMiss", err)
	}
}

func TestBundledFetcher_ComputeManifestVerbatim(t *testing.T) {
	f := NewBundledFetcher(minimalBundle())
	m, err := f.FetchComputeManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := m["core.output@1"]
	if !ok || !entry.IsPure {
		t.Fatalf("manifest keyed by node_id mismatch: %+v", m)
	}
}

// LoadSceneBundle round-trips an on-disk bundle through the real structs.
func TestLoadSceneBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	data, _ := json.Marshal(minimalBundle())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadSceneBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	f := NewBundledFetcher(b)
	if _, err := f.FetchCanvasLayout(context.Background(), sixtyFourHex); err != nil {
		t.Fatalf("loaded bundle layout fetch: %v", err)
	}
}

func TestLoadSceneBundle_Missing(t *testing.T) {
	if _, err := LoadSceneBundle(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing bundle file must error (fail-closed boot)")
	}
}
