package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// TestEngineABParity_OperationInventory is the unique-9 inventory for the
// Engine A/Engine B operation corpus. Each row either exercises both real
// seams or owns a separately named non-equivalence test; no unsupported
// operation is counted as parity.
func TestEngineABParity_OperationInventory(t *testing.T) {
	cases := []struct {
		id  string
		run func(*testing.T)
		gap parityNonEquivalenceError
	}{
		{id: "core.http.request@1", run: testParityInventoryHTTP},
		{id: "core.db.query@1", run: TestEngineABParity_DBQueryObservableAndPreviewNoQuery},
		{id: "core.service.call@1", run: testParityInventoryService},
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
	waitForState(t, aPreview.Current(), "__vars.bp.status", "0", 2*time.Second)
	aPreviewValue, _ := aPreview.Current().state.Get("__vars.bp.status")
	if got := requests.Load(); got != 2 {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine A request count=%d, want 2 (A on-air, B on-air; preview synthetic)", got)
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
	parityAssertInventoryResult(t, "core.http.request@1", "preview", aPreviewValue, bPreviewStep.Outputs["result"])
	if got := requests.Load(); got != 2 {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine B performed a network request; count=%d", got)
	}
	if string(aPreviewValue) != "0" {
		t.Fatalf("primitive=core.http.request@1 scenario=preview Engine A status=%s, want synthetic 0", aPreviewValue)
	}
}

func TestEngineABParity_EventSequenceRecoveryDedupAndOutOfOrder(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	prepare := func(t *testing.T, suffix string) (*Show, *Scene, *CanonicalEventIngress, *bluehost.Host) {
		t.Helper()
		_, a, ingress := parityPrepareAPlatformIngress(t, "inventory-event-a-"+suffix, leaf)
		h := parityPrepareBHost(t, bluehost.SlotOnAir, "inventory-event-b-"+suffix, parityBuildBEntrypointProgram(t, "platform-event", leaf))
		providers.ResetActiveIngress(h)
		t.Cleanup(func() { providers.ResetActiveIngress(h) })
		return nil, a, ingress, h
	}

	t.Run("gap-recovery-1-3-2", func(t *testing.T) {
		_, a, ingress, h := prepare(t, "recovery")
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-1", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap A seq=1 receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-1", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap B seq=1 receipt=%+v err=%v", firstB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=1")
		_, gapAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		_, gapBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		if parityIngressErrorCode(gapAErr) != "EVENT_SEQUENCE_GAP" || parityBlueErrorCode(gapBErr) != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap gap codes: A=%s err=%v B=%s err=%v", parityIngressErrorCode(gapAErr), gapAErr, parityBlueErrorCode(gapBErr), gapBErr)
		}
		secondA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-2", "platform.twitch", 2, "two"))
		if err != nil || secondA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap A seq=2 receipt=%+v err=%v", secondA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"two"`, 2*time.Second)
		secondB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-2", "platform.twitch", 2, "two"))
		if err != nil || secondB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap B seq=2 receipt=%+v err=%v", secondB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=2")
		thirdA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		if err != nil || thirdA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap A retry seq=3 receipt=%+v err=%v", thirdA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"three"`, 2*time.Second)
		thirdB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-3", "platform.twitch", 3, "three"))
		if err != nil || thirdB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap B retry seq=3 receipt=%+v err=%v", thirdB, err)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap retry seq=3")
		if firstA.RuntimeSequence != firstB.RuntimeSequence || secondA.RuntimeSequence != secondB.RuntimeSequence || thirdA.RuntimeSequence != thirdB.RuntimeSequence || string(canonicalJSONForTest(step.Outputs["result"])) != `"three"` {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap A receipts=[%+v %+v %+v] B receipts=[%+v %+v %+v] result=%#v", firstA, secondA, thirdA, firstB, secondB, thirdB, step.Outputs["result"])
		}
	})

	t.Run("exact-duplicate-execution-count", func(t *testing.T) {
		_, a, ingress, h := prepare(t, "duplicate")
		event := parityEventEnvelope(t, "evt-duplicate", "platform.twitch", 1, "same")
		firstA, err := ingress.InjectPlatform(leaf, event)
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate A first receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"same"`, 2*time.Second)
		duplicateA, err := ingress.InjectPlatform(leaf, event)
		firstB, errB := providers.InjectActivePlatform(h, leaf, event)
		if errB != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate B first receipt=%+v err=%v", firstB, errB)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "duplicate first")
		duplicateB, errB := providers.InjectActivePlatform(h, leaf, event)
		if err != nil || duplicateA != firstA || errB != nil || duplicateB != firstB || string(canonicalJSONForTest(step.Outputs["result"])) != `"same"` {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate A=(%+v err=%v duplicate=%+v) B=(%+v err=%v duplicate=%+v) result=%#v", firstA, err, duplicateA, firstB, errB, duplicateB, step.Outputs["result"])
		}
	})

	t.Run("same-sequence-conflict", func(t *testing.T) {
		_, a, ingress, h := prepare(t, "conflict")
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-conflict-one", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict A first receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)
		_, conflictAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-conflict-two", "platform.twitch", 1, "again"))
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-conflict-one", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict B first receipt=%+v err=%v", firstB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "conflict first")
		_, conflictBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-conflict-two", "platform.twitch", 1, "again"))
		if parityIngressErrorCode(conflictAErr) != "EVENT_SEQUENCE_CONFLICT" || parityBlueErrorCode(conflictBErr) != "EVENT_SEQUENCE_CONFLICT" || firstA.RuntimeSequence != firstB.RuntimeSequence {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict A receipt=%+v code=%s B receipt=%+v code=%s", firstA, parityIngressErrorCode(conflictAErr), firstB, parityBlueErrorCode(conflictBErr))
		}
	})

	t.Run("out-of-order-before-first", func(t *testing.T) {
		_, a, ingress, h := prepare(t, "out-of-order")
		_, gapAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-out-of-order", "platform.twitch", 2, "two"))
		_, gapBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-out-of-order", "platform.twitch", 2, "two"))
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-out-of-order-1", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first A recovery receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-out-of-order-1", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first B recovery receipt=%+v err=%v", firstB, err)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "out-of-order recovery")
		if parityIngressErrorCode(gapAErr) != "EVENT_SEQUENCE_GAP" || parityBlueErrorCode(gapBErr) != "EVENT_SEQUENCE_GAP" || firstA.RuntimeSequence != firstB.RuntimeSequence || string(canonicalJSONForTest(step.Outputs["result"])) != `"one"` {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first A code=%s receipt=%+v B code=%s receipt=%+v result=%#v", parityIngressErrorCode(gapAErr), firstA, parityBlueErrorCode(gapBErr), firstB, step.Outputs["result"])
		}
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
	opConfig := make(map[string]any, len(config))
	for key, item := range config {
		opConfig[key] = item
	}
	var payload any
	hasPayload := false
	if opcodeID == "core.show.emit@1" {
		payload, hasPayload = opConfig["payload"]
		delete(opConfig, "payload")
	}
	opInputs := []any{parityExecPort("in")}
	execEdges := []any{
		map[string]any{"from_node": "op", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
		map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "op", "to_port": "in", "sequence": 0},
	}
	dataEdges := []any{}
	dataLiterals := []any{map[string]any{"node_id": "mark", "port": "value", "value": value}}
	if hasPayload {
		// Blue's canonical port order places data inputs before the exec
		// trigger. Keep the added payload deterministic and schema-valid.
		opInputs = []any{parityDataPort("payload", "core.json", false), parityExecPort("in")}
		// A direct data literal is the canonical ABI for an unwired effect
		// input. Do not introduce a synthetic core.literal node here: Blue's
		// walker rejects that fixture shape before show.emit can run.
		dataLiterals = append(dataLiterals, map[string]any{"node_id": "op", "port": "payload", "value": payload})
	}
	opcodes := []any{
		map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
		map[string]any{"id": opcodeID, "kind": "control", "config": parityInventoryConfigPorts(opcodeID), "inputs": opInputs, "outputs": []any{parityExecPort("then")}},
		map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
	}
	nodes := []any{
		map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
		map[string]any{"id": "op", "opcode": opcodeID, "config": opConfig},
		map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
	}
	return parityBuildProgram(t, "ab-inventory-local",
		opcodes,
		nodes,
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		execEdges, dataEdges, dataLiterals, []any{},
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

type parityShowEmitter struct {
	show    *Show
	mu      sync.Mutex
	topic   string
	payload json.RawMessage
}

func (e *parityShowEmitter) EmitToActive(topic string, payload json.RawMessage) {
	e.mu.Lock()
	e.topic = topic
	e.payload = append(e.payload[:0], payload...)
	e.mu.Unlock()
	(&activeOnlyEmitter{show: e.show}).EmitToActive(topic, payload)
}

func (e *parityShowEmitter) snapshot() (string, json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.topic, append(json.RawMessage(nil), e.payload...)
}

func parityLocalRecord(t *testing.T, step blueruntime.StepResult, primitive string) map[string]any {
	t.Helper()
	bagName := "__" + strings.TrimSuffix(primitive[len("core."):], "@1")
	bag, ok := step.Variables[bagName].(map[string]any)
	if !ok {
		t.Fatalf("primitive=%s scenario=local-side-effect Engine B reserved observable=%#v", primitive, step.Variables)
	}
	record, ok := bag["op"].(map[string]any)
	if !ok {
		t.Fatalf("primitive=%s scenario=local-side-effect Engine B structured record=%#v", primitive, bag["op"])
	}
	if record["opcode"] != primitive || record["node_id"] != "op" {
		t.Fatalf("primitive=%s scenario=local-side-effect Engine B record identity=%#v", primitive, record)
	}
	continuation, ok := record["continuation"].(map[string]any)
	if !ok || continuation["port"] != "then" || continuation["status"] != "fired" {
		t.Fatalf("primitive=%s scenario=local-side-effect Engine B continuation=%#v", primitive, record["continuation"])
	}
	return record
}

func parityServiceCallProgram() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"call": {ID: "call", Op: OpServiceCall, Config: map[string]json.RawMessage{
				bakedRouteConfigKey: raw(echoRouteJSON),
				"params":            raw(`{"name":"alice"}`),
				"payload":           raw(`{"source":"orion-358"}`),
			}, Next: map[string]ExecTarget{
				"then":  {Node: "set.status"},
				"error": {Node: "set.err"},
			}},
			"set.status": setFromPin("set.status", "status", "call", "status", map[string]ExecTarget{"then": {Node: "set.body"}}),
			"set.body":   setFromPin("set.body", "body", "call", "body", map[string]ExecTarget{"then": {Node: "set.then"}}),
			"set.then":   constSet("set.then", "then", `"then"`),
			"set.err":    setFromPin("set.err", "error", "call", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "call"}}},
	}
}

func parityBuildBServiceProgram(t *testing.T) []byte {
	t.Helper()
	// Blue's canonical ABI carries only the compiler-baked route reference.
	// The host resolves it through the authoritative registry supplied in
	// EffectDeps; method/path/token scope never comes from this program.
	route := map[string]any{"service": "example", "route_id": "example.echo"}
	return parityBuildProgram(t, "ab-inventory-service",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{
				"id": "core.service.call@1", "kind": "control",
				"config": []any{},
				"inputs": []any{
					parityDataPort("params", "core.json", false), parityDataPort("payload", "core.json", false),
					parityDataPort("timeout_ms", "core.json", false), parityExecPort("in"),
				},
				"outputs": []any{
					parityDataPort("body", "core.json", false), parityDataPort("error", "core.json", false),
					parityDataPort("ok", "core.json", false), parityDataPort("status", "core.json", false),
					parityExecPort("error"), parityExecPort("then"),
				},
			},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{parityDataPort("variable", "core.string", true)},
				"inputs":  []any{parityDataPort("value", "core.json", true), parityExecPort("in")},
				"outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")},
			},
		},
		[]any{
			map[string]any{"id": "call", "opcode": "core.service.call@1", "config": map[string]any{"__route": route}},
			map[string]any{"id": "set-body", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "body"}},
			map[string]any{"id": "set-error", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "error"}},
			map[string]any{"id": "set-status", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "status"}},
			map[string]any{"id": "set-then", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "then"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{
			map[string]any{"from_node": "call", "from_port": "error", "to_node": "set-error", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "call", "from_port": "then", "to_node": "set-status", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-body", "from_port": "then", "to_node": "set-then", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "set-status", "from_port": "then", "to_node": "set-body", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "call", "to_port": "in", "sequence": 0},
		},
		[]any{
			map[string]any{"from_node": "call", "from_port": "body", "to_node": "set-body", "to_port": "value"},
			map[string]any{"from_node": "call", "from_port": "error", "to_node": "set-error", "to_port": "value"},
			map[string]any{"from_node": "call", "from_port": "status", "to_node": "set-status", "to_port": "value"},
		},
		[]any{
			map[string]any{"node_id": "call", "port": "params", "value": map[string]any{"name": "alice"}},
			map[string]any{"node_id": "call", "port": "payload", "value": map[string]any{"source": "orion-358"}},
			map[string]any{"node_id": "set-then", "port": "value", "value": "then"},
		}, []any{},
		[]any{
			map[string]any{"name": "body", "type": "core.json", "initial": nil},
			map[string]any{"name": "error", "type": "core.json", "initial": nil},
			map[string]any{"name": "status", "type": "core.json", "initial": nil},
			map[string]any{"name": "then", "type": "core.json", "initial": nil},
		},
	)
}

func testParityInventoryService(t *testing.T) {
	type request struct {
		method string
		path   string
		body   []byte
	}
	var mu sync.Mutex
	var requests []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("primitive=core.service.call@1 scenario=success read request body: %v", err)
		}
		mu.Lock()
		requests = append(requests, request{method: r.Method, path: r.URL.Path, body: append([]byte(nil), body...)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"source":"blue314"}`))
	}))
	defer srv.Close()
	minter := &recordingMinter{token: "inventory-token"}
	client := effects.NewServiceCallClient(srv.URL, minter.mint, nil)

	a := effectsScene(t, "inventory-service-a", parityServiceCallProgram(), &SceneEffects{
		Runner: newTestRunner(t), ServiceCall: client,
	})
	startScene(t, a)
	mustFire(t, a, "e")
	waitForState(t, a, "__vars.bp.status", "200", 2*time.Second)
	waitForState(t, a, "__vars.bp.body", `{"ok":true,"source":"blue314"}`, 2*time.Second)
	aStatus, _ := a.state.Get("__vars.bp.status")
	aBody, _ := a.state.Get("__vars.bp.body")
	aThen, _ := a.state.Get("__vars.bp.then")
	if string(aThen) != `"then"` {
		t.Fatalf("primitive=core.service.call@1 scenario=success Engine A then=%s, want \"then\"", aThen)
	}
	if _, ok := a.state.Get("__vars.bp.error"); ok {
		t.Fatalf("primitive=core.service.call@1 scenario=success Engine A unexpectedly fired error")
	}

	h := bluehost.NewHost()
	t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
	if err := h.Prepare(bluehost.SlotOnAir, "inventory-service-b", "sha256:inventory-service-b", parityBuildBServiceProgram(t), nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{ServiceCall: client, ResolveServiceRoute: parityServiceRouteResolver}, blueruntime.Execute)); err != nil {
		if parityBlueErrorCode(err) != "PROGRAM_PORT_INVALID" {
			t.Fatalf("primitive=core.service.call@1 scenario=real-blue-route-seam unexpected Engine B Host.Prepare error code=%q err=%v", parityBlueErrorCode(err), err)
		}
		t.Fatalf("primitive=core.service.call@1 scenario=real-blue-route-seam BLOCKER: Engine A status=%s body=%s path=/example/api/v1/items/alice/echo then=%q; the canonical Blue route reference __route={service,route_id} was rejected before execution with %v. Recheck the consumed Blue ABI and the authoritative Orion route resolver before claiming A/B status/path/body/then/error parity", aStatus, aBody, aThen, err)
	}
	bStep := parityBStep(t, h, bluehost.SlotOnAir, "service.call success")
	if bError, ok := bStep.Variables["error"].(string); ok && bError != "" {
		mu.Lock()
		requestCount := len(requests)
		mu.Unlock()
		if !strings.HasPrefix(bError, "EGRESS_ROUTE_UNRESOLVED") {
			t.Fatalf("primitive=core.service.call@1 scenario=real-blue-route-seam unexpected Engine B error=%q; request count=%d", bError, requestCount)
		}
		t.Fatalf("primitive=core.service.call@1 scenario=real-blue-route-seam BLOCKER: Engine A status=%s body=%s path=/example/api/v1/items/alice/echo then=%q; Engine B received the canonical route reference but the Host/API dependency bundle carried no authoritative resolver and returned %q, with no status/body/path/then and %d server requests", aStatus, aBody, aThen, bError, requestCount)
	}
	bStatus, ok := bStep.Variables["status"].(json.Number)
	if !ok || bStatus.String() != string(aStatus) {
		t.Fatalf("primitive=core.service.call@1 scenario=success status diverges: Engine A=%s Engine B=%#v", aStatus, bStep.Variables["status"])
	}
	parityAssertInventoryResult(t, "core.service.call@1", "success-response-body", aBody, bStep.Variables["body"])
	if got, _ := bStep.Variables["then"].(string); got != "then" {
		t.Fatalf("primitive=core.service.call@1 scenario=success Engine B then=%q, want then", got)
	}
	if value := bStep.Variables["error"]; value != nil {
		t.Fatalf("primitive=core.service.call@1 scenario=success Engine B unexpectedly fired error: %#v", value)
	}

	mu.Lock()
	if len(requests) != 2 {
		mu.Unlock()
		t.Fatalf("primitive=core.service.call@1 scenario=success request count=%d, want Engine A and Engine B", len(requests))
	}
	aRequest, bRequest := requests[0], requests[1]
	mu.Unlock()
	if aRequest.method != bRequest.method || aRequest.path != bRequest.path || !bytes.Equal(aRequest.body, bRequest.body) {
		t.Fatalf("primitive=core.service.call@1 scenario=success request observable diverges: Engine A=%+v Engine B=%+v", aRequest, bRequest)
	}
	if aRequest.method != http.MethodPost || aRequest.path != "/example/api/v1/items/alice/echo" || string(aRequest.body) != `{"source":"orion-358"}` {
		t.Fatalf("primitive=core.service.call@1 scenario=success unexpected request=%+v", aRequest)
	}

	t.Run("typed-error-and-error-routing", func(t *testing.T) {
		var errorHits atomic.Int32
		errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			errorHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer errorServer.Close()
		noToken := &recordingMinter{}
		aErrScene := effectsScene(t, "inventory-service-error-a", parityServiceCallProgram(), &SceneEffects{
			Runner: newTestRunner(t), ServiceCall: effects.NewServiceCallClient(errorServer.URL, noToken.mint, nil),
		})
		startScene(t, aErrScene)
		mustFire(t, aErrScene, "e")
		waitFor(t, "Engine A service.call error continuation", func() bool {
			_, ok := aErrScene.state.Get("__vars.bp.error")
			return ok
		})
		aError, _ := aErrScene.state.Get("__vars.bp.error")

		bErrHost := bluehost.NewHost()
		t.Cleanup(func() { _ = bErrHost.Release(bluehost.SlotOnAir, "test-cleanup") })
		if err := bErrHost.Prepare(bluehost.SlotOnAir, "inventory-service-error-b", "sha256:inventory-service-error-b", parityBuildBServiceProgram(t), nil, nil,
			bluehost.NewEffectHandlers(bluehost.EffectDeps{ServiceCall: effects.NewServiceCallClient(errorServer.URL, noToken.mint, nil), ResolveServiceRoute: parityServiceRouteResolver}, blueruntime.Execute)); err != nil {
			t.Fatalf("primitive=core.service.call@1 scenario=typed-error Engine B Host.Prepare: %v", err)
		}
		bErrorStep := parityBStep(t, bErrHost, bluehost.SlotOnAir, "service.call typed error")
		parityAssertInventoryResult(t, "core.service.call@1", "typed-error", aError, bErrorStep.Variables["error"])
		if _, ok := aErrScene.state.Get("__vars.bp.then"); ok || bErrorStep.Variables["then"] != nil {
			t.Fatalf("primitive=core.service.call@1 scenario=typed-error then branch fired on Engine A/B")
		}
		if got := errorHits.Load(); got != 0 {
			t.Fatalf("primitive=core.service.call@1 scenario=typed-error server hits=%d, want zero without scoped token", got)
		}
	})
}

func testParityInventoryShow(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	emitter := &parityShowEmitter{show: show}
	show.SetEmitter(emitter)
	show.LoadExec("inventory-show-a", varsGraph("inventory-show-a"), &compiler.RenderBundle{SceneVersion: "sha256:inventory-show"}, emitOnStartProg("inventory"))
	if err := show.SetActive("inventory-show-a", nil); err != nil {
		t.Fatalf("primitive=core.show.emit@1 scenario=on-air Engine A SetActive: %v", err)
	}
	a, _ := show.Get("inventory-show-a")
	waitForState(t, a, "__vars.bp.emitted", "1", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.emitted")
	topic, payload := emitter.snapshot()
	if topic != "inventory" || string(payload) != `{"k":"v"}` {
		t.Fatalf("primitive=core.show.emit@1 scenario=external-observable Engine A emission=(%q,%s), want inventory/{k:v}", topic, payload)
	}
	b := parityAssertBLocalSideEffect(t, "core.show.emit@1", 1, map[string]any{
		"topic": "inventory", "payload": map[string]any{"k": "v"},
	})
	parityAssertInventoryResult(t, "core.show.emit@1", "then-routing", aValue, b.Outputs["result"])
	record := parityLocalRecord(t, b, "core.show.emit@1")
	config, _ := record["config"].(map[string]any)
	inputs, _ := record["inputs"].(map[string]any)
	if config["topic"] != topic || string(canonicalJSONForTest(inputs["payload"])) != string(canonicalJSONForTest(map[string]any{"k": "v"})) {
		t.Fatalf("primitive=core.show.emit@1 scenario=external-observable semantic record=%#v, want topic/payload parity", record)
	}
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
	record := parityLocalRecord(t, b, "core.overlay-app.set@1")
	config, _ := record["config"].(map[string]any)
	if config["app_id"] != "inventory-app" || config["running"] != true {
		t.Fatalf("primitive=core.overlay-app.set@1 scenario=external-observable semantic record=%#v, want app_id/running parity", config)
	}
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
	if got := animScalarGen(t, a, "__anim.ov"); got != 1 {
		t.Fatalf("primitive=core.animation.play@1 scenario=renderer-trigger Engine A generation=%d, want 1", got)
	}
	record := parityLocalRecord(t, b, "core.animation.play@1")
	config, _ := record["config"].(map[string]any)
	if config["overlay_id"] != "ov" || config["animation_id"] != "fade" || fmt.Sprint(config["duration_seconds"]) != "0" {
		t.Fatalf("primitive=core.animation.play@1 scenario=renderer-trigger semantic record=%#v, want authored trigger parity", config)
	}
}

func parityBuildBAwaitProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-inventory-await",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.operator.await-value@1", "kind": "control", "config": []any{parityDataPort("await_name", "core.string", false), parityDataPort("value_type", "core.string", false)}, "inputs": []any{parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "await", "opcode": "core.operator.await-value@1", "config": map[string]any{"await_name": "confirm", "value_type": "core.primitive.integer"}},
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
	if parityBlueErrorCode(bMismatchErr) != "AWAIT_TYPE_MISMATCH" {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=type-mismatch Engine B code=%q err=%v, want AWAIT_TYPE_MISMATCH", parityBlueErrorCode(bMismatchErr), bMismatchErr)
	}
	if _, err := bMismatch.Resolve(bluehost.SlotOnAir, "confirm", json.Number("3")); err != nil {
		t.Fatalf("primitive=core.operator.await-value@1 scenario=type-mismatch Engine B did not remain recoverable after rejection: %v", err)
	}
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
