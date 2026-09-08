package lsdp

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// GenerationWires owns one LSDP server per immutable (scene, version)
// generation. Unlike the role-based preview and antenna wires, a generation
// URL never changes identity when Pulsar swaps its physical A/B lane roles.
type GenerationWires struct {
	mu     sync.Mutex
	logger *slog.Logger
	auth   auth.AuthSource
	wires  map[string]*generationWire
	clock  uint64
}

type generationWire struct {
	wire    *Wire
	owner   string
	touched uint64
}

// Two physical lanes are active at once. A small amount of headroom absorbs
// rapid operator navigation while keeping abandoned scene versions bounded.
const maxGenerationWires = 8

type ownedGenerationMirror struct {
	registry *GenerationWires
	entry    *generationWire
	owner    string
	inner    runtime.SceneMirror
}

func NewGenerationWires(logger *slog.Logger, source auth.AuthSource) *GenerationWires {
	return &GenerationWires{logger: logger, auth: source, wires: make(map[string]*generationWire)}
}

func generationKey(sceneID, version string) string { return sceneID + "\x00" + version }

// MirrorForLSML assigns the generation's single writer. A take changes that
// writer from preview to on-air while keeping the browser's URL and server
// stable; late preview ticks are then ignored instead of corrupting Program.
func (g *GenerationWires) MirrorForLSML(sceneID, version, owner string, bundle []byte) runtime.SceneMirror {
	key := generationKey(sceneID, version)
	g.mu.Lock()
	g.clock++
	entry := g.wires[key]
	if entry == nil {
		wire, err := NewWire(g.logger, g.auth)
		if err != nil {
			g.mu.Unlock()
			g.logger.Error("generation LSDP wire creation failed", "scene_id", sceneID, "error", err)
			return nil
		}
		entry = &generationWire{wire: wire}
		g.wires[key] = entry
	}
	entry.owner = owner
	entry.touched = g.clock
	inner := entry.wire.MirrorForLSML(sceneID, version, bundle)
	g.evictOldestLocked(key)
	g.mu.Unlock()
	return &ownedGenerationMirror{registry: g, entry: entry, owner: owner, inner: inner}
}

func (g *GenerationWires) evictOldestLocked(keep string) {
	for len(g.wires) > maxGenerationWires {
		var oldestKey string
		var oldest uint64
		for key, entry := range g.wires {
			if key == keep || (oldestKey != "" && entry.touched >= oldest) {
				continue
			}
			oldestKey, oldest = key, entry.touched
		}
		if oldestKey == "" {
			return
		}
		delete(g.wires, oldestKey)
	}
}

func (m *ownedGenerationMirror) Forward(message runtime.SubscriberMsg) {
	m.registry.mu.Lock()
	active := m.entry.owner == m.owner
	m.registry.mu.Unlock()
	if active {
		m.inner.Forward(message)
	}
}

func (g *GenerationWires) SetActive(sceneID, version string) {
	g.mu.Lock()
	entry := g.wires[generationKey(sceneID, version)]
	g.mu.Unlock()
	if entry != nil {
		entry.wire.SetActive(sceneID)
	}
}

// Handler resolves an exact immutable generation and delegates to that wire's
// own authenticated LSDP server. Missing identity never falls back to a role.
func (g *GenerationWires) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sceneID, version := r.URL.Query().Get("scene_id"), r.URL.Query().Get("v")
		if sceneID == "" || version == "" {
			http.NotFound(w, r)
			return
		}
		g.mu.Lock()
		entry := g.wires[generationKey(sceneID, version)]
		g.mu.Unlock()
		if entry == nil {
			http.NotFound(w, r)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/lsdp.v1"
		entry.wire.Handler().ServeHTTP(w, clone)
	})
}
