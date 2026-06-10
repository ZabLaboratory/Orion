package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Platform-event leaf binding tests (ADR 003 §3.3.2/§3.3.3, issue #84).
//
// The expected leaves below are HARDCODED literals, not rebuilt through
// the production helpers: the leaf is a cross-repo byte-contract
// (Blue's declared `signature.platform.leaf_path`, Quasar's
// `leaf_path(event)`), so the test must pin the exact bytes
// independently of how Orion concatenates them.

// platformEventLeaves pins the 14 canonical Twitch event types
// (Quasar models/canonical.py CANONICAL_EVENT_TYPES — the 14
// `quasar.twitch.*@1` manifest entries) to their expanded leaf for
// channel `zabchannel`.
var platformEventLeaves = map[string]string{
	"quasar.twitch.chat@1":                     "__inputs.platform.twitch.zabchannel.last_chat",
	"quasar.twitch.subscription@1":             "__inputs.platform.twitch.zabchannel.last_subscription",
	"quasar.twitch.subscription_gift@1":        "__inputs.platform.twitch.zabchannel.last_subscription_gift",
	"quasar.twitch.cheer@1":                    "__inputs.platform.twitch.zabchannel.last_cheer",
	"quasar.twitch.follow@1":                   "__inputs.platform.twitch.zabchannel.last_follow",
	"quasar.twitch.raid@1":                     "__inputs.platform.twitch.zabchannel.last_raid",
	"quasar.twitch.stream_online@1":            "__inputs.platform.twitch.zabchannel.last_stream_online",
	"quasar.twitch.stream_offline@1":           "__inputs.platform.twitch.zabchannel.last_stream_offline",
	"quasar.twitch.channel_point_redemption@1": "__inputs.platform.twitch.zabchannel.last_channel_point_redemption",
	"quasar.twitch.poll_begin@1":               "__inputs.platform.twitch.zabchannel.last_poll_begin",
	"quasar.twitch.poll_end@1":                 "__inputs.platform.twitch.zabchannel.last_poll_end",
	"quasar.twitch.prediction_begin@1":         "__inputs.platform.twitch.zabchannel.last_prediction_begin",
	"quasar.twitch.prediction_lock@1":          "__inputs.platform.twitch.zabchannel.last_prediction_lock",
	"quasar.twitch.prediction_end@1":           "__inputs.platform.twitch.zabchannel.last_prediction_end",
}

// platformManifest is pureManifest plus the 14 quasar.twitch entries
// (Blue declares them is_pure / is_bounded — twitch.py).
func platformManifest() ComputeManifest {
	m := pureManifest()
	for compute := range platformEventLeaves {
		m[compute] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	}
	return m
}

// platformNode builds an authored quasar node with a config.channel.
func platformNode(id, compute, channel string) BlueprintNode {
	return BlueprintNode{
		ID:      id,
		Compute: compute,
		Config:  map[string]json.RawMessage{"channel": json.RawMessage(`"` + channel + `"`)},
	}
}

func compilePlatformBlueprint(t *testing.T, bp *BlueprintGraph) (*Graph, error) {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{bp.ID: bp},
		manifest:   platformManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: bp.ID}, f)
	return g, err
}

// TestCompile_PlatformLeaf_All14Events: every canonical event node
// expands to its byte-exact leaf (criterion 8 + criterion 9, Orion
// half), compiles as Kind "input", and carries exactly one
// `platform-stream` binding per distinct leaf in graph.Bindings.
func TestCompile_PlatformLeaf_All14Events(t *testing.T) {
	bp := &BlueprintGraph{ID: "bp-1"}
	i := 0
	for compute := range platformEventLeaves {
		bp.Nodes = append(bp.Nodes, platformNode("n"+string(rune('a'+i)), compute, "zabchannel"))
		i++
	}

	g, err := compilePlatformBlueprint(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	byCompute := map[string]GraphNode{}
	for _, n := range g.Nodes {
		byCompute[n.Compute] = n
	}
	for compute, wantLeaf := range platformEventLeaves {
		n, ok := byCompute[compute]
		if !ok {
			t.Fatalf("node for %s missing from compiled graph", compute)
		}
		if n.Kind != "input" {
			t.Errorf("%s: kind = %q, want \"input\"", compute, n.Kind)
		}
		if n.Path != wantLeaf {
			t.Errorf("%s: path = %q, want byte-exact %q", compute, n.Path, wantLeaf)
		}
	}

	// One platform-stream binding per distinct leaf, carrying it.
	got := map[string]bool{}
	for _, b := range g.Bindings {
		if b.Kind != "platform-stream" {
			continue
		}
		if len(b.TargetPaths) != 1 {
			t.Fatalf("platform-stream binding must target exactly one leaf, got %v", b.TargetPaths)
		}
		// A pure acceptance binding: none of the goroutine-bearing
		// adapter fields may be set (no poller/listener must ever
		// engage on it).
		if b.URL != "" || b.FrequencyHz != nil || b.Channel != "" {
			t.Fatalf("platform-stream binding %q carries adapter-goroutine fields: %+v", b.Key, b)
		}
		got[b.TargetPaths[0]] = true
	}
	if len(got) != len(platformEventLeaves) {
		t.Fatalf("expected %d platform-stream bindings, got %d", len(platformEventLeaves), len(got))
	}
	for _, wantLeaf := range platformEventLeaves {
		if !got[wantLeaf] {
			t.Errorf("no platform-stream binding targets %q", wantLeaf)
		}
	}
}

// TestCompile_PlatformChannelCasefold is ADR 003 criterion 8 verbatim:
// `quasar.twitch.chat@1` with config.channel="ZabChannel" compiles to
// the casefolded leaf — Go's ToLower on the regex-guaranteed ASCII set
// is byte-identical to Quasar's Python casefold.
func TestCompile_PlatformChannelCasefold(t *testing.T) {
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{
		platformNode("chat", "quasar.twitch.chat@1", "ZabChannel"),
	}}
	g, err := compilePlatformBlueprint(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	const want = "__inputs.platform.twitch.zabchannel.last_chat"
	if g.Nodes[0].Path != want {
		t.Fatalf("path = %q, want %q", g.Nodes[0].Path, want)
	}
	if len(g.Bindings) != 1 || g.Bindings[0].Kind != "platform-stream" ||
		len(g.Bindings[0].TargetPaths) != 1 || g.Bindings[0].TargetPaths[0] != want {
		t.Fatalf("expected one platform-stream binding targeting %q, got %+v", want, g.Bindings)
	}
}

// TestCompile_PlatformChannelDiagnostics: structural validation of the
// authored channel — casefold precedes validation, so an
// invalid-charset channel is REJECTED (never rewritten); a missing or
// empty channel cannot expand at all.
func TestCompile_PlatformChannelDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		node BlueprintNode
		code DiagnosticCode
	}{
		{"absent config.channel", BlueprintNode{ID: "n1", Compute: "quasar.twitch.chat@1"}, ErrPlatformChannelMissing},
		{"empty channel", platformNode("n1", "quasar.twitch.chat@1", ""), ErrPlatformChannelMissing},
		{"hyphen rejected not rewritten", platformNode("n1", "quasar.twitch.chat@1", "Zab-Channel"), ErrPlatformChannelInvalid},
		{"dot rejected (path injection)", platformNode("n1", "quasar.twitch.chat@1", "zab.channel.last_raid"), ErrPlatformChannelInvalid},
		{"non-ascii rejected", platformNode("n1", "quasar.twitch.chat@1", "zäb"), ErrPlatformChannelInvalid},
		{"non-string channel", BlueprintNode{ID: "n1", Compute: "quasar.twitch.chat@1",
			Config: map[string]json.RawMessage{"channel": json.RawMessage(`42`)}}, ErrPlatformChannelInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{tc.node}}
			_, err := compilePlatformBlueprint(t, bp)
			if err == nil {
				t.Fatalf("expected %s, compile succeeded", tc.code)
			}
			var ce *CompileError
			if !errors.As(err, &ce) || !ce.HasCode(tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
		})
	}
}

// TestCompile_PlatformLeafNotKeyPrefixed: in a multi-blueprint scene
// the platform leaf stays the GLOBAL Quasar-written address — the
// blueprint key prefixes node ids and ordinary leaves, never
// `__inputs.platform.*` (the cross-repo byte-contract).
func TestCompile_PlatformLeafNotKeyPrefixed(t *testing.T) {
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{
		platformNode("chat", "quasar.twitch.chat@1", "zabchannel"),
	}}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   platformManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{{Key: "overlay", ID: "bp-1"}}}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	const want = "__inputs.platform.twitch.zabchannel.last_chat"
	if g.Nodes[0].Path != want {
		t.Fatalf("keyed blueprint rewrote the platform leaf: %q, want %q", g.Nodes[0].Path, want)
	}
	if g.Nodes[0].ID != "overlay.chat" {
		t.Fatalf("node id should still take the key prefix, got %q", g.Nodes[0].ID)
	}
}

// TestCompile_PlatformBindingDeduped: two nodes on the SAME leaf
// (same event, same channel) synthesize exactly one binding.
func TestCompile_PlatformBindingDeduped(t *testing.T) {
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{
		platformNode("chat1", "quasar.twitch.chat@1", "zabchannel"),
		platformNode("chat2", "quasar.twitch.chat@1", "ZABCHANNEL"), // folds to the same leaf
	}}
	g, err := compilePlatformBlueprint(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	if len(g.Bindings) != 1 {
		t.Fatalf("expected 1 deduped platform-stream binding, got %d: %+v", len(g.Bindings), g.Bindings)
	}
}
