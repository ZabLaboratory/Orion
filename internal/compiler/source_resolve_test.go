package compiler

import (
	"encoding/json"
	"testing"
)

// TestResolveSourceReads_FoldsDescriptor proves the ADR 012 §1.2 compile
// resolution: a core.source.read@1 node whose source_id names a declared
// ExternalAdapter gets the introspection descriptor folded into its config
// under __resolved_source, with name/kind echoing the adapter and the
// descriptor projecting label/target_paths/frequency_hz/channel.
func TestResolveSourceReads_FoldsDescriptor(t *testing.T) {
	hz := 5.0
	nodes := []GraphNode{
		{
			ID:      "rd",
			Kind:    "computed",
			Compute: coreSourceRead,
			Config:  map[string]json.RawMessage{"source_id": json.RawMessage(`"leaguepedia_feed"`)},
		},
	}
	adapters := []ExternalAdapter{
		{
			Key:         "leaguepedia_feed",
			Label:       "Leaguepedia feed",
			Kind:        "http-poll",
			TargetPaths: []string{"__inputs.feed.leagues"},
			FrequencyHz: &hz,
		},
	}
	d := &Diagnostics{}
	resolveSourceReads(nodes, adapters, d)

	if d.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", d.Items)
	}
	raw, ok := nodes[0].Config[resolvedSourceConfigKey]
	if !ok {
		t.Fatalf("__resolved_source not folded into config: %+v", nodes[0].Config)
	}
	var got struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Descriptor struct {
			Label       string   `json:"label"`
			TargetPaths []string `json:"target_paths"`
			FrequencyHz *float64 `json:"frequency_hz"`
			Channel     *string  `json:"channel"`
		} `json:"descriptor"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("resolved source not valid JSON: %v (%s)", err, raw)
	}
	if got.Name != "leaguepedia_feed" {
		t.Errorf("name = %q, want leaguepedia_feed (= adapter Key)", got.Name)
	}
	if got.Kind != "http-poll" {
		t.Errorf("kind = %q, want http-poll", got.Kind)
	}
	if got.Descriptor.Label != "Leaguepedia feed" {
		t.Errorf("descriptor.label = %q", got.Descriptor.Label)
	}
	if len(got.Descriptor.TargetPaths) != 1 || got.Descriptor.TargetPaths[0] != "__inputs.feed.leagues" {
		t.Errorf("descriptor.target_paths = %v", got.Descriptor.TargetPaths)
	}
	if got.Descriptor.FrequencyHz == nil || *got.Descriptor.FrequencyHz != 5.0 {
		t.Errorf("descriptor.frequency_hz = %v, want 5", got.Descriptor.FrequencyHz)
	}
	if got.Descriptor.Channel != nil {
		t.Errorf("descriptor.channel = %v, want null", got.Descriptor.Channel)
	}
	// The authored source_id is preserved verbatim.
	if string(nodes[0].Config["source_id"]) != `"leaguepedia_feed"` {
		t.Errorf("source_id not preserved: %s", nodes[0].Config["source_id"])
	}
}

// TestResolveSourceReads_RejectsUndeclared proves the ADR 012 §1.4
// push-time structural reject: a source_id naming no declared adapter
// raises SOURCE_NOT_DECLARED (a compute has no error port — undeclared
// sources fail at POST /push, never on air).
func TestResolveSourceReads_RejectsUndeclared(t *testing.T) {
	cases := map[string]json.RawMessage{
		"unknown source_id": json.RawMessage(`"nope"`),
		"empty source_id":   json.RawMessage(`""`),
	}
	for name, sid := range cases {
		t.Run(name, func(t *testing.T) {
			nodes := []GraphNode{{
				ID:      "rd",
				Kind:    "computed",
				Compute: coreSourceRead,
				Config:  map[string]json.RawMessage{"source_id": sid},
			}}
			adapters := []ExternalAdapter{{Key: "declared", Kind: "http-poll"}}
			d := &Diagnostics{}
			resolveSourceReads(nodes, adapters, d)
			if !d.HasErrors() {
				t.Fatalf("expected SOURCE_NOT_DECLARED, got none")
			}
			found := false
			for _, it := range d.Items {
				if it.Code == ErrSourceNotDeclared && it.Path == "rd" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing SOURCE_NOT_DECLARED diagnostic: %+v", d.Items)
			}
			// No descriptor folded on the failing node.
			if _, ok := nodes[0].Config[resolvedSourceConfigKey]; ok {
				t.Errorf("descriptor folded despite reject")
			}
		})
	}
}

// TestResolveSourceReads_IgnoresOtherNodes proves the pass touches only
// source.read nodes (single-node-type blast radius).
func TestResolveSourceReads_IgnoresOtherNodes(t *testing.T) {
	nodes := []GraphNode{
		{ID: "add", Kind: "computed", Compute: "core.math.add@1"},
		{ID: "out", Kind: "output", Compute: "core.output@1"},
	}
	d := &Diagnostics{}
	resolveSourceReads(nodes, []ExternalAdapter{}, d)
	if d.HasErrors() {
		t.Fatalf("non-source.read nodes raised diagnostics: %+v", d.Items)
	}
	for _, n := range nodes {
		if _, ok := n.Config[resolvedSourceConfigKey]; ok {
			t.Errorf("node %s wrongly got __resolved_source", n.ID)
		}
	}
}
