package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeServiceTokenStore struct {
	mu  sync.Mutex
	enc []byte
}

func testServiceAccessToken(subject, role string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]string{"sub": subject, "role": role})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test-signature"
}

func writeServiceBundle(w http.ResponseWriter, accessToken, refreshToken string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(serviceTokenBundle{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		ExpiresAt:        time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC),
		RefreshExpiresAt: time.Date(2099, time.January, 2, 0, 0, 0, 0, time.UTC),
	})
}

func (s *fakeServiceTokenStore) Get(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.enc) == 0 {
		return nil, errServiceTokenNotFound
	}
	return append([]byte(nil), s.enc...), nil
}

func (s *fakeServiceTokenStore) Put(_ context.Context, enc []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enc = append([]byte(nil), enc...)
	return nil
}

func TestServiceTokenManagerBootRotationPersistsSuccessor(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	store := &fakeServiceTokenStore{}
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Fatalf("refresh must be possession-only POST: method=%s auth=%q", r.Method, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil || payload["refresh_token"] != "seed-refresh" {
			t.Fatalf("unexpected refresh payload: %s", body)
		}
		writeServiceBundle(w, testServiceAccessToken("orion", "service"), "refresh-1")
	}))
	defer srv.Close()

	m, err := newTestServiceTokenManager(store, srv.URL, "seed-refresh", key, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Token(); got != testServiceAccessToken("orion", "service") {
		t.Fatalf("access token = %q, want the valid Orion service token", got)
	}
	if requests != 1 {
		t.Fatalf("refresh calls = %d, want 1", requests)
	}

	box, _ := newServiceTokenBox(key)
	plain, err := box.open(store.enc)
	if err != nil {
		t.Fatal(err)
	}
	var record durableServiceTokenRecord
	if err := json.Unmarshal(plain, &record); err != nil {
		t.Fatal(err)
	}
	if record.RefreshToken != "refresh-1" || record.Rotating {
		t.Fatalf("persisted record = %+v, want successor without rotating marker", record)
	}
}

func TestServiceTokenManagerRecoversInterruptedRotationFromSeed(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x43}, 32))
	box, err := newServiceTokenBox(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.seal(durableServiceTokenRecord{
		RefreshToken: "stale-refresh",
		Rotating:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeServiceTokenStore{enc: enc}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil || payload["refresh_token"] != "seed-refresh" {
			t.Fatalf("unexpected recovery refresh payload: %s", body)
		}
		writeServiceBundle(w, testServiceAccessToken("orion", "service"), "refresh-recovered")
	}))
	defer srv.Close()

	m, err := newTestServiceTokenManager(store, srv.URL, "seed-refresh", key, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Token(); got != testServiceAccessToken("orion", "service") {
		t.Fatalf("recovered access token = %q, want the valid Orion service token", got)
	}

	plain, err := box.open(store.enc)
	if err != nil {
		t.Fatal(err)
	}
	var record durableServiceTokenRecord
	if err := json.Unmarshal(plain, &record); err != nil {
		t.Fatal(err)
	}
	if record.RefreshToken != "refresh-recovered" || record.Rotating {
		t.Fatalf("recovered persisted record = %+v, want successor without rotating marker", record)
	}
}

func TestServiceTokenManagerFallsBackFromRevokedPersistedFamily(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	box, err := newServiceTokenBox(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.seal(durableServiceTokenRecord{RefreshToken: "revoked-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeServiceTokenStore{enc: enc}
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if payload["refresh_token"] == "revoked-refresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"revoked"}`))
			return
		}
		if payload["refresh_token"] != "fresh-seed" {
			t.Fatalf("unexpected fallback refresh payload: %s", body)
		}
		writeServiceBundle(w, testServiceAccessToken("orion", "service"), "refresh-fresh")
	}))
	defer srv.Close()

	m, err := newTestServiceTokenManager(store, srv.URL, "fresh-seed", key, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Token(); got != testServiceAccessToken("orion", "service") {
		t.Fatalf("access token after fallback = %q, want the valid Orion service token", got)
	}
	if requests != 2 {
		t.Fatalf("refresh calls = %d, want persisted attempt plus one seed fallback", requests)
	}

	plain, err := box.open(store.enc)
	if err != nil {
		t.Fatal(err)
	}
	var record durableServiceTokenRecord
	if err := json.Unmarshal(plain, &record); err != nil {
		t.Fatal(err)
	}
	if record.RefreshToken != "refresh-fresh" || record.Rotating {
		t.Fatalf("fallback persisted record = %+v, want fresh successor without rotating marker", record)
	}
}

func TestServiceTokenManagerFallsBackFromPersistedWrongIdentity(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x45}, 32))
	box, err := newServiceTokenBox(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.seal(durableServiceTokenRecord{RefreshToken: "legacy-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeServiceTokenStore{enc: enc}
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		if payload["refresh_token"] == "legacy-refresh" {
			// The old family is still accepted by ZabAuth, but it returns the
			// legacy installation principal that Blue must reject.
			writeServiceBundle(w, testServiceAccessToken("prism-orion-old", "service"), "legacy-successor")
			return
		}
		if payload["refresh_token"] != "fresh-seed" {
			t.Fatalf("unexpected fallback refresh payload: %s", body)
		}
		writeServiceBundle(w, testServiceAccessToken("orion", "service"), "refresh-fresh")
	}))
	defer srv.Close()

	m, err := newTestServiceTokenManager(store, srv.URL, "fresh-seed", key, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if got := m.Token(); got != testServiceAccessToken("orion", "service") {
		t.Fatalf("access token after identity fallback = %q, want the valid Orion service token", got)
	}
	if requests != 2 {
		t.Fatalf("refresh calls = %d, want persisted attempt plus one seed fallback", requests)
	}

	plain, err := box.open(store.enc)
	if err != nil {
		t.Fatal(err)
	}
	var record durableServiceTokenRecord
	if err := json.Unmarshal(plain, &record); err != nil {
		t.Fatal(err)
	}
	if record.RefreshToken != "refresh-fresh" || record.Rotating {
		t.Fatalf("identity fallback persisted record = %+v, want fresh successor without rotating marker", record)
	}
}

func TestServiceTokenRefreshURL(t *testing.T) {
	got := ServiceTokenRefreshURL("http://zabgate:4000/auth/api/v1/tokens")
	want := "http://zabgate:4000/auth/api/v1/service-tokens/refresh"
	if got != want {
		t.Fatalf("refresh URL = %q, want %q", got, want)
	}
}

func TestServiceTokenManagerDoesNotUseExpiredStaticTokenInDurableMode(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x21}, 32))
	store := &fakeServiceTokenStore{}
	m, err := newTestServiceTokenManager(store, "http://127.0.0.1:1", "", key, &http.Client{Timeout: time.Millisecond}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Token(); got != "" {
		t.Fatalf("durable manager without a refresh source returned %q", got)
	}
}
