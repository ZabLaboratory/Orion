package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// The external completion endpoint (B-syswrite, ADR 003 §3.1.3
// Amendment 1, issue #86; wire shape fixed by the phase-3 Conduit
// contract §2.2-2.4): the renderer (Solar/CEF in Pulsar) reports an
// `animation.play` completion here. This is the ONLY external producer
// of exec resumes — never a free-form `__system.*` state write.
//
// Ordered gates; ANY failure is a silent drop (counted + logged,
// resumes nothing) answered with the SAME 202 as a legitimate report,
// so a caller can never probe whether a continuation exists:
//  1. scope — role=service + `__system.anim.report` in the token's
//     `X-Authenticated-Paths`, enforced by the existing CanWritePath.
//  2. scene — `{scene_id}` routes to exactly that scene; the parked
//     map is per-scene, so a wake key of another scene can never
//     resolve (no cross-scene resume, no fan-out).
//  3. version+epoch — the existing resumeParked stamp gate (stale →
//     `orion_exec_resume_stale_total`).
//  4. parked continuation — unknown wake key → drop
//     (`orion_exec_completion_rejected_total{reason="unknown"}`).
// Gates 3-4 run on the scene goroutine; delivery is
// scene.Input(InputMsg{ResumeExec}) — cross-goroutine FIFO, never a
// direct call.
//
// R9: the route exists, but exec is dormant in prod until the phase-4
// gate (#87) — no scene has parked continuations, so every report is
// an inert 202 drop.

// animReportScope is the service-token scope the renderer's token must
// carry (contract §2.3 — an Orion-leaf-namespace scope, consumed HERE,
// never grantable as a free inbox write).
const animReportScope = "__system.anim.report"

// completionKindAnimation is the only external completion kind (#86).
const completionKindAnimation = "animation"

// maxCompletionBody bounds the report body read.
const maxCompletionBody = 64 << 10

type execCompletionBody struct {
	WakeKey string          `json:"wake_key"`
	Kind    string          `json:"kind"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *string         `json:"error"`
}

// postExecCompletion handles POST /api/v1/scenes/{id}/exec/completion.
func postExecCompletion(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sceneID := r.PathValue("id")
		reject := func(reason string) {
			if deps.Metrics != nil {
				deps.Metrics.CompletionRejected(sceneID, reason)
			}
			if deps.Logger != nil {
				deps.Logger.Warn("exec completion dropped",
					"scene_id", sceneID, "reason", reason)
			}
			completionAccepted(w) // identical to success — no leak
		}

		// Gate 1 — role + scope (existing CanWritePath enforcement).
		id := auth.FromHeaders(r.Header)
		if id.Role != auth.RoleService || !id.CanWritePath(animReportScope) {
			reject("role")
			return
		}

		var body execCompletionBody
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxCompletionBody))
		if err != nil || json.Unmarshal(raw, &body) != nil || body.WakeKey == "" {
			reject("malformed")
			return
		}
		if body.Kind != completionKindAnimation {
			reject("kind")
			return
		}

		// Gate 2 — scene match: route to exactly {scene_id}, never a
		// fan-out. Gates 3 (version+epoch) and 4 (parked continuation)
		// run inside the scene's resume path.
		scene, err := deps.Show.Get(sceneID)
		if err != nil {
			reject("scene")
			return
		}
		var errStr string
		if body.Error != nil {
			errStr = *body.Error
		}
		if !scene.Input(runtime.InputMsg{
			ResumeExec: body.WakeKey,
			ResumeEnv:  runtime.AnimReportEnv(body.Result, errStr),
			Source:     "service:" + id.UserID + "/anim-report",
		}) {
			reject("inbox")
			return
		}
		completionAccepted(w)
	}
}

// completionAccepted writes the single 202 shape every outcome shares
// (contract §2.2: never a 4xx that reveals continuation existence).
func completionAccepted(w http.ResponseWriter) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
