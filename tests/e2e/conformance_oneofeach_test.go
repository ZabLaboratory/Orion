//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// TestE2E_Conformance_OneOfEachServedType is criterion 2 (ADR 003 §6
// "No capability rejection") for the set the compiler serves TODAY: a
// single blueprint carrying one node of EACH compiler-servable manifest
// type — every pure data compute plus every quasar.* platform node —
// is pushed through the REAL push API, validated through the gate
// (#87), activated on air, and every node's leaf is observed in the
// live snapshot. No node is rejected with UNSUPPORTED_COMPUTE /
// IMPURE_COMPUTE; every value is observable.
//
// SCOPE NOTE (honest, per R9 / ADR §3.1.2). Criterion 2's FULL form
// ("one node of EVERY served type") additionally covers the exec/impure
// types (branch, delay, variable.set, http.request, animation.play, …).
// Those have registered runtime executors and real-engine execution
// tests (internal/runtime conformance_exec_test.go + the per-op tests),
// but the COMPILER PARTITION that maps Blue exec ids onto ExecPrograms
// and stops emitting IMPURE_COMPUTE has not landed (the compiler still
// emits no ExecPrograms and validateBlueprint still rejects is_pure=
// false nodes — compile.go:539). Until that partition issue ships, an
// exec/impure node cannot travel the push→compile path, so this E2E
// asserts criterion 2 over the compiler-servable subset and documents
// the remainder as gated on the compiler-partition issue. The exec
// executors themselves ARE proven through the real engine in
// internal/runtime/conformance_exec_test.go.
func TestE2E_Conformance_OneOfEachServedType(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "conformance-oneofeach"); err != nil {
		t.Fatal(err)
	}

	bp, manifest, wantLeaves := buildOneOfEachBlueprint(t)
	fetcher := &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		},
		blueprints: map[string]*compiler.BlueprintGraph{"bp-all": bp},
		manifest:   manifest,
	}

	srv, show := gateTestServer(t, st, fetcher)
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	code, body := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-all"}`)
	if code != 200 {
		t.Fatalf("push of one-of-each blueprint rejected (%d): %v — a manifest node "+
			"was refused at compile (capability rejection, doctrine §1.1 violated)", code, body)
	}

	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)

	if code, _ := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate after validate = %d", code)
	}

	active := show.Active()
	if active == nil {
		t.Fatal("no active scene after activation")
	}

	// Observe the live snapshot — exactly what a subscriber sees.
	sub, snap := active.Subscribe(8)
	t.Cleanup(func() { active.Detach(sub) })

	var missing []string
	for _, leaf := range wantLeaves {
		if _, ok := snap.State[leaf]; !ok {
			missing = append(missing, leaf)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d served node(s) produced no observable leaf: %v", len(missing), missing)
	}
	t.Logf("criterion 2 (compiler-servable subset): %d served node leaves observable on air",
		len(wantLeaves))
}

// buildOneOfEachBlueprint constructs a blueprint with one node of each
// compiler-servable manifest type, plus the manifest the stub fetcher
// returns. Pure computes are fed a literal and drained into an output
// sink so each yields a distinct observable leaf; platform nodes bind
// their __inputs.platform leaf directly. Returns (blueprint, manifest,
// expected observable leaves).
func buildOneOfEachBlueprint(t *testing.T) (*compiler.BlueprintGraph, compiler.ComputeManifest, []string) {
	t.Helper()

	manifest := compiler.ComputeManifest{
		"core.literal@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":  {IsPure: true, IsBounded: true, Version: "1"},
	}
	var nodes []compiler.BlueprintNode
	var edges []compiler.BlueprintEdge
	var wantLeaves []string

	// A shared literal source feeding every compute's `a`/`value` port.
	nodes = append(nodes, compiler.BlueprintNode{
		ID:      "lit",
		Compute: "core.literal@1",
		Config:  map[string]json.RawMessage{"value": json.RawMessage(`2`)},
	})

	idx := 0
	for _, e := range conformance.Manifest() {
		sn, ok := conformance.Classify(e.NodeID)
		if !ok {
			continue // allowlisted (the inline-only db.* atoms)
		}

		switch sn.Kind {
		case conformance.KindCompute:
			if e.NodeID == "core.output@1" {
				continue // the sink itself, added per-compute below
			}
			manifest[e.NodeID] = compiler.ComputeManifestEntry{
				IsPure: true, IsBounded: true, Version: "1",
			}
			nodeID := fmt.Sprintf("c%d", idx)
			outID := fmt.Sprintf("o%d", idx)
			leafName := "leaf_" + strings.NewReplacer(".", "_", "@", "_", "-", "_").Replace(e.NodeID)
			nodes = append(nodes,
				compiler.BlueprintNode{ID: nodeID, Compute: e.NodeID},
				compiler.BlueprintNode{ID: outID, Compute: "core.output@1",
					Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + leafName + `"`)}},
			)
			edges = append(edges,
				compiler.BlueprintEdge{FromNode: "lit", ToNode: nodeID, ToPort: "a"},
				compiler.BlueprintEdge{FromNode: nodeID, ToNode: outID, ToPort: "value"},
			)
			wantLeaves = append(wantLeaves, leafName)
			idx++

		case conformance.KindPlatformBound:
			manifest[e.NodeID] = compiler.ComputeManifestEntry{
				IsPure: true, IsBounded: true, Version: "1",
			}
			nodeID := fmt.Sprintf("p%d", idx)
			nodes = append(nodes, compiler.BlueprintNode{
				ID:      nodeID,
				Compute: e.NodeID,
				Config:  map[string]json.RawMessage{"channel": json.RawMessage(`"zabchannel"`)},
			})
			// Expanded leaf: __inputs.platform.twitch.zabchannel.last_<event>.
			wantLeaves = append(wantLeaves,
				"__inputs.platform.twitch.zabchannel.last_"+e.Name)
			idx++

		default:
			// KindExecOp / KindEntry / KindLeafBound: not compiler-servable
			// through the push path today (R9 — no ExecProgram emission, and
			// core.input/literal are surfaces, not value computes). Covered
			// by internal/runtime conformance tests instead. Skipped here on
			// purpose, documented in the test header.
		}
	}

	return &compiler.BlueprintGraph{ID: "bp-all", Nodes: nodes, Edges: edges}, manifest, wantLeaves
}
