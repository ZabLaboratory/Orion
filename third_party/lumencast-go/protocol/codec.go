package protocol

import (
	"fmt"
)

// Encode marshals a server-emitted frame to its canonical JSON
// representation. The envelope `v` and `type` fields are stamped
// regardless of what the caller put on the value, so callers can
// leave them zero-valued when constructing frames.
//
// Accepts both value and pointer receivers for ergonomics.
func Encode(msg any) ([]byte, error) {
	switch m := msg.(type) {
	case Snapshot:
		m.V, m.Type = Version, TypeSnapshot
		return marshal(m)
	case *Snapshot:
		c := *m
		c.V, c.Type = Version, TypeSnapshot
		return marshal(c)
	case Delta:
		m.V, m.Type = Version, TypeDelta
		return marshal(m)
	case *Delta:
		c := *m
		c.V, c.Type = Version, TypeDelta
		return marshal(c)
	case SceneChanged:
		m.V, m.Type = Version, TypeSceneChanged
		return marshal(m)
	case *SceneChanged:
		c := *m
		c.V, c.Type = Version, TypeSceneChanged
		return marshal(c)
	case SceneRoster:
		m.V, m.Type = Version, TypeSceneRoster
		if m.Entries == nil {
			m.Entries = []RosterEntry{}
		}
		return marshal(m)
	case *SceneRoster:
		c := *m
		c.V, c.Type = Version, TypeSceneRoster
		if c.Entries == nil {
			c.Entries = []RosterEntry{}
		}
		return marshal(c)
	case OverlayApps:
		m.V, m.Type = Version, TypeOverlayApps
		if m.Apps == nil {
			m.Apps = map[string]OverlayAppState{}
		}
		return marshal(m)
	case *OverlayApps:
		c := *m
		c.V, c.Type = Version, TypeOverlayApps
		if c.Apps == nil {
			c.Apps = map[string]OverlayAppState{}
		}
		return marshal(c)
	case Error:
		m.V, m.Type = Version, TypeError
		return marshal(m)
	case *Error:
		c := *m
		c.V, c.Type = Version, TypeError
		return marshal(c)
	case Pong:
		m.V, m.Type = Version, TypePong
		return marshal(m)
	case *Pong:
		c := *m
		c.V, c.Type = Version, TypePong
		return marshal(c)
	case Ping:
		m.V, m.Type = Version, TypePing
		return marshal(m)
	case *Ping:
		c := *m
		c.V, c.Type = Version, TypePing
		return marshal(c)
	case Subscribe:
		m.V, m.Type = Version, TypeSubscribe
		return marshal(m)
	case *Subscribe:
		c := *m
		c.V, c.Type = Version, TypeSubscribe
		return marshal(c)
	case Input:
		m.V, m.Type = Version, TypeInput
		return marshal(m)
	case *Input:
		c := *m
		c.V, c.Type = Version, TypeInput
		return marshal(c)
	case Unsubscribe:
		m.V, m.Type = Version, TypeUnsubscribe
		return marshal(m)
	case *Unsubscribe:
		c := *m
		c.V, c.Type = Version, TypeUnsubscribe
		return marshal(c)
	default:
		return nil, fmt.Errorf("protocol: cannot encode %T", msg)
	}
}

// Decode parses a client-emitted frame and dispatches on `type`.
// Returns one of (*Subscribe, *Input, *Ping, *Pong) — Pong because
// a client may answer a server-initiated ping. Forward-compatibility :
// unknown frame types yield ErrUnknownType so the WS layer can map it
// to an Error{Code: INTERNAL} response.
//
// Servers SHOULD use DecodeServer when they know they are reading
// from a client connection — it rejects server-only frame types.
func Decode(raw []byte) (any, error) {
	var env envelope
	if err := unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.V != Version {
		return nil, fmt.Errorf("%w: got v=%d", ErrVersionMismatch, env.V)
	}
	switch env.Type {
	case TypeSubscribe:
		var m Subscribe
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeInput:
		var m Input
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypePing:
		// Decode the full Ping struct so the optional Nonce
		// (LSDP/1.1 §4.3) round-trips. 1.0 clients omit Nonce ;
		// the field stays empty.
		var m Ping
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypePong:
		// Same treatment — 1.1 clients may also send Pong with a
		// Nonce (servers can ping clients per spec §3.5).
		var m Pong
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeUnsubscribe:
		var m Unsubscribe
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
}

// DecodeServer parses a server-emitted frame, used by clients (and
// the conformance harness). Returns one of (*Snapshot, *Delta,
// *SceneChanged, *SceneRoster, *Error, *Pong, *Ping). Per spec § 13,
// runtimes that
// receive an unknown frame type MUST silently ignore it ; the harness
// surfaces the unknown via ErrUnknownType so test code can choose.
func DecodeServer(raw []byte) (any, error) {
	var env envelope
	if err := unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.V != Version {
		return nil, fmt.Errorf("%w: got v=%d", ErrVersionMismatch, env.V)
	}
	switch env.Type {
	case TypeSnapshot:
		var m Snapshot
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeDelta:
		var m Delta
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeSceneChanged:
		var m SceneChanged
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeSceneRoster:
		var m SceneRoster
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeOverlayApps:
		var m OverlayApps
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeError:
		var m Error
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypePong:
		// Decode full Pong so the optional Nonce (1.1) round-trips.
		var m Pong
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case TypePing:
		var m Ping
		if err := unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
}
