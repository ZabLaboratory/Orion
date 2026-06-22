package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestFetchComputeManifest_BlueContract decodes Blue's REAL
// `{entries:[...],count}` envelope (testdata/blue_compute_manifest.json,
// shaped byte-for-byte like Blue's ManifestEntryDTO) over an httptest
// server and asserts the adapted ComputeManifest map.
//
// This is the regression guard for issue #30: decoding Blue's response
// straight into ComputeManifest (a top-level map) blew up every push
// with "cannot unmarshal array into Go value" — caught only by the live
// E2E on 2026-06-05 because the unit tests used a Go-native fake
// fetcher. This test feeds the wire shape through the real getJSON path.
func TestFetchComputeManifest_BlueContract(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "blue_compute_manifest.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	manifest, err := f.FetchComputeManifest(context.Background())
	if err != nil {
		t.Fatalf("FetchComputeManifest: %v — the Blue envelope failed to decode (this is exactly issue #30)", err)
	}

	if gotPath != "/api/v1/_compute-manifest" {
		t.Fatalf("hit path %q, want /api/v1/_compute-manifest", gotPath)
	}

	// 1) Keyed by node_id == namespace.name@version, the same ref a
	//    blueprint node carries in `definition`.
	wantKeys := []string{
		"core.math.add@1",
		"core.input@1",
		"core.cast.to-integer@1",
		"core.source.read@1",
		"quasar.twitch.chat@1",
	}
	if len(manifest) != len(wantKeys) {
		t.Fatalf("manifest has %d entries, want %d: %v", len(manifest), len(wantKeys), manifest)
	}
	for _, k := range wantKeys {
		if _, ok := manifest[k]; !ok {
			t.Errorf("manifest missing key %q (compute-id key is namespace.name@version)", k)
		}
	}

	// 2) Purity flags survive — the criterion-18 gate depends on them.
	add := manifest["core.math.add@1"]
	if !add.IsPure || !add.IsBounded {
		t.Errorf("core.math.add@1: is_pure=%v is_bounded=%v, want true/true", add.IsPure, add.IsBounded)
	}
	// source.read is pure/bounded since ADR 012 Option B (introspection
	// compute, not an external fetch) — the manifest mirrors Blue's flipped
	// seed. Asserts the parse round-trips the flag faithfully.
	read := manifest["core.source.read@1"]
	if !read.IsPure || !read.IsBounded {
		t.Errorf("core.source.read@1: is_pure=%v is_bounded=%v, want true/true", read.IsPure, read.IsBounded)
	}

	// 3) Version int → string.
	if add.Version != "1" {
		t.Errorf("core.math.add@1 Version = %q, want \"1\" (Blue int → Orion string)", add.Version)
	}

	// 4) declared_inputs list-of-dict → input names.
	if len(add.DeclaredInputs) != 2 || add.DeclaredInputs[0] != "a" || add.DeclaredInputs[1] != "b" {
		t.Errorf("core.math.add@1 DeclaredInputs = %v, want [a b]", add.DeclaredInputs)
	}
	if input := manifest["core.input@1"]; len(input.DeclaredInputs) != 0 {
		t.Errorf("core.input@1 DeclaredInputs = %v, want empty", input.DeclaredInputs)
	}

	// 5) declared_output_type: string passthrough, list joined, null empty.
	if add.DeclaredOutputType != "core.primitive.float" {
		t.Errorf("core.math.add@1 output type = %q, want core.primitive.float", add.DeclaredOutputType)
	}
	if got := manifest["core.cast.to-integer@1"].DeclaredOutputType; got != "core.primitive.integer, core.primitive.boolean" {
		t.Errorf("core.cast.to-integer@1 output type = %q, want list joined", got)
	}
	if got := manifest["core.input@1"].DeclaredOutputType; got != "" {
		t.Errorf("core.input@1 output type = %q, want empty (null)", got)
	}
}

// TestFetchBlueprint_BlueContract proves the TWO-CALL fetch against
// Blue's REAL shape: GET /blueprints/{id} returns BlueprintRead (NO
// nodes/edges, just current_version), and the graph is read from GET
// /blueprints/{id}/versions/{current_version} whose VersionRead nests it
// under `graph.{nodes,edges}`. Regression guard for the blueprint-fetch
// drift found 2026-06-06 (every prior live push was blueprint-free, so
// FetchBlueprint — which decoded the row straight into BlueprintGraph and
// got empty nodes/edges — was never exercised). Same class as issue #31.
func TestFetchBlueprint_BlueContract(t *testing.T) {
	const blueprintRead = `{"id":"bp-1","slug":"sb","name":"Scoreboard","kind":"function",
		"status":"published","current_version":2,"tags":[],
		"interface":{"inputs":[],"outputs":[]}}`
	const versionRead = `{"id":"v-1","blueprint_id":"bp-1","version":2,"status":"published",
		"interface":{"inputs":[],"outputs":[]},"annotations":{},
		"graph":{"nodes":[
			{"id":"add","definition":"core.math.add@1","config":{},"inputs":[],"outputs":[]},
			{"id":"out","definition":"core.output@1","config":{"name":"score.total"}}
		],"edges":[
			{"id":"e1","from_node":"add","from_port":"sum","to_node":"out","to_port":"value"}
		],"variables":[]}}`

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if filepath.Base(filepath.Dir(r.URL.Path)) == "versions" {
			_, _ = w.Write([]byte(versionRead))
		} else {
			_, _ = w.Write([]byte(blueprintRead))
		}
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	bp, err := f.FetchBlueprint(context.Background(), "bp-1")
	if err != nil {
		t.Fatalf("FetchBlueprint: %v", err)
	}
	// Resolved the current version (2) on the second call.
	if len(paths) != 2 || paths[1] != "/api/v1/blueprints/bp-1/versions/2" {
		t.Fatalf("paths = %v, want [.../blueprints/bp-1, .../blueprints/bp-1/versions/2]", paths)
	}
	if bp.ID != "bp-1" || len(bp.Nodes) != 2 || len(bp.Edges) != 1 {
		t.Fatalf("graph lift wrong: id=%q nodes=%d edges=%d (drift: row has no graph)", bp.ID, len(bp.Nodes), len(bp.Edges))
	}
	if bp.Nodes[0].Compute != "core.math.add@1" {
		t.Fatalf("node[0].definition→Compute = %q, want core.math.add@1", bp.Nodes[0].Compute)
	}
	if got := string(bp.Nodes[1].Config["name"]); got != `"score.total"` {
		t.Fatalf("output node config.name = %s, want \"score.total\"", got)
	}
	if bp.Edges[0].FromNode != "add" || bp.Edges[0].ToPort != "value" {
		t.Fatalf("edge decode wrong: %+v", bp.Edges[0])
	}
}

// TestFetchBlueprintGraph_PinnedEndpoint proves the ADR 014 reference-
// expansion fetch hits the PINNED, published-only endpoint
// (/blueprints/{id}/versions/{version}/graph) — never current_version — and
// decodes the served nodes/edges/interface/purity (Blue #94 contract).
func TestFetchBlueprintGraph_PinnedEndpoint(t *testing.T) {
	const graphBody = `{
		"blueprint_id":"bp-double","version":3,"status":"published",
		"nodes":[
			{"id":"in","definition":"core.input@1","config":{"name":"x"}},
			{"id":"add","definition":"core.math.add@1"},
			{"id":"out","definition":"core.output@1","config":{"name":"result"}}
		],
		"edges":[{"id":"e1","from_node":"in","from_port":"value","to_node":"add","to_port":"a"}],
		"variables":[],
		"interface":{"inputs":[{"name":"x","type":"float","required":true}],
			"outputs":[{"name":"result","type":"float","required":true}],"side_effects":[]},
		"purity":{"is_pure":true,"is_bounded":true}
	}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphBody))
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	g, err := f.FetchBlueprintGraph(context.Background(), "bp-double", 3)
	if err != nil {
		t.Fatalf("FetchBlueprintGraph: %v", err)
	}
	if gotPath != "/api/v1/blueprints/bp-double/versions/3/graph" {
		t.Fatalf("hit %q, want pinned .../versions/3/graph", gotPath)
	}
	if g.Version != 3 || len(g.Nodes) != 3 || len(g.Edges) != 1 {
		t.Fatalf("decode wrong: version=%d nodes=%d edges=%d", g.Version, len(g.Nodes), len(g.Edges))
	}
	if len(g.Interface.Inputs) != 1 || g.Interface.Inputs[0].Name != "x" {
		t.Fatalf("interface inputs decode wrong: %+v", g.Interface.Inputs)
	}
	if !g.Purity.IsPure || !g.Purity.IsBounded {
		t.Fatalf("purity decode wrong: %+v", g.Purity)
	}
}

// TestFetchBlueprintGraph_ExecPinKind proves the fetcher deserialises the
// `kind` discriminator Blue #97 added to interface pins (graph-resolution.md
// § Exec pins). An exec-triggerable function declares `exec_in`/`then` pins
// with kind:"exec"; a data pin omits kind (defaults to data). Orion #186
// reads this field to drive the exec re-wire + on-start drop; if it did not
// deserialise it, every reference would look data-only and the on-start
// would never be removed.
func TestFetchBlueprintGraph_ExecPinKind(t *testing.T) {
	const graphBody = `{
		"blueprint_id":"bp-exec","version":1,"status":"published",
		"nodes":[
			{"id":"in","definition":"core.input@1","config":{"name":"exec_in"}},
			{"id":"q","definition":"core.db.query@1"}
		],
		"edges":[],
		"variables":[],
		"interface":{
			"inputs":[{"name":"exec_in","type":"exec","kind":"exec","required":true},
			          {"name":"limit","type":"int"}],
			"outputs":[{"name":"then","type":"exec","kind":"exec"}],
			"side_effects":[]},
		"purity":{"is_pure":true,"is_bounded":true}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphBody))
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	g, err := f.FetchBlueprintGraph(context.Background(), "bp-exec", 1)
	if err != nil {
		t.Fatalf("FetchBlueprintGraph: %v", err)
	}
	if len(g.Interface.Inputs) != 2 {
		t.Fatalf("inputs decode wrong: %+v", g.Interface.Inputs)
	}
	if g.Interface.Inputs[0].Name != "exec_in" || g.Interface.Inputs[0].Kind != "exec" {
		t.Fatalf("exec input pin kind not deserialised: %+v", g.Interface.Inputs[0])
	}
	// A data pin omits `kind` on the wire → empty (treated as data).
	if g.Interface.Inputs[1].Name != "limit" || g.Interface.Inputs[1].Kind != "" {
		t.Fatalf("data pin should have empty kind: %+v", g.Interface.Inputs[1])
	}
	if len(g.Interface.Outputs) != 1 || g.Interface.Outputs[0].Kind != "exec" {
		t.Fatalf("exec output pin kind not deserialised: %+v", g.Interface.Outputs)
	}
}

// TestFetchBlueprintGraph_TypedErrorsUnresolved proves Blue's typed
// 404/422 reference errors map to ErrRefUnresolved (→ BLUEPRINT_REF_UNRESOLVED
// at the compiler), never a transport-class error and never a silent
// current_version fall-back (ADR 014 §3.4 / Blue #94 contract).
func TestFetchBlueprintGraph_TypedErrorsUnresolved(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"not-found", http.StatusNotFound, "BLUEPRINT_NOT_FOUND"},
		{"version-not-found", http.StatusNotFound, "BLUEPRINT_VERSION_NOT_FOUND"},
		{"not-published", http.StatusUnprocessableEntity, "BLUEPRINT_VERSION_NOT_PUBLISHED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"` + tc.code + `","message":"x","blueprint_id":"bp-1","version":7}`))
			}))
			defer srv.Close()

			f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
			_, err := f.FetchBlueprintGraph(context.Background(), "bp-1", 7)
			if !errors.Is(err, ErrRefUnresolved) {
				t.Fatalf("err = %v, want wrapped ErrRefUnresolved", err)
			}
		})
	}
}

// TestFetchComputeManifest_ResolvesBlueprintDefinitionRef proves the
// decoded manifest's keys line up with the compute ref a blueprint node
// references — i.e. a valid stdlib node does NOT trip a spurious
// UNKNOWN_COMPUTE_NODE. Blue's blueprint graph node carries the ref in
// `definition` = namespace.name@version (graph.py:32,40); the manifest
// is keyed by node_id = namespace.name@version (compute_manifest.py:126).
func TestFetchComputeManifest_ResolvesBlueprintDefinitionRef(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "blue_compute_manifest.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	manifest, err := NewHTTPFetcher("http://canvas.invalid", srv.URL, "").
		FetchComputeManifest(context.Background())
	if err != nil {
		t.Fatalf("FetchComputeManifest: %v", err)
	}

	// A blueprint node referencing the pure stdlib add resolves and is
	// pure — the exact path validateBlueprint walks.
	const ref = "core.math.add@1"
	entry, found := manifest[ref]
	if !found {
		t.Fatalf("compute ref %q not in manifest — validateBlueprint would emit a spurious UNKNOWN_COMPUTE_NODE", ref)
	}
	if !entry.IsPure {
		t.Fatalf("compute ref %q decoded as impure — would emit a spurious IMPURE_COMPUTE", ref)
	}
}

// TestBlueprintNode_DecodesDefinitionWireField is the contract test the
// rest of the suite was missing: every other test builds the compute ref
// as a Go string literal and indexes manifest[ref], so the JSON tag on
// BlueprintNode is never exercised. This test decodes a REALISTIC Blue
// blueprint graph payload (the shape of Blue/src/blue/schemas/graph.py:
// nodes carry the ref in `definition`, edges in from_node/from_port/
// to_node/to_port) into BlueprintGraph and proves the tag reads
// `definition`, then runs the full validateBlueprint path with a manifest
// that contains the node's ref — expecting zero diagnostics.
//
// Regression-proving: with the old tag `json:"compute"`, no producer
// emits `compute`, so bp.Nodes[0].Compute decodes to "" — the first
// assertion below fails, and validateBlueprint would look up manifest[""]
// and emit UNKNOWN_COMPUTE_NODE. This test goes red on the old tag and
// green on `json:"definition"` (issue #32, ADR 004 §7.1).
func TestBlueprintNode_DecodesDefinitionWireField(t *testing.T) {
	// A blueprint graph as Blue actually serialises it (graph.py:29-58).
	// Note: no `compute`, no `output_at`, no `args` keys — those are not
	// the producer's field names (the output_at/args drift is issue #35,
	// out of scope here). The ref lives in `definition`.
	const payload = `{
		"id": "bp-contract-1",
		"nodes": [
			{
				"id": "n1",
				"definition": "core.math.add@1",
				"config": {"label": "add A+B"},
				"inputs": [
					{"name": "a", "type": "core.primitive.float"},
					{"name": "b", "type": "core.primitive.float"}
				],
				"outputs": [
					{"name": "result", "type": "core.primitive.float"}
				]
			}
		],
		"edges": [
			{"from_node": "n0", "from_port": "out", "to_node": "n1", "to_port": "a"}
		]
	}`

	var bp BlueprintGraph
	if err := json.Unmarshal([]byte(payload), &bp); err != nil {
		t.Fatalf("unmarshal Blue blueprint payload: %v", err)
	}

	if len(bp.Nodes) != 1 {
		t.Fatalf("decoded %d nodes, want 1", len(bp.Nodes))
	}
	// The load-bearing assertion: the tag must bind to `definition`. On
	// the old `json:"compute"` tag this is "" and the test fails here.
	if got := bp.Nodes[0].Compute; got != "core.math.add@1" {
		t.Fatalf("bp.Nodes[0].Compute = %q, want %q — the JSON tag must read the `definition` wire field, not `compute`", got, "core.math.add@1")
	}
	// Sanity: the snake_case edge tags decode too (already covered, but
	// keeps this payload honest as a full graph shape).
	if bp.Edges[0].ToNode != "n1" || bp.Edges[0].ToPort != "a" {
		t.Fatalf("edge decoded as %+v, want to_node=n1 to_port=a", bp.Edges[0])
	}

	// Now the full validation path: a manifest containing the node's ref
	// must produce zero diagnostics. testdata/blue_compute_manifest.json
	// already carries core.math.add@1 as is_pure=true is_bounded=true.
	fixture, err := os.ReadFile(filepath.Join("testdata", "blue_compute_manifest.json"))
	if err != nil {
		t.Fatalf("read manifest fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	manifest, err := NewHTTPFetcher("http://canvas.invalid", srv.URL, "").
		FetchComputeManifest(context.Background())
	if err != nil {
		t.Fatalf("FetchComputeManifest: %v", err)
	}
	if _, ok := manifest["core.math.add@1"]; !ok {
		t.Fatalf("manifest fixture missing core.math.add@1 — the test premise is broken")
	}

	_, _, diags := validateBlueprint(&bp, manifest, nil)
	if len(diags) != 0 {
		t.Fatalf("validateBlueprint emitted %d diagnostic(s) for a valid pure node, want 0: %+v", len(diags), diags)
	}
}

// TestFetchCanvasLayout_BackfillsDefaultsFromBundle proves the literal
// back-fill: the `/layouts` adapter serves a layout WITHOUT a `defaults` map,
// so FetchCanvasLayout reads the constants from the wrapped LSML-bundle store
// (`/lsml-bundles/{v}` → `.bundle.defaults`) and attaches them to the layout.
// Without this, every static text/image binds to a `__lit.*` leaf nothing
// seeds and the scene paints empty.
func TestFetchCanvasLayout_BackfillsDefaultsFromBundle(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/layouts/v1":
			// The adapter drops defaults — only version + root.
			_, _ = w.Write([]byte(`{"version":"v1","root":{"kind":"stack"}}`))
		case r.URL.Path == "/api/v1/lsml-bundles/v1":
			// The store wraps the bundle; defaults live at .bundle.defaults.
			_, _ = w.Write([]byte(`{"content_hash":"v1","bundle":{"defaults":{"__lit.text.text_7":"BROKEN BLADE","__lit.image.image_6":"assets/abc.png"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "tok")
	layout, err := f.FetchCanvasLayout(context.Background(), "v1")
	if err != nil {
		t.Fatalf("FetchCanvasLayout: %v", err)
	}
	if len(layout.Defaults) != 2 {
		t.Fatalf("Defaults = %d, want 2 (back-fill from .bundle.defaults failed)", len(layout.Defaults))
	}
	if got := string(layout.Defaults["__lit.text.text_7"]); got != `"BROKEN BLADE"` {
		t.Fatalf("__lit.text.text_7 = %s, want \"BROKEN BLADE\"", got)
	}
	// It must have consulted the bundle store after the bare /layouts read.
	if len(hits) != 2 || hits[0] != "/api/v1/layouts/v1" || hits[1] != "/api/v1/lsml-bundles/v1" {
		t.Fatalf("request sequence = %v, want [layouts, lsml-bundles]", hits)
	}
}

// TestFetchCanvasLayout_KeepsInlineDefaults proves the back-fill is skipped
// when the layout already carries defaults inline (forward-compatible with a
// fixed /layouts adapter): no second request, inline values preserved.
func TestFetchCanvasLayout_KeepsInlineDefaults(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1","root":{"kind":"stack"},"defaults":{"__lit.text.a":"hi"}}`))
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "tok")
	layout, err := f.FetchCanvasLayout(context.Background(), "v1")
	if err != nil {
		t.Fatalf("FetchCanvasLayout: %v", err)
	}
	if len(layout.Defaults) != 1 || string(layout.Defaults["__lit.text.a"]) != `"hi"` {
		t.Fatalf("inline defaults not preserved: %v", layout.Defaults)
	}
	if hits != 1 {
		t.Fatalf("made %d requests, want 1 (no bundle back-fill when defaults inline)", hits)
	}
}

// TestFetchBlueprint_CarriesVariables proves the blueprint variables back-fill:
// FetchBlueprint must decode `graph.variables` so a top-level blueprint's
// CONSTANT declarations (e.g. score-to-color's `palette`) reach
// foldDeclaredVariables and seed `__vars..<name>`. Dropping them (only
// nodes+edges decoded) left `core.variable.get@1` reading an unseeded leaf →
// null → the rating-colour squares rendered transparent.
func TestFetchBlueprint_CarriesVariables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/blueprints/bp1":
			_, _ = w.Write([]byte(`{"id":"bp1","current_version":5}`))
		case r.URL.Path == "/api/v1/blueprints/bp1/versions/5":
			_, _ = w.Write([]byte(`{"graph":{"nodes":[],"edges":[],"variables":[{"id":"v_palette","name":"palette","type":"core.primitive.json","value":["#B31A1A","#4D4DFF"]}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "tok")
	bp, err := f.FetchBlueprint(context.Background(), "bp1")
	if err != nil {
		t.Fatalf("FetchBlueprint: %v", err)
	}
	if len(bp.Variables) != 1 || bp.Variables[0].Name != "palette" {
		t.Fatalf("Variables = %+v, want the palette declaration carried", bp.Variables)
	}
	if got := string(bp.Variables[0].Value); got != `["#B31A1A","#4D4DFF"]` {
		t.Fatalf("palette value = %s, want the colour list", got)
	}
}
