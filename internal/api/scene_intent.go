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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/workload"
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
	Workload      WorkloadPortal
	Host          *bluehost.Host

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
	// Bridges tracks the running bridge per bluehost.Slot so a superseding
	// Take (or a re-Prepare) stops the previous one instead of leaking a
	// goroutine stepping an instance the Host has already released.
	// Required whenever MirrorFor is set; built once by cmd/orion via
	// bluewire.NewRegistry().
	Bridges *bluewire.Registry
	// ProjectionInterval paces the bridge's injected Tick loop. <= 0 defaults to
	// 100ms.
	ProjectionInterval time.Duration
	Logger             *slog.Logger

	// Idempotency deduplicates a replayed intent (§6.4: "une idempotency_key
	// client seule n'est jamais globale" — always scoped, never a bare
	// lookup). Nil ⇒ every request is processed fresh (dark by default,
	// same posture as MirrorFor/Bridges).
	Idempotency *IdempotencyCache
}

// IdempotencyCache remembers the typed result of a scoped (principal,
// owner, tenant, stream, action, scene_digest, ref_id, idempotency_key)
// tuple — the minimum dedup key §6.4 requires. A replayed intent
// carrying the same tuple and a non-empty idempotency_key returns the
// cached result instead of re-running Prepare/Take and restarting the
// bridge. This mirrors ZabCanvas's own `issue_or_replay` idempotence
// pattern: the SAME request replayed is answered from the prior
// outcome, never re-executed.
//
// Bounded on two axes (ADR-BLUE-012 §12/B8, B3-R6-OPS-ORION — "fenêtres
// de replay/déduplication"; the principal risk named there is "surcharge
// non bornée"): a TTL retires an entry after ttl regardless of traffic
// (fail-open — outside the window a replay is processed fresh, matching
// §12's "no crash-safe durability promised" Gate-B replay posture), and
// maxEntries caps the map's size so a burst of distinct tuples cannot
// grow it without bound. Both are enforced in store, off the read path.
type IdempotencyCache struct {
	mu         sync.Mutex
	cache      map[string]idempotencyEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	metrics    IdempotencyMetrics
}

type idempotencyEntry struct {
	resp     sceneIntentResponse
	storedAt time.Time
}

// IdempotencyMetrics is the cache's observability seam. reason is "ttl"
// (lookup found an expired entry) or "capacity" (store evicted to stay
// at/under maxEntries). nil-safe: a nil cache.metrics disables counting,
// the bound itself still applies.
type IdempotencyMetrics interface {
	IdempotencyEvicted(reason string)
}

const (
	defaultIdempotencyTTL        = 60 * time.Second
	defaultIdempotencyMaxEntries = 4096
)

// NewIdempotencyCache builds a cache with the platform defaults (60s
// TTL, 4096 entries — see internal/config's ORION_IDEMPOTENCY_TTL_S /
// ORION_IDEMPOTENCY_MAX_ENTRIES documentation for the rationale).
func NewIdempotencyCache() *IdempotencyCache {
	return NewIdempotencyCacheWithLimits(defaultIdempotencyTTL, defaultIdempotencyMaxEntries, nil)
}

// NewIdempotencyCacheWithLimits builds a cache from explicit bounds —
// the production wiring path (cmd/orion), driven by config. ttl <= 0 or
// maxEntries <= 0 fall back to the platform default for that axis rather
// than disabling it: this cache has no documented unbounded mode.
func NewIdempotencyCacheWithLimits(ttl time.Duration, maxEntries int, metrics IdempotencyMetrics) *IdempotencyCache {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultIdempotencyMaxEntries
	}
	return &IdempotencyCache{
		cache:      map[string]idempotencyEntry{},
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		metrics:    metrics,
	}
}

// setNowForTest injects the clock so a test can prove TTL expiry
// deterministically. TEST-ONLY by contract — no production path reaches it.
func (c *IdempotencyCache) setNowForTest(fn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = fn
}

func (c *IdempotencyCache) lookup(key string) (sceneIntentResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return sceneIntentResponse{}, false
	}
	if c.now().Sub(e.storedAt) > c.ttl {
		delete(c.cache, key)
		if c.metrics != nil {
			c.metrics.IdempotencyEvicted("ttl")
		}
		return sceneIntentResponse{}, false
	}
	return e.resp, true
}

func (c *IdempotencyCache) store(key string, resp sceneIntentResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.cache[key] = idempotencyEntry{resp: resp, storedAt: now}
	if len(c.cache) <= c.maxEntries {
		return
	}
	// Over capacity: sweep TTL-expired entries first — the common case
	// under real traffic, where capacity pressure and staleness coincide.
	for k, e := range c.cache {
		if now.Sub(e.storedAt) > c.ttl {
			delete(c.cache, k)
			if c.metrics != nil {
				c.metrics.IdempotencyEvicted("ttl")
			}
		}
	}
	if len(c.cache) <= c.maxEntries {
		return
	}
	// Still over: evict the single oldest entry. O(n) scan — store is not
	// on the tick/fanout hot path (one call per scene-intent request), and
	// n is bounded by maxEntries itself.
	var oldestKey string
	var oldestAt time.Time
	first := true
	for k, e := range c.cache {
		if first || e.storedAt.Before(oldestAt) {
			oldestKey, oldestAt, first = k, e.storedAt, false
		}
	}
	if oldestKey != "" {
		delete(c.cache, oldestKey)
		if c.metrics != nil {
			c.metrics.IdempotencyEvicted("capacity")
		}
	}
}

// idempotencyKey builds the §6.4 minimum dedup tuple. Fields are
// NUL-joined (never user-controlled to contain NUL) rather than any
// separator that could appear in an id, so two distinct tuples can
// never collide by concatenation ambiguity.
func idempotencyKey(principal, owner, tenant, stream string, action attestation.Action, sceneDigest, refID, idempotencyKey string) string {
	return strings.Join([]string{principal, owner, tenant, stream, string(action), sceneDigest, refID, idempotencyKey}, "\x00")
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
		ticket := r.Header.Get(authContextHeader)
		if ticket == "" {
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
		inlineArtifacts := req.BlueProgram != "" || req.BlueProgramDigest != "" || req.LSMLBundle != "" || req.LSMLBundleDigest != "" || req.RenderBundle != "" || req.RenderBundleDigest != ""
		var envelopeBody []byte
		var delegation *workload.Delegation
		if inlineArtifacts {
			if inlinePortal, ok := deps.Workload.(InlineAdmissionPortal); ok {
				if err := inlinePortal.AdmitInline(ctx, ticket, json.RawMessage(raw)); err != nil {
					writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
					return
				}
			} else {
				// Compatibility for old workload implementations and test
				// doubles; production Orion implements InlineAdmissionPortal.
				delegation, err = deps.Workload.MintDelegation(ctx, ticket, json.RawMessage(raw))
				if err != nil {
					writeJSON(w, http.StatusForbidden, sceneIntentResponse{Status: "rejected", IntentID: req.IntentID, Reason: workloadReason(err)})
					return
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
		} else {
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

		// artifact.Body is `zabcanvas.resolved-scene.v1` (§6.3). The
		// blue_program bytes are verified against the SIGNED
		// claims.BlueProgramDigest before Load — §6.2: "Orion revérifie
		// tous les digests sur les bytes reçus avant Load." The LSML
		// render-bundle has NO equivalent signed claim in §6.2's claim
		// set (only blue_program_digest is covered) — its integrity here
		// rests on envelope self-consistency plus the mTLS/delegation
		// chain that fetched it, not a second signed digest. Documented
		// gap, not silently assumed equal to the program's guarantee.
		//
		// A ref whose SIGNED claims declare NO program (empty
		// blue_program_digest, ORION-NOBLUE-AND-VERSION-ALIGN #398) skips
		// program decode/verify and never Loads anything — but an envelope
		// that then DOES carry program bytes is refused: unattested program
		// bytes never ride into Orion, even unexecuted. The bundle path is
		// identical in both cases (fetch, self-integrity, SetBundle).
		noProgram := claims.BlueProgramDigest == ""
		var program []byte
		if noProgram {
			if err := verifyNoProgramEnvelope(envelopeBody); err != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
				return
			}
		} else {
			var err error
			program, err = decodeAndVerifyProgram(envelopeBody, claims.BlueProgramDigest)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
				return
			}
		}
		bundle, bundleErr := decodeAndVerifyBundle(envelopeBody)
		if bundleErr != nil {
			writeJSON(w, http.StatusBadGateway, sceneIntentResponse{Status: "failed", IntentID: req.IntentID, Reason: "CANVAS_ARTIFACT_DIGEST_MISMATCH"})
			return
		}
		mirrorBundle := []byte(nil)
		var staticState map[string]json.RawMessage
		if bundle != nil {
			mirrorBundle = append([]byte(nil), bundle...)
		}
		precompiledBundle, precompiledErr := decodeAndVerifyRenderBundle(envelopeBody, claims.RenderBundleDigest)
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
		slot := bluehost.SlotPreview
		if action == attestation.ActionTakeOnAir {
			slot = bluehost.SlotOnAir
		}
		var opErr error
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
		if slot == bluehost.SlotOnAir {
			// A successful take is a new stateless generation, even when the
			// scene digest is reused. Drop process-local ingress ordering from
			// the previous instance before the new generation receives events.
			providers.ResetActiveIngress(deps.Host)
		}

		startBridge(deps, slot, claims, req.IntentID, !noProgram, mirrorBundle, staticState)

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

// resolvedSceneEnvelope is the slice of `zabcanvas.resolved-scene.v1`
// (§6.3) this handler consumes: the pinned blue.program.v1 bytes,
// base64-encoded, plus the digest Canvas computed over them at
// publication. Every other §6.3 field (LSML render-bundle, projection
// resources, full attestation echo) is out of this handler's scope.
type resolvedSceneEnvelope struct {
	BlueProgram       string `json:"blue_program"`
	BlueProgramDigest string `json:"blue_program_digest"`
	// LSMLBundle is OPTIONAL — an envelope with no bundle (e.g. an
	// operator-only rule with nothing to render) is valid;
	// decodeAndVerifyBundle returns (nil, nil) for it. LSMLBundleDigest is
	// MANDATORY the moment LSMLBundle is present (Bastion C4, PR #346,
	// fail-closed) — decodeAndVerifyBundle refuses an envelope that
	// carries a bundle with no digest, rather than skip verification.
	LSMLBundle       string `json:"lsml_bundle,omitempty"`
	LSMLBundleDigest string `json:"lsml_bundle_digest,omitempty"`
	// RenderBundle is compiled during Canvas validation. Its digest is also
	// signed in the Canvas ref claims; when present, scene-intent loads these
	// bytes verbatim and skips StaticBundleCompiler.
	RenderBundle       string `json:"render_bundle,omitempty"`
	RenderBundleDigest string `json:"render_bundle_digest,omitempty"`
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
	computedDigest, err := blueProgramDigest(program)
	if err != nil {
		return nil, err
	}
	if computedDigest != expectedDigest {
		return nil, errors.New("scene-intent: computed canonical program digest does not match the attested claim")
	}
	return program, nil
}

// blueProgramDigest verifies Blue's self-excluding program_digest contract.
// The digest is over the canonical JSON document with only program_digest
// removed, not over the raw JSON bytes that carry the self-referential field.
// Blue, ZabCanvas and the portable runtime all use this domain.
func blueProgramDigest(program []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(program))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", errors.New("scene-intent: program contains multiple JSON values")
		}
		return "", err
	}
	claimedDigest, ok := document["program_digest"].(string)
	if !ok || claimedDigest == "" {
		return "", errors.New("scene-intent: program is missing program_digest")
	}
	delete(document, "program_digest")
	computedDigest, err := canonical.Digest(document)
	if err != nil {
		return "", err
	}
	if computedDigest != claimedDigest {
		return "", errors.New("scene-intent: program_digest does not match canonical program content")
	}
	return computedDigest, nil
}

// decodeAndVerifyBundle extracts the OPTIONAL LSML render-bundle from
// the Canvas artifact envelope. Unlike decodeAndVerifyProgram, there is
// no SIGNED claim to cross-check against (§6.2's claim set has no
// lsml_bundle_digest) — only the envelope's own self-consistency
// (declared digest == sha256 of the decoded bytes) is verified. Returns
// (nil, nil) when the envelope carries no bundle at all.
//
// Fail-closed (Bastion C4, PR #346): lsml_bundle_digest is MANDATORY once
// lsml_bundle is present. An envelope with a bundle but no digest is
// refused outright rather than served unverified — the prior fail-open
// (`if digest != ""`) let an unverified bundle ride all the way to
// GET /host/render-bundle.
func decodeAndVerifyBundle(body json.RawMessage) ([]byte, error) {
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.LSMLBundle == "" {
		return nil, nil
	}
	if envelope.LSMLBundleDigest == "" {
		return nil, errors.New("scene-intent: lsml_bundle present without lsml_bundle_digest")
	}
	bundle, err := base64.StdEncoding.DecodeString(envelope.LSMLBundle)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bundle)
	if "sha256:"+hex.EncodeToString(sum[:]) != envelope.LSMLBundleDigest {
		return nil, errors.New("scene-intent: lsml_bundle does not match its own declared digest")
	}
	return bundle, nil
}

// decodeAndVerifyRenderBundle extracts the optional Solar bundle produced by
// POST /validate/render-bundle. The expected digest comes from the signed
// Canvas claims, so the response body cannot substitute another artifact.
func decodeAndVerifyRenderBundle(body json.RawMessage, expectedDigest string) ([]byte, error) {
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.RenderBundle == "" {
		if expectedDigest != "" {
			return nil, errors.New("scene-intent: signed render_bundle_digest has no render_bundle")
		}
		return nil, nil
	}
	if expectedDigest == "" || envelope.RenderBundleDigest == "" || envelope.RenderBundleDigest != expectedDigest {
		return nil, errors.New("scene-intent: render bundle digest is not signed consistently")
	}
	bundle, err := base64.StdEncoding.DecodeString(envelope.RenderBundle)
	if err != nil || len(bundle) == 0 || !json.Valid(bundle) {
		return nil, errors.New("scene-intent: render bundle is not valid base64 JSON")
	}
	sum := sha256.Sum256(bundle)
	computed := "sha256:" + hex.EncodeToString(sum[:])
	if computed != envelope.RenderBundleDigest {
		return nil, errors.New("scene-intent: render bundle digest does not match bytes")
	}
	return bundle, nil
}

func decodeRenderBundleDefaults(bundle []byte) (map[string]json.RawMessage, error) {
	var payload struct {
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(bundle, &payload); err != nil {
		return nil, err
	}
	return payload.Defaults, nil
}

// verifyNoProgramEnvelope enforces the no-program contract on the fetched
// envelope (ORION-NOBLUE-AND-VERSION-ALIGN, #398): a ref whose SIGNED
// claims carry an empty blue_program_digest must fetch an envelope with
// NO program fields at all. An envelope that carries blue_program (or
// declares a blue_program_digest) the attestation never signed is refused
// fail-closed — those bytes are unattested and must not enter Orion, even
// though the static path would never Load them.
func verifyNoProgramEnvelope(body json.RawMessage) error {
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope.BlueProgram != "" || envelope.BlueProgramDigest != "" {
		return errors.New("scene-intent: envelope carries a program the attestation did not sign")
	}
	return nil
}

// getHostRenderBundle serves the LSML render-bundle bytes attached to a
// slot by the most recent Prepare/Take (§15 read-route migration: the
// new-path equivalent of legacy's GET /scenes/{id}/render-bundle,
// content-addressed and immutably cacheable the same way — but keyed by
// slot, not scene_id, since the new model has no persisted roster to
// address by id). ?slot=preview|on-air, default preview.
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
// hasProgram=false (a static occupation, #398) still REGISTERS the scene
// on the wire — Solar must learn (sceneID, scene_version) to know what to
// fetch — but starts no bridge (there is no instance to step). Any bridge
// previously owning the slot is stopped: a static occupation superseding
// a programmed one must not leave a goroutine stepping an instance the
// Host has already released. The announced scene_version stays
// claims.SceneDigest in both cases; for a no-program ref that value IS
// the bundle hash (== artifact_set_digest, porteur's Decision A), so the
// ?v= Solar derives matches both the resolver and the bundle's own
// scene_version by construction.
func startBridge(deps SceneIntentDeps, slot bluehost.Slot, claims *attestation.Claims, intentID string, hasProgram bool, mirrorBundle []byte, staticState map[string]json.RawMessage) {
	if deps.MirrorFor == nil || deps.Bridges == nil {
		return
	}
	if len(mirrorBundle) == 0 {
		mirrorBundle = deps.Host.Bundle(slot)
	}
	mirror := deps.MirrorFor(claims.SceneID, claims.SceneDigest, slot, mirrorBundle)
	if mirror == nil {
		return
	}
	// A compiled LSML bundle can legitimately have no authored defaults while
	// still containing a renderable static scene and dynamic bindings. Solar
	// still needs one snapshot to mount that bundle before the first operator
	// delta can be displayed.
	if len(staticState) > 0 || len(mirrorBundle) > 0 {
		// Literal leaves belong to the Canvas layout, not to the Blue
		// program. Seed them before starting/replacing the bridge so a
		// programmed scene renders its authored text/images immediately.
		deps.Bridges.Stop(slot)
		mirror.Forward(&protocol.Snapshot{
			SceneID:      claims.SceneID,
			SceneVersion: claims.SceneDigest,
			State:        staticState,
		})
	}
	if !hasProgram {
		deps.Bridges.Stop(slot)
		return
	}
	target := blueproject.TargetPreview
	if slot == bluehost.SlotOnAir {
		target = blueproject.TargetProgram
	}
	bridge := bluewire.NewBridge(deps.Host, slot, mirror, claims.SceneID, claims.SceneDigest, claims.RefID, target, claims.RevisionID, intentID)
	logger := deps.Logger
	bridge.SetLogger(logger)
	// Project the first runtime state before returning the successful intent.
	// The periodic bridge remains the live path, but making the first tick
	// synchronous removes the response-to-first-delta race: a Solar client can
	// consume the prepared slot immediately instead of waiting for the ticker.
	if err := bridge.TickOnce(0); err != nil && logger != nil {
		logger.Warn("bluewire initial projection failed", "slot", slot, "scene_id", claims.SceneID, "err", err)
	}
	interval := deps.ProjectionInterval
	if interval <= 0 {
		interval = defaultProjectionInterval
	}
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
	if slot == bluehost.SlotOnAir {
		providers.ResetActiveIngress(deps.Host)
	}
	return deps.Host.Release(slot, reason)
}

// hostStatusResponse is the new path's read equivalent of legacy's
// `GET /api/v1/show` (show summary): which scene_digest, if any, each
// bluehost.Host slot currently carries. First read-route migration of
// #15's route-by-route plan (Refs #331) — additive, registered beside
// GET /api/v1/show, not replacing it yet.
type hostStatusResponse struct {
	Preview hostSlotStatus `json:"preview"`
	OnAir   hostSlotStatus `json:"on_air"`
}

type hostSlotStatus struct {
	SceneDigest string `json:"scene_digest,omitempty"`
	Loaded      bool   `json:"loaded"`

	// Projection is the identity of the most recently forwarded LSDP
	// delta for this slot's bridge, if any has forwarded yet — the SAME
	// correlation_id/render_revision stamped on the wire (bluewire.Bridge.
	// LastForwarded). Read-only, stateless (in-memory only, never
	// persisted, lost on restart like every other field here), and purely
	// a polling convenience: an external observer (Refs B3-R6-17-PULSAR)
	// can already read this identity off a live LSDP WS subscription
	// today without this field existing at all. Orion never waits on
	// anyone reading it, and this field never reflects anything about
	// whether the projection actually reached the antenna — that
	// confirmation is PGM, observed later and elsewhere (ADR-BLUE-012
	// §4.4/B17). Omitted entirely when no bridge is running or none has
	// forwarded yet.
	Projection *hostSlotProjection `json:"projection,omitempty"`
}

type hostSlotProjection struct {
	Sequence       uint64 `json:"sequence"`
	RenderRevision string `json:"render_revision,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
}

func getHostStatus(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		previewDigest := deps.Host.Digest(bluehost.SlotPreview)
		onAirDigest := deps.Host.Digest(bluehost.SlotOnAir)
		writeJSON(w, http.StatusOK, hostStatusResponse{
			Preview: hostSlotStatus{
				SceneDigest: previewDigest, Loaded: previewDigest != "",
				Projection: lastProjection(deps.Bridges, bluehost.SlotPreview),
			},
			OnAir: hostSlotStatus{
				SceneDigest: onAirDigest, Loaded: onAirDigest != "",
				Projection: lastProjection(deps.Bridges, bluehost.SlotOnAir),
			},
		})
	})
}

// lastProjection reads slot's running bridge (if any) for its most recently
// forwarded identity. A nil registry, no running bridge, or a bridge that
// has never forwarded all return nil — never a zero-value/invented
// identity standing in for "unknown".
func lastProjection(bridges *bluewire.Registry, slot bluehost.Slot) *hostSlotProjection {
	if bridges == nil {
		return nil
	}
	bridge := bridges.Current(slot)
	if bridge == nil {
		return nil
	}
	identity, ok := bridge.LastForwarded()
	if !ok {
		return nil
	}
	return &hostSlotProjection{
		Sequence:       identity.Sequence,
		RenderRevision: identity.RenderRevision,
		CorrelationID:  identity.CorrelationID,
	}
}

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
