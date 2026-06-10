package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Criterion 11 E3 (Orion half) over the REAL WS protocol: a Quasar
// service token scoped `__inputs.platform.twitch.*` pushes a canonical
// event onto the bound twitch leaf and a subscriber sees the delta
// (≤ 100 ms half of criterion 10, intra-Orion), while the same token
// writing `__inputs.platform.youtube.*` receives WRITE_FORBIDDEN.
func TestWS_PlatformScope_TwitchDeltaYoutubeForbidden(t *testing.T) {
	const leaf = "__inputs.platform.twitch.zabchannel.last_chat"

	logger := quietLogger()
	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	test := runtime.NewTestSessionManager(registry, logger, time.Minute)

	// What the issue-#84 compiler emits for `quasar.twitch.chat@1`
	// with config.channel "zabchannel": the global input leaf plus
	// the synthesized platform-stream acceptance binding.
	graph := &compiler.Graph{
		SceneID:      "scene-pf",
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			{ID: "chat", Kind: "input", Path: leaf, Compute: "quasar.twitch.chat@1"},
		},
		Defaults: map[string]json.RawMessage{},
		Bindings: []compiler.ExternalAdapter{
			{Key: leaf, Label: "Quasar platform stream", Kind: "platform-stream", TargetPaths: []string{leaf}},
		},
	}
	show.Load("scene-pf", graph, &compiler.RenderBundle{SceneVersion: "sha256:test"})
	if err := show.SetActive("scene-pf", nil); err != nil {
		t.Fatal(err)
	}

	inbox := adapters.NewInbox(show, logger, nil)
	srv := &Server{Show: show, Inbox: inbox, Test: test, Logger: logger, Metrics: obs.NewMetrics()}
	mux := http.NewServeMux()
	mux.HandleFunc("/show/stream", srv.ServeShowStream)
	httpSrv := httptest.NewServer(mux)
	defer func() {
		httpSrv.Close()
		show.Stop()
		test.Close()
	}()
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/show/stream"

	headers := http.Header{
		"X-Authenticated-User":  []string{"quasar"},
		"X-Authenticated-Role":  []string{"service"},
		"X-Authenticated-Paths": []string{"__inputs.platform.twitch.*"},
	}
	c := dialWith(t, wsURL, headers)
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","v":1,"since_sequence":null}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err != nil { // snapshot frame
		t.Fatal(err)
	}

	// In-scope canonical event → delta on the platform leaf.
	msg := `{"type":"input","v":1,"path":"` + leaf + `","value":{"message":"hello","user":"zab"}}`
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, raw, err := c.Read(rctx)
	if err != nil {
		t.Fatal(err)
	}
	var d protocol.Delta
	if err := json.Unmarshal(raw, &d); err != nil || d.Type != protocol.TypeDelta {
		t.Fatalf("expected delta for the scoped twitch write, got %s", raw)
	}
	if len(d.Patches) == 0 || d.Patches[0].Path != leaf {
		t.Fatalf("delta does not carry the platform leaf: %s", raw)
	}

	// Out-of-scope platform → WRITE_FORBIDDEN (criterion 11's youtube case).
	bad := `{"type":"input","v":1,"path":"__inputs.platform.youtube.zabchannel.last_chat","value":{}}`
	if err := c.Write(ctx, websocket.MessageText, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	rctx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel2()
	_, raw, err = c.Read(rctx2)
	if err != nil {
		t.Fatal(err)
	}
	var errMsg protocol.Error
	if err := json.Unmarshal(raw, &errMsg); err != nil || errMsg.Code != protocol.CodeWriteForbidden {
		t.Fatalf("expected WRITE_FORBIDDEN for youtube.*, got %s", raw)
	}
}
