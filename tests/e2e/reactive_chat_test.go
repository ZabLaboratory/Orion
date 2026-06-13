//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Milestone 1 — reactive Twitch chat scene (live pipeline, no external
// effects). Proves the END-TO-END reactive dataflow the live show needs:
// a real Twitch chat event lands on the leaf Quasar writes
// (`__inputs.platform.twitch.g2nmathias.last_chat`), Orion's compiled
// graph recomputes a display string off it, and the Canvas-bound output
// leaf reflects "<author>: <message>". This is the M9 reactive-repaint
// pattern, driven this time by a platform event instead of a Blue trigger.
//
// PURE DATAFLOW (no exec, no egress). The blueprint binds only
// `quasar.twitch.chat@1` (leaf input), `core.data.get-field@1` (payload
// extraction, ADR 003 §3.3) and `core.string.concat@1` — no
// http.request / db.query, so nothing halts-at-node under the SetEffects
// egress hold (source.read is no longer a world op — ADR 012 Option B).
// Asserted structurally in TestE2E_ReactiveChat_UsesOnlyDataflowOps.
//
// The blueprint graph below is the source of truth for the Blue artefact
// the activation runbook authors (Blue/docs/runbooks/m1-reactive-chat.md
// + Blue/tests/fixtures/m1_reactive_chat.py, kept honest by
// Blue/tests/test_m1_reactive_chat.py) — same fetch contract as the canary.

// chatChannel is the operator's connected Twitch handle. Quasar writes
// the chat leaf for THIS channel (it casefolds; the handle is already
// lowercase). Orion expands the node's config.channel to the byte-exact
// leaf — the cross-repo contract pinned in
// internal/compiler/compile_platform_test.go.
const chatChannel = "g2nmathias"

// chatLeaf is the exact path Quasar writes a normalized chat message to
// (Quasar/src/quasar/core/normalizer.py::leaf_path,
// Blue _shared.py::leaf_path). The whole CanonicalEvent JSON lands here;
// get-field walks into it.
const chatLeaf = "__inputs.platform.twitch.g2nmathias.last_chat"

// chatDisplayLeaf is the output leaf the Canvas text element binds its
// `text` prop to (see the Canvas layout fixture in the runbook). The
// reactive output sink (core.output@1, named via config.name) writes the
// composed "<author>: <message>" string here on every chat event.
const chatDisplayLeaf = "chat.display"

// reactiveChatBlueprint is the M1 reactive scene's logic. Pure dataflow:
//
//	quasar.twitch.chat@1(channel=g2nmathias)
//	   ├─ get-field(path="actor.display_name") ─┐
//	   └─ get-field(path="payload.text") ───────┤
//	                                            concat(a=author+": ", b=text)  ← see below
//	                                              └→ output(name="chat.display")
//
// concat takes exactly two strings (a, b). To render "<author>: <text>"
// we first concat the author with the separator ": " (author into a, a
// ": " literal into b), then concat that with the text. Two concat
// stages, all pure string ops. The chat leaf node holds the WHOLE
// canonical event object, so both get-fields read from it (the substrate
// stores one value per node; from_port is not addressed — get-field's
// dot path selects the field).
func reactiveChatBlueprint() *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: "bp-m1-reactive-chat",
		Nodes: []compiler.BlueprintNode{
			// Leaf input — the platform event Quasar writes. No upstream,
			// no exec pin → Kind "input", Path the expanded chat leaf.
			{ID: "chat", Compute: "quasar.twitch.chat@1",
				Config: map[string]json.RawMessage{"channel": json.RawMessage(`"` + chatChannel + `"`)}},

			// Extract the author display name and the message text from the
			// event object (ADR 003 §3.3 payload extractor).
			{ID: "author", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"actor.display_name"`)},
				Inputs: []compiler.BlueprintPort{dPort("record")}},
			{ID: "text", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)},
				Inputs: []compiler.BlueprintPort{dPort("record")}},

			// ": " separator literal (seeds graph.Defaults at its own leaf).
			{ID: "sep", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`": "`)},
				Outputs: []compiler.BlueprintPort{dPort("out")}},

			// concat(author, ": ") → "<author>: "
			{ID: "authorSep", Compute: "core.string.concat@1",
				Inputs: []compiler.BlueprintPort{dPort("a"), dPort("b")}},
			// concat("<author>: ", text) → "<author>: <text>"
			{ID: "line", Compute: "core.string.concat@1",
				Inputs: []compiler.BlueprintPort{dPort("a"), dPort("b")}},

			// Output sink — writes the composed line to the display leaf.
			// Declared with ONE data port (value) and no exec pin → a pure
			// dataflow sink (Orion partitions on the graph's own ports).
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + chatDisplayLeaf + `"`)},
				Inputs: []compiler.BlueprintPort{dPort("value")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "chat", FromPort: "payload", ToNode: "author", ToPort: "record"},
			{FromNode: "chat", FromPort: "payload", ToNode: "text", ToPort: "record"},
			{FromNode: "author", FromPort: "value", ToNode: "authorSep", ToPort: "a"},
			{FromNode: "sep", FromPort: "out", ToNode: "authorSep", ToPort: "b"},
			{FromNode: "authorSep", FromPort: "out", ToNode: "line", ToPort: "a"},
			{FromNode: "text", FromPort: "value", ToNode: "line", ToPort: "b"},
			{FromNode: "line", FromPort: "out", ToNode: "out", ToPort: "value"},
		},
	}
}

// reactiveChatManifest is the COMPLETE op set the blueprint binds. Every
// entry is leaf-input / data / string — none is an egress op.
func reactiveChatManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"quasar.twitch.chat@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.concat@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// reactiveChatLayout binds a Canvas text element's `text` prop to the
// display leaf — the served form of the Canvas layout fixture the runbook
// stores in ZabCanvas (a one-text-element scene).
func reactiveChatLayout() *compiler.CanvasLayout {
	return &compiler.CanvasLayout{
		Version: "v1",
		Root: compiler.LayoutNode{
			Kind: "stack", ID: "root",
			Children: []compiler.LayoutNode{
				{Kind: "text", ID: "chatline", Bindings: map[string]string{"text": chatDisplayLeaf}},
			},
		},
	}
}

func reactiveChatFetcher() *stubFetcher {
	return &stubFetcher{
		layouts:    map[string]*compiler.CanvasLayout{"v1": reactiveChatLayout()},
		blueprints: map[string]*compiler.BlueprintGraph{"bp-m1-reactive-chat": reactiveChatBlueprint()},
		manifest:   reactiveChatManifest(),
	}
}

// dataflowOnlyOps is the closed allow-list the M1 reactive scene may bind:
// pure leaf-input / data / string nodes. An op outside this set is a
// regression that could reach an egress held under SetEffects.
var dataflowOnlyOps = map[string]struct{}{
	"quasar.twitch.chat@1":  {},
	"core.data.get-field@1": {},
	"core.literal@1":        {},
	"core.string.concat@1":  {},
	"core.string.format@1":  {},
	"core.output@1":         {},
}

// TestE2E_ReactiveChat_UsesOnlyDataflowOps guards the no-effects invariant
// by construction: the op set is a subset of the pure dataflow allow-list,
// and none is egress-shaped.
func TestE2E_ReactiveChat_UsesOnlyDataflowOps(t *testing.T) {
	for op := range reactiveChatManifest() {
		if _, ok := dataflowOnlyOps[op]; !ok {
			t.Fatalf("reactive chat binds non-dataflow op %q (M1 no-effects condition violated)", op)
		}
		if strings.Contains(op, "http") || strings.Contains(op, "db") ||
			strings.Contains(op, "request") {
			t.Fatalf("reactive chat binds egress-shaped op %q", op)
		}
	}
}

// TestE2E_ReactiveChat_FirstFlight flies the M1 reactive scene end to end:
// push (pure dataflow) → validate → activate → inject a REAL-shaped Twitch
// chat event on the leaf Quasar writes → the Canvas-bound display leaf
// reflects "<author>: <message>". This is the live reactive pipeline
// (M9 pattern) driven by a platform event — the franchissement M1.
func TestE2E_ReactiveChat_FirstFlight(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "m1-reactive-chat"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, reactiveChatFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push the pure-dataflow reactive scene — compiles + persists.
	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-m1-reactive-chat"}`); code != 200 {
		t.Fatalf("push of reactive chat scene = %d %v", code, body)
	}

	// The validation gate holds (no scene airs unvalidated).
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate-before-validate = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	// Validate, then activate.
	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate = %d", code)
	}
	waitValidated(t, base)
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate-after-validate = %d %v", code, body)
	}

	active := show.Active()
	if active == nil {
		t.Fatal("no active scene after activation")
	}

	// Inject a chat event with the EXACT canonical envelope shape Quasar
	// produces for an IRC PRIVMSG (normalizer.parse_irc_privmsg →
	// CanonicalEvent.model_dump). The whole event object lands on the leaf.
	event := `{"platform":"twitch","channel":"g2nmathias","type":"chat",` +
		`"ts":"2026-06-11T12:00:00Z",` +
		`"actor":{"platform_user_id":"12345","display_name":"NightBot",` +
		`"is_subscriber":false,"is_moderator":false,"badges":[]},` +
		`"payload":{"text":"GG WP !","emotes":[],"reply_to":null}}`
	active.Input(runtime.InputMsg{
		Path:   chatLeaf,
		Value:  json.RawMessage(event),
		Source: "quasar:twitch",
	})

	// The reactive graph recomputes and the Canvas-bound display leaf
	// reflects the message: "NightBot: GG WP !".
	assertReactiveLeaf(t, show, chatDisplayLeaf, func(v string) bool {
		return v == `"NightBot: GG WP !"`
	})

	// A second message repaints live (reactive, no re-push).
	event2 := `{"platform":"twitch","channel":"g2nmathias","type":"chat",` +
		`"ts":"2026-06-11T12:00:05Z",` +
		`"actor":{"platform_user_id":"678","display_name":"clodocapeo",` +
		`"is_subscriber":true,"is_moderator":true,"badges":["broadcaster/1"]},` +
		`"payload":{"text":"on est live","emotes":[],"reply_to":null}}`
	active.Input(runtime.InputMsg{
		Path:   chatLeaf,
		Value:  json.RawMessage(event2),
		Source: "quasar:twitch",
	})
	assertReactiveLeaf(t, show, chatDisplayLeaf, func(v string) bool {
		return v == `"clodocapeo: on est live"`
	})
}

// assertReactiveLeaf polls the active scene's snapshot until the given
// leaf satisfies pred. The display leaf is a scene-local computed leaf
// (the legacy single-blueprint push uses the empty blueprint key, so the
// configured output name `chat.display` is the leaf verbatim).
func assertReactiveLeaf(t *testing.T, show *runtime.Show, key string, pred func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if a := show.Active(); a != nil {
			sub, snap := a.Subscribe(8)
			a.Detach(sub)
			if v, ok := snap.State[key]; ok {
				last = string(v)
				if pred(last) {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("leaf %s never satisfied predicate on air; last = %q", key, last)
}
