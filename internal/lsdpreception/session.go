package lsdpreception

import (
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/lsdp"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

type sessionWire struct{ mirror *Mirror }

func (h *Hub) NewSessionWire(sceneID, version string, bundle *compiler.RenderBundle) runtime.SessionWire {
	id, err := native.NewID()
	if err != nil {
		h.producer.fail(err)
		return &sessionWire{}
	}
	return h.NewSessionWireFor(id, sceneID, version, bundle)
}
func (h *Hub) NewSessionWireFor(sessionID, sceneID, version string, bundle *compiler.RenderBundle) runtime.SessionWire {
	h.mu.Lock()
	defer h.mu.Unlock()
	if bundle == nil {
		h.producer.fail(http.ErrMissingFile)
		return &sessionWire{}
	}
	document, err := sourceDocument(sceneID, version, bundle.SourceLSML)
	if err != nil {
		h.producer.fail(err)
		return &sessionWire{}
	}
	m := &Mirror{hub: h, key: sessionID, sceneID: sceneID, version: version, document: document, surface: lsdp.RenderSurface(sceneID, bundle.SourceLSML, h.logger), session: true}
	h.producer.Apply("solar/sessions", []map[string]any{{"op": "add", "path": "/" + pointer(sessionID), "value": document}})
	return &sessionWire{mirror: m}
}
func (s *sessionWire) Mirror() runtime.SceneMirror {
	if s.mirror == nil {
		return nil
	}
	return s.mirror
}

// No retired WS endpoint: the authenticated session API returns the native selector.
func (s *sessionWire) Handler() http.Handler { return http.NotFoundHandler() }
func (s *sessionWire) Close() {
	if s.mirror == nil {
		return
	}
	h := s.mirror.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.mirror.document == nil {
		return
	}
	s.mirror.document = nil
	h.producer.Apply("solar/sessions", []map[string]any{{"op": "remove", "path": "/" + pointer(s.mirror.key)}})
}
