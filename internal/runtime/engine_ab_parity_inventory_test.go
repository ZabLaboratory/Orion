package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// TestEngineABParity_OperationInventory is intentionally a small inventory,
// not a claim that every primitive has the same host contract.  HTTP and DB
// execute through both real engines.  The remaining entries retain an
// explicit typed gap until bluehost exposes the corresponding A seam.
func TestEngineABParity_OperationInventory(t *testing.T) {
	cases := []struct {
		id  string
		run func(*testing.T)
		gap parityNonEquivalenceError
	}{
		{id: "core.http.request@1", run: testParityInventoryHTTP},
		{id: "core.db.query@1", run: TestEngineABParity_DBQueryObservableAndPreviewNoQuery},
		{id: "core.service.call@1", run: testParityInventoryServiceGap},
		{id: "core.show.emit@1", run: testParityInventoryShow},
		{id: "core.overlay-app.set@1", run: testParityInventoryOverlay},
		{id: "core.animation.play@1", run: testParityInventoryAnimation},
		{id: "core.operator.await-value@1", run: testParityInventoryAwait},
		{id: "core.flow.gate@1", run: testParityInventoryGate},
		{id: "core.flow.delay@1", run: testParityInventoryDelay},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if tc.run != nil {
				tc.run(t)
				return
			}
			parityAssertTypedGap(t, tc.gap)
		})
	}

	// The hyphenated spelling is an alias, not a tenth operation.  Keep its
	// real B load/execute proof next to the canonical HTTP case when possible.
	t.Run("core.http-request@1 alias", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}))
		defer srv.Close()
		egress := loopbackEgress(t, srv.URL)
		program := buildEngineBHTTPProgram(t, srv.URL)
		var doc map[string]any
		if err := json.Unmarshal(program, &doc); err != nil {
			t.Fatalf("primitive=core.http-request@1 scenario=alias parse: %v", err)
		}
		for _, opcode := range doc["opcodes"].([]any) {
			if item, ok := opcode.(map[string]any); ok && item["id"] == "core.http.request@1" {
				item["id"] = "core.http-request@1"
			}
		}
		for _, node := range doc["nodes"].([]any) {
			if item, ok := node.(map[string]any); ok && item["opcode"] == "core.http.request@1" {
				item["opcode"] = "core.http-request@1"
			}
		}
		delete(doc, "program_digest")
		doc["program_digest"] = digestOfForTest(doc)
		alias, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("primitive=core.http-request@1 scenario=alias marshal: %v", err)
		}
		h := bluehost.NewHost()
		t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
		if err := h.Prepare(bluehost.SlotOnAir, "inventory-http-alias", "sha256:inventory-http-alias", alias, nil, nil,
			bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute)); err != nil {
			t.Fatalf("primitive=core.http-request@1 scenario=alias Host.Prepare: %v", err)
		}
		if _, err := h.Step(bluehost.SlotOnAir); err != nil {
			t.Fatalf("primitive=core.http-request@1 scenario=alias start step: %v", err)
		}
		step, err := h.Step(bluehost.SlotOnAir)
		if err != nil {
			t.Fatalf("primitive=core.http-request@1 scenario=alias execute step: %v", err)
		}
		if status, ok := step.Outputs["result"].(json.Number); !ok || status.String() != "201" {
			t.Fatalf("primitive=core.http-request@1 scenario=alias observable=%#v, want 201", step.Outputs["result"])
		}
	})
}

func testParityInventoryHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	egress := loopbackEgress(t, srv.URL)

	a := effectsScene(t, "inventory-http-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"request": {ID: "request", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)},
				Next:   map[string]ExecTarget{"then": {Node: "status"}}},
			"status": setFromPin("status", "status", "request", "status", nil),
		},
		Entrypoints: map[string]ExecEntry{"start": {Target: ExecTarget{Node: "request"}}},
	}, &SceneEffects{Runner: newTestRunner(t), Egress: egress})
	startScene(t, a)
	mustFire(t, a, "start")
	waitForState(t, a, "__vars.bp.status", "201", 2*time.Second)
	aStatus, _ := a.state.Get("__vars.bp.status")

	b := blueruntime.NewRuntime()
	handle, err := b.Load(buildEngineBHTTPProgram(t, srv.URL))
	if err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=inventory Engine B Load: %v", err)
	}
	instance, err := b.Start(handle, blueruntime.StartOptions{
		InstanceID: "inventory-http-b", Mode: blueruntime.Execute,
		EffectHandlers: bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute),
	})
	if err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=inventory Engine B Start: %v", err)
	}
	if _, err := b.Step(instance); err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=inventory Engine B start step: %v", err)
	}
	step, err := b.Step(instance)
	if err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=inventory Engine B execute step: %v", err)
	}
	bStatus, ok := step.Outputs["result"].(json.Number)
	if !ok || bStatus.String() != string(aStatus) {
		t.Fatalf("primitive=core.http.request@1 scenario=inventory observable diverges: Engine A=%s Engine B=%#v", aStatus, step.Outputs["result"])
	}
}

func TestEngineABParity_HTTPPreviewNoNetworkAndOnAirObservable(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	egress := loopbackEgress(t, srv.URL)

	aOnAir := effectsScene(t, "inventory-http-on-air-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"request": {ID: "request", Op: OpHTTPRequest, Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)}, Next: map[string]ExecTarget{"then": {Node: "status"}}},
			"status":  setFromPin("status", "status", "request", "status", nil),
		},
		Entrypoints: map[string]ExecEntry{"call": {Target: ExecTarget{Node: "request"}}},
	}, &SceneEffects{Runner: newTestRunner(t), Egress: egress})
	startScene(t, aOnAir)
	mustFire(t, aOnAir, "call")
	waitForState(t, aOnAir, "__vars.bp.status", "201", 2*time.Second)
	aOnAirValue, _ := aOnAir.state.Get("__vars.bp.status")
	if got := requests.Load(); got != 1 {
		t.Fatalf("primitive=core.http.request@1 scenario=on-air Engine A request count=%d, want 1", got)
	}

	bOnAir := bluehost.NewHost()
	t.Cleanup(func() { _ = bOnAir.Release(bluehost.SlotOnAir, "test-cleanup") })
	if err := bOnAir.Prepare(bluehost.SlotOnAir, "inventory-http-on-air-b", "sha256:inventory-http-on-air-b", buildEngineBHTTPProgram(t, srv.URL), nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute)); err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=on-air Engine B Prepare: %v", err)
	}
	bOnAirStep := parityBStep(t, bOnAir, bluehost.SlotOnAir, "http.on-air")
	parityAssertInventoryResult(t, "core.http.request@1", "on-air", aOnAirValue, bOnAirStep.Outputs["result"])
	if got := requests.Load(); got != 2 {
		t.Fatalf("primitive=core.http.request@1 scenario=on-air Engine B request count=%d, want 2", got)
	}

	aPreviewProgram := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"request": {ID: "request", Op: OpHTTPRequest, Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)}, Next: map[string]ExecTarget{"then": {Node: "status"}}},
			"status":  setFromPin("status", "status", "request", "status", nil),
		},
		Entrypoints: map[string]ExecEntry{"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "request"}}},
	}
	aPreview := NewPreviewSlot(context.Background(), NewComputeRegistry(), parityPreviewWire{}, quietLogger())
	aPreview.SetEffects(&SceneEffects{Runner: newTestRunner(t), Egress: egress})
	aPreview.Activate("inventory-http-preview-a", effectsGraph("inventory-http-preview-a"), &compiler.RenderBundle{SceneVersion: "sha256:inventory-http-preview"}, aPreviewProgram)
	t.Cleanup(aPreview.Close)
	waitForState(t, aPreview.Current(), "__vars.bp.status", "201", 2*time.Second)
	aPreviewValue, _ := aPreview.Current().state.Get("__vars.bp.status")
	if got := requests.Load(); got != 3 {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine A request count=%d, want 3 (A on-air, B on-air, A preview)", got)
	}

	bPreview := bluehost.NewHost()
	t.Cleanup(func() { _ = bPreview.Release(bluehost.SlotPreview, "test-cleanup") })
	if err := bPreview.Prepare(bluehost.SlotPreview, "inventory-http-preview-b", "sha256:inventory-http-preview-b", buildEngineBHTTPProgram(t, srv.URL), nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Preview)); err != nil {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine B Prepare: %v", err)
	}
	bPreviewStep := parityBStep(t, bPreview, bluehost.SlotPreview, "http.preview")
	if status, ok := bPreviewStep.Outputs["result"].(json.Number); !ok || status.String() != "0" {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine B status=%#v, want synthetic 0", bPreviewStep.Outputs["result"])
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine B performed a network request; count=%d", got)
	}
	if string(aPreviewValue) != "201" {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine A status=%s, want 201", aPreviewValue)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.http.request@1", Scenario: "preview-no-network",
		EngineA: "Engine A PreviewSlot executed the bounded egress and observed HTTP 201",
		EngineB: "bluehost.Host SlotPreview returned synthetic status 0 and did not increment the httptest spy",
	})
}

func TestEngineABParity_EventSequenceRecoveryDedupAndOutOfOrder(t *testing.T) {
	t.Run("gap-recovery-1-3-2", func(t *testing.T) {
		a := execScene(t, "ab-sequence-recovery-a", parityEngineAEventPrintProgram())
		startScene(t, a)
		for _, value := range []string{"one", "two"} {
			if !a.Input(InputMsg{Path: eventsPrefix + "score", Value: raw(`"` + value + `"`), Source: "event:test"}) {
				t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap Engine A rejected %s", value)
			}
		}
		aTrace := parityNormalizeEventTrace(parityWaitForALogs(t, a, 2, 2*time.Second))

		h := parityPrepareBHost(t, bluehost.SlotOnAir, "sequence-recovery-b", parityBuildBTopicPrintProgram(t))
		first, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-1", "platform.twitch", 1, "one"))
		if err != nil || first.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap seq=1 receipt=%+v err=%v", first, err)
		}
		if got := parityNormalizeEventTrace(parityLogs(parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=1"))); fmt.Sprint(got) != "[one]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap after seq=1 trace=%v", got)
		}
		_, err = h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		if code := parityBlueErrorCode(err); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap seq=3 code=%q err=%v, want EVENT_SEQUENCE_GAP", code, err)
		}
		second, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-2", "platform.twitch", 2, "two"))
		if err != nil || second.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap seq=2 recovery receipt=%+v err=%v", second, err)
		}
		if got := parityNormalizeEventTrace(parityLogs(parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=2"))); fmt.Sprint(got) != "[one two]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap recovery trace=%v, want [one two]", got)
		}
		third, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		if err != nil || third.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap retry seq=3 receipt=%+v err=%v", third, err)
		}
		bTrace := parityNormalizeEventTrace(parityLogs(parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap retry seq=3")))
		if fmt.Sprint(bTrace) != "[one two three]" || fmt.Sprint(aTrace) != "[one two]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap normalized traces: Engine A=%v Engine B=%v", aTrace, bTrace)
		}
		parityAssertTypedGap(t, parityNonEquivalenceError{
			Primitive: "core.event.on-event@1", Scenario: "sequence-gap",
			EngineA: "InputMsg has no source-sequence admission and accepts 1 then 2",
			EngineB: "Host.Dispatch rejects 3 with EVENT_SEQUENCE_GAP, accepts 2, then accepts retried 3",
		})
	})

	t.Run("exact-duplicate-execution-count", func(t *testing.T) {
		h := parityPrepareBHost(t, bluehost.SlotOnAir, "sequence-duplicate-b", parityBuildBTopicPrintProgram(t))
		event := parityEventEnvelope(t, "evt-duplicate", "platform.twitch", 1, "same")
		first, err := h.Dispatch(bluehost.SlotOnAir, event)
		if err != nil || first.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate seq=1 receipt=%+v err=%v", first, err)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "duplicate first")
		duplicate, err := h.Dispatch(bluehost.SlotOnAir, event)
		if err != nil || duplicate.Status != "duplicate" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate receipt=%+v err=%v, want duplicate", duplicate, err)
		}
		if got := parityNormalizeEventTrace(parityLogs(step)); fmt.Sprint(got) != "[same]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate execution trace=%v, want one execution", got)
		}
		parityAssertTypedGap(t, parityNonEquivalenceError{
			Primitive: "core.event.on-event@1", Scenario: "duplicate",
			EngineA: "InputMsg does not carry event_id/payload_digest and repeated input executes twice",
			EngineB: "Host.Dispatch returns duplicate for the exact event and keeps one execution trace",
		})
	})

	t.Run("same-sequence-conflict", func(t *testing.T) {
		h := parityPrepareBHost(t, bluehost.SlotOnAir, "sequence-conflict-b", parityBuildBTopicPrintProgram(t))
		first, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-conflict-one", "platform.twitch", 1, "one"))
		if err != nil || first.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=conflict first receipt=%+v err=%v", first, err)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "conflict first")
		_, err = h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-conflict-two", "platform.twitch", 1, "again"))
		if code := parityBlueErrorCode(err); code != "EVENT_SEQUENCE_CONFLICT" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=conflict code=%q err=%v, want EVENT_SEQUENCE_CONFLICT", code, err)
		}
		if got := parityNormalizeEventTrace(parityLogs(step)); fmt.Sprint(got) != "[one]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=conflict execution trace=%v, want [one]", got)
		}
		parityAssertTypedGap(t, parityNonEquivalenceError{
			Primitive: "core.event.on-event@1", Scenario: "same-sequence-conflict",
			EngineA: "InputMsg has no source-sequence conflict check and would execute both payloads",
			EngineB: "Host.Dispatch rejects same source sequence with a different digest as EVENT_SEQUENCE_CONFLICT",
		})
	})

	t.Run("out-of-order-before-first", func(t *testing.T) {
		h := parityPrepareBHost(t, bluehost.SlotOnAir, "sequence-out-of-order-b", parityBuildBTopicPrintProgram(t))
		_, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-out-of-order", "platform.twitch", 2, "two"))
		if code := parityBlueErrorCode(err); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=out-of-order-before-first code=%q err=%v, want EVENT_SEQUENCE_GAP", code, err)
		}
		first, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-out-of-order-1", "platform.twitch", 1, "one"))
		if err != nil || first.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=out-of-order-before-first recovery receipt=%+v err=%v", first, err)
		}
		if got := parityNormalizeEventTrace(parityLogs(parityHostStep(t, h, bluehost.SlotOnAir, "out-of-order recovery"))); fmt.Sprint(got) != "[one]" {
			t.Fatalf("primitive=core.event.on-event@1 scenario=out-of-order-before-first trace=%v, want [one]", got)
		}
		parityAssertTypedGap(t, parityNonEquivalenceError{
			Primitive: "core.event.on-event@1", Scenario: "out-of-order-before-first",
			EngineA: "InputMsg has no admission sequence and cannot distinguish this case",
			EngineB: "Host.Dispatch rejects source sequence 2 before expected sequence 1 with EVENT_SEQUENCE_GAP",
		})
	})
}

func parityHostStep(t *testing.T, h *bluehost.Host, slot bluehost.Slot, scenario string) blueruntime.StepResult {
	t.Helper()
	step, err := h.Step(slot)
	if err != nil {
		t.Fatalf("primitive=core.event.on-event scenario=%s Host.Step: %v", scenario, err)
	}
	return step
}

func parityBuildBLocalOperationProgram(t *testing.T, opcodeID string, value any, config map[string]any) []byte {
	t.Helper()
	if config == nil {
		config = map[string]any{}
	}
	return parityBuildProgram(t, "ab-inventory-local",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": opcodeID, "kind": "control", "config": parityInventoryConfigPorts(opcodeID), "inputs": []any{parityExecPort("in")}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "op", "opcode": opcodeID, "config": config},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{
			map[string]any{"from_node": "op", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "op", "to_port": "in", "sequence": 0},
		},
		[]any{}, []any{map[string]any{"node_id": "mark", "port": "value", "value": value}}, []any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func parityInventoryConfigPorts(opcodeID string) []any {
	switch opcodeID {
	case "core.show.emit@1":
		return []any{parityDataPort("topic", "core.string", false)}
	case "core.overlay-app.set@1":
		return []any{parityDataPort("app_id", "core.string", false), parityDataPort("running", "core.json", false)}
	case "core.animation.play@1":
		return []any{parityDataPort("animation_id", "core.string", false), parityDataPort("duration_seconds", "core.json", false), parityDataPort("overlay_id", "core.string", false)}
	default:
		return []any{}
	}
}

func parityAssertInventoryResult(t *testing.T, primitive, scenario string, aRaw []byte, bValue any) {
	t.Helper()
	var aValue any
	decoder := json.NewDecoder(bytes.NewReader(aRaw))
	decoder.UseNumber()
	if err := decoder.Decode(&aValue); err != nil {
		t.Fatalf("primitive=%s scenario=%s Engine A result=%s is not JSON: %v", primitive, scenario, aRaw, err)
	}
	if got, want := string(canonicalJSONForTest(bValue)), string(canonicalJSONForTest(aValue)); got != want {
		t.Fatalf("primitive=%s scenario=%s observable diverges: Engine A=%s Engine B=%s", primitive, scenario, want, got)
	}
}

func parityAssertBLocalSideEffect(t *testing.T, primitive string, value any, config map[string]any) blueruntime.StepResult {
	t.Helper()
	h := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-local", parityBuildBLocalOperationProgram(t, primitive, value, config))
	step := parityBStep(t, h, bluehost.SlotOnAir, primitive)
	bagName := "__" + strings.TrimSuffix(primitive[len("core."):], "@1")
	if _, ok := step.Variables[bagName].(map[string]any); !ok {
		t.Fatalf("primitive=%s scenario=local-side-effect Engine B reserved observable=%#v", primitive, step.Variables)
	}
	return step
}

func testParityInventoryServiceGap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	minter := &recordingMinter{token: "inventory-token"}
	a := effectsScene(t, "inventory-service-a", serviceCallProgram(echoRouteJSON, `{"name":"alice"}`), &SceneEffects{
		Runner: newTestRunner(t), ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	})
	startScene(t, a)
	mustFire(t, a, "e")
	waitForState(t, a, "__vars.bp.status", "200", 2*time.Second)
	aStatus, _ := a.state.Get("__vars.bp.status")

	program := parityBuildProgram(t, "ab-inventory-service",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.service.call@1", "kind": "control", "config": []any{}, "inputs": []any{parityExecPort("in")}, "outputs": []any{parityExecPort("error"), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "call", "opcode": "core.service.call@1", "config": map[string]any{}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "call", "to_port": "in", "sequence": 0}},
		[]any{}, []any{}, []any{}, []any{},
	)
	h := bluehost.NewHost()
	t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
	err := h.Prepare(bluehost.SlotOnAir, "inventory-service-b", "sha256:inventory-service-b", program, nil, nil, nil)
	if err == nil {
		_, err = h.Step(bluehost.SlotOnAir)
	}
	if code := parityBlueErrorCode(err); code != "PROGRAM_OPCODE_UNSUPPORTED" {
		t.Fatalf("primitive=core.service.call@1 scenario=host-unsupported Engine A status=%s Engine B code=%q err=%v, want PROGRAM_OPCODE_UNSUPPORTED", aStatus, code, err)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.service.call@1", Scenario: "host-unsupported",
		EngineA: "Engine A executed serviceCallProgram and observed HTTP 200",
		EngineB: "real bluehost.Host rejected core.service.call@1 with PROGRAM_OPCODE_UNSUPPORTED",
	})
}

func testParityInventoryShow(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.SetEmitter(&activeOnlyEmitter{show: show})
	show.LoadExec("inventory-show-a", varsGraph("inventory-show-a"), &compiler.RenderBundle{SceneVersion: "sha256:inventory-show"}, emitOnStartProg("inventory"))
	if err := show.SetActive("inventory-show-a", nil); err != nil {
		t.Fatalf("primitive=core.show.emit@1 scenario=on-air Engine A SetActive: %v", err)
	}
	a, _ := show.Get("inventory-show-a")
	waitForState(t, a, "__vars.bp.emitted", "1", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.emitted")
	b := parityAssertBLocalSideEffect(t, "core.show.emit@1", 1, map[string]any{"topic": "inventory"})
	parityAssertInventoryResult(t, "core.show.emit@1", "then-routing", aValue, b.Outputs["result"])
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.show.emit@1", Scenario: "external-observable",
		EngineA: "Show emitter delivered an active-only event and then set __vars.bp.emitted=1",
		EngineB: "Host runtime only exposes __show.emit reserved state; no Host emitter/active-scene mirror exists",
	})
}

func testParityInventoryOverlay(t *testing.T) {
	rec := &overlayCapture{}
	a := execScene(t, "inventory-overlay-a", overlaySetProg(map[string]json.RawMessage{
		"app_id": raw(`"inventory-app"`), "running": raw(`true`),
	}))
	a.SetOverlayAppSetter(rec.set)
	startScene(t, a)
	mustFire(t, a, "start")
	waitForState(t, a, "__vars.bp.done", "1", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.done")
	if calls, appID, _, _ := rec.snapshot(); calls != 1 || appID != "inventory-app" {
		t.Fatalf("primitive=core.overlay-app.set@1 scenario=Engine-A-seam calls=%d app_id=%q", calls, appID)
	}
	b := parityAssertBLocalSideEffect(t, "core.overlay-app.set@1", 1, map[string]any{"app_id": "inventory-app", "running": true})
	parityAssertInventoryResult(t, "core.overlay-app.set@1", "then-routing", aValue, b.Outputs["result"])
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.overlay-app.set@1", Scenario: "external-observable",
		EngineA: "Scene.SetOverlayAppSetter observed one app_id/running mirror call and then-routing",
		EngineB: "Host runtime only exposes __overlay-app.set reserved state; no Host overlay mirror exists",
	})
}

func testParityInventoryAnimation(t *testing.T) {
	a, _, _ := animScene(t, "inventory-animation-a", "0")
	startScene(t, a)
	mustFire(t, a, "e")
	waitForState(t, a, "__vars.bp.after", `"then-fired"`, 2*time.Second)
	waitForState(t, a, "__anim.ov", "1", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.after")
	b := parityAssertBLocalSideEffect(t, "core.animation.play@1", "then-fired", map[string]any{
		"overlay_id": "ov", "animation_id": "fade", "duration_seconds": 0,
	})
	parityAssertInventoryResult(t, "core.animation.play@1", "then-routing", aValue, b.Outputs["result"])
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.animation.play@1", Scenario: "renderer-completion",
		EngineA: "Scene emitted __anim.ov generation 1 and exposes completed/error continuation",
		EngineB: "Host runtime only exposes __animation.play reserved state; no renderer report/completion seam exists",
	})
}

func parityBuildBAwaitProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-inventory-await",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.operator.await-value@1", "kind": "control", "config": []any{parityDataPort("await_name", "core.string", false)}, "inputs": []any{parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "await", "opcode": "core.operator.await-value@1", "config": map[string]any{"await_name": "confirm"}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{
			map[string]any{"from_node": "await", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "await", "to_port": "in", "sequence": 0},
		},
		[]any{map[string]any{"from_node": "await", "from_port": "value", "to_node": "mark", "to_port": "value"}}, []any{}, []any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func testParityInventoryAwait(t *testing.T) {
	a := execScene(t, "inventory-await-a", operatorAwaitProgram())
	startScene(t, a)
	mustFire(t, a, "e")
	waitForPendingAwait(t, a, "bp", "pick", 2*time.Second)
	if err := a.ResolveAwait("bp", "pick", raw(`42`)); err != nil {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=resolve Engine A: %v", err)
	}
	waitForState(t, a, "__vars.bp.got", "42", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.got")

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-await-b", parityBuildBAwaitProgram(t))
	bResolved, err := h.Resolve(bluehost.SlotOnAir, "confirm", json.Number("42"))
	if err != nil {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=resolve Engine B: %v", err)
	}
	parityAssertInventoryResult(t, "core.operator.await-value@1", "resolve", aValue, bResolved.Outputs["result"])

	aMismatch := execScene(t, "inventory-await-mismatch-a", operatorAwaitProgram())
	startScene(t, aMismatch)
	mustFire(t, aMismatch, "e")
	waitForPendingAwait(t, aMismatch, "bp", "pick", 2*time.Second)
	if err := aMismatch.ResolveAwait("bp", "pick", raw(`3.5`)); err != ErrAwaitTypeMismatch {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=type-mismatch Engine A err=%v, want ErrAwaitTypeMismatch", err)
	}
	bMismatch := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-await-mismatch-b", parityBuildBAwaitProgram(t))
	_, bMismatchErr := bMismatch.Resolve(bluehost.SlotOnAir, "confirm", 3.5)
	if bMismatchErr != nil {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=type-mismatch Engine B unexpectedly rejected value: %v", bMismatchErr)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.operator.await-value@1", Scenario: "type-mismatch",
		EngineA: "ResolveAwait rejects fractional value for core.primitive.integer with ErrAwaitTypeMismatch",
		EngineB: "bluehost.Host.Resolve accepts 3.5 because current Blue await config has no value_type admission",
	})
}

func parityBuildBGateProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-inventory-gate",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.flow.gate@1", "kind": "control", "config": []any{parityDataPort("start_closed", "core.json", false)}, "inputs": []any{parityExecPort("enter"), parityExecPort("open")}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "gate", "opcode": "core.flow.gate@1", "config": map[string]any{"start_closed": true}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{
			map[string]any{"from_node": "gate", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "gate", "to_port": "open", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "gate", "to_port": "enter", "sequence": 1},
		},
		[]any{}, []any{map[string]any{"node_id": "mark", "port": "value", "value": true}}, []any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func testParityInventoryGate(t *testing.T) {
	a := execScene(t, "inventory-gate-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq":  {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{"then_0": {Node: "gate", Port: "open"}, "then_1": {Node: "gate", Port: "enter"}}},
			"gate": {ID: "gate", Op: OpGate, Config: map[string]json.RawMessage{"start_closed": raw(`true`)}, Next: map[string]ExecTarget{"then": {Node: "mark"}}},
			"mark": constSet("mark", "result", `true`),
		},
		Entrypoints: map[string]ExecEntry{"start": {Target: ExecTarget{Node: "seq"}}},
	})
	startScene(t, a)
	mustFire(t, a, "start")
	waitForState(t, a, "__vars.bp.result", "true", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.result")
	b := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-gate-b", parityBuildBGateProgram(t))
	step := parityBStep(t, b, bluehost.SlotOnAir, "core.flow.gate@1")
	parityAssertInventoryResult(t, "core.flow.gate@1", "open-then-enter", aValue, step.Outputs["result"])
}

func parityBuildBDelayProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-inventory-delay",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.flow.delay@1", "kind": "control", "config": []any{}, "inputs": []any{parityDataPort("seconds", "core.json", true), parityExecPort("in")}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "delay", "opcode": "core.flow.delay@1", "config": map[string]any{}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{
			map[string]any{"from_node": "delay", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "delay", "to_port": "in", "sequence": 0},
		},
		[]any{}, []any{
			map[string]any{"node_id": "delay", "port": "seconds", "value": 1.5},
			map[string]any{"node_id": "mark", "port": "value", "value": true},
		}, []any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func testParityInventoryDelay(t *testing.T) {
	clock := newFakeClock()
	metrics := &fakeExecMetrics{}
	a := execScene(t, "inventory-delay-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"delay": {ID: "delay", Op: OpDelay, Config: map[string]json.RawMessage{"seconds": raw(`1.5`)}, Next: map[string]ExecTarget{"then": {Node: "mark"}}},
			"mark":  constSet("mark", "result", `true`),
		},
		Entrypoints: map[string]ExecEntry{"start": {Target: ExecTarget{Node: "delay"}}},
	})
	a.SetClock(clock)
	a.SetExecMetrics(metrics)
	startScene(t, a)
	mustFire(t, a, "start")
	waitFor(t, "inventory delay armed", func() bool {
		_, _, parked := metrics.counts()
		return parked == 1
	})
	clock.Advance(time.Second)
	stateAbsent(t, a, "__vars.bp.result")
	clock.Advance(500 * time.Millisecond)
	waitForState(t, a, "__vars.bp.result", "true", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.result")

	b := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-delay-b", parityBuildBDelayProgram(t))
	before, err := b.Tick(bluehost.SlotOnAir, 1)
	if err != nil {
		t.Fatalf("primitive=core.flow.delay@1 scenario=before-deadline Engine B Tick: %v", err)
	}
	if value, ok := before.Variables["result"]; ok && value != nil {
		t.Fatalf("primitive=core.flow.delay@1 scenario=before-deadline fired early: %#v", value)
	}
	after, err := b.Tick(bluehost.SlotOnAir, 0.5)
	if err != nil {
		t.Fatalf("primitive=core.flow.delay@1 scenario=deadline Engine B Tick: %v", err)
	}
	parityAssertInventoryResult(t, "core.flow.delay@1", "deadline", aValue, after.Variables["result"])
}
