package compiler

import (
	"context"
	"encoding/json"
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
	read := manifest["core.source.read@1"]
	if read.IsPure || read.IsBounded {
		t.Errorf("core.source.read@1: is_pure=%v is_bounded=%v, want false/false", read.IsPure, read.IsBounded)
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

	_, _, diags := validateBlueprint(&bp, manifest)
	if len(diags) != 0 {
		t.Fatalf("validateBlueprint emitted %d diagnostic(s) for a valid pure node, want 0: %+v", len(diags), diags)
	}
}
