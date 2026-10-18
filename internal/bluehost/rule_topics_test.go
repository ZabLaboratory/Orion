package bluehost

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/canonical"
)

func topicRuleProgram(t *testing.T, payloadType string, emitter bool) []byte {
	t.Helper()
	kind, opcode, extra := "topic", "core.event.on-event@1", map[string]any{"topic": "show_changed"}
	if emitter {
		kind, opcode, extra = "call", "core.operator.on-call@1", nil
	}
	raw := buildEntrypointProgram(t, kind, opcode, "payload", extra)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	document["topics"] = []any{map[string]any{"name": "show_changed", "payload_type": payloadType}}
	if emitter {
		port := func(name, kind, typ string) map[string]any {
			return map[string]any{"name": name, "kind": kind, "type": typ, "required": true}
		}
		document["opcodes"] = append(document["opcodes"].([]any), map[string]any{
			"id": "core.show.emit@1", "kind": "control",
			"config":  []any{port("topic", "data", "core.string")},
			"inputs":  []any{port("payload", "data", "core.json"), port("in", "exec", "core.exec")},
			"outputs": []any{port("then", "exec", "core.exec")},
		})
		for _, raw := range document["nodes"].([]any) {
			node := raw.(map[string]any)
			if node["id"] == "mark" {
				node["opcode"], node["config"] = "core.show.emit@1", map[string]any{"topic": "show_changed"}
			}
		}
		document["data_edges"].([]any)[0].(map[string]any)["to_port"] = "payload"
		sortByID(document["opcodes"].([]any))
	}
	delete(document, "program_digest")
	digest, err := canonical.Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	document["program_digest"] = digest
	out, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func promoteTopicRule(t *testing.T, plane *RulePlane, id string, raw []byte) *Host {
	t.Helper()
	var identity struct {
		Digest string `json:"program_digest"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		t.Fatal(err)
	}
	if err := plane.Promote(id, identity.Digest, raw); err != nil {
		t.Fatal(err)
	}
	return plane.rules[id].host
}

func topicResult(t *testing.T, host *Host) any {
	t.Helper()
	result, err := host.Tick(SlotOnAir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result.Outputs["result"]
}

func TestRulePlane_TopicFanoutIsolatesRejectionsAndDemotion(t *testing.T) {
	logs := &topicLogWriter{}
	plane := NewRulePlane(nil, nil, EffectDeps{}, 1, slog.New(slog.NewJSONHandler(logs, nil)))
	defer plane.Stop()
	// Capture errors separately: a wrong payload is reported, an unrelated
	// undeclared topic is ignored without weakening runtime admission.
	bad := promoteTopicRule(t, plane, "a-string", topicRuleProgram(t, "core.string", false))
	first := promoteTopicRule(t, plane, "b-json", topicRuleProgram(t, "core.json", false))
	second := promoteTopicRule(t, plane, "c-json", topicRuleProgram(t, "core.json", false))
	plane.EmitEvent("show_changed", map[string]any{"league": "LEC"})
	for _, host := range []*Host{first, second} {
		if value, ok := topicResult(t, host).(map[string]any); !ok || value["league"] != "LEC" {
			t.Fatalf("listener did not execute: %#v", topicResult(t, host))
		}
	}
	if topicResult(t, bad) != nil || !strings.Contains(logs.String(), "EVENT_PAYLOAD_TYPE_MISMATCH") {
		t.Fatalf("invalid listener not isolated/reported: %s", logs.String())
	}
	logs.Reset()
	plane.EmitEvent("unrelated", nil)
	if logs.String() != "" {
		t.Fatalf("irrelevant topic logged as fault: %s", logs.String())
	}
	if err := plane.Demote("c-json"); err != nil {
		t.Fatal(err)
	}
	plane.EmitEvent("show_changed", "LCK")
	if topicResult(t, bad) != "LCK" || topicResult(t, first) != "LCK" {
		t.Fatal("rejected event consumed admission sequence")
	}
	if second.Digest(SlotOnAir) != "" {
		t.Fatal("demoted listener still active")
	}
}

func TestRulePlane_CallEmitReentersAndCanDemoteWithoutPlaneLock(t *testing.T) {
	plane := NewRulePlane(nil, nil, EffectDeps{}, 1, nil)
	defer plane.Stop()
	listener := promoteTopicRule(t, plane, "listener", topicRuleProgram(t, "core.json", false))
	promoteTopicRule(t, plane, "emitter", topicRuleProgram(t, "core.json", true))
	var demoteErr error
	router := NewShowEventRouter(func(topic string, payload any) {
		plane.EmitEvent(topic, payload)
		demoteErr = plane.Demote("emitter")
	}, func() { t.Error("unexpected cascade overflow") })
	plane.SetShowEmitSink(router)
	done := make(chan error, 1)
	go func() { _, err := plane.Call("emitter", "arm", "LEC"); done <- err }()
	select {
	case err := <-done:
		if err != nil || demoteErr != nil {
			t.Fatalf("call=%v demote=%v", err, demoteErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reentrant rule emit deadlocked")
	}
	if topicResult(t, listener) != "LEC" || plane.Digest("emitter") != "" {
		t.Fatal("global topic did not reach listener/demote emitter")
	}
}

// A synchronized writer keeps diagnostics safe if the ticker ever overlaps a
// test event; tests do not replace the plane's running logger after startup.
type topicLogWriter struct {
	mu     sync.Mutex
	output bytes.Buffer
}

func (w *topicLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.Write(data)
}
func (w *topicLogWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.output.String() }
func (w *topicLogWriter) Reset()         { w.mu.Lock(); defer w.mu.Unlock(); w.output.Reset() }
