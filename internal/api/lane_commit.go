package api

import (
	"context"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"time"
)

func NativeLaneCommitter(hub *lsdpreception.Hub, host *bluehost.Host, bridges *bluewire.Registry, preview *runtime.PreviewSlot) func(context.Context, lsdpreception.LaneCommit) lsdpreception.LaneCommitReceipt {
	return func(parent context.Context, c lsdpreception.LaneCommit) lsdpreception.LaneCommitReceipt {
		refuse := func(reason string) lsdpreception.LaneCommitReceipt {
			return lsdpreception.LaneCommitReceipt{LaneCommit: c, Status: "refused", Reason: reason}
		}
		if !c.Valid() {
			return refuse("LANE_COMMIT_INVALID")
		}
		if receipt, ok := hub.ReplayCommit(c); ok {
			return receipt
		}
		ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
		defer cancel()
		releaseP, err := host.AcquireSceneCommit(ctx, bluehost.SlotOnAir)
		if err != nil {
			return refuse("LANE_COMMIT_BUSY")
		}
		defer releaseP()
		releaseV, err := host.AcquireSceneCommit(ctx, bluehost.SlotPreview)
		if err != nil {
			return refuse("LANE_COMMIT_BUSY")
		}
		defer releaseV()
		finishBridge := bridges.PauseRoleSwap()
		committed := false
		defer func() { finishBridge(committed) }()
		finishPreview, err := preview.StageRoleSwap(c.SceneID)
		if err != nil {
			return refuse(err.Error())
		}
		defer func() { finishPreview(committed) }()
		// A Hub replay must not swap the actors twice. Hub invokes its callback
		// only for a new validated physical permutation.
		receipt := hub.CommitRoles(c, func() error {
			if err := host.SwapAuthority(); err != nil {
				return err
			}
			committed = true
			return nil
		})
		return receipt
	}
}

func (c *SceneControl) commitLaneRoles(ctx context.Context, commit lsdpreception.LaneCommit, desired map[string]SceneSelection, observed map[string]any) (map[string]SceneSelection, bool) {
	receipt := lsdpreception.LaneCommitReceipt{LaneCommit: commit, Status: "refused", Reason: "LANE_COMMIT_UNAVAILABLE"}
	if c.opMu.TryLock() {
		defer c.opMu.Unlock()
		if c.deps.CommitLaneRoles != nil {
			receipt = c.deps.CommitLaneRoles(ctx, commit)
		}
	} else {
		receipt.Reason = "LANE_COMMIT_BUSY"
	}
	operations := []any{}
	if receipt.Status == "committed" && !receipt.Replayed {
		// Reconcile the logical selection, not a fresh admission. Both native
		// observed slots move together with the receipt in one transaction.
		desired = swapSelections(desired)
		for role, source := range receipt.Sources {
			stream := desired[role].StreamID
			if stream == "" {
				stream = "live"
			}
			desired[role] = SceneSelection{SceneID: source.SceneID, Version: source.ArtifactSet, StreamID: stream}
		}
		c.mu.Lock()
		c.desired = desired
		c.current = map[string]string{}
		for role, selection := range desired {
			c.current[role] = selectionKey(selection)
		}
		c.mu.Unlock()
		operations = append(operations, map[string]any{"op": "add", "path": "/scene_control/defaults/desired", "value": desired})
		for _, pair := range []struct{ dst, src string }{{"program", "preview"}, {"preview", "program"}} {
			value := observed[pair.src]
			if source, ok := receipt.Sources[pair.dst]; ok {
				value = map[string]any{"scene_id": source.SceneID, "scene_version": source.ArtifactSet, "source_version": source.SceneVersion, "status": "active", "take_command_id": commit.TakeCommandID}
			}
			operations = append(operations, map[string]any{"op": "add", "path": "/scene_control/defaults/observed/" + pair.dst, "value": value})
		}
	}
	operations = append(operations, map[string]any{"op": "add", "path": "/scene_control/defaults/observed/lane_commit", "value": receipt})
	ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.reception.WriteLeaves(ackCtx, "orion/state", operations); err != nil {
		c.log("lane commit observation unavailable", err)
	}
	if receipt.Status == "committed" {
		if err := c.persist(); err != nil {
			c.log("lane role selection persistence failed", err)
		}
		return desired, true
	}
	return desired, false
}
func swapSelections(desired map[string]SceneSelection) map[string]SceneSelection {
	result := map[string]SceneSelection{}
	if v, ok := desired["preview"]; ok {
		result["program"] = v
	}
	if p, ok := desired["program"]; ok {
		result["preview"] = p
	}
	return result
}
