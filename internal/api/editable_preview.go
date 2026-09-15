package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/coder/websocket"
)

const maxEditablePreviewBody = 4 << 20
const maxEditablePreviewPatches = 256

type editablePreviewOpenRequest struct {
	SceneID      string          `json:"scene_id"`
	SceneVersion string          `json:"scene_version"`
	EditSeq      uint64          `json:"edit_seq"`
	LSMLBundle   json.RawMessage `json:"lsml_bundle"`
}

type editablePreviewPatchRequest struct {
	SceneID     string `json:"scene_id"`
	BaseEditSeq uint64 `json:"base_edit_seq"`
	EditSeq     uint64 `json:"edit_seq"`
	Patches     []struct {
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	} `json:"patches"`
}

type editablePreviewActivateRequest struct {
	SceneID string `json:"scene_id"`
	EditSeq uint64 `json:"edit_seq"`
}

func applyEditablePreviewPatch(deps PublicDeps, body editablePreviewPatchRequest) (int, string, error) {
	if body.SceneID == "" || len(body.Patches) == 0 || len(body.Patches) > maxEditablePreviewPatches {
		return http.StatusBadRequest, "INVALID_BODY", errors.New("invalid editable preview patch body")
	}
	patches := make([]runtime.EditablePatch, 0, len(body.Patches))
	for _, patch := range body.Patches {
		if !strings.HasPrefix(patch.Path, "__editable.") || len(patch.Value) == 0 || !json.Valid(patch.Value) {
			return http.StatusBadRequest, "INVALID_PATCH", errors.New("invalid editable preview patch")
		}
		patches = append(patches, runtime.EditablePatch{Path: patch.Path, Value: patch.Value})
	}
	if err := deps.Preview.ApplyEditablePatches(body.SceneID, body.BaseEditSeq, body.EditSeq, patches); err != nil {
		status, code := http.StatusConflict, "EDITABLE_PREVIEW_CONFLICT"
		if errors.Is(err, runtime.ErrPreviewEditPath) {
			status, code = http.StatusUnprocessableEntity, "EDITABLE_PATH_UNKNOWN"
		} else if errors.Is(err, runtime.ErrPreviewBusy) {
			status, code = http.StatusServiceUnavailable, "EDITABLE_PREVIEW_BUSY"
		}
		return status, code, err
	}
	return http.StatusAccepted, "", nil
}

func postEditablePreview(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil || deps.SceneIntent == nil || deps.SceneIntent.StaticBundleCompiler == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		raw, err := readBounded(r.Body, maxEditablePreviewBody)
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "BODY_TOO_LARGE"})
			return
		}
		var body editablePreviewOpenRequest
		if err != nil || json.Unmarshal(raw, &body) != nil || body.SceneID == "" || body.SceneVersion == "" || len(body.LSMLBundle) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}
		compiled, defaults, err := deps.SceneIntent.StaticBundleCompiler(body.LSMLBundle, body.SceneID, body.SceneVersion)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"code": "STATIC_BUNDLE_COMPILE_FAILED", "message": err.Error()})
			return
		}
		var bundle compiler.RenderBundle
		if json.Unmarshal(compiled, &bundle) != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "STATIC_BUNDLE_INVALID"})
			return
		}
		graph := &compiler.Graph{
			SceneID:      body.SceneID,
			SceneVersion: body.SceneVersion,
			Defaults:     defaults,
		}
		deps.Preview.ActivateEditable(body.SceneID, graph, &bundle, body.EditSeq)
		writeJSON(w, http.StatusOK, map[string]any{
			"scene_id":      body.SceneID,
			"scene_version": body.SceneVersion,
			"edit_seq":      body.EditSeq,
			"paths":         len(defaults),
		})
	})
}

// postEditablePreviewActivate is the compilation-free editable↔Blue switch
// path. The first open (or a structural edit) still uses POST
// /editable-preview with LSML; subsequent returns only reactivate the warm
// Preview clone and preserve Solar's persistent LSDP connection.
func postEditablePreviewActivate(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		var body editablePreviewActivateRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEditablePreviewBody)).Decode(&body) != nil || body.SceneID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}
		if err := deps.Preview.ReactivateEditable(body.SceneID, body.EditSeq); err != nil {
			code := "EDITABLE_PREVIEW_CACHE_MISS"
			if errors.Is(err, runtime.ErrPreviewEditSequence) {
				code = "EDITABLE_PREVIEW_CONFLICT"
			}
			writeJSON(w, http.StatusConflict, map[string]string{"code": code, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scene_id": body.SceneID, "edit_seq": body.EditSeq, "reused": true})
	})
}

func putEditablePreviewPatch(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		var body editablePreviewPatchRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEditablePreviewBody)).Decode(&body) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}
		status, code, err := applyEditablePreviewPatch(deps, body)
		if err != nil {
			writeJSON(w, status, map[string]string{"code": code, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"scene_id": body.SceneID, "edit_seq": body.EditSeq})
	})
}

// editablePreviewSocket is Prism's prewarmed hot-edit lane. Its random
// capability is separate from both the operator handshake and Solar's viewer
// token, and the handler exists only in embedded-local. Every accepted frame
// still enters PreviewSlot.ApplyEditablePatches, preserving sequence checks,
// bounded paths and the Preview/Program isolation invariant.
func editablePreviewSocket(deps PublicDeps) http.HandlerFunc {
	type observerSet struct {
		mu    sync.Mutex
		conns map[*websocket.Conn]struct{}
	}
	observers := &observerSet{conns: make(map[*websocket.Conn]struct{})}
	removeObserver := func(conn *websocket.Conn) {
		observers.mu.Lock()
		delete(observers.conns, conn)
		observers.mu.Unlock()
	}
	observerCount := func() int {
		observers.mu.Lock()
		defer observers.mu.Unlock()
		return len(observers.conns)
	}
	broadcastAccepted := func(body editablePreviewPatchRequest) {
		observers.mu.Lock()
		connections := make([]*websocket.Conn, 0, len(observers.conns))
		for conn := range observers.conns {
			connections = append(connections, conn)
		}
		observers.mu.Unlock()
		if len(connections) == 0 {
			return
		}
		payload, err := json.Marshal(map[string]any{
			"type": "accepted_patch", "scene_id": body.SceneID,
			"edit_seq": body.EditSeq, "patches": body.Patches,
		})
		if err != nil {
			return
		}
		for _, conn := range connections {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			err := conn.Write(ctx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				removeObserver(conn)
			}
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil || deps.Config.LocalEditorToken == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		provided := r.URL.Query().Get("local_editor_token")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(deps.Config.LocalEditorToken)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"code": "FORBIDDEN"})
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		conn.SetReadLimit(maxEditablePreviewBody)
		if r.URL.Query().Get("role") == "solar" {
			observers.mu.Lock()
			observers.conns[conn] = struct{}{}
			observers.mu.Unlock()
			defer removeObserver(conn)
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		}
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var body editablePreviewPatchRequest
			if json.Unmarshal(raw, &body) != nil {
				_ = writeEditableSocketJSON(r.Context(), conn, map[string]any{"type": "error", "code": "INVALID_BODY"})
				return
			}
			_, code, applyErr := applyEditablePreviewPatch(deps, body)
			if applyErr != nil {
				_ = writeEditableSocketJSON(r.Context(), conn, map[string]any{"type": "error", "code": code, "message": applyErr.Error()})
				return
			}
			broadcastAccepted(body)
			if writeEditableSocketJSON(r.Context(), conn, map[string]any{
				"type": "ack", "scene_id": body.SceneID, "edit_seq": body.EditSeq,
				"observer_count": observerCount(),
			}) != nil {
				return
			}
		}
	}
}

func writeEditableSocketJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}
