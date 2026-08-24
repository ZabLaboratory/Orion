package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

const maxStreamRuleBlueResponse = 16 << 20
const maxStreamRuleBlueErrorResponse = 64 << 10

// StreamRulesDeps wires the restored ADR 009 HTTP surface to Orion's
// volatile Engine B RulePlane. BlueBaseURL is the existing Blue edge (the
// gateway in production). TokenFunc mints the service bearer for the two
// synchronous Blue reads needed to resolve and compile the published program.
// No credential or program is persisted.
type StreamRulesDeps struct {
	Plane       *bluehost.RulePlane
	BlueBaseURL string
	HTTPClient  *http.Client
	TokenFunc   func() string
}

type streamRuleLoadError struct {
	status         int
	code           string
	message        string
	upstreamStatus int
	upstream       any
}

func (e *streamRuleLoadError) Error() string { return e.message }

type blueBlueprintRead struct {
	Status         string `json:"status"`
	CurrentVersion int    `json:"current_version"`
}

type blueCompileResponse struct {
	SchemaVersion      string `json:"schema_version"`
	ProgramDigest      string `json:"program_digest"`
	ProgramBytesBase64 string `json:"program_bytes_base64"`
}

// postStreamRule promotes a published Blue blueprint directly into the
// global stream-rule plane. The public command remains the frozen ADR 009
// body/response: POST {"blueprint_id":"..."} -> {"stream_rule_id":"..."}.
// Scene-backed promotion is intentionally not resurrected: a stream rule is
// not a scene and no Canvas artefact participates in this path.
func postStreamRule(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SceneID     string `json:"scene_id"`
			BlueprintID string `json:"blueprint_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxOperatorBody)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if body.BlueprintID == "" {
			code := "BLUEPRINT_ID_REQUIRED"
			message := "blueprint_id is required; stream rules are not scenes"
			if body.SceneID != "" {
				code = "SCENE_STREAM_RULE_UNSUPPORTED"
			}
			writeOperatorError(w, http.StatusBadRequest, code, message)
			return
		}
		if _, err := uuid.Parse(body.BlueprintID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid blueprint_id"})
			return
		}
		if deps.StreamRules == nil || deps.StreamRules.Plane == nil {
			writeOperatorError(w, http.StatusServiceUnavailable, "STREAM_RULES_UNAVAILABLE", "stream rule runtime is not wired")
			return
		}

		program, digest, err := loadStreamRuleProgram(r, *deps.StreamRules, body.BlueprintID)
		if err != nil {
			var loadErr *streamRuleLoadError
			if errors.As(err, &loadErr) {
				writeStreamRuleLoadError(w, loadErr)
				return
			}
			writeOperatorError(w, http.StatusBadGateway, "BLUEPRINT_COMPILE_FAILED", "could not load the published Blue program")
			return
		}
		if err := deps.StreamRules.Plane.Promote(body.BlueprintID, digest, program); err != nil {
			if deps.Logger != nil {
				deps.Logger.Error("stream rule promotion failed", "rule_id", body.BlueprintID, "err", err)
			}
			writeOperatorError(w, http.StatusUnprocessableEntity, "PROGRAM_REJECTED", "Blue program was rejected by the runtime")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"stream_rule_id": body.BlueprintID})
	})
}

// getStreamRules lists the volatile active set. Prism compares it with its
// durable intent and replays missing ids after an Orion restart.
func getStreamRules(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		ids := []string{}
		if deps.StreamRules != nil && deps.StreamRules.Plane != nil {
			ids = deps.StreamRules.Plane.IDs()
		}
		writeJSON(w, http.StatusOK, map[string]any{"stream_rules": ids})
	})
}

// deleteStreamRule demotes a global rule idempotently. The historical 200
// response shape is retained for cockpit/Prism compatibility.
func deleteStreamRule(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		if deps.StreamRules != nil && deps.StreamRules.Plane != nil {
			if err := deps.StreamRules.Plane.Demote(id); err != nil {
				writeOperatorError(w, http.StatusInternalServerError, "INTERNAL", "stream rule demotion failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"demoted_scene_id": id})
	})
}

func loadStreamRuleProgram(r *http.Request, deps StreamRulesDeps, blueprintID string) ([]byte, string, error) {
	base, err := url.Parse(strings.TrimRight(deps.BlueBaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, "", &streamRuleLoadError{status: http.StatusServiceUnavailable, code: "BLUE_UNAVAILABLE", message: "Blue base URL is not configured"}
	}
	client := deps.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	var blueprint blueBlueprintRead
	if err := streamRuleBlueJSON(r, client, deps.TokenFunc, http.MethodGet, base.String()+"/api/v1/blueprints/"+url.PathEscape(blueprintID), nil, &blueprint); err != nil {
		return nil, "", err
	}
	if blueprint.Status != "published" || blueprint.CurrentVersion <= 0 {
		return nil, "", &streamRuleLoadError{status: http.StatusConflict, code: "BLUEPRINT_NOT_PUBLISHED", message: "stream rule blueprint has no current published version"}
	}

	body, err := json.Marshal(map[string]any{"pins": []map[string]any{{
		"blueprint_id": blueprintID,
		"version":      blueprint.CurrentVersion,
	}}})
	if err != nil {
		return nil, "", err
	}
	var compiled blueCompileResponse
	if err := streamRuleBlueJSON(r, client, deps.TokenFunc, http.MethodPost, base.String()+"/api/v1/programs/compile", body, &compiled); err != nil {
		return nil, "", err
	}
	if compiled.SchemaVersion != "blue.program.v1" || compiled.ProgramDigest == "" || compiled.ProgramBytesBase64 == "" {
		return nil, "", &streamRuleLoadError{status: http.StatusBadGateway, code: "BLUEPRINT_COMPILE_INVALID", message: "Blue returned an incomplete program envelope"}
	}
	program, err := base64.StdEncoding.DecodeString(compiled.ProgramBytesBase64)
	if err != nil || len(program) == 0 {
		return nil, "", &streamRuleLoadError{status: http.StatusBadGateway, code: "BLUEPRINT_COMPILE_INVALID", message: "Blue returned invalid program bytes"}
	}
	var document struct {
		SchemaVersion string `json:"schema_version"`
		ProgramDigest string `json:"program_digest"`
	}
	if json.Unmarshal(program, &document) != nil || document.SchemaVersion != "blue.program.v1" || document.ProgramDigest != compiled.ProgramDigest {
		return nil, "", &streamRuleLoadError{status: http.StatusBadGateway, code: "BLUEPRINT_COMPILE_INVALID", message: "Blue program identity does not match its envelope"}
	}
	return program, compiled.ProgramDigest, nil
}

func streamRuleBlueJSON(origin *http.Request, client *http.Client, tokenFunc func() string, method, endpoint string, body []byte, dst any) error {
	request, err := http.NewRequestWithContext(origin.Context(), method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	// Blue authorizes Orion's internal workload, not the operator credential
	// used at the public command boundary. Keep the incoming bearer only as a
	// compatibility fallback for unit fixtures that do not configure a minter.
	if tokenFunc != nil {
		if token := tokenFunc(); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	if request.Header.Get("Authorization") == "" {
		if authorization := origin.Header.Get("Authorization"); authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
	}
	if requestID := origin.Header.Get("X-Request-ID"); requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}

	response, err := client.Do(request)
	if err != nil {
		return &streamRuleLoadError{status: http.StatusBadGateway, code: "BLUE_UNAVAILABLE", message: "Blue could not be reached"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstream := readStreamRuleBlueError(response.Body)
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			authorizationState := "absent"
			if request.Header.Get("Authorization") != "" {
				authorizationState = "present"
			}
			return &streamRuleLoadError{
				status:  http.StatusBadGateway,
				code:    "BLUE_AUTHORIZATION_FAILED",
				message: fmt.Sprintf("Blue rejected Orion's service credential (authorization header %s)", authorizationState),
			}
		case http.StatusNotFound:
			return &streamRuleLoadError{
				status:         http.StatusNotFound,
				code:           "BLUEPRINT_NOT_FOUND",
				message:        "stream rule blueprint was not found",
				upstreamStatus: response.StatusCode,
				upstream:       upstream,
			}
		default:
			return &streamRuleLoadError{
				status:         http.StatusBadGateway,
				code:           "BLUEPRINT_COMPILE_FAILED",
				message:        fmt.Sprintf("Blue returned HTTP %d: %s", response.StatusCode, formatStreamRuleBlueError(upstream)),
				upstreamStatus: response.StatusCode,
				upstream:       upstream,
			}
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxStreamRuleBlueResponse))
	if err := decoder.Decode(dst); err != nil {
		return &streamRuleLoadError{status: http.StatusBadGateway, code: "BLUE_RESPONSE_INVALID", message: "Blue returned malformed JSON"}
	}
	return nil
}

func writeStreamRuleLoadError(w http.ResponseWriter, loadErr *streamRuleLoadError) {
	if loadErr.upstream == nil {
		writeOperatorError(w, loadErr.status, loadErr.code, loadErr.message)
		return
	}
	writeJSON(w, loadErr.status, map[string]any{
		"error":           loadErr.code,
		"message":         loadErr.message,
		"upstream_status": loadErr.upstreamStatus,
		"upstream":        loadErr.upstream,
	})
}

func readStreamRuleBlueError(body io.Reader) any {
	raw, err := io.ReadAll(io.LimitReader(body, maxStreamRuleBlueErrorResponse+1))
	if err != nil || len(raw) == 0 {
		return nil
	}
	truncated := len(raw) > maxStreamRuleBlueErrorResponse
	if truncated {
		raw = raw[:maxStreamRuleBlueErrorResponse]
	}
	var structured any
	if json.Unmarshal(raw, &structured) == nil {
		if truncated {
			return map[string]any{"body": structured, "truncated": true}
		}
		return structured
	}
	return map[string]any{
		"body":      string(raw),
		"truncated": truncated,
	}
}

func formatStreamRuleBlueError(upstream any) string {
	if upstream == nil {
		return "no diagnostic body"
	}
	raw, err := json.Marshal(upstream)
	if err != nil {
		return "diagnostic body could not be encoded"
	}
	return string(raw)
}
