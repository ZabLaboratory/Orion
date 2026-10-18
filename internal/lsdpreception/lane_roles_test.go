package lsdpreception

import (
	"context"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"log/slog"
	"testing"
)

func roleFixture(t *testing.T) (*Hub, *Producer, *Mirror, *Mirror) {
	t.Helper()
	p := &Producer{ctx: context.Background(), queue: make(chan delivery, 64)}
	h := NewHub(p, slog.Default())
	h.EnablePhysicalLanes()
	source := func(id string) []byte {
		return []byte(`{"lsml":"1.2","scene_id":"` + id + `","scene_version":"source-v1","defaults":{"score":0},"layout":{"type":"text","text":{"$bind":"score"}}}`)
	}
	a := h.Lane("program").MirrorForLSML("scene-a", "artifact-a", source("scene-a")).(*Mirror)
	b := h.Lane("preview").MirrorForLSML("scene-b", "artifact-b", source("scene-b")).(*Mirror)
	h.Lane("program").SetActive("scene-a")
	h.Lane("preview").SetActive("scene-b")
	for len(p.queue) > 0 {
		<-p.queue
	}
	return h, p, a, b
}
func roleCommit() LaneCommit {
	return LaneCommit{Schema: "orion.lane-commit.v1", RuntimeInstanceID: "native-runtime", PrepareCommandID: "prepare", TakeCommandID: "take", IntentID: "intent", FrameID: 12, PtsNs: 1234, ServerSeq: 4, PreviousRoleMap: RoleMap{"A", "B"}, RoleMap: RoleMap{"B", "A"}, PreviousRevisions: LaneRevisions{0, 1, 0}, Revisions: LaneRevisions{1, 1, 1}, SceneID: "scene-b", SceneVersion: "source-v1"}
}
func TestPhysicalRoleCommitKeepsMirrorsDocumentsAndWritesNewPreviewToOutgoingLane(t *testing.T) {
	h, p, a, b := roleFixture(t)
	c := roleCommit()
	called := 0
	if r := h.CommitRoles(c, func() error { called++; return nil }); r.Status != "committed" {
		t.Fatal(r)
	}
	if len(p.queue) != 0 || h.Lane("program").active != b || h.Lane("preview").active != a || b.lane.target != "solar/lane-b" {
		t.Fatal("commit reassigned a document or replaced mirror")
	}
	if r := h.CommitRoles(c, func() error { called++; return nil }); r.Status != "committed" || !r.Replayed || called != 1 {
		t.Fatal("replay changed authority", r)
	}
	// The incoming new Preview cannot drop the retained Program writer.
	h.Lane("preview").Drop("scene-a")
	<-p.queue
	b.surface = func(string) bool { return true }
	b.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: "score", Value: json.RawMessage(`42`)}}})
	job := <-p.queue
	if job.target != "solar/lane-b" || job.document != nil {
		t.Fatal("retained Program crossed to Preview", job)
	}
	if b.document["defaults"].(map[string]any)["score"] != float64(42) {
		t.Fatal("Program delta lost")
	}
}
func TestPhysicalRoleCommitFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Hub, *LaneCommit)
		reason string
	}{
		{"invalid", func(_ *Hub, c *LaneCommit) { c.PtsNs = 0 }, "LANE_COMMIT_INVALID"},
		{"busy", func(h *Hub, _ *LaneCommit) { h.Lane("preview").transitioning = true }, "LANE_COMMIT_BUSY"},
		{"identity", func(_ *Hub, c *LaneCommit) { c.SceneVersion = "wrong" }, "LANE_COMMIT_PREVIEW_MISMATCH"},
		{"roles", func(_ *Hub, c *LaneCommit) { c.PreviousRoleMap = RoleMap{"B", "A"}; c.RoleMap = RoleMap{"A", "B"} }, "LANE_COMMIT_ROLE_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p, a, b := roleFixture(t)
			c := roleCommit()
			tc.mutate(h, &c)
			r := h.CommitRoles(c, nil)
			if r.Reason != tc.reason || len(p.queue) != 0 || h.Lane("program").active != a || h.Lane("preview").active != b {
				t.Fatal(r)
			}
		})
	}
	h, p, _, _ := roleFixture(t)
	c := roleCommit()
	h.CommitRoles(c, nil)
	c.SceneID = "conflict"
	if r := h.CommitRoles(c, nil); r.Reason != "LANE_COMMIT_CONFLICT" {
		t.Fatal(r)
	}
	c = roleCommit()
	c.TakeCommandID = "next"
	c.RuntimeInstanceID = "foreign"
	if r := h.CommitRoles(c, nil); r.Reason != "LANE_COMMIT_FOREIGN_RUNTIME" {
		t.Fatal(r)
	}
	c = roleCommit()
	c.TakeCommandID = "next"
	if r := h.CommitRoles(c, nil); r.Reason != "LANE_COMMIT_STALE" || len(p.queue) != 0 {
		t.Fatal(r)
	}
}
func TestPhysicalRoleCommitRetargetsCameraAuthorityByLogicalRole(t *testing.T) {
	h, p, _, b := roleFixture(t)
	h.Lane("program").EmitViewerPayload("program-viewer")
	h.Lane("preview").EmitViewerPayload("preview-viewer")
	for len(p.queue) > 0 {
		<-p.queue
	}
	h.CommitRoles(roleCommit(), nil)
	if b.document["defaults"].(map[string]any)["__cam.viewer"] != "program-viewer" {
		t.Fatal("Preview retained Program credentials")
	}
	for len(p.queue) > 0 {
		job := <-p.queue
		if job.document != nil {
			t.Fatal("camera authority reloaded document")
		}
	}
}
