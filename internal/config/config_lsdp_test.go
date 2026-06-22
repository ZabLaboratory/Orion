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
	t.Setenv("ORION_DATABASE_URL", "postgres://x/y")
	t.Setenv("ORION_ZABAUTH_VALIDATE_URL", "http://zabauth/validate")
	t.Setenv("ORION_CANVAS_BASE_URL", "http://zabgate/canvas")
	t.Setenv("ORION_BLUE_BASE_URL", "http://zabgate/blue")
	// embedded-local-only required fields. Ignored in antenne; set here so a
	// profile-keyed Load() in either profile passes validation.
	//   - SQLite store (#222).
	//   - ZabGate loopback base (#246): required in embedded-local since the
	//     httpFetcher is now the nominal fetch path; also the `_query`
	//     delegation base. Harmless in antenne.
	// ORION_SCENE_BUNDLE_PATH is deliberately NOT seeded: since #246 it is
	// OPTIONAL in embedded-local (offline fallback only), so the helper
	// exercises the nominal HTTP fetch path; tests that need the bundle set
	// it themselves.
	t.Setenv("ORION_SQLITE_PATH", "/tmp/orion-test.db")
	t.Setenv("ORION_ZABGATE_URL", "http://zabgate")
}
