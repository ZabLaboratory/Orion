package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceTokenManager_StaticMode(t *testing.T) {
	m := &ServiceTokenManager{StaticToken: "static-abc"}
	if got := m.Token(); got != "static-abc" {
		t.Fatalf("static mode token = %q, want static-abc", got)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start in static mode should be no-op, got err=%v", err)
	}
	if got := m.Token(); got != "static-abc" {
		t.Fatalf("token after Start (static mode) = %q, want static-abc", got)
	}
	m.Stop() // safe in static mode
}

func TestServiceTokenManager_LiveModeMintAndRefresh(t *testing.T) {
	var mintCalls atomic.Int32
	var refreshCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/api/v1/service-tokens":
			mintCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer admin-jwt" {
				http.Error(w, "missing operator", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":       "live-1",
				"refresh_token":      "rt-1",
				"expires_at":         time.Now().Add(2 * time.Second).Format(time.RFC3339Nano),
				"refresh_expires_at": time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
			})
		case "/auth/api/v1/service-tokens/refresh":
			refreshCalls.Add(1)
			n := refreshCalls.Load()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":       "live-rotated-" + string(rune('0'+n)),
				"refresh_token":      "rt-rotated",
				"expires_at":         time.Now().Add(2 * time.Second).Format(time.RFC3339Nano),
				"refresh_expires_at": time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &ServiceTokenManager{
		MintURL:       srv.URL + "/auth/api/v1/service-tokens",
		RefreshURL:    srv.URL + "/auth/api/v1/service-tokens/refresh",
		OperatorToken: "admin-jwt",
		ServiceName:   "orion",
		Paths:         []string{"quasar.credentials.read"},
		RefreshLead:   1900 * time.Millisecond, // expiry 2s − lead ≈ 100ms wait
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if got := m.Token(); got != "live-1" {
		t.Fatalf("initial token = %q, want live-1", got)
	}
	if mintCalls.Load() != 1 {
		t.Fatalf("mintCalls = %d, want 1", mintCalls.Load())
	}

	// Wait for one rotation. Lead 1.9s, expiry 2s → wait ≈ 100ms.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Token() != "live-1" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := m.Token(); got == "live-1" {
		t.Fatalf("token never rotated; refresh_calls=%d", refreshCalls.Load())
	}
	if refreshCalls.Load() < 1 {
		t.Fatalf("expected at least 1 refresh call, got %d", refreshCalls.Load())
	}
}

func TestServiceTokenManager_MintFailureSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad operator"))
	}))
	defer srv.Close()

	m := &ServiceTokenManager{
		MintURL:       srv.URL + "/auth/api/v1/service-tokens",
		RefreshURL:    srv.URL + "/auth/api/v1/service-tokens/refresh",
		OperatorToken: "wrong",
		ServiceName:   "orion",
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatalf("expected mint failure to surface, got nil")
	}
	if m.Token() != "" {
		t.Fatalf("token after failed Start = %q, want empty", m.Token())
	}
}
