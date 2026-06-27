package lsdp

import (
	"log/slog"
	"net/http"

	lserver "github.com/Lumencast/lumencast-go/server"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

var _ runtime.SessionWireFactory = (*SessionWireFactory)(nil)

// SessionWireFactory builds per-test-session LSDP/1.1 endpoints — the
// PREVIEW wire (option B of the preview/antenne split). Each session gets
// its OWN lserver.Server whose sole, always-active scene is the session
// clone, so Solar — subscribing live-mode — follows ONLY that clone and can
// neither observe nor drive the global show's active scene. There is no
// shared active pointer to collide: the isolation is structural, not a
// policy. We NEVER call Server.SetActive on a session server, so the clone
// can never be swapped out from under a preview client, and a preview switch
// can never reach the antenne (the global /show/stream.lsdp wire is built
// from a different Server entirely and is untouched here).
//
// Identity is derived from the SAME AuthSource as the bespoke test WS and
// the global wire (HeaderAuthSource on antenne, localOperatorAuth on
// embedded-local): operator/test role required, no JWT validated.
type SessionWireFactory struct {
	logger *slog.Logger
	src    auth.AuthSource
}

// NewSessionWireFactory constructs the factory. Wired onto the
// TestSessionManager in dual/lsdp mode only.
func NewSessionWireFactory(logger *slog.Logger, src auth.AuthSource) *SessionWireFactory {
	return &SessionWireFactory{
		logger: logger.With("component", "lsdp-test"),
		src:    src,
	}
}

// NewSessionWire builds a fresh isolated kit server for one session and
// registers the clone as its sole, always-active scene.
func (f *SessionWireFactory) NewSessionWire(sceneID, sceneVersion string, bundle *compiler.RenderBundle) runtime.SessionWire {
	srv, err := lserver.New(lserver.Config{
		// Required by New() even though Run() is never called — the handler
		// is mounted on Orion's mux via the .lsdp route, not bound here.
		ListenAddr:          "127.0.0.1:0",
		IdentityFromRequest: identityFromRequest(f.src),
		Logger:              f.logger,
	})
	if err != nil {
		// New only errors on a missing ListenAddr / auth seam, both supplied
		// above — unreachable in practice. Degrade to a no-op wire so the
		// caller's SetMirror(nil) stays safe rather than panicking.
		f.logger.Error("session wire construct failed", "err", err)
		return noopSessionWire{}
	}
	// NewScene sets the server's active scene to the first registered id, so
	// this clone is active from birth — a live-mode subscriber gets it with
	// no SetActive call.
	sc := srv.NewScene(sceneID, lserver.WithSceneVersion(sceneVersion))
	return &sessionWire{
		srv: srv,
		mirror: &sceneMirror{
			sceneID: sceneID,
			scene:   sc,
			bound:   boundLeavesFromBundle(bundle),
		},
	}
}

// sessionWire is one session's isolated kit endpoint.
type sessionWire struct {
	srv    *lserver.Server
	mirror runtime.SceneMirror
}

func (s *sessionWire) Mirror() runtime.SceneMirror { return s.mirror }

// Handler returns the kit's mux (its own /lsdp.v1); the .lsdp route
// rewrites the request path onto it, exactly like the global wire.
func (s *sessionWire) Handler() http.Handler { return s.srv.Mux() }

// Close detaches in-flight subscribers and drops the session's scene. The
// kit exposes no per-scene removal, so Reset() is the teardown — the whole
// server is then unreferenced and GC-reclaimed.
func (s *sessionWire) Close() { s.srv.Reset() }

// noopSessionWire is the unreachable construct-failure fallback.
type noopSessionWire struct{}

func (noopSessionWire) Mirror() runtime.SceneMirror { return nil }
func (noopSessionWire) Handler() http.Handler       { return http.NotFoundHandler() }
func (noopSessionWire) Close()                      {}
