package main

import (
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// lsdpMirrorRegistry is the slice of *lsdp.Wire sceneIntentMirrorFor
// depends on — an interface so a test substitutes a fake instead of
// standing up a live LSDP websocket kit. *lsdp.Wire satisfies this
// directly (MirrorForLSML already exists there, #396).
type lsdpMirrorRegistry interface {
	MirrorForLSML(sceneID, sceneVersion string, lsmlBundle []byte) runtime.SceneMirror
}

// sceneIntentMirrorFor resolves scene-intent's LSDP projection target BY
// FLUX (#398), closing over both the preview and antenne wires so the
// flux the caller is bridging (bluehost.Slot — already computed by
// startBridge from the attestation action, SlotPreview for
// prepare-preview / SlotOnAir for take) is a required parameter of the
// resolution itself, never information the closure can be built without.
//
// Before this fix, cmd/orion wired scene-intent's MirrorFor directly onto
// antenneWire.MirrorForLSML regardless of slot — the ONLY LSDP artefact a
// prepare-preview ever reached was the live antenne wire, because the
// function's own signature had no parameter through which "which wire"
// could even be expressed. That is the defect this function closes: the
// signature itself now makes an unrouted flux inexpressible, not just the
// one call site that used to get it wrong.
//
// sceneVersion is threaded straight through to whichever wire is picked
// (ORION-TAKE-SLOT-IDENTITY, Blue#345, #401) — the caller passes the real
// digest Prepare/Take committed on the slot; this function never hardcodes
// its own "" (the pre-#401 posture, when #398's own scope didn't yet know
// about the resolver-keying fix).
func sceneIntentMirrorFor(previewWire, antenneWire lsdpMirrorRegistry) func(sceneID, sceneVersion string, slot bluehost.Slot, bundle []byte) runtime.SceneMirror {
	return func(sceneID, sceneVersion string, slot bluehost.Slot, bundle []byte) runtime.SceneMirror {
		if slot == bluehost.SlotPreview {
			return previewWire.MirrorForLSML(sceneID, sceneVersion, bundle)
		}
		return antenneWire.MirrorForLSML(sceneID, sceneVersion, bundle)
	}
}
