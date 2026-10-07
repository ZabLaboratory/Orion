package streamcontrol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStreamLSMLIntentSurvivesRestartAndRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.lsml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRule("rule", "digest", []byte(`{"schema_version":"blue.program.v1"}`)); err != nil {
		t.Fatal(err)
	}
	on := true
	if err := s.SetApp("marker", &on, &on); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	intent := restarted.Snapshot()
	if len(intent.Rules) != 1 || !intent.Apps["marker"].Running {
		t.Fatalf("intent lost: %+v", intent)
	}
	if err := restarted.RemoveRule("rule"); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Rules) != 1 {
		t.Fatal("stores share mutable intent")
	}
	if err := os.WriteFile(path, []byte(`{"lsml":"1.1","scene_id":"orion-stream-control","scene_version":"tampered","defaults":{"stream_rules":{},"overlay_apps":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt LSML reset silently")
	}
}
