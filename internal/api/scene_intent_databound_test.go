package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// chatDrivenProgram builds a self-contained blue.program.v1 fixture standing
// in for the ADR-BLUE-012 R6 §6.2 ZabCanvas#327 attested authoring fixture
// (Orion#181): a chat-driven scene reusing three chained pure computations —
// the Orion-side analogue of "reusing 3 published blueprint-functions", since
// by the time a program reaches Orion it is already flat/compiled (Canvas/
// Blue own the authoring-time expansion, not Orion — issue #181 scope is
// adaptation/host/provider only):
//
//  1. core.data.get-field@1  — extract payload.message from the chat event
//  2. core.string.concat@1   — format it behind an "overlay" prefix
//  3. core.math.add@1        — accumulate a running chat_count
//
// Both results are stored via core.variable.set@1 into declared state
// outputs (overlay_message, chat_count) — the only mechanism that feeds
// StepResult.Outputs and therefore blueproject.Project/bluewire.Bridge
// (core.output@1 writes elsewhere and does NOT feed projection, see
// blue-runtime-go handlers.go:370-375).
func chatDrivenProgram(t *testing.T, leaf string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version":   blueruntime.ProgramSchema,
		"program_id":       "orion-181-chat-overlay",
		"compiler_version": blueruntime.CompilerVersion,
		"runtime_abi":      blueruntime.RuntimeABI,
		"runtime_module":   map[string]any{"name": blueruntime.RuntimeModule, "version": blueruntime.RuntimeModuleVer},
		"source_revision": map[string]any{
			"id": "orion-181-chat-overlay", "kind": "blueprint-version", "revision": json.Number("1"),
			"digest": "sha256:3aa201ed0203ce41437936bc3d4de2f6b7f3689117eec787ad0a4b5a7eb64158",
		},
		"determinism": map[string]any{"clock": "injected", "entropy": "injected", "map_iteration": "canonical", "scheduler": "injected"},
		"errors":      map[string]any{"profile": "blue.runtime.error.v1", "unhandled": "halt"},
		"ordering": map[string]any{
			"duplicate_event": "idempotent_same_digest", "effect_completion": "explicit_fifo_event",
			"event_inbox": "fifo_by_runtime_sequence", "event_sequence_scope": "instance_per_origin",
			"exec_fanout": "ascending_edge_sequence", "sequence_gap": "reject", "timer_tie_break": "due_at_then_timer_id",
		},
		"budgets": map[string]any{
			"max_steps_per_dispatch": json.Number("32"), "max_queue_depth": json.Number("32"), "max_execution_ms": json.Number("1000"),
			"max_pending_effects": json.Number("0"), "max_effects_per_dispatch": json.Number("0"), "max_timers": json.Number("0"),
		},
		"types": []any{
			map[string]any{"id": "core.json", "kind": "primitive", "spec": map[string]any{"base": "json"}},
			map[string]any{"id": "core.string", "kind": "primitive", "spec": map[string]any{"base": "string"}},
			map[string]any{"id": "core.number", "kind": "primitive", "spec": map[string]any{"base": "number"}},
		},
		"opcodes": []any{
			map[string]any{"id": "core.event.on-platform-event@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{dataPort("payload", "core.json", false), execPort("then")}},
			map[string]any{
				"id": "core.data.get-field@1", "kind": "pure",
				"config":  []any{dataPort("path", "core.string", true)},
				"inputs":  []any{dataPort("record", "core.json", true)},
				"outputs": []any{dataPort("value", "core.json", false)},
			},
			map[string]any{
				"id": "core.literal@1", "kind": "pure",
				"config":  []any{dataPort("value", "core.json", true)},
				"inputs":  []any{},
				"outputs": []any{dataPort("value", "core.json", false)},
			},
			map[string]any{
				"id": "core.string.concat@1", "kind": "pure",
				"config":  []any{},
				"inputs":  []any{dataPort("a", "core.json", true), dataPort("b", "core.json", true)},
				"outputs": []any{dataPort("result", "core.json", false)},
			},
			map[string]any{
				"id": "core.math.add@1", "kind": "pure",
				"config":  []any{},
				"inputs":  []any{dataPort("a", "core.json", true), dataPort("b", "core.json", true)},
				"outputs": []any{dataPort("sum", "core.json", false)},
			},
			map[string]any{
				"id": "core.variable.get@1", "kind": "pure",
				"config":  []any{dataPort("variable", "core.string", true)},
				"inputs":  []any{},
				"outputs": []any{dataPort("value", "core.json", false)},
			},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{dataPort("variable", "core.string", true)},
				"inputs":  []any{dataPort("value", "core.json", true), execPort("in")},
				"outputs": []any{dataPort("value", "core.json", false), execPort("then")},
			},
		},
		"nodes": []any{
			map[string]any{"id": "platform-entry", "opcode": "core.event.on-platform-event@1", "config": map[string]any{}},
			map[string]any{"id": "get-message", "opcode": "core.data.get-field@1", "config": map[string]any{"path": "message"}},
			map[string]any{"id": "prefix-literal", "opcode": "core.literal@1", "config": map[string]any{"value": "chat: "}},
			map[string]any{"id": "format-message", "opcode": "core.string.concat@1", "config": map[string]any{}},
			map[string]any{"id": "message-set", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "overlay_message"}},
			map[string]any{"id": "counter-get", "opcode": "core.variable.get@1", "config": map[string]any{"variable": "chat_count"}},
			map[string]any{"id": "one-literal", "opcode": "core.literal@1", "config": map[string]any{"value": json.Number("1")}},
			map[string]any{"id": "counter-add", "opcode": "core.math.add@1", "config": map[string]any{}},
			map[string]any{"id": "counter-set", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "chat_count"}},
		},
		"entrypoints": []any{
			map[string]any{"id": "platform", "kind": "platform-event", "node_id": "platform-entry", "port": "then", "leaf": leaf},
		},
		// exec_edges / data_edges must be listed in the runtime's canonical
		// composite order (exec: from_node,from_port,sequence,to_node,to_port;
		// data: to_node,to_port,from_node,from_port) — program.go's
		// sortedComposite/compareExecEdge reject an unsorted array outright.
		// This is a pure serialization-order constraint; it does not affect
		// actual firing order, which the walker derives from the graph itself.
		"exec_edges": []any{
			map[string]any{"from_node": "message-set", "from_port": "then", "to_node": "counter-set", "to_port": "in", "sequence": json.Number("0")},
			map[string]any{"from_node": "platform-entry", "from_port": "then", "to_node": "message-set", "to_port": "in", "sequence": json.Number("0")},
		},
		"data_edges": []any{
			map[string]any{"from_node": "counter-get", "from_port": "value", "to_node": "counter-add", "to_port": "a"},
			map[string]any{"from_node": "one-literal", "from_port": "value", "to_node": "counter-add", "to_port": "b"},
			map[string]any{"from_node": "counter-add", "from_port": "sum", "to_node": "counter-set", "to_port": "value"},
			map[string]any{"from_node": "prefix-literal", "from_port": "value", "to_node": "format-message", "to_port": "a"},
			map[string]any{"from_node": "get-message", "from_port": "value", "to_node": "format-message", "to_port": "b"},
			map[string]any{"from_node": "platform-entry", "from_port": "payload", "to_node": "get-message", "to_port": "record"},
			map[string]any{"from_node": "format-message", "from_port": "result", "to_node": "message-set", "to_port": "value"},
		},
		"data_literals": []any{},
		"effects":       []any{},
		"requires":      []any{},
		"topics":        []any{},
		"timers":        []any{},
		"state": map[string]any{
			"variables": []any{
				map[string]any{"name": "chat_count", "type": "core.number", "initial": json.Number("0")},
				map[string]any{"name": "overlay_message", "type": "core.string", "initial": ""},
			},
			"outputs": []any{
				map[string]any{"name": "chat_count", "type": "core.number"},
				map[string]any{"name": "overlay_message", "type": "core.string"},
			},
		},
	}
	// The canonical LSML writer requires certain arrays in ascending `id`
	// order (mirrors internal/providers/events_test.go's platformIngressProgram)
	// — ParseProgram rejects `/types` etc. "not in canonical order" otherwise.
	for _, key := range []string{"types", "opcodes", "nodes", "entrypoints"} {
		items := program[key].([]any)
		sort.Slice(items, func(i, j int) bool {
			left, _ := items[i].(map[string]any)["id"].(string)
			right, _ := items[j].(map[string]any)["id"].(string)
			return left < right
		})
	}

	digest, err := providers.Digest(program)
	if err != nil {
		t.Fatalf("Digest(chat-driven program): %v", err)
	}
	program["program_digest"] = digest
	data, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal chat-driven program: %v", err)
	}
	return data
}

// chatMessageEvent builds a blue.runtime.event.v1 envelope for one chat
// message, using the same quasar.<platform>.<channel> / quasar.<platform>.
// <type> convention internal/providers.platformLeafFromEvent derives a
// canonical leaf from (mirrors internal/providers/events_test.go's
// platformEvent helper).
func chatMessageEvent(t *testing.T, eventID string, sequence uint64, message string) []byte {
	t.Helper()
	data, err := providers.BuildEvent(
		eventID, "quasar.twitch.zablab_chat", "quasar.twitch.chat_message", "corr-181",
		sequence, 1786500000000+int64(sequence),
		map[string]any{"message": message},
	)
	if err != nil {
		t.Fatalf("BuildEvent(%s): %v", eventID, err)
	}
	return data
}

// capturingMirror records every Delta bluewire.Bridge forwards, in order —
// the observation seam for asserting the actual PROJECTED EFFECT of chat
// injection, not merely that injection was accepted.
type capturingMirror struct {
	mu     sync.Mutex
	deltas []*protocol.Delta
}

func (m *capturingMirror) Forward(msg any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := msg.(*protocol.Delta); ok {
		m.deltas = append(m.deltas, d)
	}
}

func (m *capturingMirror) snapshot() []*protocol.Delta {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*protocol.Delta, len(m.deltas))
	copy(out, m.deltas)
	return out
}

func patchValue(t *testing.T, delta *protocol.Delta, path string) json.RawMessage {
	t.Helper()
	for _, p := range delta.Patches {
		if p.Path == path {
			return p.Value
		}
	}
	t.Fatalf("delta has no patch for path %q (patches: %+v)", path, delta.Patches)
	return nil
}

// TestChatDrivenScene_InjectionOrderingIdempotenceProjection is Orion#181
// (ADR-BLUE-012 R6 §§4.3,6.4; B8,B18): a chat-driven scene, taken on-air
// through the REAL postSceneIntent handler (attestation verify, workload
// fetch, digest cross-check, bluehost.Host.Take — the exact path production
// uses), then driven exactly the way a live Quasar transport would drive it
// (internal/providers.InjectActivePlatform — Orion#332's ingress adapter) and
// projected through a REAL bluewire.Bridge/blueproject.Project onto an
// observed mirror. It proves the EFFECT, not just that a call returned
// without error:
//   - injection: an accepted event actually reaches the running instance
//     and changes its state (asserted via the projected patch content)
//   - ordering: a sequence-gap event is rejected AND produces no projection;
//     two in-order events accumulate a correct running counter
//   - idempotence: a duplicate event_id (ingress layer) and a replayed
//     scene-intent request (same idempotency_key, HTTP layer) each produce
//     no re-execution and no new/corrupting projection — critically, a
//     scene-intent replay must NOT reset the ingress ordering state
//     (providers.ResetActiveIngress only fires on a FRESH Take)
//   - projection: every accepted, state-changing event is observable on the
//     mirror as a Delta carrying the expected overlay_message/chat_count
func TestChatDrivenScene_InjectionOrderingIdempotenceProjection(t *testing.T) {
	leaf, err := providers.CanonicalPlatformLeaf("twitch", "zablab_chat", "chat_message")
	if err != nil {
		t.Fatalf("CanonicalPlatformLeaf: %v", err)
	}
	program := chatDrivenProgram(t, leaf)

	pub, priv, _ := ed25519.GenerateKey(nil)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-181", attestation.ActionTakeOnAir, now, "scene-1", digest)

	wl := &fakeWorkload{body: envelope}
	host := bluehost.NewHost()
	t.Cleanup(func() { _ = host.Release(bluehost.SlotOnAir, "test-cleanup") })

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-181": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      wl,
		Host:          host,
		Idempotency:   NewIdempotencyCache(),
	}

	send := func(idempotencyKey string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(sceneIntentRequest{
			IntentID: "intent-181", IdempotencyKey: idempotencyKey, StreamID: "stream-1",
			Action: string(attestation.ActionTakeOnAir), ResolvedSceneRef: ref,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", "operator")
		req.Header.Set(authContextHeader, "opaque-ticket")
		rec := httptest.NewRecorder()
		postSceneIntent(deps)(rec, req)
		return rec
	}

	if rec := send("idem-181"); rec.Code != http.StatusOK {
		t.Fatalf("take-on-air: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Host.Take stores claims.SceneDigest (the attestation's scene_digest,
	// a distinct concept from blue_program_digest) — signedRef's fixed
	// placeholder, not the program's own digest computed above.
	wantSceneDigest := "sha256:" + strings.Repeat("a", 64)
	if got := host.Digest(bluehost.SlotOnAir); got != wantSceneDigest {
		t.Fatalf("expected on-air scene digest %q, got %q", wantSceneDigest, got)
	}

	// A bridge built directly (bluewire.NewBridge), stepped synchronously —
	// the same production type postSceneIntent's startBridge wires, just
	// driven deterministically instead of via its background goroutine, so
	// this test controls exactly when a tick happens relative to injection.
	mirror := &capturingMirror{}
	bridge := bluewire.NewBridge(host, bluehost.SlotOnAir, mirror, "scene-1", "sha256:"+strings.Repeat("a", 64), "ref-1", blueproject.TargetProgram, "rev-1", "corr-181")

	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (baseline): %v", err)
	}
	baseline := mirror.snapshot()
	if len(baseline) != 1 {
		t.Fatalf("expected exactly 1 baseline projection, got %d", len(baseline))
	}
	if got := string(patchValue(t, baseline[0], "overlay_message")); got != `""` {
		t.Fatalf("baseline overlay_message: got %s, want empty string", got)
	}
	if got := string(patchValue(t, baseline[0], "chat_count")); got != "0" {
		t.Fatalf("baseline chat_count: got %s, want 0", got)
	}

	// --- Injection + projection: first in-order chat event ---
	firstReceipt, err := providers.InjectActivePlatform(host, leaf, chatMessageEvent(t, "evt-1", 1, "gg"))
	if err != nil {
		t.Fatalf("InjectActivePlatform(evt-1): %v", err)
	}
	if firstReceipt.Status != "accepted" || firstReceipt.RuntimeSequence != 1 || firstReceipt.EventID != "evt-1" {
		t.Fatalf("unexpected first receipt: %+v", firstReceipt)
	}
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (evt-1): %v", err)
	}
	afterFirst := mirror.snapshot()
	if len(afterFirst) != 2 {
		t.Fatalf("expected 2 projections after first event, got %d", len(afterFirst))
	}
	if got := string(patchValue(t, afterFirst[1], "overlay_message")); got != `"chat: gg"` {
		t.Fatalf("overlay_message after evt-1: got %s, want %q", got, `"chat: gg"`)
	}
	if got := string(patchValue(t, afterFirst[1], "chat_count")); got != "1" {
		t.Fatalf("chat_count after evt-1: got %s, want 1", got)
	}

	// --- Ordering: a second in-order event must accumulate, not overwrite ---
	secondReceipt, err := providers.InjectActivePlatform(host, leaf, chatMessageEvent(t, "evt-2", 2, "hello"))
	if err != nil {
		t.Fatalf("InjectActivePlatform(evt-2): %v", err)
	}
	if secondReceipt.RuntimeSequence != 2 {
		t.Fatalf("expected second receipt sequence 2, got %+v", secondReceipt)
	}
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (evt-2): %v", err)
	}
	afterSecond := mirror.snapshot()
	if len(afterSecond) != 3 {
		t.Fatalf("expected 3 projections after second event, got %d", len(afterSecond))
	}
	if got := string(patchValue(t, afterSecond[2], "overlay_message")); got != `"chat: hello"` {
		t.Fatalf("overlay_message after evt-2: got %s, want %q", got, `"chat: hello"`)
	}
	if got := string(patchValue(t, afterSecond[2], "chat_count")); got != "2" {
		t.Fatalf("chat_count after evt-2: got %s (want 2 -- ordering broken if stale/overwritten)", got)
	}

	// --- Ordering: a sequence gap is rejected outright and never reaches
	// the runtime (no state change, therefore no new projection). ---
	if _, err := providers.InjectActivePlatform(host, leaf, chatMessageEvent(t, "evt-4", 4, "skip-ahead")); err == nil {
		t.Fatal("expected a sequence-gap rejection for source_sequence 4 after 2")
	} else {
		var runtimeErr *blueruntime.Error
		if !errors.As(err, &runtimeErr) || runtimeErr.Code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("expected EVENT_SEQUENCE_GAP, got %v", err)
		}
	}
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (after rejected gap): %v", err)
	}
	if got := len(mirror.snapshot()); got != 3 {
		t.Fatalf("expected the rejected gap event to produce no new projection (still 3), got %d", got)
	}

	// --- Idempotence (ingress layer): replaying evt-1 returns the cached
	// receipt verbatim and does not re-run the program (counter stays put,
	// no new projection). ---
	replayReceipt, err := providers.InjectActivePlatform(host, leaf, chatMessageEvent(t, "evt-1", 1, "gg"))
	if err != nil {
		t.Fatalf("InjectActivePlatform(evt-1 replay): %v", err)
	}
	if replayReceipt != firstReceipt {
		t.Fatalf("expected the replay to return the identical original receipt, got %+v want %+v", replayReceipt, firstReceipt)
	}
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (after ingress replay): %v", err)
	}
	if got := len(mirror.snapshot()); got != 3 {
		t.Fatalf("expected the idempotent ingress replay to produce no new projection (still 3, counter unchanged), got %d", got)
	}

	// --- Idempotence (scene-intent/HTTP layer): replaying the SAME
	// take-on-air intent must skip re-execution entirely (cached response,
	// no second Workload.Admit, no second Host.Take) -- and MUST NOT call
	// providers.ResetActiveIngress (that only fires on a fresh successful
	// Take), or the chat_count/dedup state accumulated above would be wiped.
	if rec := send("idem-181"); rec.Code != http.StatusOK {
		t.Fatalf("replayed take-on-air: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if wl.admitCalls != 1 {
		t.Fatalf("expected the scene-intent replay to skip re-execution (still 1 workload admission), got %d", wl.admitCalls)
	}
	thirdReceipt, err := providers.InjectActivePlatform(host, leaf, chatMessageEvent(t, "evt-3", 3, "still going"))
	if err != nil {
		t.Fatalf("InjectActivePlatform(evt-3, after scene-intent replay): %v", err)
	}
	if thirdReceipt.RuntimeSequence != 3 {
		t.Fatalf("expected sequence 3 to continue from 2 across the scene-intent replay (ingress ordering state was wrongly reset), got %+v", thirdReceipt)
	}
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce (evt-3): %v", err)
	}
	final := mirror.snapshot()
	if got := string(patchValue(t, final[len(final)-1], "chat_count")); got != "3" {
		t.Fatalf("chat_count after evt-3: got %s, want 3 (counter must survive an idempotent scene-intent replay)", got)
	}
	if got := string(patchValue(t, final[len(final)-1], "overlay_message")); got != `"chat: still going"` {
		t.Fatalf("overlay_message after evt-3: got %s, want %q", got, `"chat: still going"`)
	}
}
