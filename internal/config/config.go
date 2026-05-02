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
	CanvasBaseURL      string
	BlueBaseURL        string
	TickHz             int
	PushTimeout        time.Duration
	HTTPPollUserAgent  string
	LogLevel           string
	LogFormat          LogFormat
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
