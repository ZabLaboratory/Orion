package lsdpreception

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"
)

func TestNativePatchArraysEscapesAndAtomicFailure(t *testing.T) {
	before := map[string]any{"layout": map[string]any{"children": []any{map[string]any{"id": "a"}}}, "a/b~c": true}
	ops := []map[string]any{
		{"op": "test", "path": "/a~1b~0c", "value": true},
		{"op": "add", "path": "/layout/children/-", "value": map[string]any{"id": "b"}},
		{"op": "replace", "path": "/layout/children/0/id", "value": "first"},
		{"op": "remove", "path": "/layout/children/1"},
	}
	next, err := applyObjectOperations(before, ops)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, next) {
		t.Fatal("no structural mutation")
	}
	original, _ := json.Marshal(before)
	ops = append(ops, map[string]any{"op": "test", "path": "/a~1b~0c", "value": false})
	if _, err := applyObjectOperations(before, ops); err == nil {
		t.Fatal("failed test accepted")
	}
	after, _ := json.Marshal(before)
	if string(original) != string(after) {
		t.Fatal("failed batch modified original")
	}
}
func TestSceneMutationRejectsIdentityAndStaleLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewProducer(ctx, nil)
	defer p.Close()
	hub := NewHub(p, slog.Default())
	raw := []byte(`{"lsml":"1.1","scene_id":"scene","scene_version":"version","layout":{"kind":"frame","children":[]},"defaults":{}}`)
	m := hub.MirrorForLSML("scene", "version", "preview", raw).(*Mirror)
	if err := m.ApplyLSML([]map[string]any{{"op": "replace", "path": "/scene_id", "value": "other"}}); err == nil {
		t.Fatal("identity overwrite accepted")
	}
	hub.MirrorForLSML("scene", "version", "program", raw)
	if err := m.ApplyLSML([]map[string]any{{"op": "add", "path": "/defaults/title", "value": "late"}}); err == nil {
		t.Fatal("stale writer accepted")
	}
}

func TestNativeAnimationRequiresDeclaredAssetAndTarget(t *testing.T) {
	document := map[string]any{"layout": map[string]any{"kind": "frame", "id": "panel"}, "defaults": map[string]any{"__animation.play": map[string]any{"animation_id": "play"}}}
	if err := validateAnimationCommands(document); err == nil {
		t.Fatal("undeclared animation accepted")
	}
	document["animations"] = map[string]any{"play": map[string]any{"target": "missing"}}
	if err := validateAnimationCommands(document); err == nil {
		t.Fatal("missing target accepted")
	}
	document["animations"] = map[string]any{"play": map[string]any{"target": "panel"}}
	if err := validateAnimationCommands(document); err != nil {
		t.Fatal(err)
	}
}
