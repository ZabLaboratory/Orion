package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// These tests exercise the SAME chain the push handler runs before persist
// (ADR 002 §3.4 T6 / #I): EmitLSML(authoring tree) → GateLSMLBundle. They
// prove that a compiled bundle carrying a hostile authoring prop is refused
// at the gate — the handler turns a non-empty Errors() into 422
// LSML_GATE_REJECTED and never persists/serves the scene. The pure gate's
// per-rule coverage lives in internal/compiler/authoring_gate_test.go; here
// we guard the integration seam (the authoring tree actually reaches the
// gate through the real emitter).

func gateAuthoringTree(t *testing.T, root compiler.LayoutNode, assets json.RawMessage) compiler.Diagnostics {
	t.Helper()
	bundle, _, _, err := compiler.EmitLSML(
		uuid.New().String(), root, nil, nil, nil, assets,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	return compiler.GateLSMLBundle(bundle)
}

func TestPushGate_HostileSrcRefusedThroughEmitter(t *testing.T) {
	root := compiler.LayoutNode{
		Kind: "image",
		ID:   "logo",
		Props: map[string]json.RawMessage{
			"src": json.RawMessage(`"https://evil.com/x.png"`),
		},
	}
	assets := json.RawMessage(`{"allowedHosts":["cdn.example.com"]}`)
	d := gateAuthoringTree(t, root, assets)
	if !d.HasErrors() {
		t.Fatalf("hostile src must be refused through the emitter, got %+v", d.Items)
	}
}

func TestPushGate_OutOfEnumBlendRefusedThroughEmitter(t *testing.T) {
	root := compiler.LayoutNode{
		Kind: "shape",
		ID:   "s",
		Props: map[string]json.RawMessage{
			"blendMode": json.RawMessage(`"PASS_THROUGH"`),
		},
	}
	d := gateAuthoringTree(t, root, nil)
	if !d.HasErrors() {
		t.Fatalf("out-of-enum blendMode must be refused, got %+v", d.Items)
	}
}

func TestPushGate_CleanBundlePassesThroughEmitter(t *testing.T) {
	root := compiler.LayoutNode{
		Kind: "frame",
		ID:   "root",
		Props: map[string]json.RawMessage{
			"backgrounds": json.RawMessage(`[{"kind":"image","src":"https://cdn.example.com/c.png","objectFit":"cover"}]`),
		},
	}
	assets := json.RawMessage(`{"allowedHosts":["cdn.example.com"]}`)
	d := gateAuthoringTree(t, root, assets)
	if d.HasErrors() {
		t.Fatalf("clean bundle must pass the gate, got %+v", d.Items)
	}
}
