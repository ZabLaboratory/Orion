// Package config parses Orion's runtime configuration from environment
// variables. Per ADR 004 § 1.6 there is no config file format — env only,
// loaded once at startup into a typed struct that the rest of the
// service treats as immutable.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type LogFormat string

const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

// LSDPMode is the Lumencast-convergence migration flag (ADR 007 §C.5).
// It gates the additive LSML persist/serve path so a deploy of the C2
// code with the flag unset changes nothing (no-op parallel-run).
//
//   - bespoke (default): today's behaviour only. The LSML bundle is
//     neither persisted on push nor served. The bespoke RenderBundle
//     serve is the only render artefact path.
//   - dual: the LSML bundle is ALSO persisted on push and served at the
//     dedicated LSML endpoint, beside the untouched bespoke serve.
//   - lsdp: the end state (flipped after B+C prove out). For C2 it
//     behaves like dual on the serve side; the wire cutover is C3/C5.
//
// The full ORION_LSDP_MODE flag (including the WS-wire semantics) lands
// in C5; C2 introduces only the persist/serve dimension it needs, inert
// by default.
type LSDPMode string

const (
	LSDPModeBespoke LSDPMode = "bespoke"
	LSDPModeDual    LSDPMode = "dual"
	LSDPModeLSDP    LSDPMode = "lsdp"
)

// PersistsLSML reports whether the mode persists + serves the LSML
// bundle. False for bespoke (the no-op default).
func (m LSDPMode) PersistsLSML() bool {
	return m == LSDPModeDual || m == LSDPModeLSDP
}

// Config is the typed view of Orion's environment. Every field maps to
// a single env var; empty defaults are filled in by Load.
type Config struct {
	ListenAddr         string
	InternalAddr       string
	PublicBaseURL      string
	DatabaseURL        string
	AssetRoot          string
	SolarRoot          string
	ZabAuthValidateURL string
	AuthCacheTTL       time.Duration
	ServiceToken       string
	OperatorToken      string
	ServicePaths       []string
	QuasarBaseURL      string
	CanvasBaseURL      string
	BlueBaseURL        string
	TickHz             int
	PushTimeout        time.Duration
	HTTPPollUserAgent  string
	LogLevel           string
	LogFormat          LogFormat
	LSDPMode           LSDPMode
}

// Load reads env vars, applies defaults, and validates required fields.
// It returns the parsed Config and an error describing every missing
// or malformed value at once (so an operator setting up the service
// sees the full punch list, not one fix-and-retry per round).
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:         getenv("ORION_LISTEN_ADDR", "0.0.0.0:4007"),
		InternalAddr:       getenv("ORION_INTERNAL_ADDR", "0.0.0.0:4017"),
		PublicBaseURL:      strings.TrimRight(getenv("ORION_PUBLIC_BASE_URL", ""), "/"),
		DatabaseURL:        os.Getenv("ORION_DATABASE_URL"),
		AssetRoot:          getenv("ORION_ASSET_ROOT", "/var/lib/orion/assets"),
		SolarRoot:          getenv("ORION_SOLAR_ROOT", "/var/lib/orion/solar"),
		ZabAuthValidateURL: strings.TrimRight(getenv("ORION_ZABAUTH_VALIDATE_URL", ""), "/"),
		ServiceToken:       os.Getenv("ORION_SERVICE_TOKEN"),
		OperatorToken:      os.Getenv("ORION_OPERATOR_TOKEN"),
		ServicePaths:       splitCSV(getenv("ORION_SERVICE_PATHS", "quasar.credentials.read")),
		QuasarBaseURL:      strings.TrimRight(getenv("ORION_QUASAR_BASE_URL", ""), "/"),
		CanvasBaseURL:      strings.TrimRight(getenv("ORION_CANVAS_BASE_URL", ""), "/"),
		BlueBaseURL:        strings.TrimRight(getenv("ORION_BLUE_BASE_URL", ""), "/"),
		HTTPPollUserAgent:  getenv("ORION_HTTP_POLL_USER_AGENT", "orion-poller/1.0"),
		LogLevel:           strings.ToLower(getenv("ORION_LOG_LEVEL", "info")),
	}

	switch strings.ToLower(getenv("ORION_LOG_FORMAT", "json")) {
	case "json":
		cfg.LogFormat = LogFormatJSON
	case "text":
		cfg.LogFormat = LogFormatText
	default:
		problems = append(problems, "ORION_LOG_FORMAT must be 'json' or 'text'")
	}

	switch LSDPMode(strings.ToLower(getenv("ORION_LSDP_MODE", string(LSDPModeBespoke)))) {
	case LSDPModeBespoke:
		cfg.LSDPMode = LSDPModeBespoke
	case LSDPModeDual:
		cfg.LSDPMode = LSDPModeDual
	case LSDPModeLSDP:
		cfg.LSDPMode = LSDPModeLSDP
	default:
		problems = append(problems, "ORION_LSDP_MODE must be 'bespoke', 'dual', or 'lsdp'")
	}

	if v, err := getInt("ORION_TICK_HZ", 60); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 || v > 1000 {
		problems = append(problems, "ORION_TICK_HZ must be in [1, 1000]")
	} else {
		cfg.TickHz = v
	}

	if v, err := getInt("ORION_PUSH_TIMEOUT_S", 10); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 {
		problems = append(problems, "ORION_PUSH_TIMEOUT_S must be > 0")
	} else {
		cfg.PushTimeout = time.Duration(v) * time.Second
	}

	if v, err := getInt("ORION_AUTH_CACHE_TTL_S", 60); err != nil {
		problems = append(problems, err.Error())
	} else if v < 0 {
		problems = append(problems, "ORION_AUTH_CACHE_TTL_S must be >= 0")
	} else {
		cfg.AuthCacheTTL = time.Duration(v) * time.Second
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "ORION_DATABASE_URL is required")
	}
	if cfg.ZabAuthValidateURL == "" {
		problems = append(problems, "ORION_ZABAUTH_VALIDATE_URL is required")
	}
	if cfg.CanvasBaseURL == "" {
		problems = append(problems, "ORION_CANVAS_BASE_URL is required")
	}
	if cfg.BlueBaseURL == "" {
		problems = append(problems, "ORION_BLUE_BASE_URL is required")
	}

	if len(problems) > 0 {
		return Config{}, errors.New("invalid config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// splitCSV splits a comma-separated list into trimmed non-empty parts.
// Used for the service-token `paths` claim ; ZabAuth wants a JSON array.
func splitCSV(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func getInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (got %q)", key, v)
	}
	return n, nil
}
