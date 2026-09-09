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
	SetActive(sceneID string)
}

type lsdpRosterEmitter interface {
	EmitRoster(entries []runtime.RosterEntry)
}

type generationMirrorRegistry interface {
	MirrorForLSML(sceneID, sceneVersion, owner string, lsmlBundle []byte) runtime.SceneMirror
	SetActive(sceneID, sceneVersion string)
}

type fanoutSceneMirror []runtime.SceneMirror

func (f fanoutSceneMirror) Forward(message runtime.SubscriberMsg) {
	for _, mirror := range f {
		if mirror != nil {
			mirror.Forward(message)
		}
	}
}

// lsdpWires names the preview and antenne wires BY FIELD, not position
// (#398 F1, Vigil review on d448def). sceneIntentMirrorFor's prior shape
// — two positional, identically-typed *lsdp.Wire arguments — compiled
// cleanly if silently swapped at the call site (`sceneIntentMirrorFor(a,
// b)` vs `sceneIntentMirrorFor(b, a)`), and no test could have caught it:
// a test that builds its own call always writes its own arguments in
// "the right order" by construction, so it never independently observes
// a reorder at the REAL call site. Named fields do not make a reorder
// impossible — Go still accepts the unkeyed positional literal
// `lsdpWires{previewWire, antenneWire}`, `go vet`'s composites check
// only flags that for structs imported from another package, and
// lsdpWires is declared in main — so the guarantee below is
// CONVENTIONAL, held by both call sites writing the keyed
// `lsdpWires{preview: ..., antenne: ...}` form, not enforced by the
// compiler. Under that convention, the only way to misroute is to write
// the wrong FIELD NAME, a visible, deliberate edit rather than an
// invisible transposition.
type lsdpWires struct {
	preview    lsdpMirrorRegistry
	antenne    lsdpMirrorRegistry
	generation generationMirrorRegistry
}

// sceneIntentMirrorFor resolves scene-intent's LSDP projection target BY
// FLUX (#398): the flux the caller is bridging (bluehost.Slot — already
// computed by startBridge from the attestation action, SlotPreview for
// prepare-preview / SlotOnAir for take) is a required parameter of the
// resolution itself, never information the closure can be built without.
//
// Before #398, cmd/orion wired scene-intent's MirrorFor directly onto
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
//
// F2 (Vigil): bluehost.Slot is `type Slot string` (host.go:31-35), not a
// closed enum — the prior `if slot == SlotPreview { preview } else {
// antenne }` sent EVERY non-preview value, including one that is neither
// SlotPreview nor SlotOnAir, to the live antenne wire: "unknown → live"
// is exactly the fail-open shape #398 exists to remove (the same shape
// as the cockpit's unfiltered-`target` finding). No slot value other than
// the two bluehost constants reaches this function today — slot is
// derived strictly binary from the attested action in startBridge — so
// this is not a live path, but the explicit switch with a nil default
// makes an unrecognised slot fail CLOSED (no mirror, no bridge) instead
// of silently landing on the antenne.
func sceneIntentMirrorFor(wires lsdpWires) func(sceneID, sceneVersion string, slot bluehost.Slot, bundle []byte) runtime.SceneMirror {
	return func(sceneID, sceneVersion string, slot bluehost.Slot, bundle []byte) runtime.SceneMirror {
		var mirror runtime.SceneMirror
		switch slot {
		case bluehost.SlotPreview:
			mirror = wires.preview.MirrorForLSML(sceneID, sceneVersion, bundle)
		case bluehost.SlotOnAir:
			mirror = wires.antenne.MirrorForLSML(sceneID, sceneVersion, bundle)
		default:
			return nil
		}
		if wires.generation == nil {
			return mirror
		}
		generation := wires.generation.MirrorForLSML(sceneID, sceneVersion, string(slot), bundle)
		return fanoutSceneMirror{mirror, generation}
	}
}

// sceneIntentActivate changes the selected wire's active scene after the
// caller has applied the real validated keyframe. SetActive must not publish
// an empty snapshot before the render bundle state is present.
func sceneIntentActivate(wires lsdpWires) func(sceneID, sceneVersion string, slot bluehost.Slot) {
	return func(sceneID, sceneVersion string, slot bluehost.Slot) {
		switch slot {
		case bluehost.SlotPreview:
			wires.preview.SetActive(sceneID)
		case bluehost.SlotOnAir:
			wires.antenne.SetActive(sceneID)
		}
		if wires.generation != nil {
			wires.generation.SetActive(sceneID, sceneVersion)
		}
	}
}

// sceneIntentEmitRoster keeps the preload hint on the same wire as the
// scene-intent mirror. It is a best-effort capability of the dual LSDP wire:
// minimal test registries and bespoke embedders may omit it without changing
// scene-intent execution.
func sceneIntentEmitRoster(wires lsdpWires) func(bluehost.Slot, []runtime.RosterEntry) {
	return func(slot bluehost.Slot, entries []runtime.RosterEntry) {
		var target lsdpMirrorRegistry
		switch slot {
		case bluehost.SlotPreview:
			target = wires.preview
		case bluehost.SlotOnAir:
			target = wires.antenne
		default:
			return
		}
		emitter, ok := target.(lsdpRosterEmitter)
		if ok {
			emitter.EmitRoster(entries)
		}
	}
}
