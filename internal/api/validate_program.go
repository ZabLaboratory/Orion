package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// C2 contre-validation surface (ADR-BLUE-012 R6 §§6.3, 4.3): unlike
// /validate/simulate (validate_simulate.go), which compiles and dry-runs an
// AUTHORING-level Blue graph through internal/compiler (Engine A), this
// route accepts an ALREADY-COMPILED blue.program.v1 document and answers
// whether Orion's Engine B (internal/bluehost + internal/providers) can
// actually ADMIT AND RUN it — the class of failure a clean compile can never
// catch, because only Orion knows its own provider registry, and admission
// alone cannot catch a node failing on its own inputs or a program-declared
// execution budget it blows through immediately. Registered beside
// scene-intent (public.go's `if deps.SceneIntent != nil` block): it needs
// the SAME Providers/Policy/ValidationMaxSteps/ValidationMaxWall
// SceneIntentDeps already carries, and is meaningless where Engine B itself
// isn't provisioned.

// validateProgramScope is the exact service-token scope POST
// /validate/program requires — same exact-membership pattern as
// simulateScope (validate_simulate.go's requireServiceScope doc), but its
// own scope: contre-validating a compiled program against the real Engine B
// registry is a materially different (and stronger) capability than
// dry-running a draft authoring graph.
const validateProgramScope = "orion.validate.program"

// maxValidateProgramBody bounds the request body — a compiled program is
// small; a larger body is refused with 413 before any decode/Load (same
// posture as maxSimulateBody).
const maxValidateProgramBody = 1 << 20 // 1 MiB

// validateProgramRequest carries the program inline, as-received JSON —
// unlike scene_intent.go's Canvas envelope, there is no signed digest claim
// to cross-check here, so no base64/hash wrapping is needed.
type validateProgramRequest struct {
	Program json.RawMessage `json:"program"`
}

// validateProgramResponse is the contre-validation verdict. Servable=true
// means Engine B admitted the program (schema/opcode/ABI checks at Load,
// every declared `requires` satisfied by the registry under Execute-mode
// policy at Start) AND ran it for real to settlement within budget — the
// same admission a real Prepare/Take performs, plus a genuine bounded
// execution neither performs synchronously. Servable=false always carries
// Code/Stage (a blue.runtime.error.v1 code and the phase it failed at —
// "load", "start", or "step") and Reason, so a caller gets a precise
// refusal, never a bare no. Target mirrors the underlying error's Target
// when the runtime pinpointed a specific node/effect/capability.
type validateProgramResponse struct {
	Servable bool              `json:"servable"`
	Code     string            `json:"code,omitempty"`
	Stage    string            `json:"stage,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Target   map[string]string `json:"target,omitempty"`
}

// postValidateProgram handles POST /api/v1/validate/program. Never touches
// deps.Host's preview/on-air slots: bluehost.ValidateProgram builds and
// discards its own throwaway runtime instance, bounded by
// deps.ValidationMaxSteps/deps.ValidationMaxWall (see that function's doc
// for why it genuinely runs the graph yet can never fire a real effect).
// Stateless — nothing here is persisted, cached, or registered across
// requests.
func postValidateProgram(deps SceneIntentDeps) http.HandlerFunc {
	return requireServiceScope(validateProgramScope, func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBounded(r.Body, maxValidateProgramBody)
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "BODY_TOO_LARGE"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}

		var body validateProgramRequest
		if jsonErr := json.Unmarshal(raw, &body); jsonErr != nil || len(body.Program) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}

		if verdict := bluehost.ValidateProgram(body.Program, deps.Providers, deps.Policy, deps.ValidationMaxSteps, deps.ValidationMaxWall); verdict != nil {
			writeJSON(w, http.StatusOK, validateProgramResponse{
				Servable: false,
				Code:     verdict.Code,
				Stage:    verdict.Stage,
				Reason:   verdict.Message,
				Target:   verdict.Target,
			})
			return
		}
		writeJSON(w, http.StatusOK, validateProgramResponse{Servable: true})
	})
}
