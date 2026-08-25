// Package protocol implements the WebSocket envelope and message
// shapes from ADR 002. Every message round-trips through these types;
// nothing else in the service is allowed to assemble or parse a wire
// frame by hand.
//
// Design rules:
//
//   - Every message carries `type` and `v` (protocol version). v=1
//     in this revision; bumped only on a breaking change.
//   - Leaf values are passed as json.RawMessage so we do not lose
//     fidelity on round-trips (a JSON null vs missing field, integer
//     vs float coercion, etc.). The runtime treats these as opaque
//     bytes and the WS server forwards them as-is.
//   - Field tags use camelCase or snake_case to match the on-wire
//     spec exactly. ADR 002 fixes the field names; we match them.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the `v` field on every envelope. Bumping this
// is a breaking change.
const ProtocolVersion = 1

// Type tags. Constants instead of strings everywhere so a typo in a
// handler is a compile error, not a silent runtime mismatch.
const (
	TypeSnapshot     = "snapshot"
	TypeDelta        = "delta"
	TypeSceneChanged = "scene_changed"
	TypeError        = "error"
	TypePong         = "pong"
	TypeSubscribed   = "subscribed"
	TypeSubscribe    = "subscribe"
	TypeInput        = "input"
	TypeUnsubscribe  = "unsubscribe"
	TypePing         = "ping"
)

// Error codes from ADR 002 § 5. Additive: new codes append; removing
// or repurposing is a v bump.
const (
	CodeAuthDenied         = "AUTH_DENIED"
	CodeSceneNotFound      = "SCENE_NOT_FOUND"
	CodeSceneNotPushed     = "SCENE_NOT_PUSHED"
	CodeSceneInUse         = "SCENE_IN_USE"
	CodeVersionMismatch    = "VERSION_MISMATCH"
	CodeVersionGap         = "VERSION_GAP"
	CodeRateLimit          = "RATE_LIMIT"
	CodeWriteForbidden     = "WRITE_FORBIDDEN"
	CodeUnknownPath        = "UNKNOWN_PATH"
	CodeInvalidValue       = "INVALID_VALUE"
	CodeTestSessionExpired = "TEST_SESSION_EXPIRED"
	CodeInternal           = "INTERNAL"
	CodeCyclicComponent    = "CYCLIC_COMPONENT"
)

// envelope is the minimal shape used to peek at `type` on incoming
// frames before dispatching to the right typed struct.
type envelope struct {
	Type string `json:"type"`
	V    int    `json:"v"`
}

// Snapshot is the server's reply to subscribe (and the resync
// payload on scene_changed / backpressure recovery). The state map
// keys are dotted leaf paths; values are opaque JSON.
type Snapshot struct {
	Type         string                     `json:"type"`
	V            int                        `json:"v"`
	SceneID      string                     `json:"scene_id"`
	SceneVersion string                     `json:"scene_version"`
	Sequence     uint64                     `json:"sequence"`
	State        map[string]json.RawMessage `json:"state"`
}

// Delta carries one or more leaf patches produced by a single
// recompute pass. ADR 002 § 5: patch order within a delta is
// meaningful only if Logic declared it so.
type Delta struct {
	Type              string  `json:"type"`
	V                 int     `json:"v"`
	SceneID           string  `json:"scene_id"`
	Sequence          uint64  `json:"sequence"`
	SchemaVersion     string  `json:"schema_version,omitempty"`
	SceneDigest       string  `json:"scene_digest,omitempty"`
	RuntimeInstanceID string  `json:"runtime_instance_id,omitempty"`
	Target            string  `json:"target,omitempty"`
	RenderRevision    string  `json:"render_revision,omitempty"`
	CorrelationID     string  `json:"correlation_id,omitempty"`
	Patches           []Patch `json:"patches"`
	Cause             *Cause  `json:"cause,omitempty"`
}

// Patch addresses a single leaf with its new value and an optional
// transition spec the renderer interpolates over.
type Patch struct {
	Path       string          `json:"path"`
	Value      json.RawMessage `json:"value"`
	Transition json.RawMessage `json:"transition,omitempty"`
}

// Cause is debug + audit only — never load-bearing for semantics.
type Cause struct {
	Source  string `json:"source"`
	InputID string `json:"input_id,omitempty"`
}

// SceneChanged signals the operator switched scenes (or a re-push
// of the same scene id landed). Always immediately followed by a
// fresh Snapshot of the destination scene.
type SceneChanged struct {
	Type        string          `json:"type"`
	V           int             `json:"v"`
	FromSceneID string          `json:"from_scene_id"`
	ToSceneID   string          `json:"to_scene_id"`
	Transition  json.RawMessage `json:"transition,omitempty"`
}

// Error is the server-emitted error envelope. recoverable: false
// signals "stop trying", true signals "you may retry / reconnect".
type Error struct {
	Type        string `json:"type"`
	V           int    `json:"v"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

// Pong echoes the nonce from the matching ping.
type Pong struct {
	Type  string `json:"type"`
	V     int    `json:"v"`
	Nonce string `json:"nonce"`
}

// Subscribed acknowledges that the server authenticated the connection and
// accepted its initial subscription. Service writers receive this frame when
// no scene is active, where a Snapshot cannot truthfully be produced yet.
// The mode is additive diagnostics; clients must key readiness on the type.
type Subscribed struct {
	Type string `json:"type"`
	V    int    `json:"v"`
	Mode string `json:"mode,omitempty"`
}

// Subscribe is the first message a client sends after connect. nil
// since_sequence asks for a full snapshot.
type Subscribe struct {
	Type          string  `json:"type"`
	V             int     `json:"v"`
	SinceSequence *uint64 `json:"since_sequence"`
}

// Input is a write to a leaf path. ClientMsgID is optional and
// echoes back into the resulting delta's Cause.InputID for
// optimistic-UI correlation.
type Input struct {
	Type        string          `json:"type"`
	V           int             `json:"v"`
	Path        string          `json:"path"`
	Value       json.RawMessage `json:"value"`
	Source      string          `json:"source,omitempty"`
	ClientMsgID string          `json:"client_msg_id,omitempty"`
}

// Unsubscribe is a clean signal that the client is done. Server
// closes the WS in response.
type Unsubscribe struct {
	Type string `json:"type"`
	V    int    `json:"v"`
}

// Ping carries a random nonce that the peer echoes in Pong.
type Ping struct {
	Type  string `json:"type"`
	V     int    `json:"v"`
	Nonce string `json:"nonce"`
}

// Encode marshals one of the server-emitted message types with the
// envelope filled in. Pass either a typed value (Snapshot, Delta, …)
// or a pointer to one; the type field is forced to its canonical
// string regardless of what the caller put there.
func Encode(msg any) ([]byte, error) {
	switch m := msg.(type) {
	case Snapshot:
		m.Type = TypeSnapshot
		m.V = ProtocolVersion
		return marshal(m)
	case *Snapshot:
		cp := *m
		cp.Type = TypeSnapshot
		cp.V = ProtocolVersion
		return marshal(cp)
	case Delta:
		m.Type = TypeDelta
		m.V = ProtocolVersion
		return marshal(m)
	case *Delta:
		cp := *m
		cp.Type = TypeDelta
		cp.V = ProtocolVersion
		return marshal(cp)
	case SceneChanged:
		m.Type = TypeSceneChanged
		m.V = ProtocolVersion
		return marshal(m)
	case *SceneChanged:
		cp := *m
		cp.Type = TypeSceneChanged
		cp.V = ProtocolVersion
		return marshal(cp)
	case Error:
		m.Type = TypeError
		m.V = ProtocolVersion
		return marshal(m)
	case *Error:
		cp := *m
		cp.Type = TypeError
		cp.V = ProtocolVersion
		return marshal(cp)
	case Pong:
		m.Type = TypePong
		m.V = ProtocolVersion
		return marshal(m)
	case *Pong:
		cp := *m
		cp.Type = TypePong
		cp.V = ProtocolVersion
		return marshal(cp)
	case Subscribed:
		m.Type = TypeSubscribed
		m.V = ProtocolVersion
		return marshal(m)
	case *Subscribed:
		cp := *m
		cp.Type = TypeSubscribed
		cp.V = ProtocolVersion
		return marshal(cp)
	case Ping:
		m.Type = TypePing
		m.V = ProtocolVersion
		return marshal(m)
	case *Ping:
		cp := *m
		cp.Type = TypePing
		cp.V = ProtocolVersion
		return marshal(cp)
	default:
		return nil, fmt.Errorf("protocol: unknown server message %T", msg)
	}
}

// Decode parses a client-emitted frame, dispatching on the `type`
// field. Returns one of (*Subscribe, *Input, *Unsubscribe, *Ping,
// *Pong) — Pong because a client may answer a server ping. Error
// surface keeps the wire-level mismatch (`UNKNOWN_TYPE`) distinct
// from a JSON decode failure (`INTERNAL` upstream).
func Decode(raw []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("protocol: decode envelope: %w", err)
	}
	if env.V != ProtocolVersion {
		return nil, ErrVersionMismatch
	}
	switch env.Type {
	case TypeSubscribe:
		var m Subscribe
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("protocol: decode subscribe: %w", err)
		}
		return &m, nil
	case TypeInput:
		var m Input
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("protocol: decode input: %w", err)
		}
		return &m, nil
	case TypeUnsubscribe:
		return &Unsubscribe{Type: TypeUnsubscribe, V: ProtocolVersion}, nil
	case TypePing:
		var m Ping
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("protocol: decode ping: %w", err)
		}
		return &m, nil
	case TypePong:
		var m Pong
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("protocol: decode pong: %w", err)
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
}

// ErrVersionMismatch and ErrUnknownType are the two structural decode
// errors the WS layer maps to wire-level Error frames.
var (
	ErrVersionMismatch = errors.New("protocol: version mismatch")
	ErrUnknownType     = errors.New("protocol: unknown message type")
)

// marshal uses encoding/json with HTML escaping disabled so paths
// with `<`, `>`, `&` (e.g., `__inputs.platform.<channel>`) are
// preserved on the wire instead of being rewritten as \u00xx.
func marshal(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder writes a trailing newline; strip it so a single
	// frame is exactly one JSON object with no trailing whitespace.
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}
