package compiler

import "testing"

// Regression guard for the embedded-local e2e (#152 defect 1), the consumer
// half of the #221 contract. Blue stores authoring graphs whose nodes carry
// EMPTY inputs/outputs; the frozen bundle (Prism #155) captured them verbatim,
// so every node had 0 ports → the exec partition (isExecNode reads
// BlueprintPort.Kind) classified 0 exec nodes → 0 exec programs → the on-call
// LCK/LEC entrypoints never armed → incompilable chain.
//
// The fix bakes per-node ports from the node-definition signatures into the
// frozen graph (Prism scripts/build-scene-bundle.mjs). This test proves the
// partition's behaviour both ways: a portless on-call graph yields NO exec set
// (the defect), and the SAME graph with the signature ports baked arms the
// on-call entrypoint (the fix). It also pins the contract requirement now
// stated in docs/contracts/embedded-local-contracts.md §B.2/§B.3: frozen graph
// nodes MUST carry their signature ports — the bundledFetcher does not enrich.

// onCallNode is the canvas-chat-sponso `on_lck` node as Blue STORES it: an
// operator on-call entry with empty ports (the defect-1 shape).
func onCallNodePortless() BlueprintNode {
	return BlueprintNode{
		ID:      "on_lck",
		Compute: "core.operator.on-call@1",
	}
}

// withBakedPorts returns the same node with the node-definition signature ports
// baked on: on-call carries the `then` exec OUT pin + a `payload` data pin.
func withBakedPorts(n BlueprintNode) BlueprintNode {
	n.Outputs = []BlueprintPort{
		{Name: "then", Type: "core.exec", Kind: "exec", Required: true},
		{Name: "payload", Type: "core.primitive.json", Kind: "data", Required: true},
	}
	return n
}

func TestPartition_PortlessOnCall_ArmsNothing(t *testing.T) {
	g := &BlueprintGraph{Nodes: []BlueprintNode{onCallNodePortless()}}
	execSet, prog, _ := partitionBlueprint(g, "")
	if len(execSet) != 0 {
		t.Fatalf("portless on-call must NOT classify as exec, got execSet=%v", execSet)
	}
	if prog != nil {
		t.Fatalf("portless graph must yield no exec program, got %+v", prog)
	}
}

func TestPartition_BakedOnCall_ArmsEntrypoint(t *testing.T) {
	g := &BlueprintGraph{Nodes: []BlueprintNode{withBakedPorts(onCallNodePortless())}}
	execSet, prog, diags := partitionBlueprint(g, "")
	if len(execSet) != 1 {
		t.Fatalf("baked on-call must classify as exec, got execSet=%v", execSet)
	}
	if prog == nil {
		t.Fatal("baked on-call must yield an exec program (≥1 entrypoint)")
	}
	if len(prog.Entrypoints) == 0 {
		t.Fatalf("on-call must arm an entrypoint; diags=%v", diags)
	}
	// The entry must be the on-call kind (the LCK/LEC operator trigger).
	var sawOnCall bool
	for _, e := range prog.Entrypoints {
		if e.Kind == "on-call" {
			sawOnCall = true
		}
	}
	if !sawOnCall {
		t.Fatalf("expected an on-call entrypoint, got %+v", prog.Entrypoints)
	}
}
