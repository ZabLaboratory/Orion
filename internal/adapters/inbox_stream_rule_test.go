package adapters

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

type recordedPlatformWrite struct {
	leaf    string
	payload any
}

type recordingRulePlaneSink struct {
	writes []recordedPlatformWrite
}

func (s *recordingRulePlaneSink) WritePlatformEvent(leaf string, payload any) {
	s.writes = append(s.writes, recordedPlatformWrite{leaf: leaf, payload: payload})
}

// Stream-rule routing at the inbox seam (ADR 009 §3.3, issue #153). A
// write accepted by the inbox fans out to the union {active} ∪ {promoted
// rules} — each target gated individually by sceneAcceptsPath — and to
// NOTHING else: a non-promoted, non-active roster scene stays inert
// (criterion #1 ADR 008). The audit ring still records ONE entry per
// write regardless of fan-out cardinality.

// awaitLeafDelta drains a subscriber until it sees a delta carrying `path`,
// or fails after the timeout.
func awaitLeafDelta(t *testing.T, sub *runtime.Subscription, path string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Out:
			if d, ok := msg.(*protocol.Delta); ok {
				for _, p := range d.Patches {
					if p.Path == path {
						return
					}
				}
			}
		case <-deadline:
			t.Fatalf("no delta for %q observed", path)
		}
	}
}

// expectNoLeafDelta drains a subscriber for a grace window and fails if any
// delta carrying `path` arrives — the dormant-scene inertness assertion.
func expectNoLeafDelta(t *testing.T, sub *runtime.Subscription, path string) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case msg := <-sub.Out:
			if d, ok := msg.(*protocol.Delta); ok {
				for _, p := range d.Patches {
					if p.Path == path {
						t.Fatalf("dormant roster scene received a delta for %q — it leaked into routing", path)
					}
				}
			}
		case <-deadline:
			return
		}
	}
}

// TestInbox_UnionRoutesToActiveAndRule (criterion #1, the union half): a
// single accepted write reaches BOTH the active scene and a promoted
// stream rule that declares the path, while a non-promoted roster scene
// receives nothing. One audit entry per write.
func TestInbox_UnionRoutesToActiveAndRule(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}

	show.Load("scene-active", platformGraph("scene-active"), bundle)
	show.Load("scene-dormant", platformGraph("scene-dormant"), bundle)
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := show.PromoteStreamRule("rule-1", platformGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}

	active, _ := show.Get("scene-active")
	rule, _ := show.Get("rule-1")
	dormant, _ := show.Get("scene-dormant")
	activeSub, _ := active.Subscribe(16)
	ruleSub, _ := rule.Subscribe(16)
	dormantSub, _ := dormant.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	if err := inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     twitchLeaf,
		Value:    json.RawMessage(`{"message":"hi","user":"zab"}`),
		Source:   "service:quasar",
	}); err != nil {
		t.Fatalf("write refused: %v", err)
	}

	// Both the active scene and the promoted rule got the write.
	awaitLeafDelta(t, activeSub, twitchLeaf)
	awaitLeafDelta(t, ruleSub, twitchLeaf)
	// The dormant roster scene got nothing (not in the union).
	expectNoLeafDelta(t, dormantSub, twitchLeaf)
}

// TestInbox_RuleReceivesWithoutActiveScene (criterion #2): with NO active
// scene, an inbox write still reaches a promoted rule — platform events
// between scenes are not lost for rules.
func TestInbox_RuleReceivesWithoutActiveScene(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}

	if err := show.PromoteStreamRule("rule-1", platformGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	rule, _ := show.Get("rule-1")
	ruleSub, _ := rule.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	if err := inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     twitchLeaf,
		Value:    json.RawMessage(`{"message":"between-scenes"}`),
		Source:   "service:quasar",
	}); err != nil {
		t.Fatalf("write refused: %v", err)
	}
	// No active scene, yet the rule received and emitted the delta.
	awaitLeafDelta(t, ruleSub, twitchLeaf)
}

// TestInbox_RulePlaneReceivesPlatformWithoutActiveScene proves the production
// Engine B seam: an authenticated platform write reaches the global plane even
// when the Show has neither an active scene nor a legacy promoted scene.
func TestInbox_RulePlaneReceivesPlatformWithoutActiveScene(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	inbox := NewInbox(show, quietLogger(), nil)
	sink := &recordingRulePlaneSink{}
	inbox.SetStreamRulePlatformSink(sink)
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	if err := inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     twitchLeaf,
		Value:    json.RawMessage(`{"message":"between-scenes","score":7}`),
		Source:   "service:quasar",
	}); err != nil {
		t.Fatalf("write refused: %v", err)
	}

	want := []recordedPlatformWrite{{
		leaf: twitchLeaf,
		payload: map[string]any{
			"message": "between-scenes",
			"score":   json.Number("7"),
		},
	}}
	if !reflect.DeepEqual(sink.writes, want) {
		t.Fatalf("RulePlane writes = %#v, want %#v", sink.writes, want)
	}
}
