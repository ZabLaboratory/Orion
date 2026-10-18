package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"time"
)

func programReleaseWriterMatches(preview *runtime.PreviewSlot, r lsdpreception.ProgramRelease) (runtime.EditableHead, bool) {
	head, editable := preview.EditableTargetHead("program")
	return head, (editable && head.SceneID == r.SceneID && head.SceneVersion == r.SceneVersion && r.WriterID == head.WriterID && r.EditSeq != nil && *r.EditSeq == head.EditSeq) || (!editable && r.WriterID == "" && r.EditSeq == nil)
}

func NativeProgramReleaseChecker(hub *lsdpreception.Hub, preview *runtime.PreviewSlot) func(lsdpreception.ProgramRelease) lsdpreception.ProgramReleaseReceipt {
	return func(r lsdpreception.ProgramRelease) lsdpreception.ProgramReleaseReceipt {
		result := hub.CheckProgramRelease(r)
		if result.Status == "validated" {
			if _, ok := programReleaseWriterMatches(preview, r); !ok {
				result.Status = "refused"
				result.Reason = "PROGRAM_RELEASE_WRITER_MISMATCH"
			}
		}
		return result
	}
}

func NativeProgramReleaser(hub *lsdpreception.Hub, host *bluehost.Host, bridges *bluewire.Registry, preview *runtime.PreviewSlot) func(context.Context, lsdpreception.ProgramRelease) lsdpreception.ProgramReleaseReceipt {
	return func(parent context.Context, r lsdpreception.ProgramRelease) lsdpreception.ProgramReleaseReceipt {
		refuse := func(reason string) lsdpreception.ProgramReleaseReceipt {
			return lsdpreception.ProgramReleaseReceipt{ProgramRelease: r, Status: "refused", Reason: reason}
		}
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		release, err := host.AcquireSceneCommit(ctx, bluehost.SlotOnAir)
		if err != nil {
			return refuse("PROGRAM_RELEASE_BUSY")
		}
		defer release()
		checked := hub.CheckProgramRelease(r)
		if checked.Status == "released" || checked.Status == "failed" {
			return checked
		}
		if checked.Status == "refused" {
			return checked
		}
		cleanupReason := ""
		if !checked.Retired {
			head, matches := programReleaseWriterMatches(preview, r)
			if !matches {
				return refuse("PROGRAM_RELEASE_WRITER_MISMATCH")
			}
			finish, staged := hub.StageProgramRelease(r)
			if finish == nil {
				return staged
			}
			defer finish()
			host.SuspendProgramAdmissions()
			if bridges != nil {
				bridges.Stop(bluehost.SlotOnAir)
			}
			// Mirror publication is frozen before either owner can emit late work.
			if err = preview.ReleaseNativeProgram(head); err != nil {
				return refuse("PROGRAM_RELEASE_WRITER_MISMATCH")
			}
			providers.ResetActiveIngress(host)
			stopErr := host.Release(bluehost.SlotOnAir, "broadcast-stopped")
			checked = hub.RetireProgram(r)
			if !checked.Retired {
				return checked
			}
			if stopErr != nil {
				cleanupReason = "PROGRAM_RELEASE_OWNER_STOP_FAILED"
			}
		}
		// A retry after lost delivery only waits for the same queued Drop.
		if err = hub.Flush(ctx); err != nil {
			checked.Status = "failed"
			checked.Reason = "PROGRAM_RELEASE_NATIVE_UNCONFIRMED"
			return checked
		}
		result := hub.FinishProgramRelease(r, cleanupReason)
		result.Replayed = checked.Replayed
		return result
	}
}

func (c *SceneControl) releaseProgram(ctx context.Context, r lsdpreception.ProgramRelease, desired map[string]SceneSelection) (map[string]SceneSelection, bool) {
	result := lsdpreception.ProgramReleaseReceipt{ProgramRelease: r, Status: "refused", Reason: "PROGRAM_RELEASE_UNAVAILABLE"}
	if c.deps.CheckProgramRelease != nil && c.deps.ReleaseProgram != nil {
		result = c.deps.CheckProgramRelease(r)
		if result.Status != "refused" {
			if !result.Retired {
				c.mu.Lock()
				if cancel := c.inFlight["program"]; cancel != nil {
					cancel()
				}
				c.mu.Unlock()
			}
			// Cancel/join a staged admission's compensation before taking authority.
			for !c.opMu.TryLock() {
				select {
				case <-ctx.Done():
					result.Status = "refused"
					result.Reason = "PROGRAM_RELEASE_BUSY"
					goto observe
				case <-time.After(10 * time.Millisecond):
				}
			}
			defer c.opMu.Unlock()
			result = c.deps.ReleaseProgram(ctx, r)
		}
	}
observe:
	changed := result.Retired && !result.Replayed
	if result.Retired && c.deps.Host != nil {
		_, allowed := c.deps.Host.ProgramAdmissionToken()
		changed = changed || !allowed // repair observation after a lost release ACK
	}
	ack, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.reception.WriteLeavesFromState(ack, "orion/state", func(state any) ([]any, error) {
		ops := []any{}
		if changed {
			root, _ := state.(map[string]any)
			raw, _ := json.Marshal(root["scene_control"])
			var latest SceneControlDocument
			if json.Unmarshal(raw, &latest) != nil || validateSceneControl(latest) != nil {
				return nil, errors.New("SCENE_CONTROL_INVALID")
			}
			desired = latest.Defaults.Desired
			if _, exists := desired["program"]; exists {
				delete(desired, "program")
				ops = append(ops, map[string]any{"op": "remove", "path": "/scene_control/defaults/desired/program"})
			}
			ops = append(ops, map[string]any{"op": "add", "path": "/scene_control/defaults/observed/program", "value": map[string]any{"status": "inactive", "release_command_id": r.CommandID}})
		}
		observed := result
		if changed {
			observed.Status = "pending"
			observed.Reason = "PROGRAM_RELEASE_RECONCILING"
		}
		return append(ops, map[string]any{"op": "add", "path": "/scene_control/defaults/observed/release_program", "value": observed}), nil
	})
	if changed {
		c.mu.Lock()
		c.desired = desired
		delete(c.current, "program")
		c.mu.Unlock()
		if persistErr := c.persist(); persistErr != nil {
			result.Status = "failed"
			result.Reason = "PROGRAM_RELEASE_PERSIST_FAILED"
		}
		if err != nil {
			result.Status = "failed"
			result.Reason = "PROGRAM_RELEASE_CONTROL_UNCONFIRMED"
		}
		if finalErr := c.reception.WriteLeaf(ack, "orion/state", "/scene_control/defaults/observed/release_program", result); finalErr != nil {
			c.log("program release final receipt unavailable", finalErr)
		}
	}
	if err != nil {
		c.log("program release observation unavailable", err)
	}
	return desired, changed
}
