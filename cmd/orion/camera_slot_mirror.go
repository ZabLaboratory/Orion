package main

import (
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/lsdp"
)

// cameraSlotMirror fans one editor projection into the two non-Pulsar
// consumer lanes. Prism's local return already receives the same slot map via
// its peer-viewer injection; mirroring it onto the dedicated preview wire as
// well keeps a connected Solar preview and the antenne/live wire coherent.
// Program/Pulsar is intentionally absent from this fan-out.
type cameraSlotMirror struct {
	mirrors []bluehost.SlotAssignmentMirror
}

func newCameraSlotMirror(preview, antenne *lsdp.Wire) bluehost.SlotAssignmentMirror {
	mirrors := make([]bluehost.SlotAssignmentMirror, 0, 2)
	if preview != nil {
		mirrors = append(mirrors, preview)
	}
	if antenne != nil {
		mirrors = append(mirrors, antenne)
	}
	if len(mirrors) == 0 {
		return nil
	}
	return cameraSlotMirror{mirrors: mirrors}
}

func (m cameraSlotMirror) EmitSlotAssignment(slotRef, peerLabel string) {
	for _, mirror := range m.mirrors {
		mirror.EmitSlotAssignment(slotRef, peerLabel)
	}
}
