package api

import (
	"context"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"time"
)

const defaultProjectionInterval = 10 * time.Millisecond

// startBridge pairs the just-Prepared/Taken bluehost instance with the
// LSDP scene deps.MirrorFor resolves for claims.SceneID, and starts a
// bluewire.Bridge stepping it. A nil MirrorFor or Bridges leaves this a
// no-op — Prepare/Take already succeeded and the typed response is
// unaffected either way (the pre-B3-R6-12-ORION-PROJECTION posture).
// deps.Bridges.Start stops whatever bridge previously owned slot before
// starting this one, so a Take superseding the on-air instance never
// leaves a goroutine stepping an instance bluehost.Host has released.
//
// hasProgram=false still REGISTERS the admitted artifact generation on the wire,
// but starts no bridge (there is no instance to step). Any bridge
// previously owning the slot is stopped: a static occupation superseding
// a programmed one must not leave a goroutine stepping an instance the
// Host has already released. Roster/generation keys use ArtifactSetDigest;
// the native mirror preserves source.scene_version for the renderer's exact
// source fetch and includes x-orion-artifact-set. Blue execution and projection
// provenance continue to use SceneDigest.
func startBridge(deps SceneIntentDeps, slot bluehost.Slot, claims *attestation.Claims, intentID string, hasProgram bool, mirrorBundle []byte, staticState map[string]json.RawMessage) func() {
	if deps.MirrorFor == nil || deps.Bridges == nil {
		return func() {}
	}
	sceneVersion := claims.ArtifactSetDigest
	mirror := deps.MirrorFor(claims.SceneID, sceneVersion, slot, mirrorBundle)
	if mirror == nil {
		return func() {}
	}
	// The scene-intent path has no Show roster to emit the
	// scene_roster frame for it. Publish this one validated source to the
	// wire that owns the slot before the keyframe/activation. Solar's source
	// provider then warms its pinned-source cache in parallel; the later
	// snapshot reuses the same in-flight request instead of paying the bundle
	// fetch after scene_changed. Orion keeps no roster entry after this wire
	// update. On restart, the owned selection/catalog reconstruct the runtime.
	if deps.EmitRoster != nil {
		deps.EmitRoster(slot, []runtime.RosterEntry{{
			SceneID:      claims.SceneID,
			SceneVersion: sceneVersion,
		}})
	}
	// LSML source can legitimately have no authored defaults while still
	// containing a renderable scene and dynamic bindings. Solar still needs a
	// snapshot to mount that source before the first operator
	// delta can be displayed. For programmed scenes the snapshot and first
	// delta are sequenced behind a bridge startup gate so the HTTP response is
	// not held by a large LSML seed, while Run cannot tick before the seed.
	hasSnapshot := len(staticState) > 0 || len(mirrorBundle) > 0
	var initialSnapshot *protocol.Snapshot
	if hasSnapshot {
		deps.Bridges.Stop(slot)
		initialSnapshot = &protocol.Snapshot{
			SceneID:      claims.SceneID,
			SceneVersion: sceneVersion,
			State:        staticState,
		}
	}
	if !hasProgram {
		if initialSnapshot != nil {
			mirror.Forward(initialSnapshot)
		}
		if deps.Activate != nil {
			deps.Activate(claims.SceneID, sceneVersion, slot)
		}
		deps.Bridges.Stop(slot)
		deps.Bridges.SetSceneMirror(slot, mirror)
		return func() {}
	}
	target := blueproject.TargetPreview
	if slot == bluehost.SlotOnAir {
		target = blueproject.TargetProgram
	}
	// Bluewire's projection digest remains the runtime identity committed by
	// Prepare/Take; the LSDP wire itself is pinned to ArtifactSetDigest above.
	bridge := bluewire.NewBridge(deps.Host, slot, mirror, claims.SceneID, claims.SceneDigest, claims.RefID, target, claims.RevisionID, intentID)
	logger := deps.Logger
	bridge.SetLogger(logger)
	interval := deps.ProjectionInterval
	if interval <= 0 {
		interval = defaultProjectionInterval
	}
	var startupCtx context.Context
	var startupCancel context.CancelFunc
	var startupGate chan struct{}
	if hasProgram {
		startupCtx, startupCancel = context.WithCancel(context.Background())
		startupGate = make(chan struct{})
		bridge.SetStartupGate(startupGate, startupCancel)
	}
	deps.Bridges.Start(slot, bridge, interval, func(err error) {
		if logger != nil {
			logger.Warn("bluewire bridge step failed", "slot", slot, "scene_id", claims.SceneID, "err", err)
		}
	})
	// The source-pinned snapshot and wire activation are the visible scene
	// switch. Dispatch them before answering the intent so a successful
	// response cannot outrun Solar's first scene snapshot. The first Blue
	// runtime projection remains outside the HTTP critical path; the startup
	// gate keeps the periodic loop behind it and Registry can cancel the worker
	// if a newer scene wins.
	if startupCtx == nil || startupCtx.Err() == nil {
		if initialSnapshot != nil {
			mirror.Forward(initialSnapshot)
		}
		if deps.Activate != nil {
			deps.Activate(claims.SceneID, sceneVersion, slot)
		}
	}
	return func() {
		go func() {
			if startupCtx != nil && startupCtx.Err() != nil {
				return
			}
			// Tick only advances tick entrypoints and timers; it does not consume
			// the initial on-start transition. Run that transition once so scene
			// automation and operator awaits are armed before periodic ticking.
			if err := bridge.StepOnce(); err != nil && logger != nil {
				logger.Warn("bluewire initial projection failed", "slot", slot, "scene_id", claims.SceneID, "err", err)
			}
			if startupGate != nil {
				close(startupGate)
			}
		}()
	}
}

// releaseSlot stops slot's bridge (if any) BEFORE releasing the
// bluehost instance, so the bridge goroutine never observes
// bluehost.ErrNotLoaded from a Host.Release that already ran — the
// "arrêt propre du Bridge au Release" the #331 checkpoint requires.
// Not wired to any HTTP route yet (no release action exists in
// sceneIntentRequest today); exercised directly by its own lifecycle
// test until a release route is added.
func releaseSlot(deps SceneIntentDeps, slot bluehost.Slot, reason string) error {
	if deps.Bridges != nil {
		deps.Bridges.Stop(slot)
	}
	if slot == bluehost.SlotOnAir {
		providers.ResetActiveIngress(deps.Host)
	}
	return deps.Host.Release(slot, reason)
}
