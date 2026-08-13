// Package bluewire bridges a bluehost.Host instance to the SAME LSDP
// wire internal/lsdp already drives for the legacy Show-backed path
// (ADR-BLUE-012 §6.7 — "l'encapsulation LSML/LSDP ... restent Orion").
// A Bridge steps a blue-runtime-go instance and forwards its outputs,
// projected through internal/blueproject, onto a runtime.SceneMirror —
// the exact seam internal/lsdp.Wire.MirrorFor already returns for the
// legacy path, so a Solar client sees byte-identical frames regardless
// of which engine produced them.
package bluewire

import (
	"context"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// StepSource is the slice of *bluehost.Host a Bridge needs — an
// interface so tests substitute a fake instead of loading a real
// blue.program.v1 program.
type StepSource interface {
	Step(slot bluehost.Slot) (StepResult, error)
}

// StepResult mirrors blueruntime.StepResult's fields this package
// consumes. bluehost.Host.Step returns the real
// github.com/ZabLaboratory/Blue/runtime/go type, which structurally
// satisfies this shape — hostStepSource below adapts it without this
// package importing blueruntime directly.
type StepResult struct {
	RuntimeSequence uint64
	Outputs         map[string]any
}

// hostAdapter adapts *bluehost.Host to StepSource.
type hostAdapter struct{ host *bluehost.Host }

func (a hostAdapter) Step(slot bluehost.Slot) (StepResult, error) {
	r, err := a.host.Step(slot)
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{RuntimeSequence: r.RuntimeSequence, Outputs: r.Outputs}, nil
}

// Bridge steps one (host, slot) pair on an interval and forwards every
// non-empty projection onto mirror as a protocol.Delta.
type Bridge struct {
	steps       StepSource
	slot        bluehost.Slot
	mirror      runtime.SceneMirror
	sceneID     string
	sceneDigest string
	instanceID  string
	target      blueproject.Target
}

// NewBridge wires host's slot onto mirror. target should be
// blueproject.TargetPreview for bluehost.SlotPreview and
// blueproject.TargetProgram for bluehost.SlotOnAir — the caller states
// it explicitly rather than this package inferring it from the slot
// name, since the two vocabularies (Host slot vs. §6.7 target) are
// deliberately kept separate (bluehost translates Orion's own
// preview/on-air words; blueproject speaks the ADR's wire vocabulary).
func NewBridge(host *bluehost.Host, slot bluehost.Slot, mirror runtime.SceneMirror, sceneID, sceneDigest, instanceID string, target blueproject.Target) *Bridge {
	return &Bridge{
		steps:       hostAdapter{host: host},
		slot:        slot,
		mirror:      mirror,
		sceneID:     sceneID,
		sceneDigest: sceneDigest,
		instanceID:  instanceID,
		target:      target,
	}
}

// StepOnce steps the underlying instance once and forwards the result.
// An empty projection (no wire-legal outputs this step) is a no-op —
// mirrors the legacy path's zero-patch-delta drop (ADR 002 §6).
func (b *Bridge) StepOnce() error {
	result, err := b.steps.Step(b.slot)
	if err != nil {
		return err
	}
	proj := blueproject.Project(
		blueproject.StepOutputs{RuntimeSequence: result.RuntimeSequence, Outputs: result.Outputs},
		b.sceneDigest, b.instanceID, "", "", b.target,
	)
	if len(proj.Patches) == 0 {
		return nil
	}
	patches := make([]protocol.Patch, 0, len(proj.Patches))
	for path, val := range proj.Patches {
		patches = append(patches, protocol.Patch{Path: path, Value: val})
	}
	b.mirror.Forward(&protocol.Delta{
		Type:     "delta",
		V:        1,
		SceneID:  b.sceneID,
		Sequence: result.RuntimeSequence,
		Patches:  patches,
	})
	return nil
}

// Run steps the bridge every interval until ctx is cancelled. A step
// error is fail-soft (matches the legacy poller's per-scene posture,
// internal/adapters/poller.go): logged by the caller via onError, never
// aborts the loop — a single instance's misbehaviour must not take down
// every other paired instance sharing the process.
func (b *Bridge) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.StepOnce(); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
