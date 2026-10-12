package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"time"
)

// A receiver receipt is not a renderer acknowledgement. This session owns the
// separate prepare/commit/finalize exchange with the selected physical Solar.
type ScenePresentation interface {
	Commit(context.Context) error
	Finalize(context.Context) error
	Abort(context.Context) error
}
type ScenePresenter func(context.Context, bluehost.Slot, string, string, string, []byte) (ScenePresentation, error)

type presentationTimings interface {
	PresentationTimings() map[string]time.Duration
}

func replaceAdmittedScene(ctx context.Context, deps SceneIntentDeps, slot bluehost.Slot,
	claims *attestation.Claims, req sceneIntentRequest, artifacts verifiedSceneArtifacts, bundle []byte, staticState map[string]json.RawMessage) (err error) {
	phaseStarted := time.Now()
	markPhase := func(name string) {
		if deps.ObservePresentationPhase != nil {
			deps.ObservePresentationPhase(name, time.Since(phaseStarted))
		}
		phaseStarted = time.Now()
	}
	mode := blueruntime.Preview
	if slot == bluehost.SlotOnAir {
		mode = blueruntime.Execute
	}
	candidate, err := deps.Host.StageReplacement(slot, claims.RefID, claims.SceneID, claims.SceneDigest, artifacts.Program, deps.Providers, deps.Policy, bluehost.NewEffectHandlers(deps.Effects, mode))
	if err != nil {
		return fmt.Errorf("HOST_PREPARE_FAILED: %w", err)
	}
	markPhase("blueprepare")
	var resumeBridge func(bool)
	if deps.Bridges != nil {
		resumeBridge = deps.Bridges.Pause(slot)
	}
	var resumeLane func(bool)
	if deps.BeginLaneTransition != nil {
		resumeLane = deps.BeginLaneTransition(slot)
	}
	var presentation ScenePresentation
	committed := false
	defer func() {
		if committed {
			return
		}
		// Cancellation must not cancel compensation. No candidate Blue has stepped.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if deps.Bridges != nil {
			deps.Bridges.Stop(slot)
		}
		compensation := candidate.Rollback()
		if resumeLane != nil {
			resumeLane(true)
		}
		if deps.WireFlush != nil {
			compensation = errors.Join(compensation, deps.WireFlush(rollbackCtx))
		}
		if presentation != nil {
			compensation = errors.Join(compensation, presentation.Abort(rollbackCtx))
		}
		if resumeBridge != nil {
			resumeBridge(true)
		}
		if compensation != nil {
			err = errors.Join(err, fmt.Errorf("SCENE_COMPENSATION_FAILED: %w", compensation))
		}
	}()
	if deps.PresentScene != nil {
		presentation, err = deps.PresentScene(ctx, slot, req.IntentID, claims.SceneID, claims.ArtifactSetDigest, artifacts.Source)
		if err != nil {
			return fmt.Errorf("SOLAR_PREPARATION_FAILED: %w", err)
		}
		markPhase("solarprepare")
		if err = presentation.Commit(ctx); err != nil {
			return fmt.Errorf("SOLAR_COMMIT_FAILED: %w", err)
		}
		markPhase("solarcommit")
	}
	if err = candidate.Commit(); err != nil {
		return err
	}
	if bundle != nil {
		deps.Host.SetBundle(slot, bundle)
	}
	deps.Host.SetSceneInterface(slot, artifacts.Source)
	deps.Host.SetArtifactSetDigest(slot, claims.ArtifactSetDigest)
	// New mirrors seed the source while the bridge remains behind its startup gate.
	beginBlue := startBridge(deps, slot, claims, req.IntentID, len(artifacts.Program) > 0, artifacts.Source, staticState)
	if deps.WireFlush != nil {
		if err = deps.WireFlush(ctx); err != nil {
			return fmt.Errorf("NATIVE_LSDP_DELIVERY_FAILED: %w", err)
		}
	}
	markPhase("wireflush")
	if presentation != nil {
		if err = presentation.Finalize(ctx); err != nil {
			return fmt.Errorf("SOLAR_FINALIZATION_FAILED: %w", err)
		}
		markPhase("solarfinalize")
		if measured, ok := presentation.(presentationTimings); ok && deps.ObservePresentationPhase != nil {
			for name, elapsed := range measured.PresentationTimings() {
				deps.ObservePresentationPhase(name, elapsed)
			}
		}
	}
	if resumeLane != nil {
		resumeLane(false)
	}
	if resumeBridge != nil {
		resumeBridge(false)
	}
	// This is the decision point: presentation confirmed, candidate committed.
	// A cleanup failure cannot turn an accepted take into a replay of on-start.
	committed = true
	if stopErr := candidate.Finalize(); stopErr != nil && deps.Logger != nil {
		deps.Logger.Error("old scene cleanup failed after commit", "err", stopErr)
	}
	if slot == bluehost.SlotOnAir {
		providers.ResetActiveIngress(deps.Host)
	}
	beginBlue()
	return nil
}
