package api

// Local editable Preview is deliberately a small, no-Blue lane. Prism sends
// an authoring LSML bundle, Orion compiles it once into the Solar
// RenderBundle, installs it in the isolated PreviewSlot, and thereafter
// accepts only bounded __editable.* leaf updates. The same accepted update is
// mirrored to the persistent Preview LSDP wire and to Solar's optional
// sideband, so the pixels still come from the real render path.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

const (
	maxEditablePreviewBody = 16 << 20
	maxEditablePreviewWS   = 1 << 20
)

type editablePreviewDeps struct {
	Slot         *runtime.PreviewSlot
	EditorToken  string
	AssetBaseURL string
	Logger       *slog.Logger
}

type editablePreviewAPI struct {
	slot        *runtime.PreviewSlot
	editorToken string
	assetBase   string
	logger      *slog.Logger
	hub         *editablePreviewHub
}

type editablePreviewOpenRequest struct {
	SceneID      string          `json:"scene_id"`
	SceneVersion string          `json:"scene_version"`
	EditSeq      uint64          `json:"edit_seq"`
	LSMLBundle   json.RawMessage `json:"lsml_bundle"`
}

type editablePreviewActivateRequest struct {
	SceneID string `json:"scene_id"`
	EditSeq uint64 `json:"edit_seq"`
}

type editablePreviewAirRequest struct {
	SceneID string `json:"scene_id"`
	Owner   string `json:"owner"`
}

type editablePreviewPatch struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type editablePreviewPatchRequest struct {
	SceneID     string                 `json:"scene_id"`
	BaseEditSeq uint64                 `json:"base_edit_seq"`
	EditSeq     uint64                 `json:"edit_seq"`
	Patches     []editablePreviewPatch `json:"patches"`
}

type editablePreviewReceipt struct {
	SceneID      string `json:"scene_id"`
	EditSeq      uint64 `json:"edit_seq"`
	SceneVersion string `json:"scene_version,omitempty"`
	Paths        int    `json:"paths,omitempty"`
}

type editablePreviewAirReceipt struct {
	SceneID      string `json:"scene_id"`
	SceneVersion string `json:"scene_version"`
	Owner        string `json:"owner"`
}

type editablePreviewSocketFrame struct {
	SceneID     string                 `json:"scene_id"`
	BaseEditSeq uint64                 `json:"base_edit_seq"`
	EditSeq     uint64                 `json:"edit_seq"`
	Patches     []editablePreviewPatch `json:"patches"`
}

type editablePreviewPeer struct {
	conn *websocket.Conn
	role string
	mu   sync.Mutex
}

type editablePreviewHub struct {
	mu    sync.Mutex
	peers map[*editablePreviewPeer]struct{}
}

func newEditablePreviewAPI(deps editablePreviewDeps) *editablePreviewAPI {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &editablePreviewAPI{
		slot:        deps.Slot,
		editorToken: deps.EditorToken,
		assetBase:   strings.TrimRight(deps.AssetBaseURL, "/"),
		logger:      logger.With("component", "editable-preview"),
		hub:         &editablePreviewHub{peers: make(map[*editablePreviewPeer]struct{})},
	}
}

func (a *editablePreviewAPI) open(w http.ResponseWriter, r *http.Request) {
	raw, err := readBounded(r.Body, maxEditablePreviewBody)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "EDITABLE_PREVIEW_BODY_TOO_LARGE"})
		return
	}
	var req editablePreviewOpenRequest
	if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.SceneID) == "" || strings.TrimSpace(req.SceneVersion) == "" || req.EditSeq != 0 || len(req.LSMLBundle) == 0 || !json.Valid(req.LSMLBundle) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "EDITABLE_PREVIEW_REQUEST_INVALID"})
		return
	}
	compiled, defaults, err := compiler.CompileStaticLSML(req.LSMLBundle, req.SceneID, req.SceneVersion, a.assetBase)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "EDITABLE_PREVIEW_COMPILE_FAILED"})
		return
	}
	var bundle compiler.RenderBundle
	if err := json.Unmarshal(compiled, &bundle); err != nil || bundle.Root.Kind == "" || bundle.SceneVersion != req.SceneVersion {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "EDITABLE_PREVIEW_BUNDLE_INVALID"})
		return
	}
	if err := a.slot.ActivateStatic(req.SceneID, &bundle); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "EDITABLE_PREVIEW_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, editablePreviewReceipt{
		SceneID:      req.SceneID,
		EditSeq:      0,
		SceneVersion: bundle.SceneVersion,
		Paths:        len(defaults),
	})
}

func (a *editablePreviewAPI) activate(w http.ResponseWriter, r *http.Request) {
	var req editablePreviewActivateRequest
	if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.SceneID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "EDITABLE_PREVIEW_REQUEST_INVALID"})
		return
	}
	version, err := a.slot.ActivateEditable(req.SceneID, req.EditSeq)
	if err != nil {
		writeEditablePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scene_id":      req.SceneID,
		"edit_seq":      req.EditSeq,
		"scene_version": version,
		"reused":        true,
	})
}

func (a *editablePreviewAPI) air(w http.ResponseWriter, r *http.Request) {
	var req editablePreviewAirRequest
	if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.SceneID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "EDITABLE_PREVIEW_REQUEST_INVALID"})
		return
	}
	version, _, ok := a.slot.EditableState(req.SceneID)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "EDITABLE_PREVIEW_NOT_ACTIVE"})
		return
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" {
		owner = "on-air"
	}
	writeJSON(w, http.StatusOK, editablePreviewAirReceipt{SceneID: req.SceneID, SceneVersion: version, Owner: owner})
}

func (a *editablePreviewAPI) patch(w http.ResponseWriter, r *http.Request) {
	var req editablePreviewPatchRequest
	if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.SceneID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "EDITABLE_PREVIEW_REQUEST_INVALID"})
		return
	}
	version, observers, err := a.apply(req)
	if err != nil {
		writeEditablePreviewError(w, err)
		return
	}
	a.hub.broadcastAccepted(req.SceneID, req.EditSeq, req.Patches, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"scene_id":       req.SceneID,
		"edit_seq":       req.EditSeq,
		"scene_version":  version,
		"paths":          len(req.Patches),
		"observer_count": observers,
	})
}

func (a *editablePreviewAPI) apply(req editablePreviewPatchRequest) (string, int, error) {
	patches := make([]runtime.EditablePatch, 0, len(req.Patches))
	for _, patch := range req.Patches {
		patches = append(patches, runtime.EditablePatch{Path: patch.Path, Value: patch.Value})
	}
	return a.slot.PatchEditable(req.SceneID, req.BaseEditSeq, req.EditSeq, patches)
}

func (a *editablePreviewAPI) websocket(w http.ResponseWriter, r *http.Request) {
	provided := r.URL.Query().Get("local_editor_token")
	if a.editorToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(a.editorToken)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "EDITABLE_PREVIEW_TOKEN_REQUIRED"})
		return
	}
	role := r.URL.Query().Get("role")
	if role != "solar" {
		role = "editor"
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		a.logger.Warn("editable preview websocket accept failed", "err", err)
		return
	}
	peer := &editablePreviewPeer{conn: conn, role: role}
	a.hub.add(peer)
	defer func() {
		a.hub.remove(peer)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	if role == "solar" {
		a.readSolar(r.Context(), conn)
		return
	}
	a.readEditor(r.Context(), peer)
}

func (a *editablePreviewAPI) readSolar(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func (a *editablePreviewAPI) readEditor(ctx context.Context, peer *editablePreviewPeer) {
	for {
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		kind, raw, err := peer.conn.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if kind != websocket.MessageText || len(raw) > maxEditablePreviewWS {
			a.sendError(peer, "EDITABLE_PREVIEW_FRAME_INVALID")
			return
		}
		var req editablePreviewSocketFrame
		if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.SceneID) == "" {
			a.sendError(peer, "EDITABLE_PREVIEW_FRAME_INVALID")
			return
		}
		converted := editablePreviewPatchRequest{SceneID: req.SceneID, BaseEditSeq: req.BaseEditSeq, EditSeq: req.EditSeq, Patches: req.Patches}
		version, observers, err := a.apply(converted)
		if err != nil {
			a.sendError(peer, editablePreviewErrorCode(err))
			return
		}
		a.hub.broadcastAccepted(req.SceneID, req.EditSeq, req.Patches, peer)
		a.send(peer, map[string]any{
			"type":           "ack",
			"scene_id":       req.SceneID,
			"edit_seq":       req.EditSeq,
			"scene_version":  version,
			"paths":          len(req.Patches),
			"observer_count": observers,
		})
	}
}

func (a *editablePreviewAPI) send(peer *editablePreviewPeer, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = peer.conn.Write(ctx, websocket.MessageText, raw)
}

func (a *editablePreviewAPI) sendError(peer *editablePreviewPeer, code string) {
	a.send(peer, map[string]any{"type": "error", "code": code})
}

func (h *editablePreviewHub) add(peer *editablePreviewPeer) {
	h.mu.Lock()
	h.peers[peer] = struct{}{}
	h.mu.Unlock()
}

func (h *editablePreviewHub) remove(peer *editablePreviewPeer) {
	h.mu.Lock()
	delete(h.peers, peer)
	h.mu.Unlock()
}

func (h *editablePreviewHub) broadcastAccepted(sceneID string, editSeq uint64, patches []editablePreviewPatch, exclude *editablePreviewPeer) {
	body := map[string]any{"type": "accepted_patch", "scene_id": sceneID, "edit_seq": editSeq, "patches": patches}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	h.mu.Lock()
	peers := make([]*editablePreviewPeer, 0, len(h.peers))
	for peer := range h.peers {
		if peer != exclude {
			peers = append(peers, peer)
		}
	}
	h.mu.Unlock()
	for _, peer := range peers {
		peer.mu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := peer.conn.Write(ctx, websocket.MessageText, raw)
		cancel()
		peer.mu.Unlock()
		if err != nil {
			_ = peer.conn.Close(websocket.StatusGoingAway, "sideband write failed")
		}
	}
}

func decodeJSONBody(r *http.Request, target any) error {
	raw, err := readBounded(r.Body, maxEditablePreviewBody)
	if err != nil {
		return err
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("invalid json")
	}
	return json.Unmarshal(raw, target)
}

func editablePreviewErrorCode(err error) string {
	switch {
	case errors.Is(err, runtime.ErrEditablePreviewSequence):
		return "EDITABLE_PREVIEW_SEQUENCE_GAP"
	case errors.Is(err, runtime.ErrEditablePreviewPath):
		return "EDITABLE_PREVIEW_PATH_INVALID"
	case errors.Is(err, runtime.ErrEditablePreviewBusy):
		return "EDITABLE_PREVIEW_BUSY"
	case errors.Is(err, runtime.ErrEditablePreviewUnavailable):
		return "EDITABLE_PREVIEW_NOT_ACTIVE"
	default:
		return "EDITABLE_PREVIEW_FAILED"
	}
}

func writeEditablePreviewError(w http.ResponseWriter, err error) {
	code := editablePreviewErrorCode(err)
	status := http.StatusConflict
	if strings.HasSuffix(code, "PATH_INVALID") || strings.HasSuffix(code, "REQUEST_INVALID") {
		status = http.StatusBadRequest
	}
	if code == "EDITABLE_PREVIEW_BUSY" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]string{"code": code})
}
