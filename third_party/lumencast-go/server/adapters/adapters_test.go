package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lumencast/lumencast-go/protocol"
	"github.com/Lumencast/lumencast-go/server"
	"github.com/Lumencast/lumencast-go/server/adapters"
)

func newScene(t *testing.T) *server.Scene {
	t.Helper()
	srv, err := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Auth:       server.NewStaticTokens(map[string]server.Identity{"x": {Subject: "x", Role: protocol.RoleViewer}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	scene := srv.NewScene("s")
	_ = scene.Set(map[string]any{"k": 0})
	return scene
}

func TestHTTPPoll_DecodesAndEmits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"k": 7}`))
		calls.Add(1)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	sc := newScene(t)
	go func() {
		_ = adapters.HTTPPoll(ctx, sc, adapters.HTTPPollConfig{
			URL:      srv.URL,
			Interval: 50 * time.Millisecond,
			Decode:   adapters.JSONFlatDecode(""),
		})
	}()

	// Wait for at least one poll to complete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("HTTPPoll never called the upstream")
	}
}

func TestPgListen_DecodesAndEmits(t *testing.T) {
	sc := newScene(t)
	notifications := make(chan adapters.PgNotification, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = adapters.PgListen(ctx, sc, notifications,
			func(channel, payload string) (map[string]any, error) {
				if channel != "score_updates" {
					return nil, errors.New("unexpected channel")
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(payload), &m); err != nil {
					return nil, err
				}
				return m, nil
			},
			nil)
		close(done)
	}()

	notifications <- adapters.PgNotification{
		Channel: "score_updates",
		Payload: `{"k": 42}`,
	}

	// Allow the goroutine to drain.
	time.Sleep(50 * time.Millisecond)
	close(notifications)
	<-done
}

func TestPgListen_RequiresDecode(t *testing.T) {
	sc := newScene(t)
	notifications := make(chan adapters.PgNotification)
	close(notifications)
	if err := adapters.PgListen(context.Background(), sc, notifications, nil, nil); err == nil {
		t.Fatal("expected error for nil decode")
	}
}

func TestPgxListen_RejectsBadConfig(t *testing.T) {
	sc := newScene(t)
	if err := adapters.PgxListen(context.Background(), sc, adapters.PgxListenConfig{}); err == nil {
		t.Fatal("expected DSN required")
	}
	if err := adapters.PgxListen(context.Background(), sc, adapters.PgxListenConfig{
		DSN: "postgres://nowhere", Channels: nil,
	}); err == nil || !strings.Contains(err.Error(), "Channels") {
		t.Fatalf("expected Channels-required error, got %v", err)
	}
	if err := adapters.PgxListen(context.Background(), sc, adapters.PgxListenConfig{
		DSN: "postgres://nowhere", Channels: []string{"x"},
	}); err == nil || !strings.Contains(err.Error(), "Decode") {
		t.Fatalf("expected Decode-required error, got %v", err)
	}
}
