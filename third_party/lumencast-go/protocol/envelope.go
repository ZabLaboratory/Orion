// Package protocol implements LSDP/1 — the Leaf State Delta Protocol,
// version 1. It is the wire layer that Lumencast servers and runtimes
// speak over WebSocket.
//
// The package is transport-agnostic : it does not import net/http or
// anything WebSocket-related. Every frame round-trips through these
// types ; nothing else in the SDK is allowed to assemble or parse a
// wire frame by hand.
//
// Spec : https://github.com/Lumencast/lumencast-protocol/blob/main/spec/LSDP-1.md
//
// Design rules :
//
//   - Every envelope carries `v` and `type`. v == 1 in this package ;
//     bumping to 2 is a separate, parallel implementation.
//   - Server frames carry `seq` (per-subscription monotonic counter
//     starting at 1).
//   - Leaf values are passed as json.RawMessage so we do not lose
//     fidelity on round-trip — null vs missing, integer vs float, etc.
//   - Field tags match the on-wire spec exactly. Do not rename.
//   - Receivers MUST ignore unknown top-level fields (forward compat).
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the protocol major. Always 1 for LSDP/1. The constant is
// the source of truth for both encode and decode.
const Version = 1

// SubProtocol is the LSDP/1.0 WebSocket subprotocol tag, kept for
// backwards-compatible negotiation. Clients that speak only 1.0
// advertise this. Servers MUST reject upgrade requests that do not
// advertise either SubProtocol or SubProtocolV1_1 with HTTP 426.
const SubProtocol = "lsdp.v1"

// SubProtocolV1_1 is the LSDP/1.1 WebSocket subprotocol tag. Clients
// advertising this opt into the additive 1.1 frame surface
// (since_sequence resume, unsubscribe frame, per-leaf transition
// directive, cause, nonce on ping/pong, client_msg_id on input,
// from_scene_id + show transition on scene_changed). Servers MUST
// continue to accept SubProtocol for 1.0 clients.
const SubProtocolV1_1 = "lsdp.v1.1"

// SubProtocols is the canonical advertise/accept list, ordered by
// preference (1.1 preferred over 1.0). Pass this to the websocket
// upgrader's Subprotocols field — the library performs RFC-6455
// preference negotiation.
var SubProtocols = []string{SubProtocolV1_1, SubProtocol}

// Frame type discriminators. Constants instead of strings everywhere
// so a typo in a handler is a compile error, not a silent runtime
// mismatch.
const (
	TypeSnapshot     = "snapshot"
	TypeDelta        = "delta"
	TypeSceneChanged = "scene_changed"
	TypeSceneRoster  = "scene_roster" // LSDP/1.1 — additive show-level preload roster
	TypeOverlayApps  = "overlay_apps" // LSDP/1.1 — additive show-level overlay-app control
	TypeError        = "error"
	TypePong         = "pong"
	TypeSubscribe    = "subscribe"
	TypeInput        = "input"
	TypePing         = "ping"
	TypeUnsubscribe  = "unsubscribe" // LSDP/1.1 §4.4
)

// envelope is the minimal shape used to peek at `type` and `v` on
// incoming frames before dispatching to the right typed struct.
type envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// ErrVersionMismatch is returned by Decode when the envelope `v` does
// not match the protocol major. The caller (server) is expected to
// answer with an Error frame and close.
var ErrVersionMismatch = errors.New("protocol: envelope v mismatch")

// ErrUnknownType is returned by Decode when the envelope `type` is not
// one of the LSDP/1 client-side frames. Forward compatibility for
// server-emitted types is the runtime's responsibility (silently ignore).
var ErrUnknownType = errors.New("protocol: unknown frame type")

// ErrInvalidEnvelope wraps a JSON syntax error or a missing required
// field on the envelope.
var ErrInvalidEnvelope = errors.New("protocol: invalid envelope")

// marshal encodes a value with HTML escaping disabled so leaf paths
// containing `<`, `>`, `&` are preserved on the wire (instead of
// being rewritten as \u00xx, which would break canonical-form
// comparisons in the conformance suite). It also strips the trailing
// newline json.Encoder writes — a frame is exactly one JSON object.
func marshal(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// unmarshal is the symmetric decode helper. Wraps the JSON error with
// the package-level ErrInvalidEnvelope so callers can match on it.
func unmarshal(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
	}
	return nil
}
