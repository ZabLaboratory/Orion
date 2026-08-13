package providers

import (
	"encoding/json"
	"strconv"

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

// InjectActive delivers a Zab-canonical event exclusively to the on-air
// bluehost slot — the active-only routing invariant (§4.3 puce 4): a
// dormant/backstage scene never observes Zab platform events, only
// whatever slot is currently live. bluehost.ErrNotLoaded surfaces
// unchanged when nothing is on-air (nothing to route to, not a bug — the
// pre-take/post-release posture is silence, same as active-scene-only
// execution elsewhere in Orion). No roster, no store: the routing target
// is always "whatever bluehost.Host currently holds on-air", never a
// persisted rule set.
func InjectActive(host *bluehost.Host, data []byte) (blueruntime.Receipt, error) {
	return host.Dispatch(bluehost.SlotOnAir, data)
}
