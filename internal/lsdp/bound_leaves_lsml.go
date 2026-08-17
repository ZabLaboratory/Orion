package lsdp

import (
	"encoding/json"

	"github.com/Lumencast/lumencast-go/lsml"
)

// boundLeavesFromLSML is boundLeavesFromBundle's counterpart for the
// stateless path (#396, ADR-BLUE-012): the only render-bundle artefact
// that flows through internal/api/scene_intent.go's bluehost slot is the
// LSML bytes ZabCanvas's zabcanvas.resolved-scene.v1 envelope carries
// (bluehost.Host.SetBundle/Bundle), never a *compiler.RenderBundle — the
// compiler package that type belongs to is never invoked on this path
// (Compile takes a Canvas PushEnvelope, not blue.program.v1 bytes).
//
// This is not a second, independent gate: LSML's `bind` map is the SAME
// data as compiler.LayoutNode.Bindings by construction — EmitLSML
// (internal/compiler/emit_lsml.go) is documented to carry
// Kind/ID/Props/Bindings/Transitions/Children straight from the
// authoring tree onto the LSML wire unchanged (only Keyframes, a
// lowering-only field, has no LSML counterpart). So the leaf-path
// extraction mirrors collectBoundLeaves exactly, reading LSML's `bind`/
// `children` in place of Bindings/Children.
//
// A nil/empty/malformed bundle yields an empty (disabled) set — the same
// fail-open posture as boundLeavesFromBundle(nil): a decode error must
// never black out the scene, only leave the gate inert (the pre-fix
// posture), which is always safe.
func boundLeavesFromLSML(raw []byte) boundLeafSet {
	set := boundLeafSet{exact: map[string]struct{}{}}
	if len(raw) == 0 {
		return set
	}
	var bundle lsml.Bundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return set
	}
	if len(bundle.Layout) > 0 {
		collectBoundLeavesLSML(bundle.Layout, set.exact)
	}
	for _, oi := range bundle.OperatorInputs {
		if oi.Path != "" {
			set.exact[oi.Path] = struct{}{}
		}
	}
	return set
}

// collectBoundLeavesLSML recurses one LSML node (opaque json.RawMessage,
// per lsml.Node.Children/Bind — see lumencast-go/lsml.Node), adding every
// bound leaf path to dst. Malformed JSON at a node is skipped, not fatal
// — the fail-open posture of the caller: a decode gap can only leave
// some leaves out of the bound set, never fabricate a false membership.
func collectBoundLeavesLSML(raw json.RawMessage, dst map[string]struct{}) {
	var node struct {
		Bind     map[string]string `json:"bind"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return
	}
	for _, path := range node.Bind {
		if path != "" {
			dst[path] = struct{}{}
		}
	}
	for _, child := range node.Children {
		collectBoundLeavesLSML(child, dst)
	}
}
