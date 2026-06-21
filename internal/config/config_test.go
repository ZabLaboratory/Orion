package config

import (
	"strings"
	"testing"
)

// baseEnv sets the always-required vars so Load only fails on the field
// under test. Each call to t.Setenv is scoped to the test.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ORION_DATABASE_URL", "postgres://localhost/orion")
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_CANVAS_BASE_URL", "http://canvas")
	t.Setenv("ORION_BLUE_BASE_URL", "http://blue")
}

// TestLoad_AntenneDefaultUnchanged proves the default profile (antenne)
// binds the prod listen addr and ignores the local-auth vars entirely
// (invariant: antenne behaviour is byte-for-byte unchanged, RC-1).
func TestLoad_AntenneDefaultUnchanged(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileAntenne {
		t.Fatalf("profile = %q, want antenne", cfg.Profile)
	}
	if cfg.ListenAddr != "0.0.0.0:4007" {
		t.Fatalf("listen = %q, want 0.0.0.0:4007", cfg.ListenAddr)
	}
	if cfg.LocalAuthSecret != "" {
		t.Fatalf("local secret leaked on antenne: %q", cfg.LocalAuthSecret)
	}
}

// TestLoad_EmbeddedLocalRequiresHandshakeSecret proves boot fails when the
// embedded-local profile is selected without ORION_LOCAL_AUTH_SECRET — the
// guard that prevents granting operator unguarded (ADR 016 §5 R2).
func TestLoad_EmbeddedLocalRequiresHandshakeSecret(t *testing.T) {
	baseEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded without handshake secret, want failure")
	}
	if !strings.Contains(err.Error(), "ORION_LOCAL_AUTH_SECRET") {
		t.Fatalf("error %q does not mention ORION_LOCAL_AUTH_SECRET", err)
	}
}

// TestLoad_EmbeddedLocalLoopbackAndSecret proves a well-formed
// embedded-local boot pins loopback listen addrs and carries the secret.
func TestLoad_EmbeddedLocalLoopbackAndSecret(t *testing.T) {
	baseEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_AUTH_SECRET", "prism-handshake")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Profile.IsEmbeddedLocal() {
		t.Fatal("profile not embedded-local")
	}
	if cfg.ListenAddr != "127.0.0.1:4007" {
		t.Fatalf("listen = %q, want loopback", cfg.ListenAddr)
	}
	if cfg.InternalAddr != "127.0.0.1:4017" {
		t.Fatalf("internal = %q, want loopback", cfg.InternalAddr)
	}
	if cfg.LocalAuthSecret != "prism-handshake" {
		t.Fatalf("secret = %q", cfg.LocalAuthSecret)
	}
	if cfg.LocalAuthUser != "local-operator" {
		t.Fatalf("user = %q, want default", cfg.LocalAuthUser)
	}
}
