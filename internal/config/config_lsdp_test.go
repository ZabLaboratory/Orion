package config

import "testing"

// The LSDP mode flag (ADR 007 §C.5) must default to bespoke so a deploy
// of the C2 code with the env unset is a no-op (no LSML persisted or
// served), and must reject unknown values rather than silently falling
// back.
func TestLoad_LSDPModeDefaultsToBespoke(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_LSDP_MODE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LSDPMode != LSDPModeBespoke {
		t.Fatalf("default LSDP mode = %q, want %q", cfg.LSDPMode, LSDPModeBespoke)
	}
	if cfg.LSDPMode.PersistsLSML() {
		t.Fatal("bespoke mode must not persist LSML")
	}
}

func TestLoad_LSDPModeParsesDualAndLSDP(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want LSDPMode
	}{
		{"dual", LSDPModeDual},
		{"DUAL", LSDPModeDual},
		{"lsdp", LSDPModeLSDP},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			withRequiredEnv(t)
			t.Setenv("ORION_LSDP_MODE", tc.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LSDPMode != tc.want {
				t.Fatalf("LSDP mode = %q, want %q", cfg.LSDPMode, tc.want)
			}
			if !cfg.LSDPMode.PersistsLSML() {
				t.Fatalf("%q mode must persist LSML", tc.want)
			}
		})
	}
}

func TestLoad_LSDPModeRejectsUnknown(t *testing.T) {
	withRequiredEnv(t)
	t.Setenv("ORION_LSDP_MODE", "wibble")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject an unknown ORION_LSDP_MODE")
	}
}

// withRequiredEnv sets the minimum required env so Load() succeeds, then
// the test overrides ORION_LSDP_MODE.
func withRequiredEnv(t *testing.T) {
	t.Helper()
	// Every Load() test runs the embedded-local sidecar posture; the remote
	// profile is retired and must not be used to satisfy required fields.
	t.Setenv("ORION_PROFILE", "embedded-local")
	t.Setenv("ORION_LOCAL_OPERATOR_SECRET", "prism-handshake")
	// Orion is local-only: a stale PostgreSQL DSN must not be part of the
	// success baseline. The dedicated rejection test covers legacy env files.
	t.Setenv("ORION_DATABASE_URL", "")
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_CANVAS_BASE_URL", "http://127.0.0.1:4000/canvas")
	t.Setenv("ORION_BLUE_BASE_URL", "http://127.0.0.1:4000/blue")
	// embedded-local required fields for the local sidecar.
	//   - SQLite store (#222).
	//   - ZabGate loopback base (#246): required by the nominal httpFetcher and
	//     the `_query` delegation base.
	//   - Validation mirror root (#247): required in embedded-local since the
	//     air-eligibility gate imports the `validated` record from the mirror.
	// ORION_SCENE_BUNDLE_PATH is deliberately NOT seeded: since #246 it is
	// OPTIONAL in embedded-local (offline fallback only), so the helper
	// exercises the nominal HTTP fetch path; tests that need the bundle set
	// it themselves.
	t.Setenv("ORION_SQLITE_PATH", "/tmp/orion-test.db")
	t.Setenv("ORION_ZABGATE_URL", "http://127.0.0.1:4000")
	t.Setenv("ORION_VALIDATION_MIRROR_ROOT", "/tmp/orion-test-mirror")
}
