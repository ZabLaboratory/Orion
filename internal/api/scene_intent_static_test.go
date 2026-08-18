package api

import (
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

type staticSnapshotMirror struct {
	snapshot *protocol.Snapshot
}

func (m *staticSnapshotMirror) Forward(msg any) {
	if snapshot, ok := msg.(*protocol.Snapshot); ok {
		m.snapshot = snapshot
	}
}

func TestStartBridge_StaticSceneSeedsInitialSnapshot(t *testing.T) {
	mirror := &staticSnapshotMirror{}
	raw := []byte(`{"scene_id":"scene-1","layout":{"kind":"text"},"defaults":{"__lit.text.title":"Launch"}}`)
	compiled, defaults, err := compiler.CompileStaticLSML(raw, "scene-1", "sha256:scene", "https://zabgate.test/canvas/api/v1/scene-assets")
	if err != nil {
		t.Fatalf("CompileStaticLSML: %v", err)
	}
	if len(compiled) == 0 {
		t.Fatal("compiler returned empty RenderBundle")
	}

	deps := SceneIntentDeps{
		Host: bluehost.NewHost(),
		MirrorFor: func(_, _ string, _ bluehost.Slot, _ []byte) runtime.SceneMirror {
			return mirror
		},
		Bridges: bluewire.NewRegistry(),
	}
	claims := &attestation.Claims{SceneID: "scene-1", SceneDigest: "sha256:scene"}
	startBridge(deps, bluehost.SlotPreview, claims, "intent-1", false, raw, defaults)
	if mirror.snapshot == nil {
		t.Fatal("static scene did not emit its initial snapshot")
	}
	if mirror.snapshot.SceneVersion != "sha256:scene" {
		t.Fatalf("snapshot scene version = %q", mirror.snapshot.SceneVersion)
	}
	var title string
	if err := json.Unmarshal(mirror.snapshot.State["__lit.text.title"], &title); err != nil || title != "Launch" {
		t.Fatalf("snapshot title = %s, want Launch", mirror.snapshot.State["__lit.text.title"])
	}
}
