package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Lumencast/lumencast-go/protocol"
)

// Config configures a Server. The zero value is not usable ; ListenAddr
// and Auth are required.
type Config struct {
	// ListenAddr is the TCP address Run() binds to. Use ":0" in tests
	// to get an OS-assigned port (then read it via Server.Addr()).
	ListenAddr string

	// Auth is the credential validator called on every WebSocket
	// subscribe frame. The kit ships StaticTokens for development.
	//
	// REQUIRED unless IdentityFromRequest is set : New rejects a Config
	// where both Auth and IdentityFromRequest are nil.
	Auth Authenticator

	// IdentityFromRequest is the optional header-trust seam (ADR 007
	// §C.3a). When non-nil, the server derives the connection's Identity
	// from the upgrade *http.Request — typically a header injected by a
	// trusted front-proxy — and IGNORES the Subscribe frame's token.
	// When nil, the server falls back to the Auth token path.
	//
	// Additive and non-breaking : leaving this nil preserves the exact
	// token-authentication behaviour. If both Auth and IdentityFromRequest
	// are set, IdentityFromRequest takes precedence.
	IdentityFromRequest RequestIdentityFunc

	// Logger receives structured server events. Defaults to
	// slog.Default() if nil.
	Logger *slog.Logger

	// PingInterval is the server-initiated ping cadence. Defaults to
	// 30 s, matching the LSDP/1 § 12 SHOULD.
	PingInterval time.Duration

	// SubscribeTimeout is how long the server waits for the first
	// Subscribe frame before closing the WS. Defaults to 30 s.
	SubscribeTimeout time.Duration

	// MaxFrameBytes is the largest text frame the server accepts.
	// Defaults to 64 KiB per LSDP/1 § 14.3.
	MaxFrameBytes int64

	// HTTPHandler, if non-nil, is mounted at root for non-LSDP
	// traffic (static assets, /healthz, ...). LSDP routes are mounted
	// in front of it. Use Server.Mux() to compose.
	HTTPHandler http.Handler
}

// Server is the LSDP/1 server kit. It owns a set of Scenes and a
// WebSocket handler. Construct with New and run with Run.
type Server struct {
	cfg    Config
	logger *slog.Logger

	mu     sync.RWMutex
	scenes map[string]*Scene
	active string // id of the currently-active scene for the live endpoint

	// rosterMu guards the cached show roster (scene_roster frame). Kept
	// separate from mu so a roster fan-out never contends with scene
	// registration / SetActive.
	rosterMu  sync.RWMutex
	roster    []protocol.RosterEntry
	rosterSet bool

	// overlayMu guards the cached show-level overlay-app control state
	// (overlay_apps frame). Like the roster it is show metadata, fanned out
	// to every live 1.1 subscriber and replayed on join — including the
	// holding subscribers below, so it reaches a consumer with no active
	// scene.
	overlayMu  sync.RWMutex
	overlay    map[string]protocol.OverlayAppState
	overlaySet bool

	// holding carries live subscribers that connected while NO scene was
	// active (an empty show). They receive show-level frames (roster,
	// overlay_apps) but no scene state; when a scene first becomes active
	// they are migrated onto it (migrateLive) with a fresh snapshot. Created
	// once in New and never registered in scenes, so it never appears in the
	// roster nor as the active scene.
	holding *Scene

	httpSrv *http.Server
	addr    string
}

// New constructs a Server. Returns an error if Config is incomplete.
func New(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("server: Config.ListenAddr is required")
	}
	if cfg.Auth == nil && cfg.IdentityFromRequest == nil {
		return nil, errors.New("server: Config.Auth or Config.IdentityFromRequest is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.SubscribeTimeout == 0 {
		cfg.SubscribeTimeout = 30 * time.Second
	}
	if cfg.MaxFrameBytes == 0 {
		cfg.MaxFrameBytes = 64 * 1024
	}
	return &Server{
		cfg:     cfg,
		logger:  cfg.Logger,
		scenes:  make(map[string]*Scene),
		holding: newScene(holdingSceneID),
	}, nil
}

// holdingSceneID is the sentinel id of the internal holding scene a live
// subscriber attaches to while the show has no active scene. It is never a
// registered scene, so it appears in no roster and is never the active scene.
const holdingSceneID = "__holding"

// NewScene registers a new Scene under the given id and returns a
// handle. Reusing an id replaces the previous Scene atomically and
// triggers a SceneChanged on the live endpoint.
func (s *Server) NewScene(id string, opts ...SceneOption) *Scene {
	scene := newScene(id, opts...)
	s.mu.Lock()
	prev := s.scenes[id]
	s.scenes[id] = scene
	justActivated := false
	if s.active == "" {
		s.active = id
		justActivated = true
	}
	s.mu.Unlock()
	// First scene to go active: adopt any subscribers that were holding while
	// the show was empty, so a scene-less join transitions onto the real scene.
	if justActivated {
		migrateLive(s.holding, scene)
	}
	if prev != nil {
		// Best-effort : detach old subscribers. They'll reconnect
		// against the new scene through the live endpoint.
		prev.mu.Lock()
		for sub := range prev.subscribers {
			sub.close()
		}
		prev.subscribers = nil
		prev.mu.Unlock()
	}
	return scene
}

// Scene looks up a registered scene by id.
func (s *Server) Scene(id string) (*Scene, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sc, ok := s.scenes[id]
	return sc, ok
}

// SetActive changes which scene the live endpoint serves. Live
// subscribers attached to the previous active scene receive a
// SceneChanged frame followed by a fresh Snapshot at seq=1 on the new
// scene. Idempotent : setting the already-active scene is a no-op.
// Returns ErrSceneNotFound if id is unknown.
func (s *Server) SetActive(id string) error {
	s.mu.Lock()
	scene, ok := s.scenes[id]
	if !ok {
		s.mu.Unlock()
		return ErrSceneNotFound
	}
	if s.active == id {
		s.mu.Unlock()
		return nil
	}
	wasEmpty := s.active == ""
	prev := s.scenes[s.active]
	s.active = id
	s.mu.Unlock()

	// Activating from an empty show: adopt the holding subscribers.
	if wasEmpty {
		migrateLive(s.holding, scene)
	}
	if prev != nil && prev != scene {
		migrateLive(prev, scene)
	}
	return nil
}

// SetRoster caches the show's scene roster and fans a scene_roster
// frame out to every live subscriber that negotiated lsdp.v1.1. The
// cached roster is also replayed to each new subscriber right after its
// initial snapshot (see serveLSDP). Passing an empty (or nil) slice is
// valid — it advertises a show with no scenes as `entries: []`.
//
// Additive and idempotent from the caller's side : call it whenever the
// set of scenes or their versions changes.
func (s *Server) SetRoster(entries []protocol.RosterEntry) {
	cloned := make([]protocol.RosterEntry, len(entries))
	copy(cloned, entries)

	s.rosterMu.Lock()
	s.roster = cloned
	s.rosterSet = true
	s.rosterMu.Unlock()

	frame := &protocol.SceneRoster{
		Entries: cloned,
		TS:      time.Now().UTC().Format(time.RFC3339),
	}

	s.fanoutShowFrame(frame)
}

// allFanoutScenes returns every registered scene plus the holding scene — the
// full set a show-level frame (roster, overlay_apps) fans out to. Including
// holding is what reaches the subscribers that connected with no active scene.
func (s *Server) allFanoutScenes() []*Scene {
	s.mu.RLock()
	scenes := make([]*Scene, 0, len(s.scenes)+1)
	for _, sc := range s.scenes {
		scenes = append(scenes, sc)
	}
	s.mu.RUnlock()
	return append(scenes, s.holding)
}

// fanoutShowFrame sends a show-level metadata frame to every live 1.1
// subscriber (across all scenes plus holding). Back-pressured subscribers are
// marked stale, exactly as delta fan-out does. Used for both scene_roster and
// overlay_apps.
func (s *Server) fanoutShowFrame(frame any) {
	for _, sc := range s.allFanoutScenes() {
		sc.mu.Lock()
		for sub := range sc.subscribers {
			if !sub.live || !sub.proto11 {
				continue
			}
			select {
			case sub.out <- frame:
			default:
				sub.markStale()
			}
		}
		sc.mu.Unlock()
	}
}

// SetOverlayApps caches the show's complete overlay-app control state and fans
// an overlay_apps frame out to every live 1.1 subscriber — the overlay analogue
// of SetRoster. The full state (not a delta) is cached and replayed to each new
// subscriber after its snapshot (serveLSDP), INCLUDING a subscriber attached to
// the holding scene, so the state is deliverable with no active scene. Passing
// an empty (or nil) map is valid — it advertises a show with no overlay apps as
// `apps: {}`. Additive and idempotent: call it whenever the overlay-app set or
// any app's state changes.
func (s *Server) SetOverlayApps(apps map[string]protocol.OverlayAppState) {
	cloned := make(map[string]protocol.OverlayAppState, len(apps))
	for k, v := range apps {
		cloned[k] = v
	}

	s.overlayMu.Lock()
	s.overlay = cloned
	s.overlaySet = true
	s.overlayMu.Unlock()

	s.fanoutShowFrame(&protocol.OverlayApps{
		Apps: cloned,
		TS:   time.Now().UTC().Format(time.RFC3339),
	})
}

// overlayAppsFrame returns the cached overlay-app state as a ready-to-send
// frame and whether SetOverlayApps has ever been called. Used to replay the
// state to a freshly-subscribed 1.1 live connection.
func (s *Server) overlayAppsFrame() (*protocol.OverlayApps, bool) {
	s.overlayMu.RLock()
	defer s.overlayMu.RUnlock()
	if !s.overlaySet {
		return nil, false
	}
	return &protocol.OverlayApps{
		Apps: s.overlay,
		TS:   time.Now().UTC().Format(time.RFC3339),
	}, true
}

// rosterFrame returns the cached roster as a ready-to-send frame and
// whether a roster has ever been set. Used to replay the roster to a
// freshly-subscribed 1.1 live connection.
func (s *Server) rosterFrame() (*protocol.SceneRoster, bool) {
	s.rosterMu.RLock()
	defer s.rosterMu.RUnlock()
	if !s.rosterSet {
		return nil, false
	}
	return &protocol.SceneRoster{
		Entries: s.roster,
		TS:      time.Now().UTC().Format(time.RFC3339),
	}, true
}

// detach removes a subscription from whichever scene currently owns
// it. Used as the deferred cleanup in the WS handler so a migration
// during SetActive does not leak.
func (s *Server) detach(sub *subscription) {
	// Includes the holding scene, so a subscriber that connected with no
	// active scene (and was never migrated) is cleaned up too.
	for _, sc := range s.allFanoutScenes() {
		sc.unsubscribe(sub)
	}
}

// migrateLive moves every live subscription from prev to next. It
// emits SceneChanged on prev (with prev's seq advancing one final
// step), then a fresh Snapshot at next's current seq (typically 1
// for a freshly-created scene). Per LSDP/1.1 §18.1.1, the scene's
// seq counter is independent across scenes.
func migrateLive(prev, next *Scene) {
	prev.mu.Lock()
	live := make([]*subscription, 0, len(prev.subscribers))
	for sub := range prev.subscribers {
		if sub.live {
			live = append(live, sub)
		}
	}
	for _, sub := range live {
		delete(prev.subscribers, sub)
	}
	// Advance prev's seq one last time for the SceneChanged frame —
	// SceneChanged is the prev scene's final wire emission.
	prevSeq := prev.seq.NextServer()
	prev.mu.Unlock()

	if len(live) == 0 {
		return
	}

	next.mu.Lock()
	state := next.store.snapshot()
	nextSeq := next.seq.Current()
	for _, sub := range live {
		sc := &protocol.SceneChanged{
			Seq:          prevSeq,
			SceneID:      next.id,
			SceneVersion: next.version,
		}
		select {
		case sub.out <- sc:
		default:
			sub.markStale()
			continue
		}
		snap := &protocol.Snapshot{
			Seq:          nextSeq,
			SceneID:      next.id,
			SceneVersion: next.version,
			State:        state,
		}
		next.stampMetadata(snap)
		select {
		case sub.out <- snap:
		default:
			sub.markStale()
			continue
		}
		next.subscribers[sub] = struct{}{}
	}
	next.mu.Unlock()
}

// ActiveScene returns the live scene, or nil if none is registered.
func (s *Server) ActiveScene() *Scene {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == "" {
		return nil
	}
	return s.scenes[s.active]
}

// ErrSceneNotFound is returned by SetActive and Scene when the id is
// unknown.
var ErrSceneNotFound = errors.New("server: scene not found")

// Reset drops every registered scene and clears active state. In-flight
// subscribers are detached. Intended for test harnesses (interop
// control plane) — not for production use.
func (s *Server) Reset() {
	s.mu.Lock()
	scenes := s.scenes
	s.scenes = make(map[string]*Scene)
	s.active = ""
	s.mu.Unlock()
	for _, sc := range scenes {
		sc.mu.Lock()
		for sub := range sc.subscribers {
			sub.close()
		}
		sc.subscribers = nil
		sc.mu.Unlock()
	}
}

// Mux returns a configured http.ServeMux exposing :
//
//   - /lsdp.v1                 — LSDP/1 WebSocket subscribe endpoint
//   - /scenes/{id}             — per-scene scope (test mode + static)
//   - /healthz                 — simple liveness probe
//   - the user-supplied Config.HTTPHandler at root (lowest priority)
//
// The mux is regenerated on each call ; do not modify the returned
// instance after Server.Run is called.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/lsdp.v1", s.serveLSDP)
	mux.HandleFunc("/healthz", s.serveHealth)
	if s.cfg.HTTPHandler != nil {
		mux.Handle("/", s.cfg.HTTPHandler)
	}
	return mux
}

// Addr returns the bound address after Run() has started, or empty
// before that. Useful in tests with ListenAddr ":0".
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

// Run binds and serves until ctx is cancelled. The returned error is
// non-nil only on listener / Serve failure ; clean shutdown via
// ctx.Done returns nil.
func (s *Server) Run(ctx context.Context) error {
	mux := s.Mux()
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := netListen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: listen: %w", err)
	}

	s.mu.Lock()
	s.httpSrv = srv
	s.addr = listener.Addr().String()
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
