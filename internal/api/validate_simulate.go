package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Service-scoped simulate endpoint (ADR 015): a synchronous dry-run of a
// draft blueprint graph against a synthetic event, for the bluemcp agent
// (a service token). It exposes the EXISTING validation harness in an
// event-targeted firing mode — no new engine, no persistence, no roster
// reload, zero external effect (B10). Surface separate from /show/*
// (operator-only, untouched) and from /scenes/{id}/* (pushed scenes).
//
//	POST /api/v1/validate/simulate
//
// Auth is a fail-closed gate-by-scope (requireServiceScope) reusing the
// exact-membership rule of issue #86 (hasExactScope) — role=service AND
// the EXACT scope `orion.validate.session`; a parent/wildcard scope
// (orion.validate, orion.*) does NOT pass (R3, privilege escalation). A
// frank 403 is returned (unlike #86's silent drop: simulate has no parked
// continuation to hide — §3.2 R3).

// simulateScope is the exact service-token scope the simulate route
// requires (ADR 015 §3.2). First scope on the new requireServiceScope
// surface; matched by set membership, never hierarchically.
const simulateScope = "orion.validate.session"

// maxSimulateBody bounds the request body (ADR 015 §3.2). A draft graph is
// small; a larger body is refused with 413 before any decode/exec.
const maxSimulateBody = 1 << 20 // 1 MiB

// errBodyTooLarge signals the body exceeded maxSimulateBody.
var errBodyTooLarge = errors.New("request body too large")

// requireServiceScope gates a handler on role=service AND exact membership
// of `scope` in the token's X-Authenticated-Paths (ADR 015 §3.2, reusing
// the #86 hasExactScope rule). Fail-closed: header absent, role≠service,
// or a parent/wildcard/superstring scope ⇒ 403 {"code":"FORBIDDEN"}.
func requireServiceScope(scope string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := auth.FromHeaders(r.Header)
		if id.Role != auth.RoleService || !hasExactScope(id, scope) {
			writeJSON(w, http.StatusForbidden, map[string]string{"code": "FORBIDDEN"})
			return
		}
		handler(w, r)
	}
}

type simulateBody struct {
	Graph          json.RawMessage        `json:"graph"`
	SyntheticEvent runtime.SyntheticEvent `json:"synthetic_event"`
	Entrypoints    []string               `json:"entrypoints,omitempty"`
}

// postSimulate handles POST /api/v1/validate/simulate. It decodes a draft
// graph + synthetic event, fires the matched entrypoints in the harness's
// event-targeted mode, and returns the ValidationReport verbatim (200).
// Corrupt graph → 400 INVALID_GRAPH; body over the bound → 413.
func postSimulate(deps PublicDeps) http.HandlerFunc {
	return requireServiceScope(simulateScope, func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBounded(r.Body, maxSimulateBody)
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "BODY_TOO_LARGE"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}

		var body simulateBody
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Graph) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}

		// Decode the AUTHORING-level Blue graph the caller submits — a
		// compiler.BlueprintGraph ({nodes, edges, variables}), NOT a
		// compiler.Graph (ADR 015 Amendment 1 §A1.2). The earlier code decoded
		// into compiler.Graph, whose ExecPrograms field nothing in a Blue graph
		// fills, so the harness fired zero programs and returned blueprints:null
		// muet (#199, A1.1). A corrupt artefact, or one with no nodes, is a
		// malformed body → 400 INVALID_GRAPH.
		bp := &compiler.BlueprintGraph{}
		if err := json.Unmarshal(body.Graph, bp); err != nil || len(bp.Nodes) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_GRAPH"})
			return
		}

		// Compile the exec layer in-body, with NO Fetcher and zero egress
		// (§A1.3 step 2 / §A1.4): the partition reads only the in-body graph.
		// A compile error (unknown exec op, dangling exec target, cyclic
		// component, or a descoped `reference` node) → 400 COMPILE_FAILED with
		// the diagnostics — DISTINCT from INVALID_GRAPH (a malformed body) and
		// from INVALID_BUNDLE. Never a silent blueprints:null on a graph the
		// compiler rejects (§A1.3 step 3).
		compiled, cerr := compiler.CompileExecPrograms(bp, "")
		if cerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code":        "COMPILE_FAILED",
				"diagnostics": compileDiagnostics(cerr),
			})
			return
		}

		// Build the minimal data-empty Graph the harness drives: the compiled
		// exec programs + the variable seeds, nothing else (rendering and the
		// data tranche are not exercised in simulate — §A1.3 step 2).
		graph := &compiler.Graph{
			SceneID:      bp.ID,
			ExecPrograms: compiled.Programs,
			Defaults:     compiled.Defaults,
		}
		progs, err := runtime.ExecProgramsFromGraph(graph)
		if err != nil {
			// The programs were just marshalled by our own compiler, so a
			// decode failure here is an internal contract break, not bad input.
			deps.Logger.Error("simulate: exec program decode of freshly compiled graph failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// B10 structural guard FIRST: refuse to run against a world-effect
		// registry missing a validation-mode behaviour (same guard the
		// campaign runs — §3.3, R4).
		if err := runtime.ValidateValidationModeCoverage(); err != nil {
			deps.Logger.Error("simulate: validation-mode coverage guard failed (B10)", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// Rendering is not exercised in simulate; an empty bundle suffices.
		bundle := &compiler.RenderBundle{}
		report := deps.Harness.Simulate(graph, bundle, progs, body.SyntheticEvent, body.Entrypoints)
		writeJSON(w, http.StatusOK, report)
	})
}

// compileDiagnostics projects a *compiler.CompileError into the
// {code, message} list the COMPILE_FAILED response carries (ADR 015 §A1.3
// step 3). Only error-severity items surface — a warning never fails the
// compile, so it would mislead the caller into thinking the graph was
// rejected. Path is included when present so the agent can point at the
// offending node.
func compileDiagnostics(cerr *compiler.CompileError) []map[string]string {
	out := make([]map[string]string, 0, len(cerr.Diagnostics.Items))
	for _, it := range cerr.Diagnostics.Items {
		if it.Severity != "error" {
			continue
		}
		d := map[string]string{"code": string(it.Code), "message": it.Message}
		if it.Path != "" {
			d["path"] = it.Path
		}
		out = append(out, d)
	}
	return out
}

// readBounded reads up to max bytes; if the body has more, it returns
// errBodyTooLarge. It reads max+1 to distinguish "exactly max" from "over".
func readBounded(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyTooLarge
	}
	return raw, nil
}
