package lsdpreception

// ProgramInspect captures the current identity once after output shutdown.
// There is no per-edit status publisher or additional transport.
type ProgramInspect struct {
	Schema            string        `json:"schema"`
	CommandID         string        `json:"command_id"`
	RuntimeInstanceID string        `json:"runtime_instance_id"`
	TakeCommandID     string        `json:"take_command_id"`
	ExpectedRoleMap   RoleMap       `json:"expected_role_map"`
	ExpectedRevisions LaneRevisions `json:"expected_revisions"`
}
type ProgramIdentityReceipt struct {
	ProgramInspect
	Status       string  `json:"status"`
	Reason       string  `json:"reason,omitempty"`
	Target       string  `json:"target,omitempty"`
	SceneID      string  `json:"scene_id,omitempty"`
	SceneVersion string  `json:"scene_version,omitempty"`
	ArtifactSet  string  `json:"artifact_set,omitempty"`
	WriterID     string  `json:"writer_id,omitempty"`
	EditSeq      *uint64 `json:"edit_seq,omitempty"`
}

func (h *Hub) InspectProgram(r ProgramInspect) ProgramIdentityReceipt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := ProgramIdentityReceipt{ProgramInspect: r, Status: "refused"}
	refuse := func(reason string) ProgramIdentityReceipt { out.Reason = reason; return out }
	if r.Schema != "orion.program-inspect.v1" || r.CommandID == "" || len(r.CommandID) > 256 {
		return refuse("PROGRAM_INSPECT_INVALID")
	}
	if !h.physical || h.lastCommit == nil {
		return refuse("PROGRAM_RELEASE_NO_COMMIT")
	}
	c := h.lastCommit
	if c.RuntimeInstanceID != r.RuntimeInstanceID {
		return refuse("PROGRAM_RELEASE_FOREIGN_RUNTIME")
	}
	if c.TakeCommandID != r.TakeCommandID || c.Revisions.Program != r.ExpectedRevisions.Program || c.Revisions.RoleMap != r.ExpectedRevisions.RoleMap || r.ExpectedRevisions.Preview < c.Revisions.Preview {
		return refuse("PROGRAM_RELEASE_STALE_EPOCH")
	}
	if r.ExpectedRoleMap != h.roles {
		return refuse("PROGRAM_RELEASE_ROLE_MISMATCH")
	}
	p := h.lanes["program"]
	if p == nil || p.active == nil {
		return refuse("PROGRAM_RELEASE_EMPTY")
	}
	if p.transitioning {
		return refuse("PROGRAM_RELEASE_BUSY")
	}
	out.Status = "active"
	out.Target = p.target
	out.SceneID = p.active.sceneID
	out.SceneVersion, _ = p.active.document["scene_version"].(string)
	out.ArtifactSet = p.active.version
	return out
}
