package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// on-platform-event compiler mapping & acceptance (ADR 013 §6, criteria
// #1 and #2). The arming twin of on-event: a `core.event.on-platform-event@1`
// node compiles to an ExecEntry{Kind:"on-platform-event", Event:<leaf>}
// where the leaf is the canonical __inputs.platform.* path expanded from
// config platform/channel/event_type, AND surfaces as a platform-stream
// acceptance binding covering that leaf (reusing platformStreamBindings, NOT
// the __events. event-topic path). Channel invalid → PLATFORM_CHANNEL_INVALID.

func onPlatformManifest() ComputeManifest {
	m := execManifest()
	m["core.event.on-platform-event@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	return m
}

// onPlatformBlueprint: on-platform-event(platform/channel/event_type) →
// variable.set(fired). One arming entry observing the platform leaf.
func onPlatformBlueprint(platform, channel, eventType string) *BlueprintGraph {
	cfg := func(s string) json.RawMessage { return json.RawMessage(`"` + s + `"`) }
	return &BlueprintGraph{
		ID: "bp-plat",
		Nodes: []BlueprintNode{
			{ID: "onplat", Compute: "core.event.on-platform-event@1",
				Config: map[string]json.RawMessage{
					"platform":   cfg(platform),
					"channel":    cfg(channel),
					"event_type": cfg(eventType),
				},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "val", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`1`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"fired"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "onplat", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "val", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
}

func compileOnPlatform(t *testing.T, platform, channel, eventType string) (*Graph, error) {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": onPlatformBlueprint(platform, channel, eventType)},
		manifest:   onPlatformManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-plat",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	return g, err
}

// TestCompile_OnPlatformEvent_MapsEntryAndBinding (criteria #1, #2): the
// entry compiles to Kind "on-platform-event" with Event set to the canonical
// leaf (channel casefolded), and a platform-stream binding covers that exact
// leaf — NOT an __events. event-topic binding.
func TestCompile_OnPlatformEvent_MapsEntryAndBinding(t *testing.T) {
	g, err := compileOnPlatform(t, "twitch", "ZabChannel", "chat")
	if err != nil {
		t.Fatalf("on-platform-event scene rejected: %v", err)
	}
	wantLeaf := "__inputs.platform.twitch.zabchannel.last_chat"

	// The compiled exec program carries the mapped entry.
	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
	}
	var p execProgram
	if err := json.Unmarshal(g.ExecPrograms[0], &p); err != nil {
		t.Fatalf("decode program: %v", err)
	}
	var entry *execEntry
	for k := range p.Entrypoints {
		e := p.Entrypoints[k]
		if e.Kind == "on-platform-event" {
			entry = &e
			break
		}
	}
	if entry == nil {
		t.Fatalf("no on-platform-event entry in %+v", p.Entrypoints)
	}
	if entry.Event != wantLeaf {
		t.Fatalf("entry.Event = %q, want %q (canonical leaf, channel casefolded)", entry.Event, wantLeaf)
	}

	// A platform-stream binding covers the leaf; NO event-topic binding.
	var platBinding *ExternalAdapter
	for i := range g.Bindings {
		switch g.Bindings[i].Kind {
		case "platform-stream":
			if len(g.Bindings[i].TargetPaths) == 1 && g.Bindings[i].TargetPaths[0] == wantLeaf {
				platBinding = &g.Bindings[i]
			}
		case "event-topic":
			t.Fatalf("on-platform-event produced an event-topic binding (%+v) — must use platform-stream", g.Bindings[i])
		}
	}
	if platBinding == nil {
		t.Fatalf("no platform-stream binding covering %q; bindings = %+v", wantLeaf, g.Bindings)
	}
	// Pure acceptance — no goroutine-bearing fields.
	if platBinding.URL != "" || platBinding.FrequencyHz != nil {
		t.Fatalf("platform-stream binding carries adapter fields — must be pure acceptance: %+v", platBinding)
	}
}

// TestCompile_OnPlatformEvent_InvalidChannelRejected (criterion #1): an
// invalid channel handle is a structural authoring reject
// (PLATFORM_CHANNEL_INVALID), reusing the quasar.* channel discipline.
func TestCompile_OnPlatformEvent_InvalidChannelRejected(t *testing.T) {
	_, err := compileOnPlatform(t, "twitch", "Zab-Channel", "chat")
	if err == nil {
		t.Fatal("hyphenated channel must be rejected, not rewritten")
	}
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CompileError, got %T: %v", err, err)
	}
	found := false
	for _, d := range ce.Diagnostics.Items {
		if d.Code == ErrPlatformChannelInvalid {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PLATFORM_CHANNEL_INVALID, got %+v", ce.Diagnostics.Items)
	}
}

// TestCompile_OnPlatformEvent_DeterministicLeaf (criterion #1): two channels
// folding to the same handle expand to the same leaf (casefold-then-validate).
func TestCompile_OnPlatformEvent_DeterministicLeaf(t *testing.T) {
	g1, err1 := compileOnPlatform(t, "twitch", "ZabChannel", "follow")
	g2, err2 := compileOnPlatform(t, "twitch", "ZABCHANNEL", "follow")
	if err1 != nil || err2 != nil {
		t.Fatalf("compile errors: %v / %v", err1, err2)
	}
	leafOf := func(g *Graph) string {
		var p execProgram
		_ = json.Unmarshal(g.ExecPrograms[0], &p)
		for k := range p.Entrypoints {
			if e := p.Entrypoints[k]; e.Kind == "on-platform-event" {
				return e.Event
			}
		}
		return ""
	}
	if leafOf(g1) != leafOf(g2) || leafOf(g1) != "__inputs.platform.twitch.zabchannel.last_follow" {
		t.Fatalf("channel casefold not deterministic: %q vs %q", leafOf(g1), leafOf(g2))
	}
}
