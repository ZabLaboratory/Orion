package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// Show is the singleton process-wide live show. It owns the roster
// of pre-built scenes (every active scene with a non-null
// latest_pushed_version per ADR 004 § 4.4) and the active-scene
// pointer. Live-show subscribers attach to the *show* (not to a
// specific scene); on scene_changed, the show migrates them to the
// destination scene's subscriber list so the WS connection survives.
type Show struct {
	registry *ComputeRegistry
	logger   *slog.Logger

	mu       sync.RWMutex
	scenes   map[string]*Scene
	active   string
	liveSubs []*Subscription

	// mirrors is the optional LSDP/1.1 wire (ADR 007 §C.3b). nil in
	// bespoke mode — the entire kit path is then dead weight that never
	// runs (no-op deploy). In dual/lsdp mode the Show asks it for a
	// per-scene SceneMirror at Load and tells it which scene is active
	// at SetActive, so the kit serves the same source as the bespoke
	// wire.
	mirrors MirrorRegistry

	ctx    context.Context
	cancel context.CancelFunc
}

// MirrorRegistry is the Show-side handle on the LSDP/1.1 wire
// (ADR 007 §C.3b). The lsdp package implements it over a
// lumencast-go server.Server. nil = bespoke mode.
type MirrorRegistry interface {
	// MirrorFor returns the SceneMirror for the given scene id,
	// registering a paired kit scene if needed. sceneVersion is the
	// LSML/graph content address echoed on snapshot/scene_changed
	// frames.
	MirrorFor(sceneID, sceneVersion string) SceneMirror
	// SetActive tells the wire which scene the live endpoint serves,
	// mirroring Show.SetActive so the kit migrates its live subscribers.
	SetActive(sceneID string)
	// Drop removes a scene's paired kit scene (on Unload).
	Drop(sceneID string)
}

// SetMirrors installs the LSDP/1.1 wire. Called once at boot in
// dual/lsdp mode, before any scene is loaded. nil keeps bespoke mode.
func (sh *Show) SetMirrors(m MirrorRegistry) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.mirrors = m
}

// ErrSceneNotFound is the API-facing miss.
var ErrSceneNotFound = errors.New("show: scene not found")

// ErrSceneNotPushed is the API-facing reject for activating a scene
// that has never been pushed.
var ErrSceneNotPushed = errors.New("show: scene not pushed")

// NewShow constructs an empty show. Scenes are added via Load.
func NewShow(registry *ComputeRegistry, logger *slog.Logger) *Show {
	ctx, cancel := context.WithCancel(context.Background())
	return &Show{
		registry: registry,
		logger:   logger.With("component", "show"),
		scenes:   map[string]*Scene{},
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Stop cancels every scene goroutine and waits for them to drain.
func (sh *Show) Stop() {
	sh.cancel()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, s := range sh.scenes {
		s.Stop()
	}
}

// Load adds a scene to the roster, starting its goroutine. Idempotent:
// loading a scene already in the roster swaps the artefacts atomically
// (used by the push handler when a re-push lands on an already-live
// scene — ADR 004 § 7).
func (sh *Show) Load(id string, graph *compiler.Graph, bundle *compiler.RenderBundle) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if existing, ok := sh.scenes[id]; ok {
		existing.Stop()
	}
	scene := NewScene(id, graph, bundle, sh.registry, sh.logger)
	// ADR 007 §C.3b: in dual/lsdp mode, pair the scene with a kit
	// scene and tap its output port. The mirror is seeded with the
	// freshly-seeded snapshot inside SetMirror before Run starts.
	if sh.mirrors != nil {
		scene.SetMirror(sh.mirrors.MirrorFor(id, graph.SceneVersion))
	}
	sh.scenes[id] = scene
	go scene.Run(sh.ctx)
}

// Unload stops a scene and drops it from the roster.
func (sh *Show) Unload(id string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if existing, ok := sh.scenes[id]; ok {
		existing.Stop()
		delete(sh.scenes, id)
		if sh.mirrors != nil {
			sh.mirrors.Drop(id)
		}
	}
	if sh.active == id {
		sh.active = ""
	}
}

// Get returns the scene by id (or nil + ErrSceneNotFound).
func (sh *Show) Get(id string) (*Scene, error) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	s, ok := sh.scenes[id]
	if !ok {
		return nil, ErrSceneNotFound
	}
	return s, nil
}

// IDs returns the loaded scene ids — used by /show.
func (sh *Show) IDs() []string {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	out := make([]string, 0, len(sh.scenes))
	for id := range sh.scenes {
		out = append(out, id)
	}
	return out
}

// Active returns the currently active scene (or nil if none set yet).
func (sh *Show) Active() *Scene {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.active == "" {
		return nil
	}
	return sh.scenes[sh.active]
}

// SetActive flips the active-scene pointer, migrates every live-show
// subscriber from the previous scene to the new one, and emits
// scene_changed + fresh snapshot on the destination. ADR 004 § 4.4 +
// ADR 002 § 11.
func (sh *Show) SetActive(id string, transition json.RawMessage) error {
	sh.mu.Lock()
	dest, ok := sh.scenes[id]
	if !ok {
		sh.mu.Unlock()
		return ErrSceneNotFound
	}
	from := sh.active
	sh.active = id
	prev, hadPrev := sh.scenes[from]
	migrating := append([]*Subscription{}, sh.liveSubs...)
	mirrors := sh.mirrors
	sh.mu.Unlock()

	// ADR 007 §C.3b: switch the kit's active scene too, so LSDP/1.1
	// live subscribers get scene_changed + a fresh snapshot off the
	// kit's own migration path (server.SetActive). Done first so the
	// kit observes the switch before the next emit fans out on the
	// destination.
	if mirrors != nil {
		mirrors.SetActive(id)
	}

	// Step 1: detach migrating subs from the previous scene (if any).
	if hadPrev {
		for _, sub := range migrating {
			prev.Detach(sub)
		}
	}
	// Step 2: attach to destination, sending scene_changed first then
	// the fresh snapshot per ADR 002 § 5/7.
	for _, sub := range migrating {
		snap := dest.AttachExisting(sub)
		select {
		case sub.Out <- &protocol.SceneChanged{
			FromSceneID: from,
			ToSceneID:   id,
			Transition:  transition,
		}:
		default:
		}
		// Reset the destination scene's sequence so the snapshot
		// reseeds it (ADR 002 § 7).
		select {
		case sub.Out <- snap:
		default:
		}
	}
	dest.state.ResetSequence()
	return nil
}

// SubscribeLive attaches a new live-show subscription to the
// currently active scene. Returns the subscription and the initial
// snapshot.
func (sh *Show) SubscribeLive(buf int) (*Subscription, *protocol.Snapshot, error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.active == "" {
		return nil, nil, ErrSceneNotFound
	}
	scene := sh.scenes[sh.active]
	if scene == nil {
		return nil, nil, ErrSceneNotFound
	}
	sub, snap := scene.Subscribe(buf)
	sh.liveSubs = append(sh.liveSubs, sub)
	return sub, snap, nil
}

// UnsubscribeLive detaches a live-show subscription. The caller is
// expected to call this from the WS handler's defer.
func (sh *Show) UnsubscribeLive(sub *Subscription) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for i, s := range sh.liveSubs {
		if s == sub {
			sh.liveSubs = append(sh.liveSubs[:i], sh.liveSubs[i+1:]...)
			break
		}
	}
}
