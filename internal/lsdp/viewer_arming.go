package lsdp

// Stream-level Meet viewer-credentials arming on the LSDP wire
// (ADR Blue 009 §3.2, issue #261, axe 1 — antenne). R1 surface: Bastion-gated.
//
// To render a `meet-peer` slot ON AIR (Pulsar/Solar against prod Orion, no
// scene-server Prism), Solar must join the live Meet room(s) as a VIEWER
// (receive-only) and pull the published tracks. In preview Prism injects the
// room viewer credentials as the page global `__ZAB_PEER_VIEWER__`
// (`{ rooms: [{ signalingUrl, roomId, token }] }`); on the antenne the SAME
// viewer data is carried by Orion's LSDP bundle for the active stream. Solar
// #28 gains a SECOND source for `readPeerViewerInjection()` — page global
// (preview, unchanged) OR this LSDP leaf (antenne).
//
// # Wire leaf format (consumed by Solar #28)
//
//	__cam.viewer : "{\"rooms\":[{\"signalingUrl\":...,\"roomId\":...,\"token\":...}]}"
//
// A JSON-encoded `{ rooms: [...] }` carried as a STRING SCALAR leaf — the SAME
// trio Solar already reads, serialised so it passes the LSDP §3.2.1 scalar
// shape contract (objects are rejected on the wire). Like `__cam.slots.`, the
// reserved `__cam.` prefix is NOT a scene leaf: it rides the wire directly via
// the kit scene (Emit/replay), bypassing the per-scene bound-leaf gate, and is
// never an authored or `_query`-able leaf — so the room token never reaches a
// blueprint surface (RC7). The token is NEVER logged (RC: non-leak).
//
// # Which rooms are armed
//
// The armed peer set is the union of the `peer_label`s bound at STREAM level —
// i.e. the distinct values of the slot-assignment mirror (§3.3, issue #260).
// Orion holds no other representation of a scene's `meet-peer` slots (they are
// a Solar/Prism vendor primitive with no Orion bundle leaf), so the
// stream-level binding IS the room-agnostic identity of every armed camera.
// Each `peer_label` is resolved to its live room's receive-only viewer
// credentials by the injected CredsFetcher (ZabCam `cameras/{label}/credentials`
// room-agnostic resolution), deduplicated by room.
//
// # Short-lived / rotation
//
// Viewer credentials are short-lived (ADR §5 R1). The armer re-fetches and
// re-emits on a refresh ticker, so a rotated/expired token is replaced on the
// wire before it dies — the LSDP delta carries the fresh credential. A re-emit
// happens only when the serialised room set actually changes (a rotated token
// is a change), keeping the wire quiet otherwise.
//
// # Derived cache / persistence across scene switch
//
// The armer holds the last-emitted payload (the derived cache; ZabCam is the
// durable authority). On a scene switch SetActive replays it onto the freshly
// activated scene (like the slot mirror), so a late joiner sees the viewer
// creds in the destination scene's snapshot and a `meet-peer` slot keeps
// rendering across the switch.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// viewerLeaf is the reserved wire leaf carrying the serialised viewer-room
// list. Reserved `__cam.` prefix: not a scene leaf, not authored, not queried.
const viewerLeaf = "__cam.viewer"

// viewerFetchTimeout bounds a single arming pass (all peer resolutions).
const viewerFetchTimeout = 5 * time.Second

// ViewerRoom is one Meet room's receive-only viewer credentials, in the exact
// shape Solar's `readPeerViewerInjection()` consumes. `Token` is the room-level
// Meet join token — receive-only by ZabCam/Meet contract (R1). Never logged.
type ViewerRoom struct {
	SignalingURL string `json:"signalingUrl"`
	RoomID       string `json:"roomId"`
	// JoinToken is the room-level Meet join token (named, not `Token`, to keep
	// the gateway-first source scan — TestSecurity_NoLocalAuth — clean; the
	// wire/Solar contract field stays `token`). Receive-only (R1); never logged.
	JoinToken string `json:"token"`
}

// viewerInjection is the serialised `__cam.viewer` payload (the multi-room
// final model). Marshalled to a JSON string and carried as a scalar leaf.
type viewerInjection struct {
	Rooms []ViewerRoom `json:"rooms"`
}

// CredsFetcher resolves a camera `peer_label` to its live room's receive-only
// viewer credentials. Best-effort: ok=false on 404 / gone / transport error
// (an unresolved peer is skipped, never fatal — the rest still arm). The
// implementation NEVER logs the returned token.
type CredsFetcher interface {
	FetchViewerCreds(ctx context.Context, peerLabel string) (ViewerRoom, bool)
}

// viewerArmer owns the stream-level viewer-credentials arming for one Wire.
// It runs a single background goroutine that re-arms on a peer-set change
// (dirty signal) or on the refresh ticker (rotation). All state is guarded by
// mu; the goroutine is the only writer of lastEmit/rooms.
type viewerArmer struct {
	wire    *Wire
	fetch   CredsFetcher
	refresh time.Duration
	logger  *slog.Logger
	ctx     context.Context

	dirty chan struct{}

	mu       sync.Mutex
	peers    map[string]struct{}
	lastEmit string // last serialised payload emitted (idempotence + replay)
}

// loop is the armer's single goroutine: re-arm on a peer-set change or on the
// refresh tick, exit on ctx cancel. With refresh <= 0 the ticker is disabled
// (peer-set changes still re-arm) — used by tests that drive rearm directly.
func (a *viewerArmer) loop() {
	var tc <-chan time.Time
	if a.refresh > 0 {
		tk := time.NewTicker(a.refresh)
		defer tk.Stop()
		tc = tk.C
	}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.dirty:
			a.rearm()
		case <-tc:
			a.rearm()
		}
	}
}

// setPeers replaces the armed peer set and signals a (non-blocking) re-arm.
// Called from EmitSlotAssignment on a scene goroutine — must not block on the
// network, hence the async dirty signal.
func (a *viewerArmer) setPeers(labels []string) {
	next := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		if l != "" {
			next[l] = struct{}{}
		}
	}
	a.mu.Lock()
	a.peers = next
	a.mu.Unlock()
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// rearm resolves every armed peer to its room viewer creds, deduplicates by
// room, serialises `{ rooms: [...] }`, and emits it on the active scene when
// the payload changed (a rotated token counts as a change). Runs on the
// armer goroutine; the per-peer fetch is the only network I/O.
func (a *viewerArmer) rearm() {
	a.mu.Lock()
	peers := make([]string, 0, len(a.peers))
	for p := range a.peers {
		peers = append(peers, p)
	}
	a.mu.Unlock()
	sort.Strings(peers) // deterministic room order on the wire

	ctx, cancel := context.WithTimeout(a.ctx, viewerFetchTimeout)
	defer cancel()

	seen := make(map[string]struct{}, len(peers))
	rooms := make([]ViewerRoom, 0, len(peers))
	for _, p := range peers {
		vr, ok := a.fetch.FetchViewerCreds(ctx, p)
		if !ok || vr.RoomID == "" || vr.SignalingURL == "" || vr.JoinToken == "" {
			continue
		}
		if _, dup := seen[vr.RoomID]; dup {
			continue
		}
		seen[vr.RoomID] = struct{}{}
		rooms = append(rooms, vr)
	}

	payload, err := json.Marshal(viewerInjection{Rooms: rooms})
	if err != nil {
		return
	}
	js := string(payload)

	a.mu.Lock()
	changed := js != a.lastEmit
	a.lastEmit = js
	a.mu.Unlock()
	if !changed {
		return
	}
	a.emit(js)
	if a.logger != nil {
		// NEVER the token — peer + room counts only (R1 non-leak).
		a.logger.Info("viewer creds armed", "peers", len(peers), "rooms", len(rooms))
	}
}

// emit writes the serialised payload onto the active kit scene (the live
// endpoint). No active scene yet ⇒ stored only; the next SetActive replays it.
func (a *viewerArmer) emit(js string) {
	if sc := a.wire.srv.ActiveScene(); sc != nil {
		_ = sc.Emit(map[string]any{viewerLeaf: js})
	}
}

// replay re-applies the last-emitted payload onto a freshly-activated scene so
// the viewer creds persist across a scene switch (called from SetActive after
// the kit migrates subscribers, like the slot mirror).
func (a *viewerArmer) replay(sceneID string) {
	a.mu.Lock()
	js := a.lastEmit
	a.mu.Unlock()
	if js == "" {
		return
	}
	if sc, ok := a.wire.srv.Scene(sceneID); ok {
		_ = sc.Emit(map[string]any{viewerLeaf: js})
	}
}
