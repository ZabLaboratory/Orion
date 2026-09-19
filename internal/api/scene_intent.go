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
	"fmt"
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/workload"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// WorkloadPortal is the slice of *workload.Client the intent handler
// needs — an interface so tests substitute a fake instead of standing up
// a real ZabGate mTLS listener. *workload.Client satisfies this directly.
//
// There is deliberately no AdmitAuthContext here: admit is an
// operator-JWT route Prism calls itself — the ticket IS the operator's
// consent, and an Orion-side admit was a confused deputy (Bastion,
// WORKLOAD-PROTOCOL-ALIGN-M3). Orion only ever mints against the ticket
// Prism relayed.
type WorkloadPortal interface {
	MintDelegation(ctx context.Context, ticket string, intent json.RawMessage) (*workload.Delegation, error)
	// FetchCanvas needs the whole minted delegation (access_token +
	// gate_request_id ride the DelegationProxyRequest body) plus the SAME
	// ticket and raw intent bytes the mint was scoped to — Gate
	// re-validates them against the ticket bindings on the proxy call too.
	FetchCanvas(ctx context.Context, delegation *workload.Delegation, ticket string, intent json.RawMessage) (*workload.CanvasArtifact, error)
}

// InlineAdmissionPortal is the optimized path for a validated capsule whose
// artifacts are already in the relayed intent. It keeps the mTLS Gate
// revalidation boundary without minting a consumable Canvas delegation that
// this path never uses.
type InlineAdmissionPortal interface {
	AdmitInline(ctx context.Context, ticket string, intent json.RawMessage) error
}

// SceneIntentDeps groups the new stateless-path dependencies. A nil
// Workload or Host leaves the route unregistered (RegisterPublic skips
// it) — every field here is additive to PublicDeps, never a replacement.
type SceneIntentDeps struct {
	Trust         attestation.TrustSet
	LocatorPrefix string
	OwnerID       string
	TenantID      string
	// AttestationClockSkew is only populated by the embedded-local boot
	// profile. Antenne keeps the strict attestation verifier default.
	AttestationClockSkew time.Duration
	Workload             WorkloadPortal
	Host                 *bluehost.Host
	// EmbeddedLocal enables the loopback sidecar path. It skips only the
	// remote ZabGate workload ticket because Prism synchronized this signed
	// capsule during startup; attestation and every artifact digest remain
	// mandatory below.
	EmbeddedLocal bool
	// LocalArtifactRoot is Prism's content-addressed cache root. It is only
	// read by the embedded-local path after the signed scene ref has passed
	// attestation verification.
	LocalArtifactRoot string

	// StaticBundleCompiler converts ZabCanvas authoring LSML into the Solar
	// RenderBundle and returns its initial literal state. Production wiring
	// supplies the compiler; nil keeps legacy test doubles and old envelopes
	// byte-compatible until the stateless surface is enabled there.
	StaticBundleCompiler func(raw []byte, sceneID, sceneVersion string) ([]byte, map[string]json.RawMessage, error)

	// Providers is the Zab capability-provider catalogue (internal/providers
	// .Registry()) passed to bluehost.Host.Prepare/Take so a program
	// declaring `requires` can be admitted by the portable runtime's
	// checkProviders — nil means every capability-requiring program is
	// refused CAPABILITY_UNAVAILABLE (only display-only programs with no
	// `requires` succeed), same posture as before this field existed.
	Providers []map[string]any
	// Policy is the host-side CapabilityPolicy admission gate
	// (internal/providers.Policy(...)) — nil means the portable runtime's
	// own default-allow posture applies to every declared provider.
	Policy blueruntime.CapabilityPolicy

	// ValidationMaxSteps / ValidationMaxWall bound POST /validate/program's
	// Step loop (bluehost.ValidateProgram) — the SAME config-driven budget
	// Engine A's /validate/simulate harness already uses
	// (cfg.ValidationMaxSteps/cfg.ValidationMaxWall,
	// ORION_VALIDATION_MAX_STEPS/ORION_VALIDATION_MAX_WALL_S,
	// internal/runtime/validation_harness.go's DefaultValidationBudget), not
	// a second, invented number. Zero values fall back to
	// ValidateProgram's own defaults (1_000_000 steps / 5s).
	ValidationMaxSteps uint64
	ValidationMaxWall  time.Duration

	// Effects binds the direct EffectHandlers (ENGINE-B-PARITY-ORION,
	// bluehost.NewEffectHandlers) and carries the Runner/Egress fields used by
	// the Host's generic core.effect.invoke@1 adapter. The bundle is the SAME
	// egress policy / DB client / datasource map / runner Engine A's
	// SceneEffects uses. The direct handlers fail closed when its fields are
	// absent; the generic adapter remains unwired when its runner or policy is
	// absent, preserving its explicit async admission protocol.
	Effects bluehost.EffectDeps

	// MirrorFor resolves the LSDP scene pairing a bluewire.Bridge forwards
	// onto, for a given scene_id — normally cmd/orion's sceneIntentMirrorFor
	// closure over lsdp.Wire.MirrorForLSML. Nil ⇒ no bridge is ever started:
	// Prepare/Take still run, the handler still returns its typed result,
	// but nothing reaches Solar over this path yet (the
	// pre-B3-R6-12-ORION-PROJECTION posture).
	//
	// The slot parameter is the FLUX (#398): startBridge passes the SAME
	// bluehost.Slot it was itself called with (SlotPreview for
	// prepare-preview, SlotOnAir for take), so the resolution can route a
	// preview onto the preview wire and a take onto the antenne wire. This
	// is load-bearing, not advisory — before this parameter existed, no
	// implementation of MirrorFor could express "which wire" at all, and
	// cmd/orion wired every call straight onto the live antenne wire
	// regardless of flux: a prepare-preview projected its deltas onto the
	// antenne, silently.
	//
	// sceneVersion is required too (ORION-TAKE-SLOT-IDENTITY, Blue#345):
	// startBridge passes claims.SceneDigest, the SAME digest Prepare/Take
	// committed on the slot. Before this, the call site hardcoded "" here,
	// so whatever the LSDP kit told the client its scene_version was (""),
	// the client would echo back as ?v= on GET .../render-bundle — a value
	// that, post-#401, can never match host.Digest(slot). Keying the
	// resolver correctly (#401, this unit's Take fix) is necessary but not
	// sufficient on its own: the client also has to be TOLD the value that
	// will actually match. For a NO-PROGRAM ref (#398, Decision A) this
	// value is the bundle hash (scene_digest == artifact_set_digest), so
	// it also equals the scene_version the bundle itself carries — the
	// client-side lumencast check passes by construction. For a
	// with-program ref the bundle-side mismatch remains, out of this
	// unit's scope (separate chantier).
	//
	// bundle is the slot's LSML render-bundle bytes (deps.Host.Bundle(slot),
	// the value SetBundle stored for the Prepare/Take that is starting this
	// bridge) — the ONLY render-bundle artefact this path ever holds;
	// startBridge passes it through so the bound-leaf gate
	// (internal/lsdp.boundLeafSet, #396) actually executes on the stateless
	// path instead of running permanently disabled on a hardcoded nil. May
	// be nil (no lsml_bundle in the envelope), which correctly disables the
	// gate — same fail-open posture as the legacy path's binding-less
	// bundle.
	MirrorFor func(sceneID, sceneVersion string, slot bluehost.Slot, bundle []byte) runtime.SceneMirror
	// Activate flips the selected LSDP wire after the first real snapshot has
	// been applied. Registration and activation stay separate so a connected
	// client never receives an empty keyframe before the validated bundle is
	// seeded.
	Activate func(sceneID, sceneVersion string, slot bluehost.Slot)
	// Bridges tracks the running bridge per bluehost.Slot so a superseding
	// Take (or a re-Prepare) stops the previous one instead of leaking a
	// goroutine stepping an instance the Host has already released.
	// Required whenever MirrorFor is set; built once by cmd/orion via
	// bluewire.NewRegistry().
	Bridges *bluewire.Registry
	// EmitRoster publishes a short-lived, slot-scoped preload hint to the
	// matching LSDP wire. It is deliberately not persisted and does not
	// activate or prepare a future scene: it only lets an already-connected
	// Solar client fetch the exact validated bundle while the keyframe and
	// Blue projection are being assembled. Nil keeps minimal/bespoke embeds
	// unchanged.
	EmitRoster func(slot bluehost.Slot, entries []runtime.RosterEntry)
	// ProjectionInterval paces the bridge's injected Tick loop. <= 0 defaults to
	// 100ms.
	ProjectionInterval time.Duration
	Logger             *slog.Logger

	// Idempotency deduplicates a replayed intent (§6.4: "une idempotency_key
	// client seule n'est jamais globale" — always scoped, never a bare
	// lookup). Nil ⇒ every request is processed fresh (dark by default,
	// same posture as MirrorFor/Bridges).
	Idempotency *IdempotencyCache
	// ProgramVerificationCache reuses a completed canonical Blue digest
	// verification for the same signed digest and exact raw bytes. Nil keeps
	// tests and minimal embedders on the uncached fail-closed path.
	ProgramVerificationCache *VerifiedProgramCache
}

// sceneIntentRequest is the slice of `orion.scene-intent.v1` (§6.4) this
// handler reads for its OWN needs. ResolvedSceneRef is the opaque compact
// JWS ZabCanvas signed and Prism relayed unmutated; StreamID is a
// non-authoritative request — Orion trusts only the principal ZabGate
// injected and the attestation's own claims. The full intent Prism posted
// (including fields Orion has no use for: sequence, issued_at, deadline,
// correlation_id) is relayed to Gate's mint as the RAW request bytes —
// this struct is never re-serialized onto the wire, because Gate binds
// the intent's values (deadline among them) into the ticket and any
// reconstruction risks AUTH_CONTEXT_MISMATCH.
type sceneIntentRequest struct {
	SchemaVersion      string `json:"schema_version"`
	IntentID           string `json:"intent_id"`
	IdempotencyKey     string `json:"idempotency_key"`
	StreamID           string `json:"stream_id"`
	Target             string `json:"target"` // preview | on-air
	Action             string `json:"action"` // prepare-preview | take-on-air
	ResolvedSceneRef   string `json:"resolved_scene_ref"`
	BlueProgram        string `json:"blue_program,omitempty"`
	BlueProgramDigest  string `json:"blue_program_digest,omitempty"`
	LSMLBundle         string `json:"lsml_bundle,omitempty"`
	LSMLBundleDigest   string `json:"lsml_bundle_digest,omitempty"`
	RenderBundle       string `json:"render_bundle,omitempty"`
	RenderBundleDigest string `json:"render_bundle_digest,omitempty"`
	LocalArtifacts     bool   `json:"local_artifacts,omitempty"`
}

// maxSceneIntentBytes bounds the raw intent body kept for the verbatim
// mint relay — aligned on the workload surface's own 1 MiB response
// bound; the Gate caps resolved_scene_ref alone at 64 KiB.
const maxSceneIntentBytes = 1 << 20

type sceneIntentResponse struct {
	Status     string `json:"status"`
	IntentID   string `json:"intent_id"`
	SceneID    string `json:"scene_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
}

// authContextHeader carries the opaque `zabgate-auth-context.v1` ticket
// ZabGate issued to Prism at intent admission (§4.7/§6.11) — Prism admits
// with its own operator JWT and transports the ticket here. Orion never
// parses or verifies it — it only relays it, unmodified, to
// MintDelegation.
const authContextHeader = "X-ZabGate-Auth-Context"

func postSceneIntent(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		phaseStarted := time.Now()
		markPhase := func(name string) {
			if deps.EmbeddedLocal {
				w.Header().Add("Server-Timing", fmt.Sprintf("%s;dur=%.3f", name, float64(time.Since(phaseStarted).Microseconds())/1000))
			}
			phaseStarted = time.Now()
		}
		ticket := r.Header.Get(authContextHeader)
		if ticket == "" && !deps.EmbeddedLocal {
			writeJSON(w, http.StatusUnauthorized, sceneIntentResponse{Status: "rejected", Reason: "AUTH_CONTEXT_UNAVAILABLE"})
			return
		}

		// Keep the RAW body bytes: they ARE the intent Gate's mint
		// re-validates against the ticket bindings. Decoding into
		// sceneIntentRequest serves only Orion's own routing — the wire
		// artifact relayed to mint is these exact bytes, never a
		// re-serialization of the struct (which would drop the fields
		// Orion doesn't model and mutate what the ticket binds).
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxSceneIntentBytes+1))
		if err != nil || len(raw) > maxSceneIntentBytes {
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", Reason: "MALFORMED_INTENT"})
			return
		}
		var req sceneIntentRequest
		if err := json.Unmarshal(raw, &req); err != nil {
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
			ClockSkew:     deps.AttestationClockSkew,
		})
		if err != nil {
			writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "ATTESTATION_REJECTED", Message: err.Error()})
			return
		}

		// Idempotent replay (§6.4): a request scoped to the same tuple and
		// carrying the same non-empty idempotency_key gets the PRIOR
		// outcome, never a re-run — no second Admit/Mint/Fetch, no second
		// Prepare/Take, no bridge restart. A missing/empty idempotency_key
		// is never treated as a cache hit (§6.4: never a bare/global key).
		var dedupKey string
		if deps.Idempotency != nil && req.IdempotencyKey != "" {
			dedupKey = idempotencyKey(principal, deps.OwnerID, deps.TenantID, req.StreamID, action, claims.SceneDigest, claims.RefID, req.IdempotencyKey)
			if cached, ok := deps.Idempotency.lookup(dedupKey); ok {
				writeJSON(w, http.StatusOK, cached)
				return
			}
		}

		ctx := r.Context()
		markPhase("attestation")
		inlineArtifacts := req.BlueProgram != "" || req.BlueProgramDigest != "" || req.LSMLBundle != "" || req.LSMLBundleDigest != "" || req.RenderBundle != "" || req.RenderBundleDigest != ""
		localArtifacts := deps.EmbeddedLocal && req.LocalArtifacts
		if localArtifacts && inlineArtifacts {
			writeJSON(w, http.StatusBadRequest, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "MALFORMED_INTENT"})
			return
		}
		if deps.EmbeddedLocal && !inlineArtifacts && !localArtifacts {
			writeJSON(w, http.StatusConflict, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "LOCAL_SCENE_CAPSULE_REQUIRED"})
			return
		}
		slot := bluehost.SlotPreview
		if action == attestation.ActionTakeOnAir {
			slot = bluehost.SlotOnAir
		}
		// Always validate and activate through the normal loading path. A
		// loaded Host slot is not proof that its scene still owns the Preview
		// wire: an editable clone can have selected another scene meanwhile.
		var envelopeBody []byte
		var delegation *workload.Delegation
		var inlineAdmissionDone chan error
		switch {
		case localArtifacts:
			envelopeBody, err = loadLocalSceneEnvelope(deps.LocalArtifactRoot, claims)
			if err != nil {
				writeJSON(w, http.StatusConflict, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: "LOCAL_SCENE_ARTIFACT_UNAVAILABLE", Message: err.Error()})
				return
			}
		case inlineArtifacts:
			if !deps.EmbeddedLocal {
				switch inlinePortal, ok := deps.Workload.(InlineAdmissionPortal); {
				case ok:
					// Admission is the commit gate, not a prerequisite for local
					// digest verification. Run the independent mTLS round-trip in
					// parallel with those pure checks; the result is awaited before
					// Host mutates either slot, so a rejection can never launch Blue.
					inlineAdmissionDone = make(chan error, 1)
					go func() {
						inlineAdmissionDone <- inlinePortal.AdmitInline(ctx, ticket, json.RawMessage(raw))
					}()
				default:
					// Compatibility for old workload implementations and test
					// doubles; production Orion implements InlineAdmissionPortal.
					_, err = deps.Workload.MintDelegation(ctx, ticket, json.RawMessage(raw))
					if err != nil {
						writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
						return
					}
				}
			}
			// ZabGate's validated capsule is already the result of Canvas
			// validation and bundle creation. Do not dereference Canvas again
			// on activation.
			envelopeBody, err = json.Marshal(resolvedSceneEnvelope{
				BlueProgram:        req.BlueProgram,
				BlueProgramDigest:  req.BlueProgramDigest,
				LSMLBundle:         req.LSMLBundle,
				LSMLBundleDigest:   req.LSMLBundleDigest,
				RenderBundle:       req.RenderBundle,
				RenderBundleDigest: req.RenderBundleDigest,
			})
			if err != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_UNAVAILABLE", Message: err.Error()})
				return
			}
		default:
			// Legacy path: mint and consume a one-shot Canvas delegation before
			// accepting the fetched artifact envelope.
			delegation, err = deps.Workload.MintDelegation(ctx, ticket, json.RawMessage(raw))
			if err != nil {
				writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
				return
			}
			artifact, fetchErr := deps.Workload.FetchCanvas(ctx, delegation, ticket, json.RawMessage(raw))
			if fetchErr != nil {
				writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "compensating", IntentID: req.IntentID, Reason: workloadReason(fetchErr), Message: workloadMessage(fetchErr)})
				return
			}
			if artifact.Status < 200 || artifact.Status >= 300 {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_UNAVAILABLE"})
				return
			}
			envelopeBody = artifact.Body
		}
		if inlineAdmissionDone != nil {
			if err := <-inlineAdmissionDone; err != nil {
				writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
				return
			}
		}

		// artifact.Body is `zabcanvas.resolved-scene.v1` (§6.3). The
		markPhase("artifacts")
		// blue_program bytes are verified against the SIGNED
		// claims.BlueProgramDigest before Load — §6.2: "Orion revérifie
		// tous les digests sur les bytes reçus avant Load." The LSML
		// render-bundle has NO equivalent signed claim in §6.2's claim
		// set (only blue_program_digest is covered) — its integrity here
		// rests on envelope self-consistency plus the mTLS/delegation
		// chain that fetched it, not a second signed digest. Documented
		// gap, not silently assumed equal to the program's guarantee.
		//
		var envelope resolvedSceneEnvelope
		if err := json.Unmarshal(envelopeBody, &envelope); err != nil {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
			return
		}

		// A ref whose SIGNED claims declare NO program (empty
		// blue_program_digest, ORION-NOBLUE-AND-VERSION-ALIGN #398) skips
		// program decode/verify and never Loads anything — but an envelope
		// that then DOES carry program bytes is refused: unattested program
		// bytes never ride into Orion, even unexecuted. The bundle path is
		// identical in both cases (fetch, self-integrity, SetBundle).
		noProgram := claims.BlueProgramDigest == ""
		var program []byte
		if noProgram {
			if err := verifyNoProgramEnvelopeValue(envelope); err != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
				return
			}
		} else {
			var err error
			program, err = decodeAndVerifyProgramEnvelopeCached(envelope, claims.BlueProgramDigest, deps.ProgramVerificationCache)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
				return
			}
		}
		bundle, bundleErr := decodeAndVerifyBundleEnvelope(envelope)
		if bundleErr != nil {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
			return
		}
		mirrorBundle := []byte(nil)
		var staticState map[string]json.RawMessage
		if bundle != nil {
			mirrorBundle = append([]byte(nil), bundle...)
		}
		precompiledBundle, precompiledErr := decodeAndVerifyRenderBundleEnvelope(envelope, claims.RenderBundleDigest)
		if precompiledErr != nil {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
			return
		}
		if precompiledBundle != nil {
			bundle = precompiledBundle
			staticState, precompiledErr = decodeRenderBundleDefaults(bundle)
			if precompiledErr != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
				return
			}
		} else if bundle != nil && deps.StaticBundleCompiler != nil {
			compiledBundle, defaults, err := deps.StaticBundleCompiler(bundle, claims.SceneID, claims.SceneDigest)
			if err != nil {
				if deps.Logger != nil {
					deps.Logger.Error("static scene bundle compilation failed", "scene_id", claims.SceneID, "err", err)
				}
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "STATIC_BUNDLE_COMPILE_FAILED"})
				return
			}
			bundle = compiledBundle
			// Legacy refs compile at click time; validated refs carry the same
			// defaults inside the precompiled bundle instead.
			staticState = defaults
		}

		// The serving identity stays claims.SceneDigest for BOTH shapes —
		// the with-program path is byte-identical to before this unit.
		// M6 note (#398, porteur's Decision A): for a NO-PROGRAM ref,
		// ZabCanvas mints scene_digest == artifact_set_digest == the hash
		// of the bundle, and stamps that same value as the bundle's own
		// scene_version — so the version announced to Solar (startBridge →
		// MirrorFor), matched by resolveHostBundle and returned as ETag
		// equals the value @lumencast/runtime compares ?v= against, BY
		// CONSTRUCTION, with no re-keying here. The with-program
		// misalignment (scene_digest is a program-family hash, not the
		// bundle's own scene_version) is real and intentionally NOT
		// touched by this unit — separate chantier.
		var opErr error
		markPhase("verification")
		switch action {
		case attestation.ActionPreparePreview:
			if noProgram {
				opErr = deps.Host.PreparePreviewStatic(claims.SceneID, claims.SceneDigest)
			} else {
				opErr = deps.Host.PreparePreview(claims.RefID, claims.SceneID, claims.SceneDigest, program, deps.Providers, deps.Policy, bluehost.NewEffectHandlers(deps.Effects, blueruntime.Preview))
			}
			if errors.Is(opErr, bluehost.ErrAlreadyLoaded) {
				if deps.Host.Serving(slot, claims.SceneID, claims.SceneDigest) {
					opErr = nil // idempotent re-prepare: same scene, same digest already running
				}
			}
		case attestation.ActionTakeOnAir:
			// claims.SceneID now threaded through (ORION-TAKE-SLOT-IDENTITY,
			// Blue#345) — Take used to be the one caller of the two that left
			// the on-air slot's sceneID empty, silently making it unmatchable
			// by resolveHostBundle's (scene_id, v) check (#401).
			if noProgram {
				opErr = deps.Host.TakeStatic(claims.SceneID, claims.SceneDigest)
			} else {
				opErr = deps.Host.Take(claims.RefID, claims.SceneID, claims.SceneDigest, program, deps.Providers, deps.Policy, bluehost.NewEffectHandlers(deps.Effects, blueruntime.Execute))
			}
		}
		if opErr != nil {
			writeJSON(w, http.StatusInternalServerError, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "HOST_PREPARE_FAILED"})
			return
		}
		if bundle != nil {
			deps.Host.SetBundle(slot, bundle)
		}
		markPhase("host")
		deps.Host.SetArtifactSetDigest(slot, claims.ArtifactSetDigest)
		if slot == bluehost.SlotOnAir {
			// A successful take is a new stateless generation, even when the
			// scene digest is reused. Drop process-local ingress ordering from
			// the previous instance before the new generation receives events.
			providers.ResetActiveIngress(deps.Host)
		}

		startBridge(deps, slot, claims, req.IntentID, !noProgram, mirrorBundle, staticState)
		markPhase("wire")

		resp := sceneIntentResponse{
			Status:     actionResultStatus(action),
			IntentID:   req.IntentID,
			SceneID:    claims.SceneID,
			RevisionID: claims.RevisionID,
		}
		// Only a SUCCESSFUL outcome is cached — a rejection/failure is
		// never memoized, so a corrected retry (new ticket, new artifact)
		// is always re-attempted rather than replaying a stale failure.
		if dedupKey != "" {
			deps.Idempotency.store(dedupKey, resp)
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func getHostRenderBundle(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		slot := bluehost.SlotPreview
		if r.URL.Query().Get("slot") == "on-air" {
			slot = bluehost.SlotOnAir
		}
		digest := deps.Host.Digest(slot)
		bundle := deps.Host.Bundle(slot)
		if digest == "" || bundle == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "RENDER_BUNDLE_NOT_FOUND"})
			return
		}
		writeImmutable(w, digest, http.StatusOK)
		_, _ = w.Write(bundle)
	})
}

// 10ms keeps the first bridge tick inside the live switch budget. This is
// only a pacing bound for a bridge attached after the operator action; Orion
// remains stateless and does not prepare a future scene.
func actionResultStatus(a attestation.Action) string {
	if a == attestation.ActionTakeOnAir {
		return "taken"
	}
	return "prepared"
}

// workloadReason surfaces the BARE §4.7 refusal code when the workload
// client recognized one — Prism's failure vocabulary
// (SCENE_INTENT_FAILURE_REASONS) matches on the exact code, and the
// previous err.Error() spelling ("workload: CODE (http n)") collapsed
// every typed refusal into UNKNOWN_RESPONSE on the operator's screen.
func workloadReason(err error) string {
	if err == nil {
		return ""
	}
	var werr *workload.Error
	if errors.As(err, &werr) && werr.Code != "" {
		return string(werr.Code)
	}
	return err.Error()
}

func workloadMessage(err error) string {
	if err == nil {
		return ""
	}
	var workloadErr *workload.Error
	if errors.As(err, &workloadErr) && workloadErr.Message != "" {
		return workloadErr.Message
	}
	return err.Error()
}
