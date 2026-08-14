package providers

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// EventSchema is the blue.runtime.event.v1 envelope schema BuildEvent emits
// and blueruntime.ParseEvent/Dispatch verifies on receipt.
const EventSchema = "blue.runtime.event.v1"

// BuildEvent constructs a blue.runtime.event.v1 envelope from a Zab-side
// canonical event (Quasar's normalized platform event, or any other Zab
// origin) — the successor of the legacy stream-rule routing (§4.3 puce 4),
// scoped here to envelope construction: payload_digest and event_digest are
// computed via the same shared LSML canonicalization blueruntime verifies,
// so a correctly-built envelope is never rejected as EVENT_DIGEST_MISMATCH.
//
// payload must use json.Number for any numeric field (mirrors
// blueruntime's own decodeStrict convention) — decode upstream JSON with
// a json.Decoder in UseNumber mode before calling this.
func BuildEvent(eventID, origin, topic, correlationID string, sourceSequence uint64, occurredAtMs int64, payload map[string]any) ([]byte, error) {
	payloadDigest, err := Digest(payload)
	if err != nil {
		return nil, err
	}
	event := map[string]any{
		"schema_version":  EventSchema,
		"event_id":        eventID,
		"origin":          origin,
		"source_sequence": json.Number(strconv.FormatUint(sourceSequence, 10)),
		"topic":           topic,
		"occurred_at_ms":  json.Number(strconv.FormatInt(occurredAtMs, 10)),
		"payload":         payload,
		"payload_digest":  payloadDigest,
		"correlation_id":  correlationID,
	}
	eventDigest, err := Digest(event)
	if err != nil {
		return nil, err
	}
	event["event_digest"] = eventDigest
	return json.Marshal(event)
}

// activeIngressState is process-local admission state for the platform leaf
// path. It is deliberately not durable: a new on-air Host generation starts
// with a fresh state, and the API resets it on every successful Take. The
// state exists because Host.WritePlatformEvent is the canonical leaf-write
// seam and, unlike Host.Dispatch, does not own the event-envelope ordering
// contract.
type activeIngressState struct {
	origin         string
	sourceSequence uint64
	eventDigest    string
	leaf           string
	receipt        blueruntime.Receipt
}

// ActiveIngress is the explicit, stateless-host adapter for platform events.
// It admits the blue.runtime.event.v1 envelope, validates event_id and
// source_sequence per origin, then writes only the canonical platform leaf to
// Host.WritePlatformEvent on SlotOnAir. It never falls back to Host.Dispatch.
type ActiveIngress struct {
	host       *bluehost.Host
	mu         sync.Mutex
	generation string
	nextSeq    uint64
	seen       map[string]activeIngressState
	origins    map[string]uint64
}

// Ingress is a short compatibility alias for callers that do not need the
// more explicit ActiveIngress name.
type Ingress = ActiveIngress

// NewActiveIngress creates an ingress bound to one host. The host remains the
// owner of preview/on-air instances; this adapter stores no scene roster or
// durable event state.
func NewActiveIngress(host *bluehost.Host) *ActiveIngress {
	return &ActiveIngress{
		host:    host,
		seen:    map[string]activeIngressState{},
		origins: map[string]uint64{},
	}
}

// NewIngress is an alias for NewActiveIngress.
func NewIngress(host *bluehost.Host) *ActiveIngress {
	return NewActiveIngress(host)
}

var activeIngresses sync.Map // map[*bluehost.Host]*ActiveIngress

func ingressFor(host *bluehost.Host) *ActiveIngress {
	if current, ok := activeIngresses.Load(host); ok {
		return current.(*ActiveIngress)
	}
	created := NewActiveIngress(host)
	actual, _ := activeIngresses.LoadOrStore(host, created)
	return actual.(*ActiveIngress)
}

// ResetActiveIngress forgets process-local ordering for a Host. Callers use
// it after an on-air generation is committed or released; the next event is
// then admitted from source_sequence one even if the new instance reuses the
// same scene digest.
func ResetActiveIngress(host *bluehost.Host) {
	if host != nil {
		activeIngresses.Delete(host)
	}
}

// InjectActive delivers a canonical event exclusively to the on-air slot.
// The platform leaf is derived from the event's origin/topic convention used
// by BuildEvent: the topic carries `<vendor>.<platform>.<event>` and the last
// origin segment carries the canonical channel handle.
func InjectActive(host *bluehost.Host, data []byte) (blueruntime.Receipt, error) {
	return ingressFor(host).Inject(data)
}

// InjectActivePlatform is the explicit-leaf form used when the upstream
// adapter already owns the cross-service leaf contract. It is useful for
// Quasar-shaped canonical events whose platform/channel/type are not encoded
// in a blue.runtime.event.v1 origin/topic pair.
func InjectActivePlatform(host *bluehost.Host, leaf string, data []byte) (blueruntime.Receipt, error) {
	return ingressFor(host).InjectPlatform(leaf, data)
}

// Inject admits and writes a platform event using the derived canonical leaf.
func (i *ActiveIngress) Inject(data []byte) (blueruntime.Receipt, error) {
	event, err := parseEvent(data)
	if err != nil {
		return blueruntime.Receipt{}, err
	}
	leaf, err := platformLeafFromEvent(event)
	if err != nil {
		return blueruntime.Receipt{}, err
	}
	return i.inject(leaf, event)
}

// InjectPlatform admits and writes a platform event at an already-resolved
// canonical leaf.
func (i *ActiveIngress) InjectPlatform(leaf string, data []byte) (blueruntime.Receipt, error) {
	event, err := parseEvent(data)
	if err != nil {
		return blueruntime.Receipt{}, err
	}
	if !isCanonicalPlatformLeaf(leaf) {
		return blueruntime.Receipt{}, ingressError("EVENT_MALFORMED", "leaf is not canonical")
	}
	return i.inject(leaf, event)
}

func (i *ActiveIngress) inject(leaf string, event map[string]any) (blueruntime.Receipt, error) {
	if i == nil || i.host == nil {
		return blueruntime.Receipt{}, bluehost.ErrNotLoaded
	}
	activeDigest := i.host.Digest(bluehost.SlotOnAir)
	if activeDigest == "" {
		return blueruntime.Receipt{}, bluehost.ErrNotLoaded
	}

	eventID, _ := event["event_id"].(string)
	origin, _ := event["origin"].(string)
	eventDigest, _ := event["event_digest"].(string)
	sequence, err := sourceSequenceOf(event)
	if err != nil {
		return blueruntime.Receipt{}, err
	}
	payload := event["payload"]

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.generation != activeDigest {
		i.generation = activeDigest
		i.nextSeq = 0
		i.seen = map[string]activeIngressState{}
		i.origins = map[string]uint64{}
	}

	if previous, ok := i.seen[eventID]; ok {
		if previous.origin == origin && previous.sourceSequence == sequence && previous.eventDigest == eventDigest && previous.leaf == leaf {
			return previous.receipt, nil
		}
		return blueruntime.Receipt{}, ingressError("EVENT_DUPLICATE_CONFLICT", "event_id was admitted with another digest")
	}
	if previous, ok := i.origins[origin]; ok {
		if sequence <= previous {
			return blueruntime.Receipt{}, ingressError("EVENT_SEQUENCE_CONFLICT", "source sequence was already consumed")
		}
		if sequence != previous+1 {
			return blueruntime.Receipt{}, ingressError("EVENT_SEQUENCE_GAP", "source sequence contains a gap")
		}
	} else if sequence != 1 {
		return blueruntime.Receipt{}, ingressError("EVENT_SEQUENCE_GAP", "source sequence must begin at one")
	}

	// This is intentionally the only host ingress call. A platform event is
	// a state-leaf write, not a generic `__events.<topic>` dispatch.
	if _, err := i.host.WritePlatformEvent(bluehost.SlotOnAir, leaf, payload); err != nil {
		return blueruntime.Receipt{}, err
	}
	i.nextSeq++
	receipt := blueruntime.Receipt{Status: "accepted", RuntimeSequence: i.nextSeq, EventID: eventID}
	i.seen[eventID] = activeIngressState{
		origin:         origin,
		sourceSequence: sequence,
		eventDigest:    eventDigest,
		leaf:           leaf,
		receipt:        receipt,
	}
	i.origins[origin] = sequence
	return receipt, nil
}

func parseEvent(data []byte) (map[string]any, error) {
	if _, err := blueruntime.ParseEvent(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		return nil, ingressError("EVENT_MALFORMED", "event envelope cannot be decoded")
	}
	return event, nil
}

func sourceSequenceOf(event map[string]any) (uint64, error) {
	number, ok := event["source_sequence"].(json.Number)
	if !ok {
		return 0, ingressError("EVENT_MALFORMED", "source sequence is invalid")
	}
	sequence, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil {
		return 0, ingressError("EVENT_MALFORMED", "source sequence is invalid")
	}
	return sequence, nil
}

// CanonicalPlatformLeaf composes the byte-level leaf contract shared by the
// compiler, Quasar and blue-runtime-go.
func CanonicalPlatformLeaf(platform, channel, eventType string) (string, error) {
	platform = strings.ToLower(platform)
	channel = strings.ToLower(channel)
	eventType = strings.ToLower(eventType)
	if at := strings.IndexByte(eventType, '@'); at >= 0 {
		eventType = eventType[:at]
	}
	if !platformSegmentRE.MatchString(platform) || !platformChannelRE.MatchString(channel) || !platformSegmentRE.MatchString(eventType) {
		return "", ingressError("EVENT_MALFORMED", "platform event cannot produce a canonical leaf")
	}
	return "__inputs.platform." + platform + "." + channel + ".last_" + eventType, nil
}

var (
	platformSegmentRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	platformChannelRE = regexp.MustCompile(`^[a-z0-9_]+$`)
)

func platformLeafFromEvent(event map[string]any) (string, error) {
	origin, ok := event["origin"].(string)
	if !ok {
		return "", ingressError("EVENT_MALFORMED", "event origin is invalid")
	}
	topic, ok := event["topic"].(string)
	if !ok {
		return "", ingressError("EVENT_MALFORMED", "event topic is invalid")
	}
	originParts := strings.Split(origin, ".")
	topicParts := strings.Split(topic, ".")
	if len(originParts) < 2 || len(topicParts) < 2 {
		return "", ingressError("EVENT_MALFORMED", "origin/topic do not identify a platform channel")
	}
	platform := topicParts[len(topicParts)-2]
	eventType := topicParts[len(topicParts)-1]
	channel := originParts[len(originParts)-1]
	if len(originParts) >= 2 && originParts[len(originParts)-2] != platform {
		return "", ingressError("EVENT_MALFORMED", "origin/topic platform mismatch")
	}
	return CanonicalPlatformLeaf(platform, channel, eventType)
}

func isCanonicalPlatformLeaf(leaf string) bool {
	parts := strings.Split(leaf, ".")
	if len(parts) != 5 || parts[0] != "__inputs" || parts[1] != "platform" {
		return false
	}
	return platformSegmentRE.MatchString(parts[2]) && platformChannelRE.MatchString(parts[3]) && strings.HasPrefix(parts[4], "last_") && platformSegmentRE.MatchString(strings.TrimPrefix(parts[4], "last_"))
}

func ingressError(code, message string) error {
	return &blueruntime.Error{
		SchemaVersion: blueruntime.ErrorSchema,
		Code:          code,
		Stage:         "dispatch",
		Message:       message,
	}
}
