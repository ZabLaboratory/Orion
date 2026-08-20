package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

const streamRuleAPIBlueprintID = "45d43a69-34ff-42c9-bc5a-c333a93413e7"

func TestStreamRules_BlueLoadCockpitBothTargetsAndOperatorCall(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "marker_overlay_on", "called", "", "", "")
	var identity struct {
		ProgramDigest string `json:"program_digest"`
	}
	if err := json.Unmarshal(program, &identity); err != nil {
		t.Fatalf("decode program identity: %v", err)
	}

	var getCalls atomic.Int32
	var compileCalls atomic.Int32
	blue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer operator-token" {
			t.Errorf("Blue Authorization = %q, want activating operator bearer", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/blueprints/"+streamRuleAPIBlueprintID:
			getCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"status": "published", "current_version": 7})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/programs/compile":
			compileCalls.Add(1)
			var body struct {
				Pins []struct {
					BlueprintID string `json:"blueprint_id"`
					Version     int    `json:"version"`
				} `json:"pins"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode compile body: %v", err)
			}
			if len(body.Pins) != 1 || body.Pins[0].BlueprintID != streamRuleAPIBlueprintID || body.Pins[0].Version != 7 {
				t.Fatalf("compile pins = %#v", body.Pins)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"schema_version":       "blue.program.v1",
				"program_digest":       identity.ProgramDigest,
				"program_bytes_base64": base64.StdEncoding.EncodeToString(program),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer blue.Close()

	plane := bluehost.NewRulePlane(nil, nil, bluehost.EffectDeps{}, 1, testLogger())
	defer plane.Stop()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	defer show.Stop()
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(),
		Show:   show,
		StreamRules: &StreamRulesDeps{
			Plane:       plane,
			BlueBaseURL: blue.URL,
			HTTPClient:  blue.Client(),
		},
	})

	body := bytes.NewBufferString(`{"blueprint_id":"` + streamRuleAPIBlueprintID + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/show/stream-rules", body)
	request.Header.Set("X-Authenticated-User", "op-user")
	request.Header.Set("X-Authenticated-Role", "operator")
	request.Header.Set("Authorization", "Bearer operator-token")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), streamRuleAPIBlueprintID) {
		t.Fatalf("promote: got %d %s", recorder.Code, recorder.Body.String())
	}
	if getCalls.Load() != 1 || compileCalls.Load() != 1 {
		t.Fatalf("Blue calls get=%d compile=%d, want 1/1", getCalls.Load(), compileCalls.Load())
	}

	// Re-promoting the same published program is runtime-idempotent. Blue is
	// still the authority for current_version, so the command resolves it
	// again, but the RulePlane does not restart the matching digest.
	request = httptest.NewRequest(http.MethodPost, "/api/v1/show/stream-rules", bytes.NewBufferString(`{"blueprint_id":"`+streamRuleAPIBlueprintID+`"}`))
	request.Header.Set("X-Authenticated-User", "op-user")
	request.Header.Set("X-Authenticated-Role", "operator")
	request.Header.Set("Authorization", "Bearer operator-token")
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("idempotent promote: got %d %s", recorder.Code, recorder.Body.String())
	}

	for _, query := range []string{"?stream_id=show-1", "?stream_id=show-1&target=preview"} {
		response := opRequest(t, mux, http.MethodGet, "/api/v1/cockpit/contracts"+query, "operator", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("cockpit %s: got %d %s", query, response.Code, response.Body.String())
		}
		var contracts cockpitContracts
		if err := json.Unmarshal(response.Body.Bytes(), &contracts); err != nil {
			t.Fatalf("decode cockpit: %v", err)
		}
		if len(contracts.Triggers) != 1 {
			t.Fatalf("cockpit %s triggers = %#v", query, contracts.Triggers)
		}
		trigger := contracts.Triggers[0]
		if trigger.Scope != scopeStream || trigger.RuleID != streamRuleAPIBlueprintID || trigger.BlueprintKey != streamRuleAPIBlueprintID || trigger.EntrypointID != "marker_overlay_on" {
			t.Fatalf("cockpit %s trigger = %#v", query, trigger)
		}
	}

	call := opRequest(t, mux, http.MethodPost,
		"/api/v1/operator/call/"+streamRuleAPIBlueprintID+"/marker_overlay_on?rule="+streamRuleAPIBlueprintID,
		"operator", map[string]any{"payload": nil})
	if call.Code != http.StatusAccepted {
		t.Fatalf("Marker call: got %d %s", call.Code, call.Body.String())
	}

	listed := opRequest(t, mux, http.MethodGet, "/api/v1/show/stream-rules", "operator", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), streamRuleAPIBlueprintID) {
		t.Fatalf("list: got %d %s", listed.Code, listed.Body.String())
	}
	demoted := opRequest(t, mux, http.MethodDelete, "/api/v1/show/stream-rules/"+streamRuleAPIBlueprintID, "operator", nil)
	if demoted.Code != http.StatusOK || plane.Digest(streamRuleAPIBlueprintID) != "" {
		t.Fatalf("demote: got %d %s digest=%q", demoted.Code, demoted.Body.String(), plane.Digest(streamRuleAPIBlueprintID))
	}
}

func TestStreamRules_RejectsSceneCarrierWithoutCallingBlue(t *testing.T) {
	plane := bluehost.NewRulePlane(nil, nil, bluehost.EffectDeps{}, 1, testLogger())
	defer plane.Stop()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	defer show.Stop()
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:      testLogger(),
		Show:        show,
		StreamRules: &StreamRulesDeps{Plane: plane, BlueBaseURL: "http://127.0.0.1:1"},
	})
	response := opRequest(t, mux, http.MethodPost, "/api/v1/show/stream-rules", "operator", map[string]any{
		"scene_id": "11111111-1111-1111-1111-111111111111",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "SCENE_STREAM_RULE_UNSUPPORTED") {
		t.Fatalf("scene carrier: got %d %s", response.Code, response.Body.String())
	}
}

func TestStreamRules_PreservesBlueCompileDiagnostics(t *testing.T) {
	blue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/blueprints/"+streamRuleAPIBlueprintID:
			writeJSON(w, http.StatusOK, map[string]any{"status": "published", "current_version": 7})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/programs/compile":
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"code":    "UNSUPPORTED_FIELD",
				"message": "graph.nodes[3].config.foo is not supported",
				"errors": []map[string]string{{
					"path":    "graph.nodes[3].config.foo",
					"message": "remove the field before compiling",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer blue.Close()

	plane := bluehost.NewRulePlane(nil, nil, bluehost.EffectDeps{}, 1, testLogger())
	defer plane.Stop()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	defer show.Stop()
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(),
		Show:   show,
		StreamRules: &StreamRulesDeps{
			Plane:       plane,
			BlueBaseURL: blue.URL,
			HTTPClient:  blue.Client(),
		},
	})

	response := opRequest(t, mux, http.MethodPost, "/api/v1/show/stream-rules", "operator", map[string]any{
		"blueprint_id": streamRuleAPIBlueprintID,
	})
	body := response.Body.String()
	if response.Code != http.StatusBadGateway {
		t.Fatalf("compile failure: got %d %s", response.Code, body)
	}
	for _, expected := range []string{
		`"error":"BLUEPRINT_COMPILE_FAILED"`,
		`"upstream_status":422`,
		`"code":"UNSUPPORTED_FIELD"`,
		`graph.nodes[3].config.foo is not supported`,
		`remove the field before compiling`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("compile failure missing %q in %s", expected, body)
		}
	}
}
