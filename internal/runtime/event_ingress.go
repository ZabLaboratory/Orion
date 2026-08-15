package runtime

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// EventIngressReceipt is the Engine-A admission observation for the
// blue.runtime.event.v1 contract. It intentionally mirrors the fields that
// Blue's active ingress exposes without making the runtime's internal State
// sequence a wire contract.
type EventIngressReceipt struct {
	Status          string
	RuntimeSequence uint64
	EventID         string
}

// EventIngressError is a stable, transport-neutral admission error. The
// adapters and parity harness use Code to compare it with Blue's typed error
// without matching implementation text.
type EventIngressError struct {
	Code    string
	Message string
}

func (e *EventIngressError) Error() string {
	return e.Code + ": " + e.Message
}

type admittedEvent struct {
	origin         string
	sourceSequence uint64
	digest         string
	receipt        EventIngressReceipt
}

// CanonicalEventIngress is the Engine-A adapter for the same canonical event
// envelope admitted by Blue Runtime. It is stateless across active Scene
// generations: changing the active pointer starts a fresh ordering domain.
// It delivers only to Show.Active and never calls RouteTargets, preserving
// active-only admission for external event envelopes.
type CanonicalEventIngress struct {
	show *Show

	mu         sync.Mutex
	generation *Scene
	next       uint64
	seen       map[string]admittedEvent
	origins    map[string]uint64
}

func NewCanonicalEventIngress(show *Show) *CanonicalEventIngress {
	return &CanonicalEventIngress{
		show:    show,
		seen:    map[string]admittedEvent{},
		origins: map[string]uint64{},
	}
}

// Inject validates and admits one blue.runtime.event.v1 envelope. A
// duplicate with the same event identity/digest returns the original receipt;
// a conflicting replay, source gap, or stale sequence is rejected before the
// active Scene sees it.
func (i *CanonicalEventIngress) Inject(data []byte) (EventIngressReceipt, error) {
	event, err := blueruntime.ParseEvent(data)
	if err != nil {
		return EventIngressReceipt{}, &EventIngressError{Code: eventErrorCode(err), Message: err.Error()}
	}
	topic, _ := event["topic"].(string)
	return i.inject(eventsPrefix+topic, event)
}

// InjectPlatform admits the same envelope at an already-resolved canonical
// platform leaf. It is the Engine-A counterpart of providers.ActiveIngress:
// the resolver owns the leaf, while this adapter owns only validation,
// per-origin ordering, deduplication, and active-only delivery.
func (i *CanonicalEventIngress) InjectPlatform(leaf string, data []byte) (EventIngressReceipt, error) {
	event, err := blueruntime.ParseEvent(data)
	if err != nil {
		return EventIngressReceipt{}, &EventIngressError{Code: eventErrorCode(err), Message: err.Error()}
	}
	if !strings.HasPrefix(leaf, platformLeafPrefix) {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_MALFORMED", Message: "leaf is not canonical"}
	}
	return i.inject(leaf, event)
}

func (i *CanonicalEventIngress) inject(path string, event map[string]any) (EventIngressReceipt, error) {
	if i == nil || i.show == nil {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_ACTIVE_UNAVAILABLE", Message: "show is not configured"}
	}
	active := i.show.Active()
	if active == nil {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_ACTIVE_UNAVAILABLE", Message: "no active scene"}
	}
	eventID, _ := event["event_id"].(string)
	origin, _ := event["origin"].(string)
	digest, _ := event["event_digest"].(string)
	sequenceValue, _ := event["source_sequence"].(json.Number)
	sequence, parseErr := strconv.ParseUint(sequenceValue.String(), 10, 64)
	if parseErr != nil {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_MALFORMED", Message: "source sequence is invalid"}
	}
	payload, err := json.Marshal(event["payload"])
	if err != nil {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_MALFORMED", Message: "payload cannot be encoded"}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.generation != active {
		i.generation = active
		i.next = 0
		i.seen = map[string]admittedEvent{}
		i.origins = map[string]uint64{}
	}
	if previous, ok := i.seen[eventID]; ok {
		if previous.origin == origin && previous.sourceSequence == sequence && previous.digest == digest {
			return previous.receipt, nil
		}
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_DUPLICATE_CONFLICT", Message: "event_id was admitted with another digest"}
	}
	if previous, ok := i.origins[origin]; ok {
		if sequence <= previous {
			return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_SEQUENCE_CONFLICT", Message: "source sequence was already consumed"}
		}
		if sequence != previous+1 {
			return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_SEQUENCE_GAP", Message: "source sequence contains a gap"}
		}
	} else if sequence != 1 {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_SEQUENCE_GAP", Message: "source sequence must begin at one"}
	}

	if !active.Input(InputMsg{Path: path, Value: payload, Source: "event:canonical", ClientMsgID: eventID}) {
		return EventIngressReceipt{}, &EventIngressError{Code: "EVENT_INGRESS_DROPPED", Message: "active scene refused the event"}
	}
	i.next++
	receipt := EventIngressReceipt{Status: "accepted", RuntimeSequence: i.next, EventID: eventID}
	i.seen[eventID] = admittedEvent{origin: origin, sourceSequence: sequence, digest: digest, receipt: receipt}
	i.origins[origin] = sequence
	return receipt, nil
}

func eventErrorCode(err error) string {
	var blueErr *blueruntime.Error
	if !errors.As(err, &blueErr) || blueErr.Code == "" {
		return "EVENT_MALFORMED"
	}
	return blueErr.Code
}
