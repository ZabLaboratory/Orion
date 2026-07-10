// Package lsdp mounts Orion's LSDP/1.1 wire on top of the lumencast-go
// server kit (ADR 007 §C.3b), driven from the SAME reactive source as
// the bespoke WS wire (internal/ws).
//
// # What this package is, and is NOT
//
// It is a thin adapter that:
//
//   - builds one lumencast-go server.Server, configured with the
//     header-trust seam Config.IdentityFromRequest (ADR 007 §C.3a) so
//     the kit derives identity from ZabGate's injected
//     X-Authenticated-* headers and IGNORES the Subscribe Token;
//   - pairs every runtime.Scene with a kit server.Scene and forwards
//     the reactive loop's output (snapshot/delta/scene_changed) onto it
//     via Scene.Set / Scene.Emit / Server.SetActive;
//   - exposes the kit's LSDP/1.1 WebSocket handler so api.RegisterPublic
//     can mount it.
//
// It is NOT an auth layer. Orion validates no JWT — the gateway-first
// non-negotiable (_shared/architecture.md §"NO local auth on
// microservices") holds **by construction**: the identity comes from the
// configured AuthSource (HeaderAuthSource on the antenne = gateway-injected
// headers already validated at the edge; localOperatorAuth on
// embedded-local = the loopback handshake), and the kit's token
// Authenticator is never instantiated.
package lsdp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Lumencast/lumencast-go/protocol"
	lserver "github.com/Lumencast/lumencast-go/server"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

var _ runtime.MirrorRegistry = (*Wire)(nil)

// Wire owns the lumencast-go server.Server and the scene pairing. It
// implements runtime.MirrorRegistry (MirrorFor / SetActive / Drop) and
// runtime.SceneMirror (per scene, via sceneMirror).
//
// Constructed only in dual/lsdp mode; in bespoke mode no Wire exists
// and the kit code path is never reached (no-op deploy).
type Wire struct {
	srv    *lserver.Server
	logger *slog.Logger

	mu     sync.Mutex
	scenes map[string]*lserver.Scene

	// slots is the stream-level slot-assignment mirror (ADR Blue 009 §3.3,
	// issue #260): slot_ref → peer_label, the derived LSDP cache. Guarded by
	// its own mutex (written by the assign-slot op on a scene goroutine, read
	// at SetActive replay). See slot_mirror.go.
	slotMu sync.Mutex
	slots  map[string]string

	// overlay is the stream-level overlay-app control mirror (ADR 016 Prism
	// §3.2, issue #283): app_id → {running, on_air}, the derived LSDP cache
	// (memory only — RC #11). Guarded by its own mutex (written by the
	// overlay-app.set op on a scene goroutine, read at SetActive replay). See
	// overlay_mirror.go.
	overlayMu sync.Mutex
	overlay   map[string]*overlayState

	// viewer carries the stream-level Meet viewer-credentials arming on the
	// wire (ADR Blue 009 §3.2, issue #261). nil = arming disabled (bespoke
	// mode, or no CredsFetcher wired). Set once at boot by EnableViewerCreds,
	// before any scene goroutine runs; read-only thereafter. See viewer_arm.go.
	viewer *viewerArmer
}

// EnableViewerCreds turns on stream-level Meet viewer-credentials arming on
// this wire (ADR Blue 009 §3.2, issue #261). Called once at boot, before any
// scene is loaded. fetch resolves a `peer_label` to its room viewer creds;
// refresh is the rotation interval (<= 0 disables the ticker — re-arms only on
// a peer-set change). The armer goroutine exits on ctx cancel.
func (w *Wire) EnableViewerCreds(ctx context.Context, fetch CredsFetcher, refresh time.Duration) {
	if fetch == nil {
		return
	}
	a := &viewerArmer{
		wire:    w,
		fetch:   fetch,
		refresh: refresh,
		logger:  w.logger,
		ctx:     ctx,
		dirty:   make(chan struct{}, 1),
		peers:   map[string]struct{}{},
	}
	w.viewer = a
	go a.loop()
}

// NewWire builds the kit server. The kit Server is constructed but its
// own Run() is never called — Orion mounts the handler on its existing
// public mux (Handler()), so the kit shares Orion's listener rather than
// binding a second port.
//
// IdentityFromRequest is the ADR 007 §C.3a seam: serveLSDP calls it
// instead of Auth.Authenticate(token). Auth is left nil — Orion validates
// no JWT, and the kit's New() accepts IdentityFromRequest alone.
//
// src is the SAME identity seam the HTTP gates and the bespoke WS use
// (ADR 016 §3.2-2): HeaderAuthSource on the antenne (ZabGate-injected
// X-Authenticated-* headers), localOperatorAuth on embedded-local (the
// loopback handshake header X-Orion-Local-Auth). nil ⇒ HeaderAuthSource,
// keeping the antenne path byte-for-byte. Only WHO derives the Identity
// changes; the role mapping below is identical either way.
func NewWire(logger *slog.Logger, src auth.AuthSource) (*Wire, error) {
	srv, err := lserver.New(lserver.Config{
		// ListenAddr is required by New() even though we never call
		// Run(); a sentinel keeps construction valid. The kit handler
		// is mounted on Orion's mux via Handler(), not bound here.
		ListenAddr:          "127.0.0.1:0",
		IdentityFromRequest: identityFromRequest(src),
		Logger:              logger.With("component", "lsdp"),
	})
	if err != nil {
		return nil, err
	}
	return &Wire{
		srv:    srv,
		logger: logger.With("component", "lsdp"),
		scenes: make(map[string]*lserver.Scene),
	}, nil
}

// Handler returns the kit's LSDP/1.1 WebSocket handler so the public
// router can mount it (ADR 007 §C.5 dual-wire — a distinct route, the
// bespoke /show/stream is untouched). The kit's Mux() fixes the route
// at /lsdp.v1; we strip whatever prefix Orion mounts it under so the
// kit sees its own route.
func (w *Wire) Handler() http.Handler {
	return w.srv.Mux()
}

// MirrorFor registers (or returns the existing) kit scene for sceneID
// and returns a runtime.SceneMirror that forwards onto it. The first
// registered scene becomes the kit's active scene (matching the kit's
// own NewScene semantics), so a single-scene live show needs no
// explicit SetActive.
func (w *Wire) MirrorFor(sceneID, sceneVersion string, bundle *compiler.RenderBundle) runtime.SceneMirror {
	w.mu.Lock()
	defer w.mu.Unlock()
	sc, ok := w.scenes[sceneID]
	if !ok {
		sc = w.srv.NewScene(sceneID, lserver.WithSceneVersion(sceneVersion))
		w.scenes[sceneID] = sc
	} else {
		sc.SetVersion(sceneVersion)
	}
	return &sceneMirror{
		wire:    w,
		sceneID: sceneID,
		scene:   sc,
		bound:   boundLeavesFromBundle(bundle),
	}
}

// SetActive points the kit's live endpoint at sceneID and migrates its
// live subscribers (scene_changed + fresh snapshot), mirroring
// Show.SetActive. A miss is logged, not fatal — the bespoke wire is the
// source of truth for the switch; the kit is a parallel consumer.
func (w *Wire) SetActive(sceneID string) {
	if err := w.srv.SetActive(sceneID); err != nil && !errors.Is(err, lserver.ErrSceneNotFound) {
		w.logger.Warn("lsdp set active failed", "scene_id", sceneID, "err", err)
		return
	} else if errors.Is(err, lserver.ErrSceneNotFound) {
		w.logger.Warn("lsdp set active: scene not registered", "scene_id", sceneID)
		return
	}
	// Stream-level slot bindings outlive any scene (ADR Blue 009 §3.3): re-key
	// them onto the freshly-activated scene so a `meet-peer` slot persists
	// across the switch and a late joiner sees them in the destination
	// snapshot. issue #260.
	w.replaySlots(sceneID)
	// Overlay-app control is stream-level too (ADR 016 Prism §3.2, issue #283)
	// but is now carried on the show-level `overlay_apps` frame (lumencast-go
	// v0.2.0, #292), which the kit caches + replays on join independently of any
	// scene — so there is NO per-SetActive overlay replay here anymore.
	// Viewer credentials are stream-level too (ADR Blue 009 §3.2): replay the
	// last-armed `__cam.viewer` payload onto the freshly-activated scene so a
	// `meet-peer` slot keeps rendering across the switch. issue #261.
	if w.viewer != nil {
		w.viewer.replay(sceneID)
	}
}

// Drop forgets a scene's pairing on Unload. The kit has no public
// scene-removal API; dropping the local handle is enough — a re-Load of
// the same id calls NewScene again, which the kit replaces atomically.
func (w *Wire) Drop(sceneID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.scenes, sceneID)
}

// EmitRoster publishes the show's scene roster on the kit (Prism#230).
// It maps the runtime entries onto protocol.RosterEntry and hands them to
// server.SetRoster, which caches the roster and fans a scene_roster frame
// out to every live 1.1 subscriber (and replays it to each new one after
// its snapshot). An empty roster is valid (idle show → entries: []).
func (w *Wire) EmitRoster(entries []runtime.RosterEntry) {
	wire := make([]protocol.RosterEntry, len(entries))
	for i, e := range entries {
		wire[i] = protocol.RosterEntry{SceneID: e.SceneID, SceneVersion: e.SceneVersion}
	}
	w.srv.SetRoster(wire)
}

// identityFromRequest builds the kit's identity seam (ADR 007
// §C.3a/§C.3.seam) over a configurable AuthSource — the SAME seam the
// HTTP gates and the bespoke WS use (ADR 016 §3.2-2). It maps the derived
// Orion Identity onto the kit's server.Identity. The Subscribe frame's
// Token is never consulted; no JWT is validated here.
//
// On the antenne the source is HeaderAuthSource (ZabGate-injected
// X-Authenticated-* headers — the historical behaviour); on embedded-local
// it is localOperatorAuth, so the loopback handshake header
// X-Orion-Local-Auth is honoured on the .lsdp route too. A nil source
// defaults to HeaderAuthSource, keeping every existing call site
// byte-for-byte.
//
// Role mapping: Orion's RoleAdmin has no kit equivalent (the kit knows
// viewer/operator/service/test). Admins write everywhere, which is the
// kit operator's privilege, so admin maps to operator. An anonymous /
// unrecognised role yields an Anonymous identity, which the kit treats
// as auth failure (closes with AUTH_DENIED) — same as the bespoke
// wire's IsAuthenticated gate.
func identityFromRequest(src auth.AuthSource) func(*http.Request) (lserver.Identity, error) {
	if src == nil {
		src = auth.HeaderAuthSource{}
	}
	return func(r *http.Request) (lserver.Identity, error) {
		id := src.FromHeaders(r.Header)
		role, ok := mapRole(id.Role)
		if !ok {
			return lserver.Anonymous(), nil
		}
		return lserver.Identity{
			Subject: id.UserID,
			Role:    role,
			Paths:   id.Paths,
		}, nil
	}
}
