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
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
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

		// artifact.Body is `zabcanvas.resolved-scene.v1` (§6.3): among
		// other pinned artefacts it carries the blue.program.v1 bytes
		// blue-runtime-go loads directly. This slice hosts exactly one
		// scene's program; the full envelope parse (LSML bundle, digest
		// cross-checks against claims.*Digest) is the remaining wiring
		// work, not yet implemented here — see AGENT_REPORT.
		program := artifact.Body

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

		writeJSON(w, http.StatusOK, sceneIntentResponse{
			Status:     actionResultStatus(action),
			IntentID:   req.IntentID,
			SceneID:    claims.SceneID,
			RevisionID: claims.RevisionID,
		})
	})
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
