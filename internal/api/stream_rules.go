package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Stream-level Blue rule promotion/demotion (ADR 009 §3.1, issue #154).
//
// A promoted rule is a roster scene the operator elects to run ALWAYS —
// routed alongside the active scene (the §3.3 union, #153/#156), never
// frozen at a switch (§3.4). Promotion is operator-gated (the SAME gate as
// active-scene) and guarded:
//
//   - only a pushed, R9-VALIDATED scene is promotable (SCENE_NOT_VALIDATED
//     otherwise) — a rule executes exec logic, so it must clear the same
//     validation bar as the antenna (ADR 009 §3 "Sécurité — exec gated R9");
//   - the currently ACTIVE scene cannot be promoted (RULE_IS_ACTIVE_SCENE,
//     runtime.ErrRuleIsActiveScene) — a rule and the antenna are disjoint
//     roles (§3.1).
//
// The set is persisted (show_stream_rules) and reseeded at boot
// (cmd/orion::loadActiveScenes), so a restart restores the same rules —
// criterion #11, the same durability the active pointer gets.

// postStreamRule promotes a scene into a stream-level Blue rule.
// Body: {"scene_id": "..."}.
func postStreamRule(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SceneID string `json:"scene_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		sid, ok := parseUUID(body.SceneID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene_id"})
			return
		}

		// Reject scenes never pushed (criterion 2 parity with active-scene).
		scene, err := deps.Store.GetScene(r.Context(), sid)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		if scene.LatestPushedVersion == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "SCENE_NOT_PUSHED"})
			return
		}

		// R9 validation gate (ADR 009 §3 — a rule executes exec logic, so it
		// must be validated). Fail-closed on a DB error: an unproven scene
		// never becomes an always-on rule.
		eligible, err := isAirEligible(r.Context(), deps, sid, *scene.LatestPushedVersion)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		if !eligible {
			writeJSON(w, http.StatusConflict, map[string]string{
				"code":          sceneNotValidatedCode,
				"scene_version": *scene.LatestPushedVersion,
			})
			return
		}

		// Promote in-memory through the #153 hook (which refuses the active
		// scene → RULE_IS_ACTIVE_SCENE, loads ungated + on-air, fires
		// on-start once). Resolve the validated artefacts the hook needs.
		if err := promoteStreamRuleFromStore(r.Context(), deps, sid); err != nil {
			if errors.Is(err, runtime.ErrRuleIsActiveScene) {
				status, code := codeFromError(err)
				writeJSON(w, status, map[string]string{"code": code})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// Persist the selection so it survives a restart (criterion #11).
		// Best-effort after the in-memory promotion: the rule is already
		// running; a DB write failure must not 500 a successful promotion,
		// but it is logged so a persistence outage is visible.
		if err := deps.Store.AddStreamRule(r.Context(), sid); err != nil {
			deps.Logger.Error("persist stream rule failed", "scene_id", body.SceneID, "err", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"stream_rule_id": body.SceneID})
	})
}

// deleteStreamRule demotes a scene from the stream-level rule set, cancels
// its live tasks (DemoteStreamRule), and drops the persisted selection.
// Demotion is idempotent: demoting a non-rule is a no-op 200.
func deleteStreamRule(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sid, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		// In-memory demote first (cancels live tasks, clears the on-air flag
		// unless it is the active scene). Safe even if the id is not a rule.
		deps.Show.DemoteStreamRule(sid.String())
		// Drop the persisted selection (idempotent — zero rows if absent).
		if err := deps.Store.RemoveStreamRule(r.Context(), sid); err != nil {
			deps.Logger.Error("remove stream rule failed", "scene_id", sid.String(), "err", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"demoted_scene_id": sid.String()})
	})
}

// promoteStreamRuleFromStore resolves a scene's validated pushed-version
// artefacts and promotes it into the live roster as a stream rule through
// the #153 hook. The hook refuses the active scene (ErrRuleIsActiveScene),
// loads the instance ungated + on-air, and fires on-start once. Mirrors
// loadSceneFromStore but routes through PromoteStreamRule (which owns the
// rule-set membership + the ungated load) rather than a plain LoadExec.
func promoteStreamRuleFromStore(ctx context.Context, deps PublicDeps, sceneID uuid.UUID) error {
	pv, err := deps.Store.GetLatestPushedVersion(ctx, sceneID)
	if err != nil {
		return fmt.Errorf("promote rule: latest pushed version: %w", err)
	}
	var graph compiler.Graph
	var bundle compiler.RenderBundle
	if err := json.Unmarshal(pv.GraphJSON, &graph); err != nil {
		return fmt.Errorf("promote rule: graph json: %w", err)
	}
	if err := json.Unmarshal(pv.BundleJSON, &bundle); err != nil {
		return fmt.Errorf("promote rule: bundle json: %w", err)
	}
	// A promoted rule is always validated (the caller checked isAirEligible),
	// so execForAir returns its exec program set; pass it to PromoteStreamRule
	// so the rule runs its blueprints, not dataflow-only.
	progs, _, err := execForAir(ctx, deps, sceneID, pv.SceneVersion, &graph)
	if err != nil {
		return fmt.Errorf("promote rule: resolve exec: %w", err)
	}
	return deps.Show.PromoteStreamRule(sceneID.String(), &graph, &bundle, progs...)
}
