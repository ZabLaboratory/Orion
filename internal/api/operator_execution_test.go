package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestOperatorExecutionCorrelatesEffectsWithoutExposingPayloads(t *testing.T) {
	response := httptest.NewRecorder()
	writeOperatorExecution(response, 202, "fired", blueruntime.StepResult{Status: "runnable", Invocations: []map[string]any{{"invocation_id": "invocation-1", "capability": "arbitrary-provider", "operation": "operation", "request": map[string]any{"secret": "PRIVATE_OPERATOR_PAYLOAD"}}}})
	var body struct {
		Execution struct {
			Invocations []struct {
				InvocationID string `json:"invocation_id"`
			}
		}
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Execution.Invocations) != 1 || body.Execution.Invocations[0].InvocationID != "invocation-1" || strings.Contains(response.Body.String(), "PRIVATE_OPERATOR_PAYLOAD") {
		t.Fatal("execution correlation lost or payload exposed", response.Body.String())
	}
}

// Transport fixtures exercise arbitrary authored commands. They are not a
// functional validator and have no roster, league or business-output oracle.
func executionTransportProgram(t *testing.T, failing bool) []byte {
	t.Helper()
	await := "input"
	if failing {
		await = ""
	}
	raw := buildEngineBOperatorProgram(t, "first", "called", await, "core.json", "received")
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"custom_action_2026", "another-command", "new.command"} {
		node := fmt.Sprintf("extra-%d", index)
		doc["nodes"] = append(doc["nodes"].([]any), map[string]any{"id": node, "opcode": "core.operator.on-call@1", "config": map[string]any{}})
		doc["entrypoints"] = append(doc["entrypoints"].([]any), map[string]any{"id": name, "kind": "call", "node_id": node, "port": "then"})
		doc["exec_edges"] = append(doc["exec_edges"].([]any), map[string]any{"from_node": node, "from_port": "then", "to_node": "mark-called", "to_port": "in", "sequence": json.Number("0")})
	}
	// An operational runtime failure, without making the program malformed.
	if failing {
		doc["budgets"].(map[string]any)["max_steps_per_dispatch"] = json.Number("1")
		doc["nodes"] = append(doc["nodes"].([]any), map[string]any{"id": "second-mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "called"}})
		doc["exec_edges"] = append(doc["exec_edges"].([]any), map[string]any{"from_node": "mark-called", "from_port": "then", "to_node": "second-mark", "to_port": "in", "sequence": json.Number("0")})
		doc["data_edges"] = append(doc["data_edges"].([]any), map[string]any{"from_node": "call-entry", "from_port": "payload", "to_node": "second-mark", "to_port": "value"})
	}
	sortByIDForAPITest(doc["nodes"].([]any))
	sortByIDForAPITest(doc["entrypoints"].([]any))
	sortExecEdgesForAPITest(doc["exec_edges"].([]any))
	sortDataEdgesForAPITest(doc["data_edges"].([]any))
	delete(doc, "program_digest")
	digest, err := canonical.Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["program_digest"] = digest
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireDispatchReceipt(t *testing.T, response *httptest.ResponseRecorder, status string) {
	t.Helper()
	var body struct {
		Status    string
		Execution struct {
			Phase         string
			RuntimeStatus string `json:"runtime_status"`
		}
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Status != status || body.Execution.Phase != "dispatch-returned" || body.Execution.RuntimeStatus == "" {
		t.Fatalf("missing runtime receipt: %s", response.Body.String())
	}
}

func TestOperatorExecutionDiscoversEveryCommandAcrossScopes(t *testing.T) {
	for _, target := range []string{"program", "preview", "stream-rule"} {
		t.Run(target, func(t *testing.T) {
			program := executionTransportProgram(t, false)
			host := bluehost.NewHost()
			show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
			defer show.Stop()
			plane := bluehost.NewRulePlane(nil, nil, bluehost.EffectDeps{}, 1, testLogger())
			defer plane.Stop()
			slot := bluehost.SlotOnAir
			query := ""
			if target == "preview" {
				slot = bluehost.SlotPreview
				query = "?target=preview"
			}
			if target == "stream-rule" {
				var doc struct {
					Digest string `json:"program_digest"`
				}
				json.Unmarshal(program, &doc)
				if err := plane.Promote("arbitrary-rule", doc.Digest, program); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := loadEngineBSlot(host, slot, program); err != nil {
					t.Fatal(err)
				}
				defer host.Release(slot, "test")
				if _, err := host.Step(slot); err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			RegisterPublic(mux, PublicDeps{Logger: testLogger(), Show: show, SceneIntent: &SceneIntentDeps{Host: host}, StreamRules: &StreamRulesDeps{Plane: plane}})
			contracts := opRequest(t, mux, "GET", "/api/v1/cockpit/contracts?stream_id=test"+func() string {
				if target == "preview" {
					return "&target=preview"
				}
				return ""
			}(), "operator", nil)
			var discovered cockpitContracts
			if contracts.Code != 200 || json.Unmarshal(contracts.Body.Bytes(), &discovered) != nil {
				t.Fatal(contracts.Body.String())
			}
			if len(discovered.Triggers) != 4 || len(discovered.Awaits) != 1 {
				t.Fatalf("discovery incomplete: %s", contracts.Body.String())
			}
			for _, command := range discovered.Triggers {
				selector := query
				if command.Scope == scopeStream {
					selector = "?rule=" + url.QueryEscape(command.RuleID)
				}
				response := opRequest(t, mux, "POST", "/api/v1/operator/call/"+url.PathEscape(command.BlueprintKey)+"/"+url.PathEscape(command.EntrypointID)+selector, "operator", map[string]any{"payload": map[string]any{"opaque": "operator-value"}})
				if response.Code != 202 {
					t.Fatalf("%s: %d %s", command.EntrypointID, response.Code, response.Body.String())
				}
				requireDispatchReceipt(t, response, "fired")
			}
			for _, await := range discovered.Awaits {
				selector := query
				if await.Scope == scopeStream {
					selector = "?rule=" + url.QueryEscape(await.RuleID)
				}
				path := "/api/v1/operator/resolve/" + url.PathEscape(await.BlueprintKey) + "/" + url.PathEscape(await.AwaitName) + selector
				response := opRequest(t, mux, "POST", path, "operator", map[string]any{"value": []any{"opaque", true, 7}})
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				requireDispatchReceipt(t, response, "resolved")
				if again := opRequest(t, mux, "POST", path, "operator", map[string]any{"value": nil}); again.Code != 410 {
					t.Fatal("resolved await executed twice")
				}
			}
		})
	}
}

func TestOperatorExecutionPreservesRuntimeFailure(t *testing.T) {
	for _, slot := range []bluehost.Slot{bluehost.SlotOnAir, bluehost.SlotPreview} {
		t.Run(string(slot), func(t *testing.T) {
			f := newEngineBOperatorFixtureOnSlot(t, slot, executionTransportProgram(t, true))
			query := ""
			if slot == bluehost.SlotPreview {
				query = "?target=preview"
			}
			response := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/first"+query, "operator", map[string]any{"payload": nil})
			var failure struct {
				Error        string
				RuntimeError blueruntime.Error `json:"runtime_error"`
			}
			if response.Code != 500 || json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Error != "BLUE_EXECUTION_FAILED" || failure.RuntimeError.Code == "" || failure.RuntimeError.Stage == "" {
				t.Fatalf("lost execution error: %d %s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("stream-rule", func(t *testing.T) {
		program := executionTransportProgram(t, true)
		var identity struct {
			Digest string `json:"program_digest"`
		}
		json.Unmarshal(program, &identity)
		plane := bluehost.NewRulePlane(nil, nil, bluehost.EffectDeps{}, 1, testLogger())
		defer plane.Stop()
		if err := plane.Promote("arbitrary-rule", identity.Digest, program); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		RegisterPublic(mux, PublicDeps{Logger: testLogger(), StreamRules: &StreamRulesDeps{Plane: plane}})
		response := opRequest(t, mux, "POST", "/api/v1/operator/call/arbitrary-rule/first?rule=arbitrary-rule", "operator", map[string]any{"payload": nil})
		var failure struct {
			Error        string
			RuntimeError blueruntime.Error `json:"runtime_error"`
		}
		if response.Code != 500 || json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Error != "BLUE_EXECUTION_FAILED" || failure.RuntimeError.Code == "" {
			t.Fatalf("stream-rule error lost: %d %s", response.Code, response.Body.String())
		}
	})
}
