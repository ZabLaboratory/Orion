package lsdp

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// m3AuthoringGraph is the pre-lowering (authoring vocab) equivalent of
// m3Bundle() (bound_leaves_test.go): the same two bindings, in the shape
// EmitLSML consumes (LayoutNode.Bindings/Children).
func m3AuthoringGraph() compiler.LayoutNode {
	return compiler.LayoutNode{
		Kind: "stack",
		Children: []compiler.LayoutNode{
			{Kind: "text", ID: "board", Bindings: map[string]string{"value": "__vars..leaderboard_display"}},
			{Kind: "text", ID: "chat", Bindings: map[string]string{"value": "chat.display"}},
		},
	}
}

// emitM3LSML runs Orion's own compiler emitter (EmitLSML) over
// m3AuthoringGraph. IMPORTANT (Vigil, CHANGES_REQUIRED on 463838d):
// EmitLSML has NO live call site in production — the served
// zabcanvas.resolved-scene.v1 lsml_bundle is produced by Prism
// (from-scene.ts), never by Orion. EmitLSML is used here ONLY to prove
// the Bindings-passthrough claim (a `bind`/`children`-only tree), never
// as a stand-in for the real wire shape — it cannot even represent a
// `repeat` node's `template` field. See
// TestBoundLeavesFromLSML_RepeatTemplateAbsoluteLitPath below for the
// production-representative case, built as a literal fixture instead.
func emitM3LSML(t *testing.T) []byte {
	t.Helper()
	bundle, _, _, err := compiler.EmitLSML("scene-1", m3AuthoringGraph(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal lsml bundle: %v", err)
	}
	return raw
}

// TestBoundLeavesFromLSML_ParityWithCompiledBundle proves the narrower
// claim that still holds: for a `bind`/`children`-only tree (no
// `repeat`/`template`), the LSML-derived set matches the legacy
// compiler.RenderBundle-derived set — EmitLSML carries Bindings through
// unchanged. This does NOT establish parity for repeat templates; see
// TestBoundLeavesFromLSML_RepeatTemplateAbsoluteLitPath for that case,
// which EmitLSML is structurally unable to produce.
func TestBoundLeavesFromLSML_ParityWithCompiledBundle(t *testing.T) {
	legacy := boundLeavesFromBundle(m3Bundle())
	fromLSML := boundLeavesFromLSML("scene-1", emitM3LSML(t), nil)

	if !fromLSML.active() {
		t.Fatal("LSML-derived bound set must be active — the scene has real bindings")
	}
	for _, p := range m3WantKept {
		if !fromLSML.renderable(p) {
			t.Errorf("LSML-derived set must keep %q, matching the legacy gate", p)
		}
		if legacy.renderable(p) != fromLSML.renderable(p) {
			t.Errorf("parity break on %q: legacy=%v lsml=%v", p, legacy.renderable(p), fromLSML.renderable(p))
		}
	}
	for _, p := range m3WantDropped {
		if fromLSML.renderable(p) {
			t.Errorf("LSML-derived set must drop unbound leaf %q", p)
		}
	}
}

// repeatTemplateLSML is a LITERAL LSML bundle, in the exact shape
// Prism's serializeRepeat/serializeText/serializeImage
// (from-scene.ts@984abf7) actually emit for a `repeat` component — never
// generated via EmitLSML, which cannot produce a `template` field at
// all. Mirrors the real producer:
//
//   - the repeat node carries `bind: {items: "rows"}` (serializeRepeat)
//     AND a singular `template` node (not an array) — the item shape,
//     serialized once, replayed per iteration by the runtime.
//   - the template's text node is bound to an ABSOLUTE literal path
//     `__lit.text.<id>` (serializeText: `litPath = "__lit.text." + id`),
//     not a descendant of `rows` — a real, independent binding that only
//     `template` traversal reaches.
//   - a sibling `chat` text node, outside the repeat, bound normally —
//     the control leaf proving the walker still works on ordinary
//     children alongside a repeat.
const repeatTemplateLSML = `{
  "lsml": "1.2",
  "scene_id": "scene-1",
  "scene_version": "sha256:test-1",
  "layout": {
    "kind": "stack",
    "children": [
      {
        "kind": "repeat",
        "bind": {"items": "rows"},
        "template": {
          "kind": "text",
          "id": "row-label",
          "bind": {"value": "__lit.text.row_label_abc123"}
        }
      },
      {
        "kind": "text",
        "id": "chat",
        "bind": {"value": "chat.display"}
      }
    ]
  }
}`

// TestBoundLeavesFromLSML_RepeatTemplateAbsoluteLitPath is the B1
// regression Vigil found: a repeat's `template` subtree carries real,
// independent bindings (here an absolute `__lit.text.*` literal path,
// never a descendant of the repeat's own `items` binding) that a walker
// reading only `children` silently drops — emptying the leaf at the
// antenna on a scene that renders correctly today. The fixture is
// literal LSML, not EmitLSML output (see emitM3LSML's doc): EmitLSML
// cannot produce a `template` field, so it cannot exercise this path.
func TestBoundLeavesFromLSML_RepeatTemplateAbsoluteLitPath(t *testing.T) {
	bound := boundLeavesFromLSML("scene-1", []byte(repeatTemplateLSML), nil)

	if !bound.active() {
		t.Fatal("bound set must be active — the scene has real bindings")
	}
	if !bound.renderable("rows") {
		t.Error("repeat's own `items` binding (\"rows\") must be renderable")
	}
	if !bound.renderable("__lit.text.row_label_abc123") {
		t.Fatal("repeat TEMPLATE's absolute __lit binding must be renderable — dropping it blanks the label at the antenna (B1)")
	}
	if !bound.renderable("chat.display") {
		t.Error("sibling text binding outside the repeat must still be renderable")
	}
	// A leaf that merely shares the `__lit.text.` prefix but isn't the
	// exact bound path (nor a dot-descendant of one) must not be smuggled
	// through — the walker adds real paths, not a permissive prefix rule
	// beyond boundLeafSet's own documented dot-descendant semantics.
	if bound.renderable("__lit.text.row_label_other") {
		t.Error("an unrelated __lit path must not be renderable")
	}
}

// TestMirrorForLSML_GatesUnboundLeaves is TestBoundLeaves_SnapshotEmitsOnlyBound
// (bound_leaves_test.go) driven through the stateless-path entry point,
// MirrorForLSML, proving boundLeafSet actually executes end to end on
// this path (#396 resolution criterion #1).
func TestMirrorForLSML_GatesUnboundLeaves(t *testing.T) {
	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorForLSML("scene-1", "", emitM3LSML(t)).(*sceneMirror)

	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        m3SceneStateWithInternals(),
	})

	got := wire.kitState("scene-1")
	for _, p := range m3WantKept {
		if _, ok := got[p]; !ok {
			t.Errorf("bound leaf %q was dropped on the stateless (LSML) path", p)
		}
	}
	for _, p := range m3WantDropped {
		if v, ok := got[p]; ok {
			t.Errorf("unbound intermediate %q leaked to the wire as %q — gate inert", p, v)
		}
	}
}

// TestMirrorForLSML_RepeatTemplateEndToEnd is
// TestBoundLeavesFromLSML_RepeatTemplateAbsoluteLitPath driven through
// Forward, proving the repeat-template leaf actually reaches the wire
// (not just that the extractor's set contains the path).
func TestMirrorForLSML_RepeatTemplateEndToEnd(t *testing.T) {
	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorForLSML("scene-1", "", []byte(repeatTemplateLSML)).(*sceneMirror)

	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State: map[string]json.RawMessage{
			"rows":                        json.RawMessage(`["a","b"]`),
			"__lit.text.row_label_abc123": json.RawMessage(`"Score"`),
			"chat.display":                json.RawMessage(`"hello"`),
			"catA0":                       json.RawMessage(`"unbound work leaf"`),
		},
	})

	got := wire.kitState("scene-1")
	if _, ok := got["__lit.text.row_label_abc123"]; !ok {
		t.Fatal("repeat template's __lit label was dropped end to end — antenna blank (B1)")
	}
	if _, ok := got["chat.display"]; !ok {
		t.Error("sibling bound leaf dropped")
	}
	if _, ok := got["catA0"]; ok {
		t.Error("unbound work leaf leaked")
	}
}

// TestMirrorForLSML_NilBundleDisablesGate is the mutation-proof pin for
// #396: this is EXACTLY the pre-fix behaviour (cmd/orion/main.go hardcoded
// a nil bundle) — reproduced here so the parity is explicit.
func TestMirrorForLSML_NilBundleDisablesGate(t *testing.T) {
	if boundLeavesFromLSML("scene-1", nil, nil).active() {
		t.Fatal("a nil LSML bundle must yield a disabled (inactive) gate — fail-open, never a black screen")
	}
	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorForLSML("scene-1", "", nil).(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State: map[string]json.RawMessage{
			"catA0": json.RawMessage(`"x"`),
		},
	})
	got := wire.kitState("scene-1")
	if _, ok := got["catA0"]; !ok {
		t.Errorf("gate disabled (nil bundle): scalar leaf catA0 must still pass the scalar filter")
	}
}

// TestBoundLeavesFromLSML_ToleratesNonStringBindValue proves B2's decode
// robustness: the LSML validator explicitly tolerates a non-string bind
// value (lsml/validate.go's validateNode skips it rather than rejecting
// the node) — collectBoundLeavesLSML must not lose the REST of the
// node's bindings, nor its children/template subtree, just because one
// bind value has an unexpected shape.
func TestBoundLeavesFromLSML_ToleratesNonStringBindValue(t *testing.T) {
	raw := []byte(`{
		"lsml": "1.0", "scene_id": "s", "scene_version": "sha256:x",
		"layout": {
			"kind": "frame",
			"bind": {"value": "good.path", "weird": {"nested": "object"}},
			"children": [
				{"kind": "text", "bind": {"value": "child.path"}}
			]
		}
	}`)
	bound := boundLeavesFromLSML("scene-1", raw, nil)
	if !bound.renderable("good.path") {
		t.Error("a well-formed sibling bind value must survive a malformed neighbour")
	}
	if !bound.renderable("child.path") {
		t.Error("children must still be walked despite a malformed bind value on the parent")
	}
}

// recordingHandler is a minimal slog.Handler that captures every record's
// message and level, for asserting exactly what boundLeavesFromLSML logs
// (and, just as important, what it does NOT log).
type recordingHandler struct {
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestBoundLeavesFromLSML_ObservableFailOpen is B2's observability
// requirement: an absent or malformed bundle must WARN (distinct
// wording — Eleven/Keeper can tell the two apart from logs alone), so
// the gate silently going inert again is never signal-free like the
// pre-#396 defect. A bundle that decodes but legitimately binds nothing
// (a passthrough/operator-only scene — the SAME fail-open case
// boundLeavesFromBundle(nil) already treats as unremarkable) must NOT
// warn — that's expected, not a regression signal.
func TestBoundLeavesFromLSML_ObservableFailOpen(t *testing.T) {
	newLogger := func() (*slog.Logger, *[]slog.Record) {
		var records []slog.Record
		return slog.New(recordingHandler{records: &records}), &records
	}

	t.Run("absent", func(t *testing.T) {
		logger, records := newLogger()
		boundLeavesFromLSML("scene-1", nil, logger)
		if len(*records) != 1 || (*records)[0].Level != slog.LevelWarn {
			t.Fatalf("expected exactly one WARN for an absent bundle, got %+v", *records)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		logger, records := newLogger()
		boundLeavesFromLSML("scene-1", []byte("not json"), logger)
		if len(*records) != 1 || (*records)[0].Level != slog.LevelWarn {
			t.Fatalf("expected exactly one WARN for a malformed bundle, got %+v", *records)
		}
	})

	t.Run("absent and malformed messages differ", func(t *testing.T) {
		_, absentRecords := newLogger()
		boundLeavesFromLSML("scene-1", nil, slog.New(recordingHandler{records: absentRecords}))
		_, malformedRecords := newLogger()
		boundLeavesFromLSML("scene-1", []byte("not json"), slog.New(recordingHandler{records: malformedRecords}))
		if (*absentRecords)[0].Message == (*malformedRecords)[0].Message {
			t.Fatalf("absent and malformed must log distinguishable messages, both were %q", (*absentRecords)[0].Message)
		}
	})

	t.Run("legitimately empty (well-formed, no bindings) does not warn", func(t *testing.T) {
		logger, records := newLogger()
		raw := []byte(`{"lsml":"1.0","scene_id":"s","scene_version":"sha256:x","layout":{"kind":"stack"}}`)
		bound := boundLeavesFromLSML("scene-1", raw, logger)
		if bound.active() {
			t.Fatal("precondition: this fixture must have no bindings")
		}
		if len(*records) != 0 {
			t.Fatalf("a legitimately binding-less (but well-formed) bundle must not warn, got %+v", *records)
		}
	})
}
