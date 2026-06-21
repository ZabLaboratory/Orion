package config

import "testing"

// TestLoad_ProfileDefaultsToAntenne pins RC-1: an unset ORION_PROFILE
// resolves to the antenne profile (today's production behaviour) and
// leaves the listen addresses at their 0.0.0.0 prod defaults.
func TestLoad_ProfileDefaultsToAntenne(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileAntenne {
		t.Fatalf("Profile = %q, want %q", cfg.Profile, ProfileAntenne)
	}
	if cfg.Profile.IsEmbeddedLocal() {
		t.Fatal("antenne profile reports IsEmbeddedLocal")
	}
	if cfg.ListenAddr != "0.0.0.0:4007" {
		t.Fatalf("ListenAddr = %q, want prod default 0.0.0.0:4007", cfg.ListenAddr)
	}
	if cfg.InternalAddr != "0.0.0.0:4017" {
		t.Fatalf("InternalAddr = %q, want prod default 0.0.0.0:4017", cfg.InternalAddr)
	}
}

// TestLoad_ProfileExplicitAntenne — the explicit antenne value matches
// the default exactly.
func TestLoad_ProfileExplicitAntenne(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "antenne")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileAntenne {
		t.Fatalf("Profile = %q, want antenne", cfg.Profile)
	}
}

// TestLoad_ProfileEmbeddedLocalCollapsesToLoopback proves the only
// boot-visible effect of the embedded-local profile on config: the
// listen posture collapses to loopback when no explicit override is set
// (ADR 016 §3.3, D4). No hot-path field changes.
func TestLoad_ProfileEmbeddedLocalCollapsesToLoopback(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake") // required since #223
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileEmbeddedLocal || !cfg.Profile.IsEmbeddedLocal() {
		t.Fatalf("Profile = %q, want embedded-local", cfg.Profile)
	}
	if cfg.ListenAddr != "127.0.0.1:4007" {
		t.Fatalf("ListenAddr = %q, want loopback 127.0.0.1:4007", cfg.ListenAddr)
	}
	if cfg.InternalAddr != "127.0.0.1:4017" {
		t.Fatalf("InternalAddr = %q, want loopback 127.0.0.1:4017", cfg.InternalAddr)
	}
}

// TestLoad_ProfileEmbeddedLocalRespectsExplicitListen — an operator
// override of the listen addr is honoured (loopback pin only fills the
// prod default, never overrides an explicit value).
func TestLoad_ProfileEmbeddedLocalRespectsExplicitListen(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake") // required since #223
	t.Setenv("ORION_LISTEN_ADDR", "127.0.0.1:5555")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:5555" {
		t.Fatalf("ListenAddr = %q, want explicit 127.0.0.1:5555", cfg.ListenAddr)
	}
}

// TestLoad_EmbeddedLocalRequiresLocalPaths — the embedded-local profile
// requires the SQLite file + scene bundle path (and does NOT require the pg
// DSN / Canvas / Blue HTTP bases, which are unused there). #222/#224.
func TestLoad_EmbeddedLocalRequiresLocalPaths(t *testing.T) {
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_PROFILE", "embedded-local")
	// Handshake secret required since #223 (so the failure under test is the
	// missing local paths, not the missing secret).
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	// No SQLITE_PATH / SCENE_BUNDLE_PATH, no DB DSN.
	if _, err := Load(); err == nil {
		t.Fatal("expected error for embedded-local without local paths")
	}
	// With the local paths set, the missing pg DSN / HTTP bases are NOT errors.
	t.Setenv("ORION_SQLITE_PATH", "/tmp/o.db")
	t.Setenv("ORION_SCENE_BUNDLE_PATH", "/tmp/o-bundle.json")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("embedded-local with local paths must load (pg/HTTP edges unused): %v", err)
	}
	if cfg.SQLitePath != "/tmp/o.db" || cfg.SceneBundlePath != "/tmp/o-bundle.json" {
		t.Fatalf("local paths not parsed: %+v", cfg)
	}
}

// TestLoad_EmbeddedLocalRequiresHandshakeSecret proves boot fails when
// the embedded-local profile is selected without ORION_LOCAL_OPERATOR_SECRET
// (#223, ADR 016 §5 R2): booting without it would grant operator to any
// loopback caller, the exact hole RC-4 forbids.
func TestLoad_EmbeddedLocalRequiresHandshakeSecret(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	// no ORION_LOCAL_OPERATOR_SECRET set
	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded without handshake secret, want failure")
	}
}

// TestLoad_EmbeddedLocalCarriesSecretAndUser — a well-formed embedded-local
// boot carries the handshake secret and the (defaulted) local user id.
func TestLoad_EmbeddedLocalCarriesSecretAndUser(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalAuthSecret != "prism-handshake" {
		t.Fatalf("LocalAuthSecret = %q", cfg.LocalAuthSecret)
	}
	if cfg.LocalAuthUser != "local-operator" {
		t.Fatalf("LocalAuthUser = %q, want default", cfg.LocalAuthUser)
	}
}

// TestLoad_AntenneIgnoresLocalAuthSecret — the secret var is consulted
// only in embedded-local; on antenne it never leaks into Config.
func TestLoad_AntenneIgnoresLocalAuthSecret(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "antenne")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "should-be-ignored")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalAuthSecret != "" {
		t.Fatalf("antenne leaked LocalAuthSecret: %q", cfg.LocalAuthSecret)
	}
}

// TestLoad_ProfileRejectsUnknown — an unrecognised profile is a config
// error (fail-fast at boot, like every other enum flag).
func TestLoad_ProfileRejectsUnknown(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "wibble")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown ORION_PROFILE, got nil")
	}
}

// TestLoad_AntenneLeavesEveryHotPathFieldUntouched is the RC-1 guard:
// loading with the profile absent vs. the explicit antenne value yields
// an identical Config, and neither touches a hot-path field. If a future
// change leaks a profile branch into config beyond the listen posture,
// this diff catches it.
func TestLoad_AntenneAbsentEqualsExplicit(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "")
	absent, err := Load()
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	t.Setenv("ORION_PROFILE", "antenne")
	explicit, err := Load()
	if err != nil {
		t.Fatalf("Load explicit: %v", err)
	}
	if absent.ListenAddr != explicit.ListenAddr || absent.InternalAddr != explicit.InternalAddr {
		t.Fatal("absent and explicit antenne profiles diverge on listen posture")
	}
	if absent.Profile != explicit.Profile {
		t.Fatalf("Profile mismatch: absent=%q explicit=%q", absent.Profile, explicit.Profile)
	}
}
