package main

import (
	"errors"
	"log/slog"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
)

type platformEventWriter func(bluehost.Slot, string, any) (blueruntime.StepResult, error)
type platformEventForwarder func(bluehost.Slot, bluewire.StepResult) error

// newStatelessPlatformEventSink executes every canonical platform event in
// both Engine-B scene slots and projects the resulting outputs through the
// bridge that owns that slot. WritePlatformEvent advances Blue, but it does
// not publish its StepResult to LSDP by itself; dropping that result leaves
// Solar frozen even though the blueprint did execute.
//
// Preview and on-air are deliberately independent. A missing or failed slot
// cannot suppress delivery to the other slot, and the inbox write remains
// best-effort just like the pre-existing platform ingress contract.
func newStatelessPlatformEventSink(
	write platformEventWriter,
	forward platformEventForwarder,
	logger *slog.Logger,
) func(string, any) {
	return func(path string, payload any) {
		for _, slot := range []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir} {
			result, err := write(slot, path, payload)
			if err != nil {
				if !errors.Is(err, bluehost.ErrNotLoaded) {
					logger.Warn("stateless platform event delivery failed", "slot", slot, "path", path, "error", err)
				}
				continue
			}
			if err := forward(slot, bluewire.StepResult{
				RuntimeSequence: result.RuntimeSequence,
				Outputs:         result.Outputs,
			}); err != nil {
				logger.Warn("stateless platform event projection failed", "slot", slot, "path", path, "error", err)
			}
		}
	}
}
