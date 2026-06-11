package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// pushScene handles POST /api/v1/scenes/{id}/push. Either compiles a
// new pushed version from a definition envelope, or re-points
// latest_pushed_version at an existing scene_version (rollback).
func pushScene(deps PublicDeps) http.HandlerFunc {
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

		started := time.Now()
		graph, bundle, sceneVersion, err := compiler.Compile(ctx, sceneID.String(), envelope, deps.Fetcher)
		if err != nil {
			deps.Metrics.PushTotal.WithLabelValues("compile_error").Inc()
			deps.Metrics.PushDuration.WithLabelValues("compile_error").Observe(time.Since(started).Seconds())
			writePushError(w, err)
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
		if deps.Config.LSDPMode.PersistsLSML() {
			sceneVersion = persistLSMLAndMaybeAdopt(deps, sceneID, sceneVersion, envelope.LSMLBundleHash, bundle, &pv)
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
		err = deps.Store.Tx(ctx, func(tx pgx.Tx) error {
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
		// gate (ADR 003 §3.2.2, B3 — critical). Two cases:
		//
		//   - the scene is NOT the active one (first push, or a push to an
		//     off-air scene): load/swap freely. Loading an off-air scene
		//     touches no antenna; activation is separately gated by
		//     postActiveScene.
		//   - the scene IS active on air (mid-broadcast re-push): the swap
		//     mutates the LIVE graph, so it is gated. If the new version is
		//     validated, swap normally (criterion 9). If it is NOT, the
		//     version still PERSISTED above (authoring is never blocked),
		//     but the antenna KEEPS the last validated version — no Load, no
		//     scene_changed, no snapshot — and the response surfaces
		//     SCENE_NOT_VALIDATED so the author knows air did not move.
		notValidated := false
		active := deps.Show.Active()
		isActive := active != nil && active.ID() == sceneID.String()
		if isActive {
			// Push-swap of the LIVE scene (B3, criterion #9). The new
			// version mutates the antenna, so it goes through execForAir:
			// validated → swap in with its exec installed (R9 lift); not
			// validated → the antenna keeps the last validated version
			// (no Load, no scene_changed) though the push still persisted
			// above. Fail-closed: a DB error refuses the swap.
			progs, eligible, gerr := execForAir(ctx, deps, sceneID, sceneVersion, graph)
			if gerr != nil {
				deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
				return
			}
			if eligible {
				deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)
				active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
				active.EmitFreshSnapshot()
			} else {
				// Antenna unchanged: the live graph keeps serving the last
				// validated version until the author validates this one.
				notValidated = true
			}
		} else {
			// Off-air scene: load/swap the roster instance freely. A freshly
			// pushed version is never validated yet (new hash, no record),
			// so execForAir returns nil and exec stays uninstalled — the
			// seam is here for the re-push of an already-validated,
			// byte-identical version, which arms its exec now so a later
			// activation airs it live. Loading an off-air scene touches no
			// antenna; activation is separately gated by postActiveScene,
			// and the instance is exec-quiescent until it goes on air.
			progs, _, gerr := execForAir(ctx, deps, sceneID, sceneVersion, graph)
			if gerr != nil {
				deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
				return
			}
			deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)
		}

		deps.Metrics.PushTotal.WithLabelValues("ok").Inc()
		deps.Metrics.PushDuration.WithLabelValues("ok").Observe(time.Since(started).Seconds())

		resp := map[string]any{
			"scene_version": sceneVersion,
			"diagnostics": map[string]any{
				"errors":   []string{},
				"warnings": []string{},
			},
		}
		if notValidated {
			// Authoring succeeded (200); the antenna did not move.
			resp["code"] = sceneNotValidatedCode
			resp["air_version"] = active.Graph().SceneVersion
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// persistLSMLAndMaybeAdopt emits the LSML 1.1 bundle for a compiled
// scene, persists it on the pushed-version row, and resolves the C4
// identity question. It returns the scene_version the caller must use
// as the pushed-version PK + latest pointer.
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
//     LSML_HASH_MISMATCH warning. Never fails, never silently adopts.
func persistLSMLAndMaybeAdopt(
	deps PublicDeps,
	sceneID uuid.UUID,
	sceneVersion string,
	canvasHash string,
	bundle *compiler.RenderBundle,
	pv *store.ScenePushedVersion,
) string {
	// EmitLSML MUST read the AUTHORING tree, not the lowered render Root.
	// The LSML 1.1 bundle is authoring-vocab (ADR 007 §9.6), and C4
	// adopt-on-verify compares this hash against Prism's, which is computed
	// from the authoring tree (`sceneToLsml`). Feeding bundle.Root (lowered
	// to `size`/`colour`/`width`/`kind`) emitted render-vocab LSML and made
	// HashBundle diverge from Prism's → LSML_HASH_MISMATCH never collapsed
	// (Vigil's finding on PR #42). bundle.AuthoringRoot is the pre-lowering
	// `expanded` tree the compiler now carries (compiler.RenderBundle,
	// json:"-" — never on the wire, so Solar's served Root stays lowered).
	lsmlBundle, lsmlHash, _, emitErr := compiler.EmitLSML(
		sceneID.String(), bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if emitErr != nil {
		deps.Logger.Warn("lsml emit failed; persisting bespoke only",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "error", emitErr)
		return sceneVersion
	}

	pv.LSMLBundleJSON = mustJSON(lsmlBundle)
	pv.LSMLBundleHash = &lsmlHash

	if canvasHash == "" {
		// No Canvas-supplied identity to reconcile: persist for the C2
		// serve only, identity stays the legacy mint.
		return sceneVersion
	}

	if canvasHash == lsmlHash {
		// Byte-match: adopt the LSML content address as scene_version.
		// The two addresses collapse; the C2 serve resolves at
		// ?v={scene_version}. Realign the PK so the persisted row is
		// keyed by the unified identity.
		pv.SceneVersion = lsmlHash
		deps.Logger.Info("lsml identity adopted (byte-match)",
			"scene_id", sceneID.String(), "scene_version", lsmlHash)
		return lsmlHash
	}

	// Mismatch: drift between Canvas's hash and Orion's recomputed hash.
	// Never adopt — fall back to the legacy mint and surface a warning.
	deps.Logger.Warn("LSML_HASH_MISMATCH",
		"scene_id", sceneID.String(),
		"canvas_hash", canvasHash,
		"orion_hash", lsmlHash,
		"scene_version", sceneVersion)
	return sceneVersion
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

	err = deps.Store.Tx(ctx, func(tx pgx.Tx) error {
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
