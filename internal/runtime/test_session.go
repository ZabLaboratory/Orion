package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// TestSessionManager owns the set of live test sessions per ADR 002
// § 3 + ADR 004 § 9. Sessions are fully isolated from the live show
// — they operate on a private clone of the scene's graph and never
// reach the broadcast.
type TestSessionManager struct {
	registry *ComputeRegistry
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*testSession

	graceWindow time.Duration
}

type testSession struct {
	id       string
	scene    *Scene
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

// Open creates a fresh isolated clone of the scene's graph + bundle
// (the inputs are copies; the scene's loop runs in its own goroutine
// without touching the live show).
func (m *TestSessionManager) Open(ctx context.Context, sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle) (string, *Scene) {
	id := uuid.NewString()
	gcopy := *graph
	bcopy := *bundle
	scene := NewScene(sceneID, &gcopy, &bcopy, m.registry, m.logger.With("test_session", id))
	go scene.Run(ctx)

	sess := &testSession{
		id:       id,
		scene:    scene,
		wsActive: false,
	}
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	return id, scene
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
		delete(m.sessions, id)
	}
}
