package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// pushScene handles POST /api/v1/scenes/{id}/push. Either compiles a
// new pushed version from a definition envelope, or re-points
// latest_pushed_version at an existing scene_version (rollback).
func pushScene(deps PublicDeps) http.HandlerFunc {
	// Process-local idempotence cache, shared across every request this
	// handler serves (the closure is built once at route registration). See
	// push_dedup.go — a go-live's double push skips the second compile.
	dedup := newPushDedup(256)
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		var envelope compiler.PushEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid push envelope"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), deps.Config.PushTimeout)
		defer cancel()

		// ADR 002 §3.1: upsert-on-push. Canvas authors the scene in its
		// own DB and never seeds Orion, so a first push lands on a scene
		// whose row does not exist here. UpsertScene creates it (status=
		// active, name=placeholder=scene_id — Canvas stays the source of
		// truth for the name, §3.2) or no-ops if it already exists,
		// returning the row either way. This both removes the spurious
		// 404 NOT_FOUND on the first-push path AND guarantees the FK
		// target scene_definitions.scene_id→scenes(id) exists before
		// InsertDefinition. Race-safe under concurrent first-pushes (R2).
		// The placeholder name is never overwritten on re-push.
		scene, err := deps.Store.UpsertScene(ctx, sceneID, sceneID.String())
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		// Archived guard preserved (ADR 002 §3.1, criterion 5): the
		// upsert no-ops on an existing archived scene (DO UPDATE SET
		// id=id never touches status), so this runs on the real row and
		// still rejects with 409 SCENE_ARCHIVED.
		if scene.Status == store.SceneArchived {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "SCENE_ARCHIVED"})
			return
		}

		if envelope.IsRollback() {
			handleRollback(ctx, w, deps, sceneID, envelope.RollbackTo)
			return
		}

		// ADR 001 §3.1: the legacy singular blue_blueprint_id and the new
		// blueprints[] list are mutually exclusive. Both set → 400
		// ENVELOPE_BLUEPRINT_CONFLICT, rejected here BEFORE Compile so it is
		// an envelope-shape error (400), not a compile diagnostic (422).
		if _, err := compiler.NormalizeBlueprints(envelope); errors.Is(err, compiler.ErrEnvelopeBlueprintConflict) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "ENVELOPE_BLUEPRINT_CONFLICT"})
			return
		}

		// Idempotence (switch fix): a byte-identical re-push — same scene,
		// same envelope — compiles to the artefact it already persisted. The
		// go-live flow pushes twice (push → validate → re-push) and every
		// compile re-fetches the Canvas layout + blueprints over the WAN.
		// Fingerprint the envelope (a pure hash of the compile INPUTS,
		// knowable without a compile) and, on a hit, skip Compile + every
		// upstream fetch, resolving the stored pushed version instead. The
		// dedup map is a hint only: the store is authoritative, so a dangling
		// hint (row archived/purged) falls through to a full compile.
		fingerprint, fpErr := compiler.EnvelopeFingerprint(sceneID.String(), envelope)
		if fpErr == nil {
			if cachedVersion, ok := dedup.get(fingerprint); ok {
				if done := serveIdempotentPush(ctx, w, deps, sceneID, cachedVersion); done {
					return
				}
				// Stored version gone: forget the stale hint and recompile.
				dedup.drop(fingerprint)
			}
		}

		started := time.Now()
		graph, bundle, sceneVersion, err := compiler.Compile(ctx, sceneID.String(), envelope, deps.Fetcher)
		if err != nil {
			deps.Metrics.PushTotal.WithLabelValues("compile_error").Inc()
			deps.Metrics.PushDuration.WithLabelValues("compile_error").Observe(time.Since(started).Seconds())
			writePushError(w, err)
			return
		}

		// ADR 002 §3.4 T6 / #I — authoring validation gate. Orion emits the
		// LSML bundle and RE-VALIDATES it independently (defence in depth: it
		// trusts no upstream figma/Canvas diagnostic that travelled on the
		// wire). A bundle that trips any `error` — a `src`/`mask.source` host
		// outside assets.allowedHosts (T1/T2), an enum outside the closed set
		// (T4), a dangling/cyclic mask shape-ref (#K), or a complexity budget
		// overflow (T5) — is REFUSED here, BEFORE it is persisted or served to
		// the antenna. Authoring is NOT silently dropped: the author gets a
		// clear 422 LSML_GATE_REJECTED. The runtime keeps re-gating (Solar
		// host-allow/css-color) as defence in depth, not the only barrier.
		//
		// The gate runs in EVERY LSDP mode (it gates the artefact bound for
		// the antenna, independent of whether Orion also persists LSML). In
		// LSDP-persisting mode the same emitted bundle is reused below to avoid
		// a second emit. An emit FAILURE here is itself a gate refusal — Orion
		// must not serve a scene whose LSML it could not even produce.
		gateBundle, _, _, emitErr := compiler.EmitLSML(
			sceneID.String(), bundle.AuthoringRoot, bundle.OperatorInputs,
			bundle.ExternalAdapters, nil, bundle.LSMLAssets,
		)
		if emitErr != nil {
			deps.Metrics.PushTotal.WithLabelValues("gate_error").Inc()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"code": lsmlGateRejectedCode,
				"diagnostics": map[string]any{
					"errors": []map[string]string{{
						"code":    "GATE_EMIT_FAILED",
						"message": "could not emit LSML bundle for validation",
					}},
					"warnings": []string{},
				},
			})
			return
		}
		if gd := compiler.GateLSMLBundle(gateBundle); gd.HasErrors() {
			deps.Metrics.PushTotal.WithLabelValues("gate_rejected").Inc()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"code": lsmlGateRejectedCode,
				"diagnostics": map[string]any{
					"errors":   gd.Errors(),
					"warnings": gd.Warnings(),
				},
			})
			return
		}

		definitionID := uuid.New()

		pv := store.ScenePushedVersion{
			SceneID:      sceneID,
			SceneVersion: sceneVersion,
			DefinitionID: definitionID,
			GraphJSON:    mustJSON(graph),
			BundleJSON:   mustJSON(bundle),
			CreatedAt:    time.Now(),
		}

		// ADR 007 §C.2/§C.4 — additive LSML persist + adopt-on-verify,
		// gated by ORION_LSDP_MODE. In bespoke mode (the default) this is
		// skipped entirely, so the pushed-version row carries NULL LSML
		// columns, scene_version stays the legacy mint, and the deploy is
		// a no-op. In dual|lsdp the compiler's expanded tree is also
		// emitted as an LSML 1.1 bundle (EmitLSML, C1) and persisted
		// beside the bespoke RenderBundle, content-addressed by its own
		// lsml.HashBundle. A failure to emit is a warning, never a push
		// failure — the bespoke path already succeeded by this point.
		//
		// C4 identity collapse (adopt-on-verify, never adopt-on-trust):
		// when Canvas supplies its own lsml_bundle_hash in the envelope
		// AND it byte-matches Orion's freshly-recomputed hash, Orion
		// adopts that hash as the scene_version. The two previously
		// distinct addresses (the bespoke graph+bundle scene_version and
		// the LSML content address) collapse into one, so the C2 serve
		// at /lsml-bundle?v={scene_version} resolves on the same identity.
		// On mismatch (drift) or absence (old Canvas), scene_version stays
		// the legacy mint and a mismatch emits an LSML_HASH_MISMATCH
		// warning — Orion never blindly trusts a hash it did not verify.
		var lsmlWarnings []compiler.Diagnostic
		if deps.Config.LSDPMode.PersistsLSML() {
			sceneVersion, lsmlWarnings = persistLSMLAndMaybeAdopt(
				deps, sceneID, sceneVersion, envelope.LSMLBundleHash, bundle, &pv)
		}

		// Persist definition + pushed version + advance pointer in ONE
		// transaction so external observers never see torn state AND so the
		// definition_version sequence is race-safe (R2). NextDefinitionVersionTx
		// takes a SELECT ... FOR UPDATE on the scenes row: concurrent first-pushes
		// of the same scene_id serialise on that lock, so each reads a fresh
		// MAX(definition_version) and they get distinct versions instead of all
		// computing 1 and colliding on UNIQUE(scene_id, definition_version)
		// → 23505 → 500 (Probe #56 / PR #61). InsertPushedVersion's
		// ON CONFLICT DO NOTHING keeps the deterministic-scene_version re-push
		// idempotent (criterion 6.2/6.8).
		err = deps.Store.Tx(ctx, func(tx store.Tx) error {
			nextDefVer, err := deps.Store.NextDefinitionVersionTx(ctx, tx, sceneID)
			if err != nil {
				return err
			}
			definition := store.SceneDefinition{
				ID:                definitionID,
				SceneID:           sceneID,
				DefinitionVersion: nextDefVer,
				CanvasVersion:     envelope.CanvasVersion,
				BlueBlueprintID:   envelope.BlueBlueprintID,
				ComponentsJSON:    mustJSON(envelope.Components),
				CreatedAt:         time.Now(),
			}
			if err := deps.Store.InsertDefinitionTx(ctx, tx, definition); err != nil {
				return err
			}
			if err := deps.Store.InsertPushedVersion(ctx, tx, pv); err != nil {
				return err
			}
			return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, &sceneVersion)
		})
		if err != nil {
			deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// Surface the new version into the runtime — UNDER the validation
		// gate and the on-air swap guard (see surfacePushedVersion).
		notValidated, airVersion, serr := surfacePushedVersion(ctx, deps, sceneID, sceneVersion, graph, bundle)
		if serr != nil {
			deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// Record the compile-input → scene_version mapping so an identical
		// re-push in this process skips the compile entirely (idempotence).
		if fpErr == nil {
			dedup.put(fingerprint, sceneVersion)
		}

		deps.Metrics.PushTotal.WithLabelValues("ok").Inc()
		deps.Metrics.PushDuration.WithLabelValues("ok").Observe(time.Since(started).Seconds())

		// Warnings reach the wire. A hash mismatch used to exist only in
		// Orion's own log, which made it invisible to the producer: from
		// Canvas's side a non-adopted identity looks exactly like a bespoke-mode
		// push or an older Orion, so it could not tell "your bundle drifted"
		// from "this deployment does not collapse identities" and had to mirror
		// the legacy mint either way. Surfacing the diagnostic is what lets the
		// caller refuse instead of guess — the field already existed and Canvas
		// already relays it verbatim, so this is purely additive.
		warnings := lsmlWarnings
		if warnings == nil {
			warnings = []compiler.Diagnostic{}
		}
		resp := map[string]any{
			"scene_version": sceneVersion,
			"diagnostics": map[string]any{
				"errors":   []string{},
				"warnings": warnings,
			},
		}
		if notValidated {
			// Authoring succeeded (200); the antenna did not move.
			resp["code"] = sceneNotValidatedCode
			resp["air_version"] = airVersion
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// surfacePushedVersion brings a persisted pushed version onto the runtime,
// shared by the compile path and the idempotent-reuse path. It composes two
// invariants:
//
//   - Validation gate (ADR 003 §3.2.2, B3 — critical). Off-air scenes load
//     freely (activation is separately gated by postActiveScene); an active
//     scene swaps only when the new version is validated. A not-validated
//     re-push of the active scene still persisted upstream (authoring is
//     never blocked) but the antenna keeps the last validated version —
//     surfaced as SCENE_NOT_VALIDATED with the held air_version.
//
//   - On-air swap guard (switch fix). A re-push of the version ALREADY on
//     air is a silent no-op: no LoadExec, no scene_changed, no snapshot. A
//     byte-identical re-push of the live scene therefore never reloads the
//     antenna — closing the visible-reload / black-screen-recidive class
//     (runbook canevas-chat-sponso, 2026-06-29), where a redundant re-push
//     took the antenna immediately. Only a REAL content change (a different
//     scene_version) moves air.
//
// Returns notValidated (the antenna held on an unproven version) and, when
// it did, the air_version the caller surfaces.
func surfacePushedVersion(
	ctx context.Context,
	deps PublicDeps,
	sceneID uuid.UUID,
	sceneVersion string,
	graph *compiler.Graph,
	bundle *compiler.RenderBundle,
) (notValidated bool, airVersion string, err error) {
	active := deps.Show.Active()
	isActive := active != nil && active.ID() == sceneID.String()
	if isActive {
		// On-air swap guard: the version already aired is byte-identical to
		// what is running, so a re-push changes nothing — no reload.
		if g := active.Graph(); g != nil && g.SceneVersion == sceneVersion {
			return false, "", nil
		}
		// Push-swap of the LIVE scene (B3, criterion #9), gated through
		// execForAir: validated → swap in with its exec installed (R9 lift);
		// not validated → keep the last validated version. Fail-closed on a
		// DB error.
		progs, eligible, gerr := execForAir(ctx, deps, sceneID, sceneVersion, graph)
		if gerr != nil {
			return false, "", gerr
		}
		if eligible {
			deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)
			active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
			active.EmitFreshSnapshot()
			return false, "", nil
		}
		// Antenna unchanged: the live graph keeps serving the last validated
		// version until the author validates this one.
		return true, active.Graph().SceneVersion, nil
	}
	// Off-air scene: load/swap the roster instance freely. A freshly pushed
	// version is never validated yet (new hash, no record) so execForAir
	// returns nil and exec stays uninstalled; the re-push of an
	// already-validated byte-identical version arms its exec now so a later
	// activation airs it live. Loading off-air touches no antenna.
	progs, _, gerr := execForAir(ctx, deps, sceneID, sceneVersion, graph)
	if gerr != nil {
		return false, "", gerr
	}
	deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)
	return false, "", nil
}

// serveIdempotentPush handles a fingerprint cache hit: it resolves the
// already-compiled pushed version from the store, advances the latest
// pointer, surfaces it (under the same gate + on-air guard as a fresh
// compile), and writes the 200 response. It returns true when it fully
// handled the request; false means the stored version is gone (the caller
// falls through to a normal compile). No Compile, no upstream fetch.
func serveIdempotentPush(
	ctx context.Context,
	w http.ResponseWriter,
	deps PublicDeps,
	sceneID uuid.UUID,
	sceneVersion string,
) bool {
	pv, err := deps.Store.GetPushedVersion(ctx, sceneID, sceneVersion)
	if err != nil {
		// Missing (purged/archived) → let the caller recompile. Any other
		// read error also falls through: a real compile will surface it.
		return false
	}
	graph := &compiler.Graph{}
	bundle := &compiler.RenderBundle{}
	if json.Unmarshal(pv.GraphJSON, graph) != nil || json.Unmarshal(pv.BundleJSON, bundle) != nil {
		return false
	}

	// Advance the latest pointer exactly as a fresh push would (the artefact
	// already exists, so this is the only persisted mutation).
	if err := deps.Store.Tx(ctx, func(tx store.Tx) error {
		return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, &pv.SceneVersion)
	}); err != nil {
		deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return true
	}

	notValidated, airVersion, serr := surfacePushedVersion(ctx, deps, sceneID, pv.SceneVersion, graph, bundle)
	if serr != nil {
		deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return true
	}

	deps.Metrics.PushTotal.WithLabelValues("idempotent").Inc()

	resp := map[string]any{
		"scene_version": pv.SceneVersion,
		"idempotent":    true,
		"diagnostics": map[string]any{
			"errors":   []string{},
			"warnings": []string{},
		},
	}
	if notValidated {
		resp["code"] = sceneNotValidatedCode
		resp["air_version"] = airVersion
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// persistLSMLAndMaybeAdopt emits the LSML 1.1 bundle for a compiled
// scene, persists it on the pushed-version row, and resolves the C4
// identity question. It returns the scene_version the caller must use
// as the pushed-version PK + latest pointer, plus the warnings the
// caller must put on the wire (empty on every path but the mismatch).
//
// Behaviour (ADR 007 §C.4):
//   - emit fails        → log a warning, persist nothing LSML-side, keep
//     the legacy scene_version. Never fails the push.
//   - canvasHash == ""  → old/legacy Canvas (or LSML-unaware push). Persist
//     the LSML bundle keyed by its own hash for the C2 serve, but DO NOT
//     collapse identity — scene_version stays the legacy mint. No warning.
//   - canvasHash matches → adopt-on-verify: scene_version becomes the LSML
//     content address, collapsing the two addresses. pv.SceneVersion and
//     pv.LSMLBundleHash are realigned to the adopted hash so the C2 serve
//     at ?v={scene_version} resolves on the unified identity.
//   - canvasHash differs → drift. Keep the legacy mint, still persist the
//     LSML bundle under its own (Orion-computed) hash, and emit an
//     LSML_HASH_MISMATCH warning — in the log AND on the wire. Never
//     fails, never silently adopts.
func persistLSMLAndMaybeAdopt(
	deps PublicDeps,
	sceneID uuid.UUID,
	sceneVersion string,
	canvasHash string,
	bundle *compiler.RenderBundle,
	pv *store.ScenePushedVersion,
) (string, []compiler.Diagnostic) {
	// EmitLSML MUST read the AUTHORING tree, not the lowered render Root.
	// The LSML 1.1 bundle is authoring-vocab (ADR 007 §9.6), and C4
	// adopt-on-verify compares this hash against Prism's, which is computed
	// from the authoring tree (`sceneToLsml`). Feeding bundle.Root (lowered
	// to `size`/`colour`/`width`/`kind`) emitted render-vocab LSML and made
	// HashBundle diverge from Prism's → LSML_HASH_MISMATCH never collapsed
	// (Vigil's finding on PR #42). bundle.AuthoringRoot is the pre-lowering
	// `expanded` tree the compiler now carries (compiler.RenderBundle,
	// json:"-" — never on the wire, so Solar's served Root stays lowered).
	// The trailing nil/assets args carry the operator-authored animation
	// tree (always nil on this path) and the bundle-level asset block
	// (allowedHosts/fonts/preload) the compiler lifted from the authoring
	// layout. EmitLSML preserves the asset block verbatim so the host
	// allowlist reaches Solar's runtime gate (ADR 002 §3.4 T6) — Orion
	// fabricates no host and strips nothing.
	lsmlBundle, lsmlHash, _, emitErr := compiler.EmitLSML(
		sceneID.String(), bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, bundle.LSMLAssets,
	)
	if emitErr != nil {
		deps.Logger.Warn("lsml emit failed; persisting bespoke only",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "error", emitErr)
		return sceneVersion, nil
	}

	pv.LSMLBundleJSON = mustJSON(lsmlBundle)
	pv.LSMLBundleHash = &lsmlHash

	if canvasHash == "" {
		// No Canvas-supplied identity to reconcile: persist for the C2
		// serve only, identity stays the legacy mint. NOT a mismatch, so no
		// warning: the caller supplied nothing to contradict.
		return sceneVersion, nil
	}

	if canvasHash == lsmlHash {
		// Byte-match: adopt the LSML content address as scene_version.
		// The two addresses collapse; the C2 serve resolves at
		// ?v={scene_version}. Realign the PK so the persisted row is
		// keyed by the unified identity.
		pv.SceneVersion = lsmlHash
		deps.Logger.Info("lsml identity adopted (byte-match)",
			"scene_id", sceneID.String(), "scene_version", lsmlHash)
		return lsmlHash, nil
	}

	// Mismatch: drift between Canvas's hash and Orion's recomputed hash.
	// Never adopt — fall back to the legacy mint and surface a warning.
	deps.Logger.Warn("LSML_HASH_MISMATCH",
		"scene_id", sceneID.String(),
		"canvas_hash", canvasHash,
		"orion_hash", lsmlHash,
		"scene_version", sceneVersion)

	// Both hashes go on the wire. Without them the producer learns only that
	// its identity was not adopted, which is also what a bespoke-mode Orion
	// reports — the two values are what make the drift diagnosable rather
	// than merely signalled. Neither is a secret: the supplied one is the
	// caller's own, the recomputed one addresses content the caller may fetch.
	var diags compiler.Diagnostics
	diags.AddWarning(compiler.WarnLSMLHashMismatch,
		"supplied lsml_bundle_hash %s does not match the bundle Orion emitted (%s); "+
			"identity not collapsed, scene_version stays %s",
		canvasHash, lsmlHash, sceneVersion)
	return sceneVersion, diags.Warnings()
}

// writePushError translates a *compiler.CompileError into the
// chantier-spec'd response shape.
func writePushError(w http.ResponseWriter, err error) {
	var ce *compiler.CompileError
	if errors.As(err, &ce) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code": "COMPILE_FAILED",
			"diagnostics": map[string]any{
				"errors":   ce.Diagnostics.Errors(),
				"warnings": ce.Diagnostics.Warnings(),
			},
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL", "error": err.Error()})
}

func handleRollback(ctx context.Context, w http.ResponseWriter, deps PublicDeps, sceneID uuid.UUID, target string) {
	pv, err := deps.Store.GetPushedVersion(ctx, sceneID, target)
	if err != nil {
		status, code := codeFromError(err)
		writeJSON(w, status, map[string]string{"code": code})
		return
	}

	// Validation gate (ADR 003 §3.2.2, B-rollback — critical). Rollback
	// re-points the live graph WITHOUT recompile, so it is air-eligible
	// only if the target version carries a `validated` record for the
	// current harness_version. Otherwise it is refused — including a
	// version whose record an archive purge deleted (ON DELETE CASCADE):
	// re-pushing byte-identical content re-mints the hash but resurrects
	// no record, so rollback to it still refuses until re-validation.
	eligible, gerr := isAirEligible(ctx, deps, sceneID, pv.SceneVersion)
	if gerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	if !eligible {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code":          sceneNotValidatedCode,
			"scene_version": pv.SceneVersion,
		})
		return
	}

	err = deps.Store.Tx(ctx, func(tx store.Tx) error {
		return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, &pv.SceneVersion)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}

	// Re-load the runtime scene from the rolled-back artefacts, WITH its
	// exec installed (R9 lift, ADR 006 §3.4). The rollback target passed
	// the gate above, so it is validated by definition → execForAir
	// resolves its program set (fail-loud only if the validated artefact
	// is corrupt). The seam keeps the install keyed on the SAME validation
	// record the gate just checked — no path around #87.
	graph := &compiler.Graph{}
	bundle := &compiler.RenderBundle{}
	if err := json.Unmarshal(pv.GraphJSON, graph); err == nil {
		_ = json.Unmarshal(pv.BundleJSON, bundle)
		progs, _, perr := execForAir(ctx, deps, sceneID, pv.SceneVersion, graph)
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)
		if active := deps.Show.Active(); active != nil && active.ID() == sceneID.String() {
			active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
			active.EmitFreshSnapshot()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scene_version": pv.SceneVersion,
		"rolled_back":   true,
	})
}

// mustJSON marshals or panics. Used for fields whose error path is
// already covered upstream — a compile failure shouldn't leave us
// trying to marshal the artefacts here.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
