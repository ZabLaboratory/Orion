package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/coder/websocket"
)

type editableAPIMirror struct{}

func (editableAPIMirror) Forward(runtime.SubscriberMsg) {}

type editableAPIWire struct{ active string }

func (w *editableAPIWire) MirrorFor(string, string, *compiler.RenderBundle) runtime.SceneMirror {
	return editableAPIMirror{}
}
func (w *editableAPIWire) SetActive(sceneID string) { w.active = sceneID }
func (w *editableAPIWire) Drop(string)              {}

func editableAPIFixture(t *testing.T, profile config.Profile) (*http.ServeMux, *editableAPIWire) {
	t.Helper()
	wire := &editableAPIWire{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	preview := runtime.NewPreviewSlot(context.Background(), runtime.NewComputeRegistry(), wire, logger)
	t.Cleanup(preview.Close)
	staticCompiler := func(_ []byte, _, sceneVersion string) ([]byte, map[string]json.RawMessage, error) {
		compiled, err := json.Marshal(compiler.RenderBundle{SceneVersion: sceneVersion})
		return compiled, map[string]json.RawMessage{
			"__editable.61.x": json.RawMessage(`10`),
		}, err
	}
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:  logger,
		Config:  config.Config{Profile: profile, LocalEditorToken: "editor-capability"},
		Preview: preview,
		SceneIntent: &SceneIntentDeps{
			StaticBundleCompiler: staticCompiler,
		},
	})
	return mux, wire
}

func TestEditablePreviewSocketUsesDistinctCapabilityAndOrderedSequence(t *testing.T) {
	mux, _ := editableAPIFixture(t, config.ProfileEmbeddedLocal)
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{
		"scene_id": "editable-ws", "scene_version": "sha256:ws", "edit_seq": 0, "lsml_bundle": map[string]any{"lsml": "1.1"},
	}, "operator")
	if opened.Code != http.StatusOK {
		t.Fatalf("open = %d %s", opened.Code, opened.Body.String())
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	wsBase := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/show/editable-preview.ws"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsBase+"?local_editor_token=wrong", nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong capability: err=%v response=%v", err, resp)
	}

	conn, resp, err := websocket.Dial(ctx, wsBase+"?local_editor_token=editor-capability", nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	observer, resp, err := websocket.Dial(ctx, wsBase+"?local_editor_token=editor-capability&role=solar", nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(websocket.StatusNormalClosure, "")
	for seq, value := range []int{25, 35} {
		payload, _ := json.Marshal(map[string]any{
			"scene_id": "editable-ws", "base_edit_seq": seq, "edit_seq": seq + 1,
			"patches": []map[string]any{{"path": "__editable.61.x", "value": value}},
		})
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		_, accepted, err := observer.Read(ctx)
		if err != nil || !strings.Contains(string(accepted), `"type":"accepted_patch"`) || !strings.Contains(string(accepted), `"edit_seq":`+strconv.Itoa(seq+1)) {
			t.Fatalf("observer seq %d: %s err=%v", seq+1, accepted, err)
		}
		_, receipt, err := conn.Read(ctx)
		if err != nil || !strings.Contains(string(receipt), `"type":"ack"`) {
			t.Fatalf("receipt seq %d: %s err=%v", seq+1, receipt, err)
		}
	}
}

func editableAPIRequest(t *testing.T, mux *http.ServeMux, method string, body any, role string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/api/v1/show/editable-preview", bytes.NewReader(raw))
	if role != "" {
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", role)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestEditablePreviewAPIIsEmbeddedOperatorOnlyAndSequenceChecked(t *testing.T) {
	antenne, _ := editableAPIFixture(t, config.ProfileAntenne)
	refusedProfile := editableAPIRequest(t, antenne, http.MethodPost, map[string]any{
		"scene_id": "editable-a", "scene_version": "sha256:a", "edit_seq": 0, "lsml_bundle": map[string]any{"lsml": "1.1"},
	}, "operator")
	if refusedProfile.Code != http.StatusNotFound {
		t.Fatalf("antenne profile = %d, want 404", refusedProfile.Code)
	}

	mux, wire := editableAPIFixture(t, config.ProfileEmbeddedLocal)
	unauthenticated := editableAPIRequest(t, mux, http.MethodPost, map[string]any{}, "")
	if unauthenticated.Code != http.StatusForbidden {
		t.Fatalf("anonymous = %d, want 403", unauthenticated.Code)
	}
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{
		"scene_id": "editable-a", "scene_version": "sha256:a", "edit_seq": 4, "lsml_bundle": map[string]any{"lsml": "1.1"},
	}, "operator")
	if opened.Code != http.StatusOK || wire.active != "editable-a" {
		t.Fatalf("open = %d %s, active=%q", opened.Code, opened.Body.String(), wire.active)
	}
	reactivatedReq := httptest.NewRequest(http.MethodPost, "/api/v1/show/editable-preview/activate", bytes.NewBufferString(`{"scene_id":"editable-a","edit_seq":4}`))
	reactivatedReq.Header.Set("X-Authenticated-User", "operator-1")
	reactivatedReq.Header.Set("X-Authenticated-Role", "operator")
	reactivated := httptest.NewRecorder()
	mux.ServeHTTP(reactivated, reactivatedReq)
	if reactivated.Code != http.StatusOK || !strings.Contains(reactivated.Body.String(), `"reused":true`) {
		t.Fatalf("reactivate = %d %s", reactivated.Code, reactivated.Body.String())
	}

	patched := editableAPIRequest(t, mux, http.MethodPut, map[string]any{
		"scene_id": "editable-a", "base_edit_seq": 4, "edit_seq": 5,
		"patches": []map[string]any{{"path": "__editable.61.x", "value": 35}},
	}, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatalf("patch = %d %s", patched.Code, patched.Body.String())
	}
	stale := editableAPIRequest(t, mux, http.MethodPut, map[string]any{
		"scene_id": "editable-a", "base_edit_seq": 4, "edit_seq": 5,
		"patches": []map[string]any{{"path": "__editable.61.x", "value": 36}},
	}, "operator")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
	}
}
