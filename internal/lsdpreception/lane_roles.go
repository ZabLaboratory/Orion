package lsdpreception

import (
	"encoding/json"
	"strings"
)

type RoleMap struct {
	OnAir   string `json:"on_air"`
	Preview string `json:"preview"`
}
type LaneRevisions struct {
	Program uint64 `json:"program"`
	Preview uint64 `json:"preview"`
	RoleMap uint64 `json:"role_map"`
}
type LaneCommit struct {
	Schema            string        `json:"schema"`
	RuntimeInstanceID string        `json:"runtime_instance_id"`
	PrepareCommandID  string        `json:"prepare_command_id"`
	TakeCommandID     string        `json:"take_command_id"`
	IntentID          string        `json:"intent_id"`
	FrameID           uint64        `json:"frame_id"`
	PtsNs             uint64        `json:"pts_ns"`
	ServerSeq         uint64        `json:"server_seq"`
	PreviousRoleMap   RoleMap       `json:"previous_role_map"`
	RoleMap           RoleMap       `json:"role_map"`
	PreviousRevisions LaneRevisions `json:"previous_revisions"`
	Revisions         LaneRevisions `json:"revisions"`
	SceneID           string        `json:"scene_id"`
	SceneVersion      string        `json:"scene_version"`
}
type LaneCommitReceipt struct {
	LaneCommit
	Status   string                `json:"status"`
	Reason   string                `json:"reason,omitempty"`
	Targets  map[string]string     `json:"targets,omitempty"`
	Replayed bool                  `json:"replayed,omitempty"`
	Sources  map[string]LaneSource `json:"sources,omitempty"`
}

type LaneSource struct {
	SceneID      string `json:"scene_id"`
	SceneVersion string `json:"scene_version"`
	ArtifactSet  string `json:"artifact_set"`
}

func (c LaneCommit) Valid() bool {
	for _, s := range []string{c.RuntimeInstanceID, c.PrepareCommandID, c.TakeCommandID, c.IntentID, c.SceneID, c.SceneVersion} {
		if s == "" || len(s) > 256 {
			return false
		}
	}
	validMap := func(r RoleMap) bool {
		return r.OnAir != r.Preview && (r.OnAir == "A" || r.OnAir == "B") && (r.Preview == "A" || r.Preview == "B")
	}
	return c.Schema == "orion.lane-commit.v1" && c.FrameID > 0 && c.PtsNs > 0 && c.ServerSeq > 0 && validMap(c.RoleMap) && validMap(c.PreviousRoleMap) &&
		c.RoleMap.OnAir == c.PreviousRoleMap.Preview && c.RoleMap.Preview == c.PreviousRoleMap.OnAir &&
		c.Revisions.RoleMap == c.PreviousRevisions.RoleMap+1 && c.Revisions.Program == c.PreviousRevisions.Program+1 && c.Revisions.Preview == c.PreviousRevisions.Preview
}

// EnablePhysicalLanes is a boot-only operation, before any Lane is acquired.
func (h *Hub) EnablePhysicalLanes() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lanes) != 0 {
		panic("physical lanes must be enabled before acquisition")
	}
	h.physical = true
	h.roles = RoleMap{OnAir: "A", Preview: "B"}
}
func physicalTarget(id string) string {
	if id == "A" {
		return "solar/lane-a"
	}
	return "solar/lane-b"
}
func (l *Lane) Target() string { l.hub.mu.Lock(); defer l.hub.mu.Unlock(); return l.target }

func (h *Hub) ReplayCommit(c LaneCommit) (LaneCommitReceipt, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.replayCommit(c)
}
func (h *Hub) replayCommit(c LaneCommit) (LaneCommitReceipt, bool) {
	cached, ok := h.commitCache[c.RuntimeInstanceID+"\x00"+c.TakeCommandID]
	if !ok {
		return LaneCommitReceipt{}, false
	}
	b1, _ := json.Marshal(c)
	b2, _ := json.Marshal(cached.LaneCommit)
	if string(b1) != string(b2) {
		return LaneCommitReceipt{LaneCommit: c, Status: "refused", Reason: "LANE_COMMIT_CONFLICT"}, true
	}
	r := cached
	r.Replayed = true
	return r, true
}

// CommitRoles changes authority only. The same Mirror objects and documents stay
// on the same physical resource. before commits paused host actors and must not
// publish LSDP or acquire a Lane lock. A replay never invokes it again.
func (h *Hub) CommitRoles(c LaneCommit, before func() error) LaneCommitReceipt {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := LaneCommitReceipt{LaneCommit: c, Status: "refused"}
	refuse := func(reason string) LaneCommitReceipt { r.Reason = reason; return r }
	if !h.physical || !c.Valid() {
		return refuse("LANE_COMMIT_INVALID")
	}
	if replay, ok := h.replayCommit(c); ok {
		return replay
	}
	if h.lastCommit != nil {
		if c.RuntimeInstanceID != h.lastCommit.RuntimeInstanceID {
			return refuse("LANE_COMMIT_FOREIGN_RUNTIME")
		}
		if c.ServerSeq <= h.lastCommit.ServerSeq || c.FrameID <= h.lastCommit.FrameID || c.PtsNs <= h.lastCommit.PtsNs || c.PreviousRevisions.RoleMap != h.lastCommit.Revisions.RoleMap || c.PreviousRevisions.Program != h.lastCommit.Revisions.Program || c.PreviousRevisions.Preview < h.lastCommit.Revisions.Preview {
			return refuse("LANE_COMMIT_STALE")
		}
	}
	if c.PreviousRoleMap != h.roles {
		return refuse("LANE_COMMIT_ROLE_MISMATCH")
	}
	p, v := h.lanes["program"], h.lanes["preview"]
	if p == nil || v == nil || p.transitioning || v.transitioning {
		return refuse("LANE_COMMIT_BUSY")
	}
	if v.active == nil || v.active.sceneID != c.SceneID || v.active.document["scene_version"] != c.SceneVersion {
		return refuse("LANE_COMMIT_PREVIEW_MISMATCH")
	}
	if before != nil {
		if err := before(); err != nil {
			return refuse(err.Error())
		}
	}
	p.target, v.target = v.target, p.target
	p.active, v.active = v.active, p.active
	p.scenes, v.scenes = v.scenes, p.scenes
	for _, l := range []*Lane{p, v} {
		for _, m := range l.scenes {
			m.lane = l
		}
		// Camera/viewer authority follows the logical role. Absolute leaves
		// update the retained document; no scene assignment or source acquisition.
		if l.active != nil {
			ops := []map[string]any{}
			defaults := l.active.document["defaults"].(map[string]any)
			for key := range defaults {
				if strings.HasPrefix(key, "__cam.") {
					if _, ok := l.reserved[key]; !ok {
						defaults[key] = nil
						ops = append(ops, map[string]any{"op": "add", "path": "/defaults/" + pointer(key), "value": nil})
					}
				}
			}
			for key, value := range l.reserved {
				l.active.document["defaults"].(map[string]any)[key] = value
				ops = append(ops, map[string]any{"op": "add", "path": "/defaults/" + pointer(key), "value": value})
			}
			if len(ops) > 0 {
				h.producer.Apply(l.target, ops)
			}
		}
	}
	h.roles = c.RoleMap
	r.Status = "committed"
	r.Targets = map[string]string{"program": p.target, "preview": v.target}
	r.Sources = map[string]LaneSource{}
	for _, l := range []*Lane{p, v} {
		if l.active != nil {
			r.Sources[l.role] = LaneSource{SceneID: l.active.sceneID, SceneVersion: l.active.document["scene_version"].(string), ArtifactSet: l.active.version}
		}
	}
	h.lastCommit = &r
	if h.commitCache == nil {
		h.commitCache = map[string]LaneCommitReceipt{}
	}
	key := c.RuntimeInstanceID + "\x00" + c.TakeCommandID
	h.commitCache[key] = r
	h.commitOrder = append(h.commitOrder, key)
	if len(h.commitOrder) > 64 {
		delete(h.commitCache, h.commitOrder[0])
		h.commitOrder = h.commitOrder[1:]
	}
	return r
}

func isSolarSceneTarget(target string) bool {
	return target == "solar/program" || target == "solar/preview" || target == "solar/lane-a" || target == "solar/lane-b"
}
