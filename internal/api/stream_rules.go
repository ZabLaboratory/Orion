package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
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
			SceneID     string `json:"scene_id"`
			BlueprintID string `json:"blueprint_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}

		// Blueprint-direct stream rule (no carrier scene). A blueprint is a
		// blueprint, not a scene: the pilotage registers it directly. We
		// compile the bare Blue graph (the simulate machinery: FetchBlueprint →
		// CompileExecPrograms → ExecProgramsFromGraph) and promote it keyed by
		// blueprint_id, with an empty bundle — a rule runs exec, it never
		// renders. In-memory only for now: the show_stream_rules reseed is
		// scene-based, so blueprint-rule durability across an Orion restart is
		// a follow-up (a rule_kind column + a blueprint reseed path).
		if body.BlueprintID != "" {
			promoteBlueprintStreamRule(w, r, deps, body.BlueprintID)
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

// getStreamRules lists the promoted stream-level rules — the backing surface
// of the cockpit's "Blueprints Stream-level" tab. Returns the in-memory rule
// id set (scene_id OR blueprint_id keys), the authority that includes the
// blueprint-direct rules the store does not persist yet.
func getStreamRules(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stream_rules": deps.Show.StreamRuleIDs()})
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
		// Read the kind BEFORE demote (the map entry is gone after) so we clean
		// the matching persistence table (#287). scene → show_stream_rules,
		// blueprint-direct → show_blueprint_stream_rules.
		kind, wasRule := deps.Show.StreamRuleKind(sid.String())
		// In-memory demote first (cancels live tasks, clears the on-air flag
		// unless it is the active scene). Safe even if the id is not a rule.
		deps.Show.DemoteStreamRule(sid.String())
		// Drop the persisted selection (idempotent — zero rows if absent). When
		// the id is not a live rule (already demoted / never promoted) the kind
		// is unknown, so clean BOTH tables to keep re-demote idempotent.
		var rmErr error
		switch {
		case wasRule && kind == runtime.RuleKindBlueprint:
			rmErr = deps.Store.RemoveBlueprintStreamRule(r.Context(), sid)
		case wasRule:
			rmErr = deps.Store.RemoveStreamRule(r.Context(), sid)
		default:
			if err := deps.Store.RemoveStreamRule(r.Context(), sid); err != nil {
				rmErr = err
			}
			if err := deps.Store.RemoveBlueprintStreamRule(r.Context(), sid); err != nil {
				rmErr = err
			}
		}
		if rmErr != nil {
			deps.Logger.Error("remove stream rule failed", "rule_id", sid.String(), "err", rmErr)
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

// promoteBlueprintStreamRule promotes a Blue blueprint DIRECTLY into a
// stream-level rule, with no carrier scene (a blueprint is a blueprint, not a
// scene — the pilotage owns it). It fetches the blueprint's current published
// graph from Blue, compiles its exec layer in-body (the SAME machinery as the
// simulate endpoint: CompileExecPrograms → ExecProgramsFromGraph), and promotes
// it keyed by blueprint_id with an empty RenderBundle — a rule runs exec, it
// never renders. The slot-assignment effects + viewer arming the rule emits are
// stream-level (survive scene switches), so this is the natural home of the
// cam-arming rule (ADR Blue 009 §3.3).
func promoteBlueprintStreamRule(w http.ResponseWriter, r *http.Request, deps PublicDeps, blueprintID string) {
	if _, ok := parseUUID(blueprintID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid blueprint_id"})
		return
	}
	bp, err := deps.Fetcher.FetchBlueprint(r.Context(), blueprintID)
	if err != nil {
		deps.Logger.Error("stream rule: fetch blueprint failed", "blueprint_id", blueprintID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "BLUEPRINT_FETCH_FAILED"})
		return
	}
	compiled, cerr := compiler.CompileExecPrograms(bp, "")
	if cerr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":        "COMPILE_FAILED",
			"diagnostics": compileDiagnostics(cerr),
		})
		return
	}
	graph := &compiler.Graph{
		SceneID:      bp.ID,
		ExecPrograms: compiled.Programs,
		Defaults:     compiled.Defaults,
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		deps.Logger.Error("stream rule: exec decode of freshly compiled blueprint failed", "blueprint_id", blueprintID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	if err := deps.Show.PromoteBlueprintStreamRule(blueprintID, graph, &compiler.RenderBundle{}, progs...); err != nil {
		if errors.Is(err, runtime.ErrRuleIsActiveScene) {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		deps.Logger.Error("stream rule: promote blueprint failed", "blueprint_id", blueprintID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	// Persist the blueprint-direct selection so it survives a restart (#287).
	// Best-effort after the in-memory promotion, mirroring the scene path: the
	// rule already runs; a DB write failure must not fail a live promotion, but
	// it is logged so a persistence outage is visible. bpID parsed above.
	if bpUUID, ok := parseUUID(blueprintID); ok {
		if err := deps.Store.AddBlueprintStreamRule(r.Context(), bpUUID); err != nil {
			deps.Logger.Error("persist blueprint stream rule failed", "blueprint_id", blueprintID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"stream_rule_id": blueprintID})
}

// ReloadBlueprintStreamRules reseeds the persisted blueprint-direct stream
// rules into the roster after a restart (#287). Distinct from reloadStreamRules
// (scene-based, which reseeds from stored pushed versions): a blueprint-direct
// rule has no carrier scene / pushed version, so it reseeds by re-fetching +
// recompiling from Blue — the SAME machinery as promoteBlueprintStreamRule
// (FetchBlueprint → CompileExecPrograms(bp, "") → ExecProgramsFromGraph). Only
// IDENTITY is durable: the rule reseeds from declared defaults and fires
// on-start once (criterion #11, ADR 009 §3.4) — no live leaf state is restored.
// Fail-soft per rule: an unreachable/deleted blueprint is skipped, never aborts
// boot. Must run AFTER the compiler fetcher is wired (unlike the scene reseed,
// which needs no fetcher), so cmd/orion calls it post-selectFetcher.
func ReloadBlueprintStreamRules(ctx context.Context, st store.Store, fetcher compiler.Fetcher, show *runtime.Show, logger *slog.Logger) {
	ids, err := st.ListBlueprintStreamRules(ctx)
	if err != nil {
		logger.Error("cold start: read blueprint stream rule set failed; rules stay dormant", "err", err)
		return
	}
	for _, id := range ids {
		bpID := id.String()
		bp, err := fetcher.FetchBlueprint(ctx, bpID)
		if err != nil {
			logger.Warn("cold start: blueprint stream rule fetch failed; skipped", "blueprint_id", bpID, "err", err)
			continue
		}
		compiled, cerr := compiler.CompileExecPrograms(bp, "")
		if cerr != nil {
			logger.Warn("cold start: blueprint stream rule compile failed; skipped", "blueprint_id", bpID, "err", cerr)
			continue
		}
		graph := &compiler.Graph{
			SceneID:      bp.ID,
			ExecPrograms: compiled.Programs,
			Defaults:     compiled.Defaults,
		}
		progs, err := runtime.ExecProgramsFromGraph(graph)
		if err != nil {
			logger.Warn("cold start: blueprint stream rule exec decode failed; skipped", "blueprint_id", bpID, "err", err)
			continue
		}
		if err := show.PromoteBlueprintStreamRule(bpID, graph, &compiler.RenderBundle{}, progs...); err != nil {
			logger.Warn("cold start: blueprint stream rule promotion refused; skipped", "blueprint_id", bpID, "err", err)
		}
	}
}
