package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/secretbox"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// captureLogger returns a logger writing JSON into buf, so a test can assert
// that a refusal was actually SAID. A guard that degrades silently is the
// failure mode these criteria exist to prevent.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testKeyB64(t *testing.T) string {
	t.Helper()
	raw := make([]byte, secretbox.KeyBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// refreshSpy is a stand-in for ZabAuth that counts every refresh call. The
// criteria are phrased as "emits ZERO refresh calls", so the assertion has to
// be on the wire, not on an internal flag.
type refreshSpy struct {
	srv   *httptest.Server
	calls atomic.Int64
}

func newRefreshSpy(t *testing.T) *refreshSpy {
	t.Helper()
	s := &refreshSpy{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-g1",
			"refresh_token": "refresh-g1",
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// nilStore is a Store that panics on every durable method. Any wiring bug that
// let a non-antenne or lock-less process reach the store would surface as a
// panic rather than as a silent success.
type nilStore struct{ store.Store }

func (nilStore) GetServiceRefreshToken(context.Context) ([]byte, error) {
	panic("durable store reached by a manager that must not have armed")
}
func (nilStore) PutServiceRefreshToken(context.Context, []byte) error {
	panic("durable store reached by a manager that must not have armed")
}

// RC 45 — a boot under ORION_PROFILE=embedded-local with the seed PRESENT in
// the environment ignores it, logs the refusal, and emits zero refresh calls.
func TestWireServiceTokens_EmbeddedLocalIgnoresSeedLoudly(t *testing.T) {
	spy := newRefreshSpy(t)
	var buf bytes.Buffer
	cfg := config.Config{
		Profile:             config.ProfileEmbeddedLocal,
		ZabAuthValidateURL:  spy.srv.URL + "/tokens",
		ServiceRefreshToken: "seed-that-must-not-be-used",
		EncryptionKey:       testKeyB64(t),
		ServiceToken:        "static-dev-token",
	}

	m := wireServiceTokens(cfg, nilStore{}, true /* lock irrelevant off antenne */, captureLogger(&buf))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start must not fail the boot: %v", err)
	}
	t.Cleanup(m.Stop)

	if m.Seed != "" {
		t.Fatal("the seed was consumed under embedded-local")
	}
	if m.Store != nil || m.Box != nil {
		t.Fatal("the durable model armed under embedded-local")
	}
	if n := spy.calls.Load(); n != 0 {
		t.Fatalf("refresh calls = %d, want 0", n)
	}
	logged := buf.String()
	if !strings.Contains(logged, "ORION_SERVICE_REFRESH_TOKEN") || !strings.Contains(logged, "ORION_ENCRYPTION_KEY") {
		t.Fatalf("the refusal did not name the ignored vars: %s", logged)
	}
	if strings.Contains(logged, "seed-that-must-not-be-used") || strings.Contains(logged, cfg.EncryptionKey) {
		t.Fatal("the refusal log leaked credential material")
	}
	// Static mode is retained: embedded-local keeps its dev/test posture.
	if m.Token() != "static-dev-token" {
		t.Fatalf("Token() = %q, want the static dev token", m.Token())
	}
}

// The embedded-local guard is silent when there is nothing to refuse — a loud
// log on a clean environment would train operators to ignore it.
func TestWireServiceTokens_EmbeddedLocalSilentWithoutDurableMaterial(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{Profile: config.ProfileEmbeddedLocal, ZabAuthValidateURL: "http://unused/tokens"}
	wireServiceTokens(cfg, nilStore{}, true, captureLogger(&buf))
	if strings.Contains(buf.String(), "IGNORED") {
		t.Fatalf("refusal logged with no durable material present: %s", buf.String())
	}
}

// RC 44 — a second Orion process on the same database does not get the lock,
// so it does NOT arm the manager: it logs the refusal, emits zero refresh
// calls, and keeps serving degraded (Start returns nil — the boot succeeds).
func TestWireServiceTokens_AntenneWithoutLockDoesNotArm(t *testing.T) {
	spy := newRefreshSpy(t)
	var buf bytes.Buffer
	cfg := config.Config{
		Profile:             config.ProfileAntenne,
		ZabAuthValidateURL:  spy.srv.URL + "/tokens",
		ServiceRefreshToken: "seed",
		EncryptionKey:       testKeyB64(t),
	}

	m := wireServiceTokens(cfg, nilStore{}, false /* lock NOT held */, captureLogger(&buf))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("a lost lock must not fail the boot: %v", err)
	}
	t.Cleanup(m.Stop)

	if m.Store != nil || m.Box != nil {
		t.Fatal("the durable model armed without the rotation lock")
	}
	if n := spy.calls.Load(); n != 0 {
		t.Fatalf("refresh calls = %d, want 0", n)
	}
	if got := m.State(); got != auth.ServiceTokenDegraded {
		t.Fatalf("State() = %q, want degraded", got)
	}
	if m.Token() != "" {
		t.Fatal("a lock-less process holds a token; outbound calls must fail closed")
	}
	if !strings.Contains(buf.String(), "advisory lock") {
		t.Fatalf("the lock refusal was not logged: %s", buf.String())
	}
}

// The positive control for both guards: antenne WITH the lock arms the durable
// model. Without this, the two tests above would pass on a manager that never
// arms at all.
func TestWireServiceTokens_AntenneWithLockArms(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		Profile:             config.ProfileAntenne,
		ZabAuthValidateURL:  "http://zabgate:4000/auth/api/v1/tokens",
		ServiceRefreshToken: "seed",
		EncryptionKey:       testKeyB64(t),
	}
	m := wireServiceTokens(cfg, nilStore{}, true, captureLogger(&buf))
	if m.Store == nil || m.Box == nil {
		t.Fatal("the durable model did not arm on antenne with the lock held")
	}
	if m.Seed != "seed" {
		t.Fatalf("Seed = %q, want the configured seed", m.Seed)
	}
	if m.RefreshURL != "http://zabgate:4000/auth/api/v1/service-tokens/refresh" {
		t.Fatalf("RefreshURL = %q", m.RefreshURL)
	}
}

// An unusable encryption key degrades — it never falls back to plaintext and
// never fails the boot.
func TestWireServiceTokens_AntenneBadKeyDegrades(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		Profile:            config.ProfileAntenne,
		ZabAuthValidateURL: "http://unused/tokens",
		EncryptionKey:      "not-base64-of-32-bytes",
	}
	m := wireServiceTokens(cfg, nilStore{}, true, captureLogger(&buf))
	if m.Box != nil || m.Store != nil {
		t.Fatal("armed under an unusable ORION_ENCRYPTION_KEY")
	}
	if m.State() != auth.ServiceTokenDegraded {
		t.Fatalf("State() = %q, want degraded", m.State())
	}
}

// ORION_SERVICE_TOKEN stays refused on antenne (§ A3.3 part 5, RC 47) — the
// guards added here must not reopen the standing-credential fallback.
func TestWireServiceTokens_AntenneStillRefusesStaticToken(t *testing.T) {
	for _, lockHeld := range []bool{true, false} {
		var buf bytes.Buffer
		cfg := config.Config{
			Profile:            config.ProfileAntenne,
			ZabAuthValidateURL: "http://unused/tokens",
			ServiceToken:       "standing-credential",
			EncryptionKey:      testKeyB64(t),
		}
		m := wireServiceTokens(cfg, nilStore{}, lockHeld, captureLogger(&buf))
		if m.StaticToken != "" {
			t.Fatalf("lockHeld=%v: ORION_SERVICE_TOKEN was honoured on antenne", lockHeld)
		}
		if !strings.Contains(buf.String(), "ORION_SERVICE_TOKEN is set but REFUSED") {
			t.Fatalf("lockHeld=%v: the refusal was not logged", lockHeld)
		}
	}
}
