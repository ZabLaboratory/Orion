package lsdp

import (
	"encoding/json"
	"log/slog"
)

// boundLeavesFromLSML is boundLeavesFromBundle's counterpart for the
// stateless path (#396, ADR-BLUE-012): the only render-bundle artefact
// that flows through internal/api/scene_intent.go's bluehost slot is the
// LSML bytes ZabCanvas's zabcanvas.resolved-scene.v1 envelope carries
// (bluehost.Host.SetBundle/Bundle), never a *compiler.RenderBundle — the
// compiler package that type belongs to is never invoked on this path
// (Compile takes a Canvas PushEnvelope, not blue.program.v1 bytes).
//
// # The real producer, not EmitLSML
//
// The bytes this function decodes are produced by Prism
// (src/renderer/src/lib/lsml/from-scene.ts, serializeText/serializeImage/
// serializeRepeat et al.) and merely stored, content-addressed, by
// ZabCanvas — Orion's own internal/compiler/emit_lsml.go emitter has NO
// live call site (only its own tests) and must NEVER be treated as a
// stand-in oracle for this wire shape: it cannot even represent a
// `repeat` node's `template` (it only walks `Children`), so a parity
// test built against it is structurally blind to the exact shape a real
// authored repeat produces. See collectBoundLeavesLSML's `template`
// handling below, and bound_leaves_lsml_test.go's literal (non-EmitLSML)
// fixture.
//
// A nil/empty/malformed bundle yields an empty (disabled) set — the same
// fail-open posture as boundLeavesFromBundle(nil): a decode gap must
// never black out the scene, only leave the gate inert (the pre-#396
// posture). That posture is a documented, valid case too — lsml_bundle
// is OPTIONAL on the envelope (scene_intent.go's resolvedSceneEnvelope) —
// so an empty/absent/malformed bundle is logged at WARN (distinct
// wording per cause) rather than silently reproducing the original
// defect with no signal at all. A bundle that decodes but legitimately
// binds nothing (a passthrough/operator-only scene) is NOT logged — that
// is the same fail-open case boundLeavesFromBundle already treats as
// unremarkable.
func boundLeavesFromLSML(sceneID string, raw []byte, logger *slog.Logger) boundLeafSet {
	set := boundLeafSet{exact: map[string]struct{}{}}
	if len(raw) == 0 {
		if logger != nil {
			logger.Warn("lsml bundle absent for stateless mirror — bound-leaf gate disabled (fail-open)", "scene_id", sceneID)
		}
		return set
	}
	var bundle struct {
		Layout         json.RawMessage `json:"layout"`
		OperatorInputs []struct {
			Path string `json:"path"`
		} `json:"operator_inputs"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		if logger != nil {
			logger.Warn("lsml bundle malformed for stateless mirror — bound-leaf gate disabled (fail-open)", "scene_id", sceneID, "err", err)
		}
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

// collectBoundLeavesLSML recurses one LSML node (opaque json.RawMessage),
// adding every bound leaf path to dst. It reads three fields, each
// decoded independently so a decode gap on one (e.g. a non-string `bind`
// value — the LSML validator explicitly tolerates one, lsml/validate.go's
// validateNode skips non-string bind values rather than rejecting the
// node) never discards the other two, unlike a single combined struct
// decode where one field's type mismatch fails the whole node and silently
// drops its entire subtree:
//
//   - `bind` (map[string]LeafPath, cf. lsml/validate.go's `node["bind"]`)
//     — the node's own leaf-path bindings.
//   - `children` ([]Node) — stack/grid/frame's ordered child list.
//   - `template` (Node, singular — NOT an array) — a `repeat` node's item
//     template (lsml/capture.go's CheckZabCaptureNodes walks the exact
//     same shape; lsml/validate.go:137 recurses into it when kind=="repeat").
//     Prism's serializeRepeat (from-scene.ts) emits `bind:{items:...}` on
//     the repeat node itself PLUS a `template` subtree whose own bound
//     leaves (e.g. a literal text/image hoisted to an ABSOLUTE
//     `__lit.<kind>.<id>` path, not a descendant of `items`) are real,
//     independent bindings — omitting `template` silently drops every
//     leaf a repeat's item authors, exactly the #396-class regression
//     this whole function exists to close.
func collectBoundLeavesLSML(raw json.RawMessage, dst map[string]struct{}) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}

	if bindRaw, ok := fields["bind"]; ok {
		var bind map[string]json.RawMessage
		if err := json.Unmarshal(bindRaw, &bind); err == nil {
			for _, v := range bind {
				var path string
				if err := json.Unmarshal(v, &path); err == nil && path != "" {
					dst[path] = struct{}{}
				}
			}
		}
	}

	if childrenRaw, ok := fields["children"]; ok {
		var children []json.RawMessage
		if err := json.Unmarshal(childrenRaw, &children); err == nil {
			for _, child := range children {
				collectBoundLeavesLSML(child, dst)
			}
		}
	}

	if tmplRaw, ok := fields["template"]; ok {
		collectBoundLeavesLSML(tmplRaw, dst)
	}
}

// KNOWN GAP (M1, non-blocking — Vigil CHANGES_REQUIRED on 463838d): unlike
// boundLeavesFromBundle (bound_leaves.go:105-112, which reads the lowered
// compiler.LayoutNode's Keyframes field), collectBoundLeavesLSML collects
// no keyframes.key equivalent — LSML carries no keyframes field at all
// (compiler.RenderBundle.AuthoringRoot's doc: Keyframes "rides ONLY on
// the lowered render bundle", never authored, so EmitLSML never emits it
// either). No reachable path makes this a live gap today (the stateless
// path has no keyframe-driven leaf yet), but it stays a real divergence
// between the two gates if/when one lands.
