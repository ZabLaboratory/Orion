package api

import (
	"context"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"time"
)

func NativeProgramInspector(hub *lsdpreception.Hub, host *bluehost.Host, preview *runtime.PreviewSlot) func(context.Context, lsdpreception.ProgramInspect) lsdpreception.ProgramIdentityReceipt {
	return func(parent context.Context, r lsdpreception.ProgramInspect) lsdpreception.ProgramIdentityReceipt {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		release, err := host.AcquireSceneCommit(ctx, bluehost.SlotOnAir)
		if err != nil {
			return lsdpreception.ProgramIdentityReceipt{ProgramInspect: r, Status: "refused", Reason: "PROGRAM_RELEASE_BUSY"}
		}
		defer release()
		result := hub.InspectProgram(r)
		if result.Status != "active" {
			return result
		}
		if head, editable := preview.EditableTargetHead("program"); editable {
			current, _, err := preview.EditableTargetState(ctx, "program", head.SceneID)
			if err != nil || current.SceneID != result.SceneID || current.SceneVersion != result.SceneVersion {
				result.Status = "refused"
				result.Reason = "PROGRAM_RELEASE_WRITER_MISMATCH"
				return result
			}
			result.WriterID = current.WriterID
			seq := current.EditSeq
			result.EditSeq = &seq
		}
		if err = hub.Flush(ctx); err != nil {
			result.Status = "refused"
			result.Reason = "PROGRAM_RELEASE_NATIVE_UNCONFIRMED"
		}
		return result
	}
}
func (c *SceneControl) inspectProgram(ctx context.Context, r lsdpreception.ProgramInspect) {
	result := lsdpreception.ProgramIdentityReceipt{ProgramInspect: r, Status: "refused", Reason: "PROGRAM_RELEASE_UNAVAILABLE"}
	if c.deps.InspectProgram != nil {
		result = c.deps.InspectProgram(ctx, r)
	}
	ack, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.reception.WriteLeaf(ack, "orion/state", "/scene_control/defaults/observed/inspect_program", result); err != nil {
		c.log("program identity observation unavailable", err)
	}
}
