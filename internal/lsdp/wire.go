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
// microservices") holds **by construction**: the only identity source
// here is auth.FromHeaders (gateway-injected headers, already validated
// at the edge), and the kit's token Authenticator is never instantiated.
package lsdp

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"

	lserver "github.com/Lumencast/lumencast-go/server"

	"github.com/ZabLaboratory/Orion/internal/auth"
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
}

// NewWire builds the kit server in header-trust mode. The kit Server is
// constructed but its own Run() is never called — Orion mounts the
// handler on its existing public mux (Handler()), so the kit shares
// Orion's listener rather than binding a second port.
//
// IdentityFromRequest is the ADR 007 §C.3a seam: serveLSDP calls it
// instead of Auth.Authenticate(token). Auth is left nil — header-trust
// deployments need no token validator, and the kit's New() accepts
// IdentityFromRequest alone.
func NewWire(logger *slog.Logger) (*Wire, error) {
	srv, err := lserver.New(lserver.Config{
		// ListenAddr is required by New() even though we never call
		// Run(); a sentinel keeps construction valid. The kit handler
		// is mounted on Orion's mux via Handler(), not bound here.
		ListenAddr:          "127.0.0.1:0",
		IdentityFromRequest: IdentityFromRequest,
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
func (w *Wire) MirrorFor(sceneID, sceneVersion string) runtime.SceneMirror {
	w.mu.Lock()
	defer w.mu.Unlock()
	sc, ok := w.scenes[sceneID]
	if !ok {
		sc = w.srv.NewScene(sceneID, lserver.WithSceneVersion(sceneVersion))
		w.scenes[sceneID] = sc
	} else {
		sc.SetVersion(sceneVersion)
	}
	return &sceneMirror{wire: w, sceneID: sceneID, scene: sc}
}

// SetActive points the kit's live endpoint at sceneID and migrates its
// live subscribers (scene_changed + fresh snapshot), mirroring
// Show.SetActive. A miss is logged, not fatal — the bespoke wire is the
// source of truth for the switch; the kit is a parallel consumer.
func (w *Wire) SetActive(sceneID string) {
	if err := w.srv.SetActive(sceneID); err != nil && !errors.Is(err, lserver.ErrSceneNotFound) {
		w.logger.Warn("lsdp set active failed", "scene_id", sceneID, "err", err)
	} else if errors.Is(err, lserver.ErrSceneNotFound) {
		w.logger.Warn("lsdp set active: scene not registered", "scene_id", sceneID)
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

// IdentityFromRequest is the header-trust seam Orion supplies to the
// kit (ADR 007 §C.3a/§C.3.seam). It maps ZabGate's injected
// X-Authenticated-* headers to the kit's server.Identity — the exact
// same trust path the bespoke WS uses (ws/server.go:45). The Subscribe
// frame's Token is never consulted; no JWT is validated here.
//
// Role mapping: Orion's RoleAdmin has no kit equivalent (the kit knows
// viewer/operator/service/test). Admins write everywhere, which is the
// kit operator's privilege, so admin maps to operator. An anonymous /
// unrecognised role yields an Anonymous identity, which the kit treats
// as auth failure (closes with AUTH_DENIED) — same as the bespoke
// wire's IsAuthenticated gate.
func IdentityFromRequest(r *http.Request) (lserver.Identity, error) {
	id := auth.FromHeaders(r.Header)
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
