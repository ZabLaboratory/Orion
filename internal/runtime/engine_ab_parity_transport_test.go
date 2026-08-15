package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// parityTransportObservation is deliberately made of externally observable
// values. A status-only comparison is not enough for #358: the same status
// can hide a different path, query, request body, response body, or
// continuation branch.
type parityTransportObservation struct {
	Status []byte
	Body   []byte
	OK     []byte
	Then   []byte
	Error  []byte
}

type parityHTTPCall struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

func parityRawJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func parityCanonicalJSONBytes(data []byte) []byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return append([]byte(nil), data...)
	}
	return canonicalJSONForTest(value)
}

func parityServiceRouteResolver(service, routeID string) (bluehost.ServiceCallRoute, bool) {
	if service != "example" || routeID != "example.echo" {
		return bluehost.ServiceCallRoute{}, false
	}
	return bluehost.ServiceCallRoute{
		Service:      service,
		RouteID:      routeID,
		Method:       http.MethodPost,
		PathTemplate: "/example/api/v1/items/{name}/echo",
		Params:       []string{"name"},
		TokenPaths:   []string{"example.echo"},
	}, true
}

func buildParityEngineAHTTPTransportProgram(targetURL, method string, query, headers, body map[string]any) *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"request": {
				ID: "request",
				Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url":     parityRawJSON(targetURL),
					"method":  parityRawJSON(method),
					"query":   parityRawJSON(query),
					"headers": parityRawJSON(headers),
					"body":    parityRawJSON(body),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.status"},
					"error": {Node: "set.error"},
				},
			},
			"set.status": setFromPin("set.status", "status", "request", "status", map[string]ExecTarget{
				"then": {Node: "set.body"},
			}),
			"set.body": setFromPin("set.body", "body", "request", "body", map[string]ExecTarget{
				"then": {Node: "set.ok"},
			}),
			"set.ok": setFromPin("set.ok", "ok", "request", "ok", map[string]ExecTarget{
				"then": {Node: "set.then"},
			}),
			"set.then":  constSet("set.then", "then", `"then"`),
			"set.error": setFromPin("set.error", "error", "request", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "request"}},
		},
	}
}

func buildParityEngineBHTTPTransportProgram(t *testing.T, targetURL, method, opcodeID string, query, headers, body map[string]any) []byte {
	t.Helper()
	dataPort := func(name string) map[string]any {
		return parityDataPort(name, "core.json", false)
	}
	return parityBuildProgram(t, "ab-parity-http-transport",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{
				"id": opcodeID, "kind": "control", "config": []any{},
				"inputs":  []any{dataPort("body"), dataPort("headers"), dataPort("method"), dataPort("query"), dataPort("timeout_ms"), dataPort("url"), parityExecPort("in")},
				"outputs": []any{dataPort("body"), dataPort("error"), dataPort("headers"), dataPort("ok"), dataPort("status"), parityExecPort("error"), parityExecPort("then")},
			},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{parityDataPort("variable", "core.string", true)},
				"inputs":  []any{parityDataPort("value", "core.json", true), parityExecPort("in")},
				"outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")},
			},
		},
		[]any{
			map[string]any{"id": "entry", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "request", "opcode": opcodeID, "config": map[string]any{}},
			map[string]any{"id": "set-body", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "body"}},
			map[string]any{"id": "set-error", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "error"}},
			map[string]any{"id": "set-ok", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "ok"}},
			map[string]any{"id": "set-status", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "status"}},
			map[string]any{"id": "set-then", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "then"}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "entry", "port": "then"}},
		[]any{
			map[string]any{"from_node": "entry", "from_port": "then", "to_node": "request", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "request", "from_port": "error", "to_node": "set-error", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "request", "from_port": "then", "to_node": "set-status", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-body", "from_port": "then", "to_node": "set-ok", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-ok", "from_port": "then", "to_node": "set-then", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-status", "from_port": "then", "to_node": "set-body", "to_port": "in", "sequence": 0},
		},
		[]any{
			map[string]any{"from_node": "request", "from_port": "body", "to_node": "set-body", "to_port": "value"},
			map[string]any{"from_node": "request", "from_port": "error", "to_node": "set-error", "to_port": "value"},
			map[string]any{"from_node": "request", "from_port": "ok", "to_node": "set-ok", "to_port": "value"},
			map[string]any{"from_node": "request", "from_port": "status", "to_node": "set-status", "to_port": "value"},
		},
		[]any{
			map[string]any{"node_id": "request", "port": "body", "value": body},
			map[string]any{"node_id": "request", "port": "headers", "value": headers},
			map[string]any{"node_id": "request", "port": "method", "value": method},
			map[string]any{"node_id": "request", "port": "query", "value": query},
			map[string]any{"node_id": "request", "port": "url", "value": targetURL},
			map[string]any{"node_id": "set-then", "port": "value", "value": "then"},
		},
		[]any{},
		[]any{
			map[string]any{"name": "body", "type": "core.json", "initial": nil},
			map[string]any{"name": "error", "type": "core.json", "initial": nil},
			map[string]any{"name": "ok", "type": "core.json", "initial": nil},
			map[string]any{"name": "status", "type": "core.json", "initial": nil},
			map[string]any{"name": "then", "type": "core.json", "initial": nil},
		},
	)
}

func parityObserveSceneState(sc *Scene, names ...string) parityTransportObservation {
	get := func(name string) []byte {
		value, ok := sc.state.Get("__vars.bp." + name)
		if !ok {
			return nil
		}
		return append([]byte(nil), value...)
	}
	return parityTransportObservation{
		Status: get(names[0]),
		Body:   get(names[1]),
		OK:     get(names[2]),
		Then:   get(names[3]),
		Error:  get(names[4]),
	}
}

func parityObserveBVariables(variables map[string]any) parityTransportObservation {
	get := func(name string) []byte {
		value, ok := variables[name]
		if !ok || value == nil {
			return nil
		}
		return canonicalJSONForTest(value)
	}
	return parityTransportObservation{
		Status: get("status"),
		Body:   get("body"),
		OK:     get("ok"),
		Then:   get("then"),
		Error:  get("error"),
	}
}

func parityCompareTransportObservation(t *testing.T, primitive, scenario string, a, b parityTransportObservation) {
	t.Helper()
	compare := func(name string, left, right []byte) {
		var leftValue, rightValue any
		if len(left) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(left))
			decoder.UseNumber()
			if err := decoder.Decode(&leftValue); err != nil {
				t.Fatalf("primitive=%s scenario=%s %s Engine A value=%s is invalid JSON: %v", primitive, scenario, name, left, err)
			}
		}
		if len(right) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(right))
			decoder.UseNumber()
			if err := decoder.Decode(&rightValue); err != nil {
				t.Fatalf("primitive=%s scenario=%s %s Engine B value=%s is invalid JSON: %v", primitive, scenario, name, right, err)
			}
		}
		if got, want := string(canonicalJSONForTest(rightValue)), string(canonicalJSONForTest(leftValue)); got != want {
			t.Fatalf("primitive=%s scenario=%s %s diverges: Engine A=%s Engine B=%s", primitive, scenario, name, want, got)
		}
	}
	compare("status", a.Status, b.Status)
	compare("body", a.Body, b.Body)
	compare("ok", a.OK, b.OK)
	compare("then", a.Then, b.Then)
	compare("error", a.Error, b.Error)
}

func TestEngineABParity_HTTPTransportComparesSuccessErrorPathQueryBodyAndAlias(t *testing.T) {
	const method = http.MethodPost
	query := map[string]any{"q": "blue host", "page": 2}
	headers := map[string]any{"X-Parity": "transport"}
	body := map[string]any{"source": "orion-358"}

	cases := []struct {
		name       string
		opcode     string
		path       string
		status     int
		response   string
		invalidURL bool
	}{
		{name: "canonical-success", opcode: "core.http.request@1", path: "/parity/success", status: http.StatusCreated, response: `{"accepted":true}`},
		{name: "canonical-http-status-error-is-then", opcode: "core.http.request@1", path: "/parity/status-error", status: http.StatusServiceUnavailable, response: `{"accepted":false,"reason":"busy"}`},
		{name: "canonical-invalid-input-is-error", opcode: "core.http.request@1", path: "/parity/invalid", invalidURL: true},
		{name: "hyphenated-alias-success", opcode: "core.http-request@1", path: "/parity/alias", status: http.StatusCreated, response: `{"accepted":true,"alias":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := make([]parityHTTPCall, 0, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("primitive=core.http.request@1 scenario=%s read body: %v", tc.name, err)
				}
				mu.Lock()
				calls = append(calls, parityHTTPCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query().Encode(), Body: append([]byte(nil), requestBody...)})
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()

			targetURL := server.URL + tc.path
			if tc.invalidURL {
				targetURL = "http://[invalid"
			}
			egress := loopbackEgress(t, server.URL)
			a := effectsScene(t, "ab-http-transport-a-"+tc.name, buildParityEngineAHTTPTransportProgram(targetURL, method, query, headers, body), &SceneEffects{
				Runner: newTestRunner(t), Egress: egress,
			})
			startScene(t, a)
			mustFire(t, a, "start")
			waitFor(t, "Engine A HTTP continuation", func() bool {
				isSet := func(path string) bool {
					value, ok := a.state.Get(path)
					return ok && len(value) > 0 && string(value) != "null"
				}
				return isSet("__vars.bp.status") || isSet("__vars.bp.error")
			})
			aObservation := parityObserveSceneState(a, "status", "body", "ok", "then", "error")

			h := bluehost.NewHost()
			t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
			program := buildParityEngineBHTTPTransportProgram(t, targetURL, method, tc.opcode, query, headers, body)
			if err := h.Prepare(bluehost.SlotOnAir, "ab-http-transport-b-"+tc.name, "scene-http-transport-"+tc.name, "sha256:http-transport-"+tc.name, program, nil, nil,
				bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute)); err != nil {
				t.Fatalf("primitive=%s scenario=%s Engine B Host.Prepare: %v", tc.opcode, tc.name, err)
			}
			bStep := parityBStep(t, h, bluehost.SlotOnAir, "http.transport "+tc.name)
			bObservation := parityObserveBVariables(bStep.Variables)
			parityCompareTransportObservation(t, tc.opcode, tc.name, aObservation, bObservation)

			mu.Lock()
			observedCalls := append([]parityHTTPCall(nil), calls...)
			mu.Unlock()
			if tc.invalidURL {
				if len(observedCalls) != 0 {
					t.Fatalf("primitive=%s scenario=%s invalid input emitted %d requests, want zero", tc.opcode, tc.name, len(observedCalls))
				}
				return
			}
			if len(observedCalls) != 2 {
				t.Fatalf("primitive=%s scenario=%s request count=%d, want one Engine A and one Engine B request", tc.opcode, tc.name, len(observedCalls))
			}
			if observedCalls[0].Method != observedCalls[1].Method || observedCalls[0].Path != observedCalls[1].Path || observedCalls[0].Query != observedCalls[1].Query || !bytes.Equal(observedCalls[0].Body, observedCalls[1].Body) {
				t.Fatalf("primitive=%s scenario=%s request observable diverges: Engine A=%+v Engine B=%+v", tc.opcode, tc.name, observedCalls[0], observedCalls[1])
			}
			if observedCalls[0].Method != method || observedCalls[0].Path != tc.path || observedCalls[0].Query != "page=2&q=blue+host" || string(observedCalls[0].Body) != `{"source":"orion-358"}` {
				t.Fatalf("primitive=%s scenario=%s request observable=%+v", tc.opcode, tc.name, observedCalls[0])
			}
		})
	}
}

func buildParityEngineBDBTransportProgram(t *testing.T, descriptor map[string]any) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-parity-db-transport",
		[]any{
			map[string]any{"id": "core.db.query@1", "kind": "control", "config": []any{parityDataPort("datasource", "core.string", true)}, "inputs": []any{parityDataPort("descriptor", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("count", "core.json", false), parityDataPort("elapsed_ms", "core.json", false), parityDataPort("error", "core.json", false), parityDataPort("rows", "core.json", false), parityExecPort("error"), parityExecPort("then")}},
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "db", "opcode": "core.db.query@1", "config": map[string]any{"datasource": "truth"}},
			map[string]any{"id": "entry", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "set-count", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "count"}},
			map[string]any{"id": "set-elapsed", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "elapsed_ms"}},
			map[string]any{"id": "set-error", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "error"}},
			map[string]any{"id": "set-rows", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "rows"}},
			map[string]any{"id": "set-then", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "then"}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "entry", "port": "then"}},
		[]any{
			map[string]any{"from_node": "db", "from_port": "error", "to_node": "set-error", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "db", "from_port": "then", "to_node": "set-count", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "entry", "from_port": "then", "to_node": "db", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-count", "from_port": "then", "to_node": "set-rows", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-elapsed", "from_port": "then", "to_node": "set-then", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-rows", "from_port": "then", "to_node": "set-elapsed", "to_port": "in", "sequence": 0},
		},
		[]any{
			map[string]any{"from_node": "db", "from_port": "count", "to_node": "set-count", "to_port": "value"},
			map[string]any{"from_node": "db", "from_port": "elapsed_ms", "to_node": "set-elapsed", "to_port": "value"},
			map[string]any{"from_node": "db", "from_port": "error", "to_node": "set-error", "to_port": "value"},
			map[string]any{"from_node": "db", "from_port": "rows", "to_node": "set-rows", "to_port": "value"},
		},
		[]any{
			map[string]any{"node_id": "db", "port": "descriptor", "value": descriptor},
			map[string]any{"node_id": "set-then", "port": "value", "value": "then"},
		},
		[]any{},
		[]any{
			map[string]any{"name": "count", "type": "core.json", "initial": nil},
			map[string]any{"name": "elapsed_ms", "type": "core.json", "initial": nil},
			map[string]any{"name": "error", "type": "core.json", "initial": nil},
			map[string]any{"name": "rows", "type": "core.json", "initial": nil},
			map[string]any{"name": "then", "type": "core.json", "initial": nil},
		},
	)
}

func buildParityEngineADBTransportProgram(t *testing.T, descriptor map[string]any) *ExecProgram {
	t.Helper()
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"db": {
				ID: "db", Op: OpDBQuery,
				Config: map[string]json.RawMessage{
					"datasource": raw(`"truth"`),
					"descriptor": parityRawJSON(descriptor),
				},
				Next: map[string]ExecTarget{"then": {Node: "set.count"}, "error": {Node: "set.error"}},
			},
			"set.count":   setFromPin("set.count", "count", "db", "count", map[string]ExecTarget{"then": {Node: "set.rows"}}),
			"set.rows":    setFromPin("set.rows", "rows", "db", "rows", map[string]ExecTarget{"then": {Node: "set.elapsed"}}),
			"set.elapsed": setFromPin("set.elapsed", "elapsed_ms", "db", "elapsed_ms", map[string]ExecTarget{"then": {Node: "set.then"}}),
			"set.then":    constSet("set.then", "then", `"then"`),
			"set.error":   setFromPin("set.error", "error", "db", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "db"}}},
	}
}

type parityDBObservation struct {
	Count   []byte
	Rows    []byte
	Elapsed []byte
	Then    []byte
	Error   []byte
}

func parityObserveADBState(sc *Scene) parityDBObservation {
	get := func(name string) []byte {
		value, ok := sc.state.Get("__vars.bp." + name)
		if !ok {
			return nil
		}
		return append([]byte(nil), value...)
	}
	return parityDBObservation{Count: get("count"), Rows: get("rows"), Elapsed: get("elapsed_ms"), Then: get("then"), Error: get("error")}
}

func parityObserveBDBVariables(variables map[string]any) parityDBObservation {
	get := func(name string) []byte {
		value, ok := variables[name]
		if !ok || value == nil {
			return nil
		}
		return canonicalJSONForTest(value)
	}
	return parityDBObservation{Count: get("count"), Rows: get("rows"), Elapsed: get("elapsed_ms"), Then: get("then"), Error: get("error")}
}

func parityCompareDBObservation(t *testing.T, scenario string, a, b parityDBObservation) {
	t.Helper()
	parityCompareTransportObservation(t, "core.db.query@1", scenario,
		parityTransportObservation{Status: a.Count, Body: a.Rows, OK: a.Elapsed, Then: a.Then, Error: a.Error},
		parityTransportObservation{Status: b.Count, Body: b.Rows, OK: b.Elapsed, Then: b.Then, Error: b.Error})
}

func TestEngineABParity_DBTransportComparesQueryRowsCountThenAndError(t *testing.T) {
	descriptor := map[string]any{"table": "players", "select": []any{"id"}}

	t.Run("success", func(t *testing.T) {
		var mu sync.Mutex
		var requestBodies [][]byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read DB request: %v", err)
			}
			mu.Lock()
			requestBodies = append(requestBodies, append([]byte(nil), body...))
			mu.Unlock()
			if r.Method != http.MethodPost || r.URL.Path != "/truth/api/v1/_query" {
				t.Errorf("DB request method/path=%s %s, want POST /truth/api/v1/_query", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rows":[{"id":"p1"}],"count":1,"elapsed_ms":3}`)
		}))
		defer server.Close()
		db := effects.NewDBQueryClient(server.URL, "parity-db-token", nil)
		ds := map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}}

		a := effectsScene(t, "ab-db-transport-a", buildParityEngineADBTransportProgram(t, descriptor), &SceneEffects{Runner: newTestRunner(t), DB: db, DataSources: ds})
		startScene(t, a)
		mustFire(t, a, "start")
		waitForState(t, a, "__vars.bp.count", "1", 2*time.Second)
		aObservation := parityObserveADBState(a)

		h := bluehost.NewHost()
		t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
		if err := h.Prepare(bluehost.SlotOnAir, "ab-db-transport-b", "scene-db-transport", "sha256:db-transport", buildParityEngineBDBTransportProgram(t, descriptor), nil, nil,
			bluehost.NewEffectHandlers(bluehost.EffectDeps{DB: db, DataSources: ds}, blueruntime.Execute)); err != nil {
			t.Fatalf("Engine B Host.Prepare: %v", err)
		}
		bStep := parityBStep(t, h, bluehost.SlotOnAir, "db.transport success")
		bObservation := parityObserveBDBVariables(bStep.Variables)
		parityCompareDBObservation(t, "success", aObservation, bObservation)

		mu.Lock()
		observedBodies := append([][]byte(nil), requestBodies...)
		mu.Unlock()
		if len(observedBodies) != 2 {
			t.Fatalf("DB request count=%d, want one Engine A and one Engine B request", len(observedBodies))
		}
		if !bytes.Equal(parityCanonicalJSONBytes(observedBodies[0]), parityCanonicalJSONBytes(observedBodies[1])) || string(parityCanonicalJSONBytes(observedBodies[0])) != `{"select":["id"],"table":"players"}` {
			t.Fatalf("DB descriptor diverges: Engine A=%s Engine B=%s", observedBodies[0], observedBodies[1])
		}
	})

	t.Run("unconfigured-error", func(t *testing.T) {
		a := effectsScene(t, "ab-db-transport-error-a", buildParityEngineADBTransportProgram(t, descriptor), &SceneEffects{Runner: newTestRunner(t)})
		startScene(t, a)
		mustFire(t, a, "start")
		waitFor(t, "Engine A DB error continuation", func() bool {
			_, ok := a.state.Get("__vars.bp.error")
			return ok
		})
		aObservation := parityObserveADBState(a)

		h := bluehost.NewHost()
		t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
		if err := h.Prepare(bluehost.SlotOnAir, "ab-db-transport-error-b", "scene-db-transport-error", "sha256:db-transport-error", buildParityEngineBDBTransportProgram(t, descriptor), nil, nil,
			bluehost.NewEffectHandlers(bluehost.EffectDeps{}, blueruntime.Execute)); err != nil {
			t.Fatalf("Engine B Host.Prepare: %v", err)
		}
		bStep := parityBStep(t, h, bluehost.SlotOnAir, "db.transport error")
		bObservation := parityObserveBDBVariables(bStep.Variables)
		parityCompareDBObservation(t, "unconfigured-error", aObservation, bObservation)
	})
}
