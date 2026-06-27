package runtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// SessionWire is a per-test-session LSDP/1.1 endpoint — the PREVIEW wire.
// Each test session gets its OWN isolated lumencast-go server whose sole,
// always-active scene is this session's clone. Solar subscribes live-mode
// against Handler() and follows ONLY the clone; it can neither observe nor
// drive the global show's active scene. This is the structural isolation the
// preview/antenne split needs: no shared active pointer, ever. Close tears
// the kit scene down when the session dies.
type SessionWire interface {
	// Mirror is the output tap fed onto the session's kit scene — wired
	// onto the clone via Scene.SetMirror before it runs.
	Mirror() SceneMirror
	// Handler is the kit WS handler for this session (its own /lsdp.v1).
	Handler() http.Handler
	// Close detaches the kit subscribers and drops the session server.
	Close()
}

// SessionWireFactory builds a fresh isolated SessionWire per test session.
// nil in bespoke mode (ORION_LSDP_MODE unset) — TestSessionManager then
// attaches no mirror and the per-session LSDP route is never registered.
type SessionWireFactory interface {
	NewSessionWire(sceneID, sceneVersion string, bundle *compiler.RenderBundle) SessionWire
}

// TestSessionManager owns the set of live test sessions per ADR 002
// § 3 + ADR 004 § 9. Sessions are fully isolated from the live show
// — they operate on a private clone of the scene's graph and never
// reach the broadcast.
type TestSessionManager struct {
	registry *ComputeRegistry
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*testSession

	// wires builds the per-session preview LSDP endpoint. nil ⇒ bespoke
	// mode: sessions still run, but no kit mirror is attached and the
	// .lsdp route is absent (Connect/ConnectWire-less degradation).
	wires SessionWireFactory

	graceWindow time.Duration
}

type testSession struct {
	id       string
	scene    *Scene
	wire     SessionWire // per-session preview LSDP endpoint; nil in bespoke mode
	wsActive bool
	closeAt  time.Time
}

// ErrTestSessionExpired is returned for connect attempts on an id
// past its 5-minute grace window.
var ErrTestSessionExpired = errors.New("test session expired")

// NewTestSessionManager builds the manager. graceWindow is the
// reconnect grace per ADR 002 § 3 (default 5 min).
func NewTestSessionManager(registry *ComputeRegistry, logger *slog.Logger, graceWindow time.Duration) *TestSessionManager {
	if graceWindow <= 0 {
		graceWindow = 5 * time.Minute
	}
	return &TestSessionManager{
		registry:    registry,
		logger:      logger.With("component", "test_session"),
		sessions:    map[string]*testSession{},
		graceWindow: graceWindow,
	}
}

// SetSessionWires installs the per-session preview-LSDP factory. Called
// once at boot in dual/lsdp mode, before any session is opened. nil keeps
// bespoke mode (no kit mirror, no .lsdp route).
func (m *TestSessionManager) SetSessionWires(w SessionWireFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wires = w
}

// Open creates a fresh isolated clone of the scene's graph + bundle
// (the inputs are copies; the scene's loop runs in its own goroutine
// without touching the live show).
//
// progs, when non-empty, installs the scene's exec layer — the full set
// of the scene's blueprint programs (ADR 003 §3.1; multi-program lift,
// ADR 006 §3.3 / issue #105): test sessions are where exec runs before
// the phase-4 gate (R9), and each open fires every `on-start` (§3.1.3).
// The API handler passes nil until the compiler partition emits programs
// — inert until then.
func (m *TestSessionManager) Open(ctx context.Context, sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) (string, *Scene) {
	id := uuid.NewString()
	gcopy := *graph
	bcopy := *bundle
	scene := NewScene(sceneID, &gcopy, &bcopy, m.registry, m.logger.With("test_session", id))
	scene.InstallExec(progs...)

	// In dual/lsdp mode, pair the clone with its OWN isolated kit server
	// (option B): the clone is that server's sole, always-active scene, so
	// Solar subscribes live-mode against the session route and follows ONLY
	// this clone — never the global show's active scene. The mirror is
	// keyed by sessionID (unique per Open), so two sessions on the same
	// sceneID — and a session sharing a sceneID with the live show — never
	// collide. SetMirror seeds the kit scene with the clone's snapshot and
	// must run before Run starts.
	m.mu.Lock()
	wires := m.wires
	m.mu.Unlock()
	var wire SessionWire
	if wires != nil {
		wire = wires.NewSessionWire(sceneID, gcopy.SceneVersion, &bcopy)
		scene.SetMirror(wire.Mirror())
	}

	go scene.Run(ctx)
	scene.FireOnStart("system:test-session")

	sess := &testSession{
		id:       id,
		scene:    scene,
		wire:     wire,
		wsActive: false,
	}
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	return id, scene
}

// ConnectWire marks a session WS-active (grace reset) and returns its
// per-session preview-LSDP handler — the .lsdp-route analogue of Connect.
// Returns ErrTestSessionExpired if the session is unknown / past grace, or
// if the session has no kit wire (bespoke mode).
func (m *TestSessionManager) ConnectWire(id string) (http.Handler, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, ErrTestSessionExpired
	}
	if !sess.wsActive && !sess.closeAt.IsZero() && time.Now().After(sess.closeAt) {
		// Past grace — destroy.
		sess.scene.Stop()
		if sess.wire != nil {
			sess.wire.Close()
		}
		delete(m.sessions, id)
		return nil, ErrTestSessionExpired
	}
	if sess.wire == nil {
		return nil, ErrTestSessionExpired
	}
	sess.wsActive = true
	sess.closeAt = time.Time{}
	return sess.wire.Handler(), nil
}

// Connect marks a session WS-active and returns its scene. If the
// session expired (no reconnect within grace), returns ErrTestSessionExpired.
func (m *TestSessionManager) Connect(id string) (*Scene, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, ErrTestSessionExpired
	}
	if !sess.wsActive && !sess.closeAt.IsZero() && time.Now().After(sess.closeAt) {
		// Past grace — destroy.
		sess.scene.Stop()
		if sess.wire != nil {
			sess.wire.Close()
		}
		delete(m.sessions, id)
		return nil, ErrTestSessionExpired
	}
	sess.wsActive = true
	sess.closeAt = time.Time{}
	return sess.scene, nil
}

// Disconnect arms the grace window. The session stays alive in
// memory; another Connect() call within the window resumes it.
func (m *TestSessionManager) Disconnect(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return
	}
	sess.wsActive = false
	sess.closeAt = time.Now().Add(m.graceWindow)
}

// Sweep destroys sessions whose grace expired. Run periodically by
// the show's lifecycle.
func (m *TestSessionManager) Sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, sess := range m.sessions {
		if !sess.wsActive && !sess.closeAt.IsZero() && now.After(sess.closeAt) {
			sess.scene.Stop()
			if sess.wire != nil {
				sess.wire.Close()
			}
			delete(m.sessions, id)
		}
	}
}

// Close destroys all sessions (process shutdown).
func (m *TestSessionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, sess := range m.sessions {
		sess.scene.Stop()
		if sess.wire != nil {
			sess.wire.Close()
		}
		delete(m.sessions, id)
	}
}
