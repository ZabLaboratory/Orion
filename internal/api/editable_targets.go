package api

import (
	"context"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"net/http"
	"time"
)

func acquireEditableCommit(ctx context.Context, deps PublicDeps, target string) (func(), error) {
	if deps.SceneIntent == nil || deps.SceneIntent.Host == nil {
		return func() {}, nil
	}
	slot := bluehost.SlotPreview
	if target == "program" {
		slot = bluehost.SlotOnAir
	}
	return deps.SceneIntent.Host.AcquireSceneCommit(ctx, slot)
}

func editableTarget(target string) (string, bool) {
	if target == "" {
		target = "preview"
	}
	return target, target == "preview" || target == "program"
}

func flushEditableTarget(ctx context.Context, deps PublicDeps, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := deps.Preview.FlushEditableTarget(ctx, target); err != nil {
		return err
	}
	if deps.NativeLSDPFlush != nil {
		return deps.NativeLSDPFlush(ctx)
	}
	return nil
}

func getEditableTargetState(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		target, valid := editableTarget(r.URL.Query().Get("target"))
		if !valid {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_TARGET"})
			return
		}
		head, editable := deps.Preview.EditableTargetHead(target)
		id := r.URL.Query().Get("scene_id")
		if !editable || id == "" || id != head.SceneID {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "EDITABLE_PREVIEW_CONFLICT"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"target": target, "scene_id": head.SceneID, "scene_version": head.SceneVersion, "edit_seq": head.EditSeq, "writer_id": head.WriterID})
	})
}
