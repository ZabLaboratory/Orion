package config

import "testing"

// TestLoad_ProfileDefaultsToEmbeddedLocal pins the local-only default: an
// unset ORION_PROFILE resolves to the embedded Prism sidecar and both
// listeners stay on loopback.
func TestLoad_ProfileDefaultsToEmbeddedLocal(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileEmbeddedLocal {
		t.Fatalf("Profile = %q, want %q", cfg.Profile, ProfileEmbeddedLocal)
	}
	if !cfg.Profile.IsEmbeddedLocal() {
		t.Fatal("default profile must report IsEmbeddedLocal")
	}
	if cfg.ListenAddr != "127.0.0.1:4007" {
		t.Fatalf("ListenAddr = %q, want local default 127.0.0.1:4007", cfg.ListenAddr)
	}
	if cfg.InternalAddr != "127.0.0.1:4017" {
		t.Fatalf("InternalAddr = %q, want local default 127.0.0.1:4017", cfg.InternalAddr)
	}
}

// TestLoad_ProfileRejectsRetiredAntenne proves the old remotely reachable
// execution profile cannot be re-enabled by a stale environment file.
func TestLoad_ProfileRejectsRetiredAntenne(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "antenne")
	if _, err := Load(); err == nil {
		t.Fatal("expected retired antenne profile to be rejected")
	}
}

// TestLoad_ProfileEmbeddedLocalCollapsesToLoopback proves the local
// sidecar's boot posture.
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

// TestLoad_ProfileEmbeddedLocalRespectsExplicitListen accepts a custom
// loopback port while retaining the local-only boundary.
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

func TestLoad_ProfileRejectsNonLoopbackListeners(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		addr string
	}{
		{name: "wildcard-listen", key: "ORION_LISTEN_ADDR", addr: "0.0.0.0:4007"},
		{name: "public-listen", key: "ORION_LISTEN_ADDR", addr: "192.0.2.10:4007"},
		{name: "wildcard-internal", key: "ORION_INTERNAL_ADDR", addr: "[::]:4017"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRequiredEnv(t)
			t.Setenv(tc.key, tc.addr)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s=%q to be rejected", tc.key, tc.addr)
			}
		})
	}
}

func TestLoad_ProfileRejectsRemotePublicBaseURL(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PUBLIC_BASE_URL", "https://zabgate.example/orion")
	if _, err := Load(); err == nil {
		t.Fatal("expected remote ORION_PUBLIC_BASE_URL to be rejected")
	}
}

func TestLoad_ProfileRejectsLegacyDatabaseURL(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_DATABASE_URL", "postgres://legacy.example/orion")
	if _, err := Load(); err == nil {
		t.Fatal("expected legacy ORION_DATABASE_URL to be rejected")
	}
}

// TestLoad_EmbeddedLocalRequiresStoreAndBases — the embedded-local profile
// requires the SQLite store + the three loopback base-URLs (Canvas/Blue/
// ZabGate, gateway sidecar #163) + the validation mirror root (#247). It does
// NOT require the pg DSN (SQLite store) nor the scene bundle path (optional
// offline fallback since #246).
func TestLoad_EmbeddedLocalRequiresStoreAndBases(t *testing.T) {
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_PROFILE", "embedded-local")
	// Handshake secret required since #223 (so the failure under test is the
	// missing store/bases, not the missing secret).
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	// No SQLITE_PATH / base-URLs / mirror root, no DB DSN.
	if _, err := Load(); err == nil {
		t.Fatal("expected error for embedded-local without store/bases/mirror")
	}
	// With the SQLite store + the three loopback bases + the mirror root set,
	// the missing pg DSN is NOT an error, and the scene bundle path is NOT
	// required (#246).
	t.Setenv("ORION_SQLITE_PATH", "/tmp/o.db")
	t.Setenv("ORION_CANVAS_BASE_URL", "http://127.0.0.1:4000/canvas")
	t.Setenv("ORION_BLUE_BASE_URL", "http://127.0.0.1:4000/blue")
	t.Setenv("ORION_ZABGATE_URL", "http://127.0.0.1:4000")
	t.Setenv("ORION_VALIDATION_MIRROR_ROOT", "/tmp/o-mirror")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("embedded-local with store+bases+mirror must load (pg unused, bundle optional): %v", err)
	}
	if cfg.SQLitePath != "/tmp/o.db" {
		t.Fatalf("SQLite path not parsed: %+v", cfg)
	}
	if cfg.ValidationMirrorRoot != "/tmp/o-mirror" {
		t.Fatalf("mirror root not parsed: %q", cfg.ValidationMirrorRoot)
	}
	if cfg.SceneBundlePath != "" {
		t.Fatalf("scene bundle path leaked when unset: %q", cfg.SceneBundlePath)
	}
}

// TestLoad_EmbeddedLocalRequiresEachEdge — each Canvas/Blue/ZabGate base-URL is
// independently required in embedded-local; a missing one is a clear boot
// failure (RC-A1/RC-A5 §2/§4). The scene bundle path AND the validation mirror
// root are optional (full-prod model), so their absence must never mask a
// missing edge.
func TestLoad_EmbeddedLocalRequiresEachEdge(t *testing.T) {
	for _, missing := range []string{"ORION_CANVAS_BASE_URL", "ORION_BLUE_BASE_URL", "ORION_ZABGATE_URL"} {
		t.Run("missing_"+missing, func(t *testing.T) {
			t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
			t.Setenv("ORION_PROFILE", "embedded-local")
			t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
			t.Setenv("ORION_SQLITE_PATH", "/tmp/o.db")
			t.Setenv("ORION_CANVAS_BASE_URL", "http://127.0.0.1:4000/canvas")
			t.Setenv("ORION_BLUE_BASE_URL", "http://127.0.0.1:4000/blue")
			t.Setenv("ORION_ZABGATE_URL", "http://127.0.0.1:4000")
			t.Setenv(missing, "")
			if _, err := Load(); err == nil {
				t.Fatalf("expected boot failure with %s unset in embedded-local", missing)
			}
		})
	}
}

// TestLoad_EmbeddedLocalMirrorRootOptional — the validation mirror root is an
// OPTIONAL offline fallback: the local sidecar boots without it (the air gate
// then reads the local store), and parses it when set.
func TestLoad_EmbeddedLocalMirrorRootOptional(t *testing.T) {
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	t.Setenv("ORION_SQLITE_PATH", "/tmp/o.db")
	t.Setenv("ORION_CANVAS_BASE_URL", "http://127.0.0.1:4000/canvas")
	t.Setenv("ORION_BLUE_BASE_URL", "http://127.0.0.1:4000/blue")
	t.Setenv("ORION_ZABGATE_URL", "http://127.0.0.1:4000")
	// Mirror root unset → boot must still succeed (store-backed air gate).
	cfg, err := Load()
	if err != nil {
		t.Fatalf("embedded-local without mirror root must load: %v", err)
	}
	if cfg.ValidationMirrorRoot != "" {
		t.Fatalf("expected empty ValidationMirrorRoot, got %q", cfg.ValidationMirrorRoot)
	}
	// Mirror root set → parsed (offline fallback opt-in).
	t.Setenv("ORION_VALIDATION_MIRROR_ROOT", "/tmp/o-mirror")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("embedded-local with mirror root must load: %v", err)
	}
	if cfg.ValidationMirrorRoot != "/tmp/o-mirror" {
		t.Fatalf("mirror root not parsed: %q", cfg.ValidationMirrorRoot)
	}
}

// TestLoad_EmbeddedLocalBundleOptional — the scene bundle path is parsed when
// set (offline fallback) and absent otherwise; either way boot succeeds given
// the loopback bases (#246).
func TestLoad_EmbeddedLocalBundleOptional(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	// Absent bundle path → nominal HTTP path, boot OK.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("embedded-local without bundle must load: %v", err)
	}
	if cfg.SceneBundlePath != "" {
		t.Fatalf("expected empty SceneBundlePath, got %q", cfg.SceneBundlePath)
	}
	// Present bundle path → retained for the offline fallback.
	t.Setenv("ORION_SCENE_BUNDLE_PATH", "/tmp/o-bundle.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("embedded-local with bundle must load: %v", err)
	}
	if cfg.SceneBundlePath != "/tmp/o-bundle.json" {
		t.Fatalf("SceneBundlePath = %q, want fallback path retained", cfg.SceneBundlePath)
	}
}

// TestLoad_EmbeddedLocalRequiresHandshakeSecret proves boot fails when
// the embedded-local profile is selected without ORION_LOCAL_OPERATOR_SECRET
// (#223, ADR 016 §5 R2): booting without it would grant operator to any
// loopback caller, the exact hole RC-4 forbids.
func TestLoad_EmbeddedLocalRequiresHandshakeSecret(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "")
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

// TestLoad_ProfileRejectsUnknown — an unrecognised profile is a config
// error (fail-fast at boot, like every other enum flag).
func TestLoad_ProfileRejectsUnknown(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "wibble")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown ORION_PROFILE, got nil")
	}
}

// TestLoad_ProfileAbsentEqualsExplicitLocal guards the local-only default:
// leaving the profile unset and spelling embedded-local explicitly produces
// the same boot configuration.
func TestLoad_ProfileAbsentEqualsExplicitLocal(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_PROFILE", "")
	absent, err := Load()
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	t.Setenv("ORION_PROFILE", "embedded-local")
	explicit, err := Load()
	if err != nil {
		t.Fatalf("Load explicit: %v", err)
	}
	if absent.ListenAddr != explicit.ListenAddr || absent.InternalAddr != explicit.InternalAddr {
		t.Fatal("absent and explicit local profiles diverge on listen posture")
	}
	if absent.Profile != explicit.Profile {
		t.Fatalf("Profile mismatch: absent=%q explicit=%q", absent.Profile, explicit.Profile)
	}
}
