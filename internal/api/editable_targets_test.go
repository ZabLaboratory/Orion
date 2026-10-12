package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/coder/websocket"
)

func TestEditableSocketContinuesSameWriterOnProgramAfterTake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	p := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), &editableAPIWire{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetEditableProgramWire(&editableAPIWire{})
	defer p.Close()
	host := bluehost.NewHost()
	intent := &SceneIntentDeps{Host: host, StaticRenderBundleCompiler: func(_ []byte, id, version string) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
		return &compiler.RenderBundle{SceneVersion: version}, map[string]json.RawMessage{"__editable.x": json.RawMessage(`10`)}, nil
	}}
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: slog.Default(), Config: config.Config{Profile: config.ProfileEmbeddedLocal, LocalEditorToken: "editor-capability"}, Preview: p, SceneIntent: intent})
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": "a", "scene_version": "v1", "edit_seq": 4, "lsml_bundle": map[string]any{"lsml": "1.1"}}, "operator")
	if opened.Code != http.StatusOK {
		t.Fatal(opened.Code, opened.Body.String())
	}
	head, _ := p.EditableTargetHead("preview")
	actor := p.Current()
	server := httptest.NewServer(mux)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/show/editable-preview.ws?local_editor_token=editor-capability", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	send := func(target, writer string, base, seq uint64) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"target": target, "writer_id": writer, "scene_id": "a", "base_edit_seq": base, "edit_seq": seq, "patches": []map[string]any{{"path": "__editable.x", "value": seq}}})
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, reply, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ack map[string]any
		if err = json.Unmarshal(reply, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}
	if ack := send("preview", head.WriterID, 4, 5); ack["type"] != "ack" {
		t.Fatal(ack)
	}
	if err = host.TakeStatic("old-program", "old-v"); err != nil {
		t.Fatal(err)
	}
	finish, err := p.StageRoleSwap("a")
	if err != nil {
		t.Fatal(err)
	}
	if err = host.SwapAuthority(); err != nil {
		finish(false)
		t.Fatal(err)
	}
	finish(true)
	for _, tc := range []struct {
		query, role string
		want        int
	}{{"target=program&scene_id=a", "operator", http.StatusOK}, {"target=preview&scene_id=a", "operator", http.StatusConflict}, {"target=program&scene_id=retired", "operator", http.StatusConflict}, {"target=invalid&scene_id=a", "operator", http.StatusBadRequest}, {"target=program&scene_id=a", "", http.StatusForbidden}} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/show/editable-preview/state?"+tc.query, nil)
		if tc.role != "" {
			r.Header.Set("X-Authenticated-User", "operator-1")
			r.Header.Set("X-Authenticated-Role", tc.role)
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, r)
		if out.Code != tc.want {
			t.Fatal("active read-back guard", tc, out.Code, out.Body.String())
		}
	}
	if ack := send("program", head.WriterID, 5, 6); ack["type"] != "ack" || ack["target"] != "program" || ack["observer_count"] != float64(0) {
		t.Fatal(ack)
	}
	_, _, state := actor.SnapshotState()
	if string(state["__editable.x"]) != "6" {
		t.Fatal("Program inbox not flushed before ack", state)
	}
	if ack := send("program", "retired", 6, 7); ack["type"] != "error" || ack["code"] != "EDITABLE_PREVIEW_CONFLICT" {
		t.Fatal(ack)
	}
	structure := map[string]any{"target": "program", "scene_id": "a", "scene_version": "v2", "base_edit_seq": 6, "edit_seq": 7, "writer_id": head.WriterID, "lsml_bundle": map[string]any{"lsml": "1.1"}}
	if replaced := editableAPIRequest(t, mux, http.MethodPost, structure, "operator"); replaced.Code != http.StatusOK {
		t.Fatal(replaced.Code, replaced.Body.String())
	}
	newHead, _ := p.EditableTargetHead("program")
	if newHead.WriterID == head.WriterID || !host.Serving(bluehost.SlotOnAir, "a", "v2") || !host.Serving(bluehost.SlotPreview, "old-program", "old-v") {
		t.Fatal(newHead)
	}
	if stale := editableAPIRequest(t, mux, http.MethodPost, structure, "operator"); stale.Code != http.StatusConflict {
		t.Fatal("stale snapshot accepted", stale.Body.String())
	}
}

func TestEditableCompilationCannotOverwriteConcurrentHotEdit(t *testing.T) {
	p := runtime.NewPreviewSlot(context.Background(), runtime.NewComputeRegistry(), &editableAPIWire{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer p.Close()
	host := bluehost.NewHost()
	calls := 0
	intent := &SceneIntentDeps{Host: host, StaticRenderBundleCompiler: func(_ []byte, id, version string) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
		calls++
		if calls == 2 {
			if err := p.ApplyEditablePatches("a", 0, 1, []runtime.EditablePatch{{Path: "__editable.x", Value: json.RawMessage(`42`)}}); err != nil {
				t.Fatal(err)
			}
		}
		return &compiler.RenderBundle{SceneVersion: version}, map[string]json.RawMessage{"__editable.x": json.RawMessage(`10`)}, nil
	}}
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: slog.Default(), Config: config.Config{Profile: config.ProfileEmbeddedLocal}, Preview: p, SceneIntent: intent})
	request := map[string]any{"scene_id": "a", "scene_version": "v1", "edit_seq": 0, "lsml_bundle": map[string]any{"lsml": "1.1"}}
	if opened := editableAPIRequest(t, mux, http.MethodPost, request, "operator"); opened.Code != http.StatusOK {
		t.Fatal(opened.Body.String())
	}
	request["scene_version"] = "v2"
	request["edit_seq"] = 2
	if stale := editableAPIRequest(t, mux, http.MethodPost, request, "operator"); stale.Code != http.StatusConflict {
		t.Fatal("stale compilation accepted", stale.Body.String())
	}
	if !host.Serving(bluehost.SlotPreview, "a", "v1") {
		t.Fatal("host mutated before fence")
	}
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, state := p.Current().SnapshotState()
	if string(state["__editable.x"]) != "42" {
		t.Fatal(state)
	}
}
