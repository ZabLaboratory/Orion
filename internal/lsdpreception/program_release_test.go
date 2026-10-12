package lsdpreception

import (
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"testing"
)

func releaseFixture(t *testing.T) (*Hub, *Producer, *Mirror, *Mirror, ProgramRelease) {
	h, p, a, b := roleFixture(t)
	c := roleCommit()
	if got := h.CommitRoles(c, nil); got.Status != "committed" {
		t.Fatal(got)
	}
	return h, p, a, b, ProgramRelease{Schema: "orion.program-release.v1", CommandID: "release-1", StopCommandID: "stop-1", OutputState: "stopped", RuntimeInstanceID: c.RuntimeInstanceID, TakeCommandID: c.TakeCommandID, ExpectedRoleMap: c.RoleMap, ExpectedRevisions: c.Revisions, SceneID: "scene-b", SceneVersion: "source-v1", ArtifactSet: "artifact-b"}
}

func TestProgramReleasePreservesPreviewAndPhysicalRoles(t *testing.T) {
	h, p, a, b, r := releaseFixture(t)
	before, _ := json.Marshal(a.document)
	finish, receipt := h.StageProgramRelease(r)
	if finish == nil || receipt.Target != "solar/lane-b" {
		t.Fatal(receipt)
	}
	b.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: "score", Value: json.RawMessage(`9`)}}})
	if len(p.queue) != 0 {
		t.Fatal("publication crossed retirement fence")
	}
	retired := h.RetireProgram(r)
	finish()
	if !retired.Retired || retired.Status != "pending" {
		t.Fatal(retired)
	}
	job := <-p.queue
	raw, _ := json.Marshal(job.document)
	if job.target != "solar/lane-b" || string(raw) != "null" || len(p.queue) != 0 {
		t.Fatal("retirement targeted another resource", job)
	}
	b.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: "score", Value: json.RawMessage(`10`)}}})
	if err := b.ApplyLSML([]map[string]any{{"op": "add", "path": "/defaults/score", "value": 11}}); err == nil {
		t.Fatal("retired mirror accepted mutation")
	}
	after, _ := json.Marshal(a.document)
	if string(before) != string(after) || h.Lane("preview").active != a || h.Lane("program").active != nil || h.roles != r.ExpectedRoleMap || len(p.queue) != 0 {
		t.Fatal("Preview or roles changed")
	}
	if got := h.FinishProgramRelease(r, ""); got.Status != "released" {
		t.Fatal(got)
	}
	if finish, replay := h.StageProgramRelease(r); finish != nil || !replay.Replayed || replay.Status != "released" {
		t.Fatal(replay)
	}
	if len(p.queue) != 0 {
		t.Fatal("replay published another null")
	}
}

func TestProgramReleaseRejectsStaleAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*ProgramRelease)
	}{
		{"runtime", "PROGRAM_RELEASE_FOREIGN_RUNTIME", func(r *ProgramRelease) { r.RuntimeInstanceID = "other" }},
		{"epoch", "PROGRAM_RELEASE_STALE_EPOCH", func(r *ProgramRelease) { r.ExpectedRevisions.RoleMap++ }},
		{"program", "PROGRAM_RELEASE_STALE_EPOCH", func(r *ProgramRelease) { r.ExpectedRevisions.Program++ }},
		{"take", "PROGRAM_RELEASE_STALE_EPOCH", func(r *ProgramRelease) { r.TakeCommandID = "older" }},
		{"roles", "PROGRAM_RELEASE_ROLE_MISMATCH", func(r *ProgramRelease) { r.ExpectedRoleMap = RoleMap{"A", "B"} }},
		{"source", "PROGRAM_RELEASE_IDENTITY_MISMATCH", func(r *ProgramRelease) { r.SceneVersion = "other" }},
		{"artifact", "PROGRAM_RELEASE_IDENTITY_MISMATCH", func(r *ProgramRelease) { r.ArtifactSet = "other" }},
		{"not stopped", "PROGRAM_RELEASE_INVALID", func(r *ProgramRelease) { r.OutputState = "live" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p, a, b, r := releaseFixture(t)
			tc.change(&r)
			finish, got := h.StageProgramRelease(r)
			if finish != nil || got.Reason != tc.reason || h.Lane("program").active != b || h.Lane("preview").active != a || len(p.queue) != 0 {
				t.Fatal(got)
			}
		})
	}
	h, _, _, _, r := releaseFixture(t)
	finish, _ := h.StageProgramRelease(r)
	h.RetireProgram(r)
	finish()
	h.FinishProgramRelease(r, "")
	r.SceneVersion = "changed"
	if got := h.CheckProgramRelease(r); got.Reason != "PROGRAM_RELEASE_CONFLICT" {
		t.Fatal(got)
	}
}

func TestFirstPhysicalTakeAllowsEmptyProgramWithoutDocumentReload(t *testing.T) {
	h, p, _, b := roleFixture(t)
	h.Lane("program").Drop("scene-a")
	<-p.queue
	if got := h.CommitRoles(roleCommit(), nil); got.Status != "committed" {
		t.Fatal(got)
	}
	if h.Lane("program").active != b || h.Lane("preview").active != nil || len(p.queue) != 0 {
		t.Fatal("first start reloaded or duplicated Preview")
	}
}
