package bluehost

import (
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/streamcontrol"
	"path/filepath"
	"testing"
)

func TestRulePlaneRestoresCachedProgramAndDurablyDemotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream-control.lsml")
	store, err := streamcontrol.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	program := markerOverlayProgram(t)
	var identity struct {
		ProgramDigest string `json:"program_digest"`
	}
	if err = json.Unmarshal(program, &identity); err != nil {
		t.Fatal(err)
	}
	first := NewRulePlane(nil, nil, EffectDeps{}, 60, nil)
	first.SetStore(store)
	if err = first.Promote(markerRuleID, identity.ProgramDigest, program); err != nil {
		t.Fatal(err)
	}
	first.Stop()
	restored, err := streamcontrol.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mirror := &lockedOverlayMirror{}
	second := NewRulePlane(nil, nil, EffectDeps{OverlayMirror: mirror}, 60, nil)
	defer second.Stop()
	second.SetStore(restored)
	for id, rule := range restored.Snapshot().Rules {
		if err = second.Promote(id, rule.Digest, rule.Program); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = second.Call(markerRuleID, "marker_overlay_on", nil); err != nil {
		t.Fatal(err)
	}
	if len(mirror.snapshot()) != 1 {
		t.Fatal("restored published rule cannot execute")
	}
	if err = second.Demote(markerRuleID); err != nil {
		t.Fatal(err)
	}
	next, err := streamcontrol.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Snapshot().Rules) != 0 {
		t.Fatal("demotion lost at restart")
	}
}
