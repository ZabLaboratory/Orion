package lsdpreception

import (
	"context"
	"encoding/json"
)

func (h *Hub) Flush(ctx context.Context) error { return h.producer.Flush(ctx) }

// ProgramRelease is an explicit local stop receipt, not a scene selection.
// The host application owns output confirmation; Orion owns scene retirement.
type ProgramRelease struct {
	Schema            string        `json:"schema"`
	CommandID         string        `json:"command_id"`
	Attempt           uint64        `json:"attempt,omitempty"`
	StopCommandID     string        `json:"stop_command_id"`
	OutputState       string        `json:"output_state"`
	RuntimeInstanceID string        `json:"runtime_instance_id"`
	TakeCommandID     string        `json:"take_command_id"`
	ExpectedRoleMap   RoleMap       `json:"expected_role_map"`
	ExpectedRevisions LaneRevisions `json:"expected_revisions"`
	SceneID           string        `json:"scene_id"`
	SceneVersion      string        `json:"scene_version"`
	ArtifactSet       string        `json:"artifact_set"`
	WriterID          string        `json:"writer_id,omitempty"`
	EditSeq           *uint64       `json:"edit_seq,omitempty"`
}

type ProgramReleaseReceipt struct {
	ProgramRelease
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Target   string `json:"target,omitempty"`
	Retired  bool   `json:"retired"`
	Replayed bool   `json:"replayed,omitempty"`
}

func (r ProgramRelease) Valid() bool {
	for _, s := range []string{r.CommandID, r.StopCommandID, r.RuntimeInstanceID, r.TakeCommandID, r.SceneID, r.SceneVersion, r.ArtifactSet} {
		if s == "" || len(s) > 256 {
			return false
		}
	}
	return r.Schema == "orion.program-release.v1" && r.OutputState == "stopped" && len(r.WriterID) <= 256 &&
		((r.WriterID == "" && r.EditSeq == nil) || (r.WriterID != "" && r.EditSeq != nil))
}

func releaseKey(r ProgramRelease) string { return r.RuntimeInstanceID + "\x00" + r.CommandID }
func sameRelease(a, b ProgramRelease) bool {
	a.Attempt = 0
	b.Attempt = 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// CheckProgramRelease does not change authority, including when a pending
// presentation must first be cancelled and compensated by SceneControl.
func (h *Hub) CheckProgramRelease(r ProgramRelease) ProgramReleaseReceipt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checkProgramRelease(r, false)
}
func (h *Hub) checkProgramRelease(r ProgramRelease, requireIdle bool) ProgramReleaseReceipt {
	out := ProgramReleaseReceipt{ProgramRelease: r, Status: "refused"}
	refuse := func(reason string) ProgramReleaseReceipt { out.Reason = reason; return out }
	if !h.physical || !r.Valid() {
		return refuse("PROGRAM_RELEASE_INVALID")
	}
	if cached, ok := h.releases[releaseKey(r)]; ok {
		if !sameRelease(cached.ProgramRelease, r) {
			return refuse("PROGRAM_RELEASE_CONFLICT")
		}
		cached.Replayed = true
		cached.Attempt = r.Attempt
		return cached
	}
	if h.lastCommit == nil {
		return refuse("PROGRAM_RELEASE_NO_COMMIT")
	}
	c := h.lastCommit
	if c.RuntimeInstanceID != r.RuntimeInstanceID {
		return refuse("PROGRAM_RELEASE_FOREIGN_RUNTIME")
	}
	if c.TakeCommandID != r.TakeCommandID || c.Revisions.Program != r.ExpectedRevisions.Program || c.Revisions.RoleMap != r.ExpectedRevisions.RoleMap {
		return refuse("PROGRAM_RELEASE_STALE_EPOCH")
	}
	if r.ExpectedRevisions.Preview < c.Revisions.Preview {
		return refuse("PROGRAM_RELEASE_STALE_EPOCH")
	}
	if r.ExpectedRoleMap != h.roles {
		return refuse("PROGRAM_RELEASE_ROLE_MISMATCH")
	}
	p := h.lanes["program"]
	if p == nil || p.active == nil {
		return refuse("PROGRAM_RELEASE_EMPTY")
	}
	if p.active.sceneID != r.SceneID || p.active.document["scene_version"] != r.SceneVersion || p.active.version != r.ArtifactSet {
		return refuse("PROGRAM_RELEASE_IDENTITY_MISMATCH")
	}
	if requireIdle && p.transitioning {
		return refuse("PROGRAM_RELEASE_BUSY")
	}
	out.Status = "validated"
	out.Target = p.target
	return out
}

// StageProgramRelease fences retained mirrors while their owners are stopped.
// Caller holds the Program Host lease. No owner is stopped under the Hub lock.
func (h *Hub) StageProgramRelease(r ProgramRelease) (func(), ProgramReleaseReceipt) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.checkProgramRelease(r, true)
	if out.Status != "validated" {
		return nil, out
	}
	p := h.lanes["program"]
	p.transitioning = true
	return func() { h.mu.Lock(); defer h.mu.Unlock(); p.transitioning = false }, out
}

// RetireProgram drops every Program mirror, including in-flight candidate
// mirrors, but retains the Lane object and its physical resource identity.
func (h *Hub) RetireProgram(r ProgramRelease) ProgramReleaseReceipt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.checkProgramRelease(r, false)
	if out.Status != "validated" {
		return out
	}
	p := h.lanes["program"]
	p.dropLocked(r.SceneID)
	p.scenes = map[string]*Mirror{}
	out.Status = "pending"
	out.Retired = true
	if h.releases == nil {
		h.releases = map[string]ProgramReleaseReceipt{}
	}
	h.releases[releaseKey(r)] = out
	h.releaseOrder = append(h.releaseOrder, releaseKey(r))
	if len(h.releaseOrder) > 64 {
		delete(h.releases, h.releaseOrder[0])
		h.releaseOrder = h.releaseOrder[1:]
	}
	return out
}

// FinishProgramRelease is called only after the native application barrier.
func (h *Hub) FinishProgramRelease(r ProgramRelease, reason string) ProgramReleaseReceipt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out, ok := h.releases[releaseKey(r)]
	if !ok || !sameRelease(out.ProgramRelease, r) {
		return ProgramReleaseReceipt{ProgramRelease: r, Status: "refused", Reason: "PROGRAM_RELEASE_CONFLICT"}
	}
	out.Attempt = r.Attempt
	if reason != "" {
		out.Status = "failed"
		out.Reason = reason
	} else {
		out.Status = "released"
		out.Reason = ""
	}
	h.releases[releaseKey(r)] = out
	return out
}
