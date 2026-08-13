// Package api — scene-intent surface (ADR-BLUE-012 §4.4/§6.4). This is
// the FIRST slice of the stateless cutover (#331): it proves the new
// path — attestation verification, ZabGate workload delegation, and the
// blue-runtime-go preview/on-air host — end to end, additively, beside
// the existing Store/Show-backed routes. It does not yet replace any
// existing route; the legacy surface stays live until this path is
// proven and every consumer (Prism) has migrated (non-cohabitation rule
// in the #331 work-unit bail: no partial cutover ever reaches main).
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

// WorkloadPortal is the slice of *workload.Client the intent handler
// needs — an interface so tests substitute a fake instead of standing up
// a real ZabGate mTLS listener. *workload.Client satisfies this directly.
type WorkloadPortal interface {
	AdmitAuthContext(ctx context.Context, ticket string) (*workload.AuthContextAdmission, error)
	MintDelegation(ctx context.Context, admission *workload.AuthContextAdmission) (*workload.Delegation, error)
	FetchCanvas(ctx context.Context, jti string) (*workload.CanvasArtifact, error)
}

// SceneIntentDeps groups the new stateless-path dependencies. A nil
// Workload or Host leaves the route unregistered (RegisterPublic skips
// it) — every field here is additive to PublicDeps, never a replacement.
type SceneIntentDeps struct {
	Trust         attestation.TrustSet
	LocatorPrefix string
	OwnerID       string
	TenantID      string
	Workload      WorkloadPortal
	Host          *bluehost.Host

	// MirrorFor resolves the LSDP scene pairing a bluewire.Bridge forwards
	// onto, for a given scene_id — normally lsdp.Wire.MirrorFor. Nil ⇒ no
	// bridge is ever started: Prepare/Take still run, the handler still
	// returns its typed result, but nothing reaches Solar over this path
	// yet (the pre-B3-R6-12-ORION-PROJECTION posture).
	MirrorFor func(sceneID string) runtime.SceneMirror
	// Bridges tracks the running bridge per bluehost.Slot so a superseding
	// Take (or a re-Prepare) stops the previous one instead of leaking a
	// goroutine stepping an instance the Host has already released.
	// Required whenever MirrorFor is set; built once by cmd/orion via
	// bluewire.NewRegistry().
	Bridges *bluewire.Registry
	// ProjectionInterval paces the bridge's Step loop. <= 0 defaults to
	// 100ms.
	ProjectionInterval time.Duration
	Logger             *slog.Logger
}

// sceneIntentRequest is `orion.scene-intent.v1` (§6.4). ResolvedSceneRef
// is the opaque compact JWS ZabCanvas signed and Prism relayed unmutated;
// StreamID/OperatorContext are non-authoritative requests — Orion trusts
// only the principal ZabGate injected and the attestation's own claims.
type sceneIntentRequest struct {
	SchemaVersion    string `json:"schema_version"`
	IntentID         string `json:"intent_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	StreamID         string `json:"stream_id"`
	Target           string `json:"target"` // preview | on-air
	Action           string `json:"action"` // prepare-preview | take-on-air
	ResolvedSceneRef string `json:"resolved_scene_ref"`
}

type sceneIntentResponse struct {
	Status     string `json:"status"`
	IntentID   string `json:"intent_id"`
	SceneID    string `json:"scene_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// authContextHeader carries the opaque `zabgate-auth-context.v1` ticket
// ZabGate mints at intent admission (§4.7/§6.11). Orion never parses or
// verifies it — it only relays it, unmodified, to AdmitAuthContext.
const authContextHeader = "X-ZabGate-Auth-Context"

func postSceneIntent(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		ticket := r.Header.Get(authContextHeader)
		if ticket == "" {
			writeJSON(w, http.StatusUnauthorized, sceneIntentResponse{Status: "rejected", Reason: "AUTH_CONTEXT_UNAVAILABLE"})
			return
		}

		var req sceneIntentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", Reason: "MALFORMED_INTENT"})
			return
		}

		var action attestation.Action
		switch req.Action {
		case string(attestation.ActionPreparePreview):
			action = attestation.ActionPreparePreview
		case string(attestation.ActionTakeOnAir):
			action = attestation.ActionTakeOnAir
		default:
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "UNKNOWN_ACTION"})
			return
		}

		principal := authSource.FromHeaders(r.Header).UserID

		claims, err := attestation.Verify(req.ResolvedSceneRef, deps.Trust, attestation.Options{
			Principal:     principal,
			OwnerID:       deps.OwnerID,
			TenantID:      deps.TenantID,
			StreamID:      req.StreamID,
			Action:        action,
			LocatorPrefix: deps.LocatorPrefix,
		})
		if err != nil {
			writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "ATTESTATION_REJECTED"})
			return
		}

		ctx := r.Context()
		admission, err := deps.Workload.AdmitAuthContext(ctx, ticket)
		if err != nil {
			writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
			return
		}

		delegation, err := deps.Workload.MintDelegation(ctx, admission)
		if err != nil {
			writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
			return
		}

		artifact, err := deps.Workload.FetchCanvas(ctx, delegation.JTI)
		if err != nil {
			writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "compensating", IntentID: req.IntentID, Reason: workloadReason(err)})
			return
		}
		if artifact.Status < 200 || artifact.Status >= 300 {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_UNAVAILABLE"})
			return
		}

		// artifact.Body is `zabcanvas.resolved-scene.v1` (§6.3). This slice
		// consumes only the pinned blue_program bytes it carries; the LSML
		// render-bundle / projection resources are the Phase-B WS-wiring
		// scope Conduit is scoping separately. Every byte is verified
		// against claims.BlueProgramDigest BEFORE Load — §6.2: "Orion
		// revérifie tous les digests sur les bytes reçus avant Load."
		program, err := decodeAndVerifyProgram(artifact.Body, claims.BlueProgramDigest)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
			return
		}

		slot := bluehost.SlotPreview
		var opErr error
		switch action {
		case attestation.ActionPreparePreview:
			opErr = deps.Host.Prepare(slot, claims.RefID, claims.SceneDigest, program, nil, nil)
			if errors.Is(opErr, bluehost.ErrAlreadyLoaded) {
				if deps.Host.Digest(slot) == claims.SceneDigest {
					opErr = nil // idempotent re-prepare of the same digest
				}
			}
		case attestation.ActionTakeOnAir:
			opErr = deps.Host.Take(claims.RefID, claims.SceneDigest, program, nil, nil)
		}
		if opErr != nil {
			writeJSON(w, http.StatusInternalServerError, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "HOST_PREPARE_FAILED"})
			return
		}

		startBridge(deps, slot, action, claims, req.IntentID)

		writeJSON(w, http.StatusOK, sceneIntentResponse{
			Status:     actionResultStatus(action),
			IntentID:   req.IntentID,
			SceneID:    claims.SceneID,
			RevisionID: claims.RevisionID,
		})
	})
}

// resolvedSceneEnvelope is the slice of `zabcanvas.resolved-scene.v1`
// (§6.3) this handler consumes: the pinned blue.program.v1 bytes,
// base64-encoded, plus the digest Canvas computed over them at
// publication. Every other §6.3 field (LSML render-bundle, projection
// resources, full attestation echo) is out of this handler's scope.
type resolvedSceneEnvelope struct {
	BlueProgram       string `json:"blue_program"`
	BlueProgramDigest string `json:"blue_program_digest"`
}

// decodeAndVerifyProgram parses the Canvas artifact envelope, decodes
// the pinned program bytes, and cross-checks BOTH the envelope's own
// claimed digest and the freshly computed sha256 of the received bytes
// against expectedDigest (claims.BlueProgramDigest, from the SIGNED
// attestation — the only digest actually trusted). A mismatch anywhere
// in this chain fails closed: Orion never Loads bytes it cannot prove
// match what ZabCanvas attested to sign.
func decodeAndVerifyProgram(body json.RawMessage, expectedDigest string) ([]byte, error) {
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.BlueProgramDigest != expectedDigest {
		return nil, errors.New("scene-intent: envelope blue_program_digest does not match the attested claim")
	}
	program, err := base64.StdEncoding.DecodeString(envelope.BlueProgram)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(program)
	if "sha256:"+hex.EncodeToString(sum[:]) != expectedDigest {
		return nil, errors.New("scene-intent: computed program digest does not match the attested claim")
	}
	return program, nil
}

const defaultProjectionInterval = 100 * time.Millisecond

// startBridge pairs the just-Prepared/Taken bluehost instance with the
// LSDP scene deps.MirrorFor resolves for claims.SceneID, and starts a
// bluewire.Bridge stepping it. A nil MirrorFor or Bridges leaves this a
// no-op — Prepare/Take already succeeded and the typed response is
// unaffected either way (the pre-B3-R6-12-ORION-PROJECTION posture).
// deps.Bridges.Start stops whatever bridge previously owned slot before
// starting this one, so a Take superseding the on-air instance never
// leaves a goroutine stepping an instance bluehost.Host has released.
func startBridge(deps SceneIntentDeps, slot bluehost.Slot, action attestation.Action, claims *attestation.Claims, intentID string) {
	if deps.MirrorFor == nil || deps.Bridges == nil {
		return
	}
	mirror := deps.MirrorFor(claims.SceneID)
	if mirror == nil {
		return
	}
	target := blueproject.TargetPreview
	if action == attestation.ActionTakeOnAir {
		target = blueproject.TargetProgram
	}
	bridge := bluewire.NewBridge(deps.Host, slot, mirror, claims.SceneID, claims.SceneDigest, claims.RefID, target, claims.RevisionID, intentID)
	interval := deps.ProjectionInterval
	if interval <= 0 {
		interval = defaultProjectionInterval
	}
	logger := deps.Logger
	deps.Bridges.Start(slot, bridge, interval, func(err error) {
		if logger != nil {
			logger.Warn("bluewire bridge step failed", "slot", slot, "scene_id", claims.SceneID, "err", err)
		}
	})
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
	return deps.Host.Release(slot, reason)
}

func actionResultStatus(a attestation.Action) string {
	if a == attestation.ActionTakeOnAir {
		return "taken"
	}
	return "prepared"
}

func workloadReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
