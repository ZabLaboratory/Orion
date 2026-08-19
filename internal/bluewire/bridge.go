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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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

// TickSource is the scheduler seam used by a production bridge. The host
// owns the instance clock; the bridge only injects the elapsed interval. A
// test can provide this seam without starting a bluehost.Host or sleeping.
type TickSource interface {
	Tick(slot bluehost.Slot, deltaSeconds float64) (StepResult, error)
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

func (a hostAdapter) Tick(slot bluehost.Slot, deltaSeconds float64) (StepResult, error) {
	r, err := a.host.Tick(slot, deltaSeconds)
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{RuntimeSequence: r.RuntimeSequence, Outputs: r.Outputs}, nil
}

// SequenceStaleError reports a non-monotone runtime sequence that was not an
// identical replay. Returning a typed error lets the registry's diagnostic
// sink observe the drift without allowing an old projection onto the wire.
type SequenceStaleError struct {
	Received uint64
	Last     uint64
}

func (e *SequenceStaleError) Error() string {
	return fmt.Sprintf("bluewire: stale runtime sequence %d after %d", e.Received, e.Last)
}

// Bridge steps one (host, slot) pair on an interval and forwards every
// non-empty projection onto mirror as a protocol.Delta.
type Bridge struct {
	steps          StepSource
	ticks          TickSource
	slot           bluehost.Slot
	mirror         runtime.SceneMirror
	sceneID        string
	sceneDigest    string
	instanceID     string
	target         blueproject.Target
	renderRevision string
	correlationID  string
	logger         *slog.Logger

	mu                   sync.Mutex
	lastRuntimeSequence  uint64
	lastOutputSequence   uint64
	lastProjectionDigest string
}

// NewBridge wires host's slot onto mirror. target should be
// blueproject.TargetPreview for bluehost.SlotPreview and
// blueproject.TargetProgram for bluehost.SlotOnAir — the caller states
// it explicitly rather than this package inferring it from the slot
// name, since the two vocabularies (Host slot vs. §6.7 target) are
// deliberately kept separate (bluehost translates Orion's own
// preview/on-air words; blueproject speaks the ADR's wire vocabulary).
// renderRevision/correlationID are stamped on every Projection this
// bridge produces — the caller's revision_id/intent_id, not derived
// here.
func NewBridge(host *bluehost.Host, slot bluehost.Slot, mirror runtime.SceneMirror, sceneID, sceneDigest, instanceID string, target blueproject.Target, renderRevision, correlationID string) *Bridge {
	return &Bridge{
		steps:          hostAdapter{host: host},
		ticks:          hostAdapter{host: host},
		slot:           slot,
		mirror:         mirror,
		sceneID:        sceneID,
		sceneDigest:    sceneDigest,
		instanceID:     instanceID,
		target:         target,
		renderRevision: renderRevision,
		correlationID:  correlationID,
	}
}

// SetLogger installs the structured-trace sink for successful, non-duplicate
// forwards (ADR-BLUE-012 §16.1 — "les traces corrèlent Prism, Canvas/Gate,
// Blue, Orion, Solar et Pulsar"). nil-safe: a nil logger (SetLogger never
// called) disables tracing; the forward itself is unaffected either way —
// same posture as SetWSMetrics/SetExecMetrics elsewhere in this codebase.
// Call before Run/StepOnce/TickOnce; not safe to change concurrently with a
// running bridge.
func (b *Bridge) SetLogger(logger *slog.Logger) { b.logger = logger }

// StepOnce steps the underlying instance once and forwards the result.
// An empty projection (no wire-legal outputs this step) is a no-op —
// mirrors the legacy path's zero-patch-delta drop (ADR 002 §6).
func (b *Bridge) StepOnce() error {
	return b.step(0)
}

// TickOnce advances the production host clock by deltaSeconds and forwards
// the resulting projection. NewBridge uses Host.Tick through TickSource;
// legacy test doubles that only implement StepSource continue to work via
// the compatibility path in step.
func (b *Bridge) TickOnce(deltaSeconds float64) error {
	return b.step(deltaSeconds)
}

func (b *Bridge) step(deltaSeconds float64) error {
	result, err := b.advance(deltaSeconds)
	if err != nil {
		return err
	}
	return b.ForwardResult(result)
}

// ForwardResult projects a result already produced by the hosted runtime
// onto the same LSDP mirror as the bridge ticker. Operator calls are
// synchronous in Engine B: waiting for the next Tick would let a successful
// LEC trigger return 202 while leaving the scene unchanged until an unrelated
// clock step (or forever when the runtime emits no tick output).
// Keeping this method on Bridge guarantees that immediate calls and periodic
// ticks share sequence, deduplication, projection and metadata rules.
func (b *Bridge) ForwardResult(result StepResult) error {
	proj := blueproject.Project(
		blueproject.StepOutputs{RuntimeSequence: result.RuntimeSequence, Outputs: result.Outputs},
		b.sceneDigest, b.instanceID, b.renderRevision, b.correlationID, b.target,
	)
	if len(proj.Patches) == 0 {
		if len(result.Outputs) > 0 && b.logger != nil {
			outputKeys := make([]string, 0, len(result.Outputs))
			outputKinds := map[string]int{}
			for path, value := range result.Outputs {
				outputKeys = append(outputKeys, path)
				outputKinds[fmt.Sprintf("%T", value)]++
			}
			sort.Strings(outputKeys)
			b.logger.Warn("bluewire projection empty",
				"scene_id", b.sceneID,
				"slot", b.slot,
				"runtime_sequence", result.RuntimeSequence,
				"output_count", len(result.Outputs),
				"output_keys", outputKeys,
				"output_kinds", outputKinds,
			)
		}
		b.recordRuntimeSequence(result.RuntimeSequence)
		return nil
	}

	paths := make([]string, 0, len(proj.Patches))
	for path := range proj.Patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	patches := make([]protocol.Patch, 0, len(paths))
	for _, path := range paths {
		patches = append(patches, protocol.Patch{Path: path, Value: proj.Patches[path]})
	}

	projectionDigest, err := projectionDigest(proj, b.sceneID, patches)
	if err != nil {
		return err
	}
	sequence, duplicate, err := b.sequenceFor(result.RuntimeSequence, projectionDigest)
	if err != nil || duplicate {
		return err
	}
	proj.OutputSequence = sequence
	if b.mirror == nil {
		return errors.New("bluewire: nil scene mirror")
	}
	b.mirror.Forward(&protocol.Delta{
		Type:              "delta",
		V:                 1,
		SceneID:           b.sceneID,
		Sequence:          sequence,
		SchemaVersion:     proj.SchemaVersion,
		SceneDigest:       proj.SceneDigest,
		RuntimeInstanceID: proj.RuntimeInstanceID,
		Target:            string(proj.Target),
		RenderRevision:    proj.RenderRevision,
		CorrelationID:     proj.CorrelationID,
		Patches:           patches,
	})
	b.logForward(sequence, proj)
	return nil
}

// logForward emits the out-of-wire trace an external PGM observer (Refs
// B3-R6-17-PULSAR) correlates by TIME against its own frame-level
// observation — never a substitute for that observation, and never
// anything Orion waits on (ADR-BLUE-012 §4.4/B17: PGM is truth observed
// later, not an Orion transaction). A nil logger (SetLogger never called)
// makes this a no-op, matching every other optional sink in this codebase.
func (b *Bridge) logForward(sequence uint64, proj blueproject.Projection) {
	if b.logger == nil {
		return
	}
	b.logger.Info("bluewire projection forwarded",
		"scene_id", b.sceneID,
		"slot", b.slot,
		"sequence", sequence,
		"target", proj.Target,
		"scene_digest", proj.SceneDigest,
		"runtime_instance_id", proj.RuntimeInstanceID,
		"render_revision", proj.RenderRevision,
		"correlation_id", proj.CorrelationID,
	)
}

// ForwardedIdentity is the projection identity of the most recent
// successful forward this Bridge made — the same fields stamped on every
// protocol.Delta it produces (blueproject.Project's Target/SceneDigest/
// RuntimeInstanceID/RenderRevision/CorrelationID), plus the wire Sequence
// they rode on. Read-only snapshot; never mutated by a caller.
type ForwardedIdentity struct {
	Sequence          uint64
	SceneDigest       string
	RuntimeInstanceID string
	Target            string
	RenderRevision    string
	CorrelationID     string
}

// LastForwarded reports the identity of the most recent successful,
// non-duplicate forward, and whether one has happened yet. This is a
// stateless, in-memory snapshot — nothing here is persisted, and nothing
// about it makes Orion wait on anything: a caller (e.g. a read-only status
// route) polls it opportunistically, the Bridge itself never blocks or
// changes behavior because it was read. Sequence starts at 0 and only ever
// becomes positive via sequenceFor, so ok reports the same thing sequence>0
// would — kept explicit so a caller never has to know that invariant.
func (b *Bridge) LastForwarded() (identity ForwardedIdentity, ok bool) {
	b.mu.Lock()
	sequence := b.lastOutputSequence
	b.mu.Unlock()
	if sequence == 0 {
		return ForwardedIdentity{}, false
	}
	return ForwardedIdentity{
		Sequence:          sequence,
		SceneDigest:       b.sceneDigest,
		RuntimeInstanceID: b.instanceID,
		Target:            string(b.target),
		RenderRevision:    b.renderRevision,
		CorrelationID:     b.correlationID,
	}, true
}

func (b *Bridge) advance(deltaSeconds float64) (StepResult, error) {
	if b.ticks != nil {
		return b.ticks.Tick(b.slot, deltaSeconds)
	}
	if b.steps != nil {
		return b.steps.Step(b.slot)
	}
	return StepResult{}, errors.New("bluewire: no step or tick source")
}

func (b *Bridge) recordRuntimeSequence(sequence uint64) {
	if sequence == 0 {
		return
	}
	b.mu.Lock()
	if sequence > b.lastRuntimeSequence {
		b.lastRuntimeSequence = sequence
	}
	b.mu.Unlock()
}

func (b *Bridge) sequenceFor(runtimeSequence uint64, digest string) (uint64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if runtimeSequence > 0 {
		if runtimeSequence < b.lastRuntimeSequence {
			return 0, false, &SequenceStaleError{Received: runtimeSequence, Last: b.lastRuntimeSequence}
		}
		if runtimeSequence == b.lastRuntimeSequence {
			if digest == b.lastProjectionDigest {
				return 0, true, nil
			}
			return 0, false, &SequenceStaleError{Received: runtimeSequence, Last: b.lastRuntimeSequence}
		}
		b.lastRuntimeSequence = runtimeSequence
	}

	// The host Tick ABI deliberately returns no runtime sequence. The bridge
	// therefore owns the wire sequence for clock-driven projections. It also
	// suppresses a repeated projection before allocating a new wire sequence.
	if digest == b.lastProjectionDigest {
		return 0, true, nil
	}
	sequence := runtimeSequence
	if sequence == 0 || sequence <= b.lastOutputSequence {
		sequence = b.lastOutputSequence + 1
	}
	b.lastOutputSequence = sequence
	b.lastProjectionDigest = digest
	return sequence, false, nil
}

type projectionIdentity struct {
	SchemaVersion     string
	SceneID           string
	SceneDigest       string
	RuntimeInstanceID string
	Target            blueproject.Target
	RenderRevision    string
	CorrelationID     string
	Patches           []protocol.Patch
}

func projectionDigest(proj blueproject.Projection, sceneID string, patches []protocol.Patch) (string, error) {
	identity := projectionIdentity{
		SchemaVersion:     proj.SchemaVersion,
		SceneID:           sceneID,
		SceneDigest:       proj.SceneDigest,
		RuntimeInstanceID: proj.RuntimeInstanceID,
		Target:            proj.Target,
		RenderRevision:    proj.RenderRevision,
		CorrelationID:     proj.CorrelationID,
		Patches:           patches,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("bluewire: fingerprint projection: %w", err)
	}
	return string(data), nil
}

// Run steps the bridge every interval until ctx is cancelled. A step
// error is fail-soft (matches the legacy poller's per-scene posture,
// internal/adapters/poller.go): logged by the caller via onError, never
// aborts the loop — a single instance's misbehaviour must not take down
// every other paired instance sharing the process.
func (b *Bridge) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.TickOnce(interval.Seconds()); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
