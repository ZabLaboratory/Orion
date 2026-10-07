package lsdpreception

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/lsdp"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Hub owns logical lanes/generations, never a native process or WS server.
type Hub struct {
	mu          sync.Mutex
	producer    *Producer
	logger      *slog.Logger
	generations map[string]*Mirror
	clock       uint64
	lanes       map[string]*Lane
}
type Lane struct {
	hub           *Hub
	target        string
	active        *Mirror
	scenes        map[string]*Mirror
	reserved      map[string]any
	viewer        *lsdp.ViewerCredentials
	transitioning bool
}
type Mirror struct {
	lane                         *Lane
	hub                          *Hub
	key, owner, sceneID, version string
	document                     map[string]any
	surface                      func(string) bool
	touched                      uint64
	session                      bool
}

func GenerationKey(sceneID, version string) string {
	sum := sha256.Sum256([]byte(sceneID + "\x00" + version))
	return hex.EncodeToString(sum[:])
}
func NewHub(p *Producer, logger *slog.Logger) *Hub {
	return &Hub{producer: p, logger: logger, generations: map[string]*Mirror{}, lanes: map[string]*Lane{}}
}
func (h *Hub) Lane(name string) *Lane {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.lanes[name]; l != nil {
		return l
	}
	l := &Lane{hub: h, target: "solar/" + name, scenes: map[string]*Mirror{}, reserved: map[string]any{}}
	h.lanes[name] = l
	return l
}
func sourceDocument(sceneID, version string, raw []byte) (map[string]any, error) {
	var document map[string]any
	if len(raw) == 0 {
		return nil, errors.New("NATIVE_LSML_SOURCE_REQUIRED")
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	_, layoutObject := document["layout"].(map[string]any)
	if sceneID == "" || version == "" || document["scene_id"] != sceneID || !layoutObject {
		return nil, errors.New("NATIVE_LSML_IDENTITY_INVALID")
	}
	// The native document must remain fetchable by Canvas's LSML address.
	// `version` identifies the admitted artifact set, not this source address.
	sourceVersion, ok := document["scene_version"].(string)
	if !ok || sourceVersion == "" {
		return nil, errors.New("NATIVE_LSML_IDENTITY_INVALID")
	}
	document["x-orion-artifact-set"] = version
	if document["defaults"] == nil {
		document["defaults"] = map[string]any{}
	}
	if _, ok := document["defaults"].(map[string]any); !ok {
		return nil, errors.New("NATIVE_LSML_DEFAULTS_INVALID")
	}
	return document, nil
}
func (l *Lane) MirrorForLSML(sceneID, version string, raw []byte) runtime.SceneMirror {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	document, err := sourceDocument(sceneID, version, raw)
	if err != nil {
		l.hub.producer.fail(err)
		return nil
	}
	m := &Mirror{lane: l, hub: l.hub, sceneID: sceneID, version: version, document: document, surface: lsdp.RenderSurface(sceneID, raw, l.hub.logger)}
	l.scenes[sceneID] = m
	return m
}
func (l *Lane) MirrorFor(sceneID, version string, bundle *compiler.RenderBundle) runtime.SceneMirror {
	if bundle == nil {
		l.hub.producer.fail(errors.New("NATIVE_LSML_SOURCE_REQUIRED"))
		return nil
	}
	return l.MirrorForLSML(sceneID, version, bundle.SourceLSML)
}
func (l *Lane) SetActive(sceneID string) {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	m := l.scenes[sceneID]
	if m == nil {
		return
	}
	l.active = m
	defaults := m.document["defaults"].(map[string]any)
	for key, value := range l.reserved {
		defaults[key] = value
	}
	l.hub.producer.Replace(l.target, m.document)
}
func (l *Lane) Drop(sceneID string) {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	m := l.scenes[sceneID]
	delete(l.scenes, sceneID)
	if l.active == m && m != nil {
		l.active = nil
		l.hub.producer.Replace(l.target, nil)
	}
}
func (l *Lane) EmitRoster(entries []runtime.RosterEntry) {
	l.hub.producer.Apply("orion/state", []map[string]any{{"op": "add", "path": "/roster_" + pointer(l.target), "value": entries}})
}
func (h *Hub) MirrorForLSML(sceneID, version, owner string, raw []byte) runtime.SceneMirror {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := GenerationKey(sceneID, version)
	document, err := sourceDocument(sceneID, version, raw)
	if err != nil {
		h.producer.fail(err)
		return nil
	}
	h.clock++
	m := &Mirror{hub: h, key: key, owner: owner, sceneID: sceneID, version: version, document: document, surface: lsdp.RenderSurface(sceneID, raw, h.logger), touched: h.clock}
	for _, lane := range h.lanes {
		if ownerMatchesLane(owner, lane.target) {
			for key, value := range lane.reserved {
				document["defaults"].(map[string]any)[key] = value
			}
		}
	}
	// A new writer lease invalidates the previous mirror, including the same owner.
	h.generations[key] = m
	// Bounded retention: never evict either active Program/Preview generation.
	for len(h.generations) > 8 {
		oldest := ""
		clock := ^uint64(0)
		for k, candidate := range h.generations {
			pinned := k == key
			for _, lane := range h.lanes {
				if lane.active != nil && GenerationKey(lane.active.sceneID, lane.active.version) == k {
					pinned = true
				}
			}
			if !pinned && candidate.touched < clock {
				oldest = k
				clock = candidate.touched
			}
		}
		if oldest == "" {
			break
		}
		delete(h.generations, oldest)
		h.producer.Apply("solar/generations", []map[string]any{{"op": "remove", "path": "/" + oldest}})
	}
	h.producer.Apply("solar/generations", []map[string]any{{"op": "add", "path": "/" + key, "value": document}})
	return m
}
func (h *Hub) SetActive(sceneID, version string) {} // generation identity never retargets

func (m *Mirror) Forward(message runtime.SubscriberMsg) {
	h := m.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if m.document == nil || (m.lane != nil && m.lane.transitioning) {
		return
	}
	if m.lane != nil && m.lane.scenes[m.sceneID] != m {
		return
	}
	if m.lane == nil && !m.session && h.generations[m.key] != m {
		return
	}
	defaults := m.document["defaults"].(map[string]any)
	operations := []map[string]any{}
	appendValue := func(path string, raw json.RawMessage) {
		if !m.surface(path) {
			return
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		defaults[path] = value
		operations = append(operations, map[string]any{"op": "add", "path": "/defaults/" + pointer(path), "value": value})
	}
	switch v := message.(type) {
	case *protocol.Snapshot:
		for key, value := range v.State {
			appendValue(key, value)
		}
	case *protocol.Delta:
		for _, patch := range v.Patches {
			appendValue(patch.Path, patch.Value)
		}
		if len(operations) > 0 && v.RuntimeInstanceID != "" {
			metadata := map[string]any{"schema_version": v.SchemaVersion, "scene_digest": v.SceneDigest, "runtime_instance_id": v.RuntimeInstanceID, "target": v.Target, "render_revision": v.RenderRevision, "correlation_id": v.CorrelationID, "cause": v.Cause}
			m.document["x-orion"] = metadata
			operations = append(operations, map[string]any{"op": "add", "path": "/x-orion", "value": metadata})
		}
	default:
		return
	}
	if m.lane != nil {
		if m.lane.active == m {
			h.producer.Apply(m.lane.target, operations)
		}
	} else {
		target := "solar/generations"
		if m.session {
			target = "solar/sessions"
		}
		scoped := make([]map[string]any, 0, len(operations))
		for _, operation := range operations {
			copy := map[string]any{}
			for k, v := range operation {
				copy[k] = v
			}
			copy["path"] = "/" + pointer(m.key) + operation["path"].(string)
			scoped = append(scoped, copy)
		}
		h.producer.Apply(target, scoped)
	}
}
func (l *Lane) EmitSlotAssignment(ref, label string) {
	if ref != "" {
		l.setReserved("__cam.slots."+ref, label)
	}
}
func (l *Lane) EmitSlotCleared(ref string) {
	if ref != "" {
		l.setReserved("__cam.slots."+ref, nil)
	}
}
func (l *Lane) setReserved(key string, value any) {
	h := l.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	l.reserved[key] = value
	if l.active != nil && !l.transitioning {
		l.active.document["defaults"].(map[string]any)[key] = value
		h.producer.Apply(l.target, []map[string]any{{"op": "add", "path": "/defaults/" + pointer(key), "value": value}})
	}
	for k, m := range h.generations {
		if ownerMatchesLane(m.owner, l.target) {
			m.document["defaults"].(map[string]any)[key] = value
			h.producer.Apply("solar/generations", []map[string]any{{"op": "add", "path": "/" + k + "/defaults/" + pointer(key), "value": value}})
		}
	}
	if l.viewer != nil && key != "__cam.viewer" {
		peers := []string{}
		for k, v := range l.reserved {
			if strings.HasPrefix(k, "__cam.slots.") {
				if label, ok := v.(string); ok && label != "" {
					peers = append(peers, label)
				}
			}
		}
		l.viewer.SetPeers(peers)
	}
}
func (l *Lane) EmitViewerPayload(js string) { l.setReserved("__cam.viewer", js) }
func (l *Lane) EnableViewerCreds(ctx context.Context, fetch lsdp.CredsFetcher, refresh time.Duration) {
	l.viewer = lsdp.NewViewerCredentials(ctx, fetch, refresh, l.hub.logger, l)
}
func ownerMatchesLane(owner, target string) bool {
	// Pulsar's physical Program lanes use explicit owner IDs. Preview is the
	// one reserved owner that must never receive Program's camera authority.
	return (target == "solar/preview" && owner == "preview") ||
		(target == "solar/program" && owner != "preview")
}
func (l *Lane) EmitOverlayApp(id string, running, onAir *bool) {
	if id == "" {
		return
	}
	ops := []map[string]any{}
	if running != nil {
		ops = append(ops, map[string]any{"op": "add", "path": "/overlay_running_" + pointer(id), "value": *running})
	}
	if onAir != nil {
		ops = append(ops, map[string]any{"op": "add", "path": "/overlay_on_air_" + pointer(id), "value": *onAir})
	}
	l.hub.producer.Apply("orion/state", ops)
}

// BeginTransition freezes scene projection while preserving the actual old mirror.
// The control coordinator owns the native presentation during this bounded window.
func (l *Lane) BeginTransition() func(bool) {
	h := l.hub
	h.mu.Lock()
	previous := l.active
	previousScenes := map[string]*Mirror{}
	for key, value := range l.scenes {
		previousScenes[key] = value
	}
	l.transitioning = true
	h.mu.Unlock()
	return func(restore bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		l.transitioning = false
		if restore {
			l.active = previous
			l.scenes = previousScenes
		}
		if l.active != nil {
			for key, value := range l.reserved {
				l.active.document["defaults"].(map[string]any)[key] = value
			}
			h.producer.Replace(l.target, l.active.document)
		} else if restore {
			h.producer.Replace(l.target, nil)
		}
	}
}

func (l *Lane) PrepareSource(sceneID, version string, raw []byte) (map[string]any, error) {
	document, err := sourceDocument(sceneID, version, raw)
	if err != nil {
		return nil, err
	}
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	defaults := document["defaults"].(map[string]any)
	for key, value := range l.reserved {
		defaults[key] = value
	}
	return document, nil
}
