package bluehost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/canonical"
)

const showEmitBag = "__show.emit"

// ShowEmitSink receives one authored core.show.emit@1 emission after the
// portable runtime has executed it. The embedding decides which active slot
// instances receive the resulting topic event.
type ShowEmitSink func(topic string, payload any)

// SetShowEmitSink wires the host-side projector for core.show.emit@1.
func (h *Host) SetShowEmitSink(sink ShowEmitSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.showEmitSink = sink
}

// EmitEvent injects a topic event into one Engine-B slot and advances it once
// immediately. This is the stateless-slot equivalent of the legacy
// adapters.Inbox.EmitToActive path: the event is admitted with the canonical
// blue.runtime.event.v1 envelope, then the slot's own runtime consumes it.
func (h *Host) EmitEvent(slot Slot, topic string, payload any) error {
	if strings.TrimSpace(topic) == "" {
		return fmt.Errorf("bluehost: show.emit topic is empty")
	}

	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok || e.instance == nil {
		h.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	sequence := e.showEmitSequence + 1
	raw, err := buildShowEmitEvent(e.showEmitOrigin, topic, sequence, payload)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	e.showEmitSequence = sequence
	instance := e.instance
	h.mu.Unlock()

	h.runtimeMu.Lock()
	if _, err := h.runtime.Dispatch(instance, raw); err != nil {
		h.runtimeMu.Unlock()
		return err
	}
	result, err := h.runtime.Step(instance)
	h.runtimeMu.Unlock()
	if err != nil {
		return err
	}
	result = normalizeRuntimeOutputs(result)
	h.dispatchInvocations(slot, instance, result.Invocations)
	h.dispatchShowEmit(slot, instance, result.Variables)
	h.dispatchOverlayAppSet(slot, instance, result.Variables)
	return nil
}

func showEmitOrigin(instanceID string, slot Slot) string {
	digest := sha256.Sum256([]byte(instanceID))
	return "orion_show_" + strings.ReplaceAll(string(slot), "-", "_") + "_" + hex.EncodeToString(digest[:8])
}

func buildShowEmitEvent(origin, topic string, sequence uint64, payload any) ([]byte, error) {
	payloadDigest, err := canonical.Digest(payload)
	if err != nil {
		return nil, fmt.Errorf("bluehost: show.emit payload cannot be canonicalized: %w", err)
	}
	eventID := fmt.Sprintf("%s_%d", origin, sequence)
	event := map[string]any{
		"schema_version":  "blue.runtime.event.v1",
		"event_id":        eventID,
		"origin":          origin,
		"source_sequence": json.Number(strconv.FormatUint(sequence, 10)),
		"topic":           topic,
		"occurred_at_ms":  json.Number(strconv.FormatInt(time.Now().UnixMilli(), 10)),
		"payload":         payload,
		"payload_digest":  payloadDigest,
		"correlation_id":  eventID,
	}
	eventDigest, err := canonical.Digest(event)
	if err != nil {
		return nil, fmt.Errorf("bluehost: show.emit event cannot be canonicalized: %w", err)
	}
	event["event_digest"] = eventDigest
	return json.Marshal(event)
}

type showEmitCall struct {
	topic   string
	payload any
}

// dispatchShowEmit projects the cumulative local-effect bag exactly once per
// (node, causation event). Causation identity is emitted by Blue runtime; the
// fallback digest keeps older runtimes fail-closed and non-repeating. This is
// intentionally separate from overlay-app dispatch because show.emit is a
// topic event, not a wire-level overlay mutation.
func (h *Host) dispatchShowEmit(slot Slot, instance *blueruntime.InstanceHandle, variables map[string]any) {
	if modeFor(slot) != blueruntime.Execute {
		return
	}
	h.mu.Lock()
	sink := h.showEmitSink
	e, live := h.slots[slot]
	if sink == nil || !live || e.instance != instance {
		h.mu.Unlock()
		return
	}
	bag, _ := variables[showEmitBag].(map[string]any)
	if len(bag) == 0 {
		h.mu.Unlock()
		return
	}
	if e.showEmitSeen == nil {
		e.showEmitSeen = map[string]struct{}{}
	}
	nodeIDs := make([]string, 0, len(bag))
	for nodeID := range bag {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	calls := make([]showEmitCall, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		record, _ := bag[nodeID].(map[string]any)
		topic, payload, identity, ok := showEmitRecordFields(record)
		if !ok {
			continue
		}
		key := nodeID + "\x00" + identity
		if _, seen := e.showEmitSeen[key]; seen {
			continue
		}
		e.showEmitSeen[key] = struct{}{}
		calls = append(calls, showEmitCall{topic: topic, payload: payload})
	}
	h.mu.Unlock()
	for _, call := range calls {
		sink(call.topic, call.payload)
	}
}

func showEmitRecordFields(record map[string]any) (topic string, payload any, identity string, ok bool) {
	if record == nil {
		return "", nil, "", false
	}
	config, _ := record["config"].(map[string]any)
	topic, _ = config["topic"].(string)
	if topic == "" {
		return "", nil, "", false
	}
	inputs, _ := record["inputs"].(map[string]any)
	payload = inputs["payload"]
	identity, _ = record["causation_event_id"].(string)
	if identity == "" {
		digest, err := canonical.Digest(record)
		if err != nil {
			return "", nil, "", false
		}
		identity = "record:" + digest
	}
	return topic, payload, identity, true
}
