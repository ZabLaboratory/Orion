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

// Profile is the embedded-local execution-profile flag (ADR 016 §3.3).
// It is purely additive: it selects which edge implementations
// (Store, AuthSource, Fetcher) are wired AT BOOT and the listen
// posture — it introduces no branch on the hot path (requireOperator,
// db.query, tick, inbox). An unset or "antenne" value reproduces
// today's production behaviour exactly (RC-1).
//
//   - antenne (default): pgStore + headerAuth + httpFetcher, binding on
//     ORION_LISTEN_ADDR as configured (0.0.0.0:4007 in prod). The strict
//     production path — every existing test exercises this.
//   - embedded-local: the single-binary, zero-infra Prism sidecar
//     posture. Listen collapses to loopback only. The local edge impls
//     (sqliteStore #222, localOperatorAuth #223, bundledFetcher) land in
//     follow-up issues; until then this profile wires the same defaults,
//     so it boots without panicking and shares the antenne hot path.
type Profile string

const (
	ProfileAntenne       Profile = "antenne"
	ProfileEmbeddedLocal Profile = "embedded-local"
)

// IsEmbeddedLocal reports whether the embedded-local edge wiring is
// selected. The hot path never consults this — only boot wiring does.
func (p Profile) IsEmbeddedLocal() bool { return p == ProfileEmbeddedLocal }

// IsAntenne reports whether this is the on-air profile. It is the POSITIVE
// form deliberately: the durable service-token model is armed only for antenne
// (ADR ZabAuth 003 Am.3 § A3.3 part 3), and a guard written as "not
// embedded-local" would silently arm any third profile added later. Load
// rejects anything outside the two known values, so today the two forms agree;
// the positive one keeps agreeing tomorrow.
func (p Profile) IsAntenne() bool { return p == ProfileAntenne }

// Config is the typed view of Orion's environment. Every field maps to
// a single env var; empty defaults are filled in by Load.
type Config struct {
	ListenAddr    string
	InternalAddr  string
	PublicBaseURL string
	DatabaseURL   string
	AssetRoot     string
	SolarRoot     string
	// CompilerCacheDir roots the compiler's content-addressed disk cache for
	// immutable upstream fetches (Canvas layout + pinned Blue blueprint
	// graph), ORION_COMPILER_CACHE_DIR. Empty disables the cache (every push
	// re-fetches over the WAN, the pre-cache behaviour). Content-addressed →
	// no eviction / TTL; a changed artefact carries a new key.
	CompilerCacheDir   string
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

	// --- durable service token (ADR ZabAuth 003 Am.3 § A3.3 parts 1/2/5) ---
	//
	// ServiceRefreshToken is the étage-1 bootstrap refresh token
	// (ORION_SERVICE_REFRESH_TOKEN, ADR ZabAuth 003 Am.3 § A3.3 part 1). It is
	// an amorce: resolved only when the database holds nothing, consumed by the
	// first rotation, then purged at étage 1 — never rewritten by the service.
	ServiceRefreshToken string
	// EncryptionKey is base64 of the 32 random bytes that encrypt the durable
	// refresh token at rest (ORION_ENCRYPTION_KEY, § A3.3 part 2). Absent or
	// malformed ⇒ the durable manager refuses to arm; it never degrades to a
	// plaintext write, and it never fails the boot either.
	EncryptionKey string

	// --- scene-validation gate (ADR 003 §3.2, issue #87) ---
	// ValidationMaxSteps / ValidationMaxWall bound one entrypoint's proof
	// (env-tunable, ADR §3.2.1 defaults 1 M steps / 5 s). The budget
	// bounds the PROOF, never the engine: a divergent logic crosses it,
	// fails validation, and never reaches air. ValidationTimeout bounds a
	// whole campaign's persist context.
	ValidationMaxSteps uint64
	ValidationMaxWall  time.Duration
	ValidationTimeout  time.Duration

	// --- phase-3 async effects (ADR 003 §3.1.3, issue #85) ---
	// Parsed and validated here so the étage-1 contract is fixed; the
	// runtime wiring is gated behind the phase-4 validation gate (R9 —
	// exec stays dormant in prod until #87).
	//
	// HTTPEgressAllowHosts is the `http.request` host allowlist
	// (ORION_HTTP_EGRESS_ALLOW_HOSTS, CSV of hostnames). Empty =
	// deny-all (fail-closed).
	HTTPEgressAllowHosts []string
	// HTTPEgressAllowHTTP relaxes the https-only egress policy
	// (ORION_HTTP_EGRESS_ALLOW_HTTP, default false).
	HTTPEgressAllowHTTP bool
	// DataSources is the parsed ORION_DATASOURCES allowlist
	// (`<logical_name>=<zabgate_svc>`, CSV). Empty = no db.query
	// DataSource declared (DATASOURCE_NOT_DECLARED at compile).
	DataSources map[string]string
	// ZabGateURL is the gateway base the `_query` delegation calls
	// (ORION_ZABGATE_URL). Required iff DataSources is non-empty.
	ZabGateURL string
	// EffectWorkers / EffectQueue bound the async-effect worker pool
	// (ORION_EFFECT_WORKERS / ORION_EFFECT_QUEUE).
	EffectWorkers int
	EffectQueue   int

	// EgressBudgetPerStream / EgressBudgetWindowS are the per-stream
	// `core.service.call@1` egress budget (ADR Blue 009 Amendment 2 §B,
	// item 9 — the R3 condition of the G0 Bastion clearance). At most
	// EgressBudgetPerStream curated egress calls per EgressBudgetWindowS
	// seconds, PER STREAM (the live show, or an isolated preview / test
	// session). Over budget ⇒ the node fails closed to its `error` port
	// (EGRESS_BUDGET_EXCEEDED), never a request. ORION_EGRESS_BUDGET_PER_STREAM
	// (default 60) and ORION_EGRESS_BUDGET_WINDOW_S (default 10). Setting
	// the budget to 0 disables the bound (opt-out, logged at boot).
	EgressBudgetPerStream int
	EgressBudgetWindowS   int

	// ViewerCredsRefreshS is the rotation interval for the stream-level Meet
	// viewer-credentials arming on the LSDP wire (ADR Blue 009 §3.2, issue
	// #261 — R1). Every ViewerCredsRefreshS seconds Orion re-fetches the
	// receive-only viewer credentials of the armed rooms and re-emits them on
	// `__cam.viewer`, so a short-lived/rotated token is replaced before it
	// expires. ORION_VIEWER_CREDS_REFRESH_S (default 240). 0 disables the
	// ticker (arming still re-runs on a slot-assignment change).
	ViewerCredsRefreshS int

	HTTPPollUserAgent string
	LogLevel          string
	LogFormat         LogFormat
	LSDPMode          LSDPMode
	// Profile selects the execution-profile edge wiring at boot
	// (ORION_PROFILE, ADR 016 §3.3). Default antenne = unchanged prod.
	Profile Profile

	// SQLitePath is the embedded-local store file (ADR 016 §3.2, #222),
	// ORION_SQLITE_PATH. Unused in antenne; required in embedded-local.
	// SceneBundlePath is the frozen scene bundle the bundledFetcher serves
	// (#224), ORION_SCENE_BUNDLE_PATH. Since #246 it is OPTIONAL in
	// embedded-local — an offline fallback only; unset selects the nominal
	// httpFetcher path. Unused in antenne.
	SQLitePath      string
	SceneBundlePath string

	// ValidationMirrorRoot is the embedded-local validation mirror root
	// (ORION_VALIDATION_MIRROR_ROOT, ADR 016 Amendment 1 / #247): the
	// directory that contains `canvas/validated/<scene_id>/<bare-64hex>.json`
	// seeds. In embedded-local the air-eligibility gate imports the
	// `validated` record from this mirror instead of a DB row; required
	// there, unused in antenne.
	ValidationMirrorRoot string

	// LocalAuthSecret is the Prism↔Orion handshake secret used by
	// localOperatorAuth (ORION_LOCAL_OPERATOR_SECRET, ADR 016 §3.2-2). Prism
	// generates it at sidecar spawn and passes it via env; Orion requires
	// it on every loopback request before granting operator. Consulted
	// ONLY in embedded-local; required there (boot fails without it so the
	// sidecar can never grant operator unguarded — ADR 016 §5 R2).
	LocalAuthSecret string
	// LocalAuthUser is the cosmetic user id stamped on the local operator
	// Identity (ORION_LOCAL_AUTH_USER). Optional; role is what gates.
	LocalAuthUser string
}

// Load reads env vars, applies defaults, and validates required fields.
// It returns the parsed Config and an error describing every missing
// or malformed value at once (so an operator setting up the service
// sees the full punch list, not one fix-and-retry per round).
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:           getenv("ORION_LISTEN_ADDR", "0.0.0.0:4007"),
		InternalAddr:         getenv("ORION_INTERNAL_ADDR", "0.0.0.0:4017"),
		PublicBaseURL:        strings.TrimRight(getenv("ORION_PUBLIC_BASE_URL", ""), "/"),
		DatabaseURL:          os.Getenv("ORION_DATABASE_URL"),
		AssetRoot:            getenv("ORION_ASSET_ROOT", "/var/lib/orion/assets"),
		SolarRoot:            getenv("ORION_SOLAR_ROOT", "/var/lib/orion/solar"),
		CompilerCacheDir:     getenv("ORION_COMPILER_CACHE_DIR", "/var/lib/orion/compiler-cache"),
		ZabAuthValidateURL:   strings.TrimRight(getenv("ORION_ZABAUTH_VALIDATE_URL", ""), "/"),
		ServiceToken:         os.Getenv("ORION_SERVICE_TOKEN"),
		OperatorToken:        os.Getenv("ORION_OPERATOR_TOKEN"),
		ServicePaths:         splitCSV(getenv("ORION_SERVICE_PATHS", "quasar.credentials.read")),
		ServiceRefreshToken:  os.Getenv("ORION_SERVICE_REFRESH_TOKEN"),
		EncryptionKey:        os.Getenv("ORION_ENCRYPTION_KEY"),
		QuasarBaseURL:        strings.TrimRight(getenv("ORION_QUASAR_BASE_URL", ""), "/"),
		CanvasBaseURL:        strings.TrimRight(getenv("ORION_CANVAS_BASE_URL", ""), "/"),
		BlueBaseURL:          strings.TrimRight(getenv("ORION_BLUE_BASE_URL", ""), "/"),
		SQLitePath:           getenv("ORION_SQLITE_PATH", ""),
		SceneBundlePath:      getenv("ORION_SCENE_BUNDLE_PATH", ""),
		ValidationMirrorRoot: getenv("ORION_VALIDATION_MIRROR_ROOT", ""),
		HTTPPollUserAgent:    getenv("ORION_HTTP_POLL_USER_AGENT", "orion-poller/1.0"),
		LogLevel:             strings.ToLower(getenv("ORION_LOG_LEVEL", "info")),
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

	// Execution profile (ADR 016 §3.3). Additive: default antenne is
	// byte-for-byte today's behaviour. embedded-local only changes boot
	// wiring (edge impls + loopback listen), never the hot path.
	switch Profile(strings.ToLower(getenv("ORION_PROFILE", string(ProfileAntenne)))) {
	case ProfileAntenne:
		cfg.Profile = ProfileAntenne
	case ProfileEmbeddedLocal:
		cfg.Profile = ProfileEmbeddedLocal
		// Loopback-only posture: the embedded sidecar must never be
		// reachable off-host (ADR 016 §3.3, D4 — refined in #223). If the
		// operator left the listen addrs at their 0.0.0.0 prod defaults,
		// pin them to loopback; an explicit override is respected.
		if _, ok := os.LookupEnv("ORION_LISTEN_ADDR"); !ok {
			cfg.ListenAddr = "127.0.0.1:4007"
		}
		if _, ok := os.LookupEnv("ORION_INTERNAL_ADDR"); !ok {
			cfg.InternalAddr = "127.0.0.1:4017"
		}
		// Handshake secret (ADR 016 §3.2-2, RC-4). Required in
		// embedded-local: without it localOperatorAuth would grant operator
		// to any loopback caller (R2). Fail the boot rather than open that.
		cfg.LocalAuthSecret = os.Getenv("ORION_LOCAL_OPERATOR_SECRET")
		cfg.LocalAuthUser = getenv("ORION_LOCAL_AUTH_USER", "local-operator")
		if cfg.LocalAuthSecret == "" {
			problems = append(problems, "ORION_LOCAL_OPERATOR_SECRET is required when ORION_PROFILE=embedded-local")
		}
	default:
		problems = append(problems, "ORION_PROFILE must be 'antenne' or 'embedded-local'")
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

	// Phase-3 async-effect config (issue #85). Parsed fail-closed: the
	// egress allowlist defaults to empty (deny-all), https-only.
	cfg.HTTPEgressAllowHosts = splitCSV(getenv("ORION_HTTP_EGRESS_ALLOW_HOSTS", ""))
	switch strings.ToLower(getenv("ORION_HTTP_EGRESS_ALLOW_HTTP", "false")) {
	case "false", "0", "no":
		cfg.HTTPEgressAllowHTTP = false
	case "true", "1", "yes":
		cfg.HTTPEgressAllowHTTP = true
	default:
		problems = append(problems, "ORION_HTTP_EGRESS_ALLOW_HTTP must be a boolean")
	}
	cfg.ZabGateURL = strings.TrimRight(getenv("ORION_ZABGATE_URL", ""), "/")
	cfg.DataSources = map[string]string{}
	for _, part := range splitCSV(getenv("ORION_DATASOURCES", "")) {
		name, svc, ok := strings.Cut(part, "=")
		name, svc = strings.TrimSpace(name), strings.TrimSpace(svc)
		if !ok || name == "" || svc == "" {
			problems = append(problems, fmt.Sprintf("ORION_DATASOURCES entry %q is not <name>=<svc>", part))
			continue
		}
		if _, dup := cfg.DataSources[name]; dup {
			problems = append(problems, fmt.Sprintf("ORION_DATASOURCES duplicate name %q", name))
			continue
		}
		cfg.DataSources[name] = svc
	}
	if len(cfg.DataSources) > 0 && cfg.ZabGateURL == "" {
		problems = append(problems, "ORION_ZABGATE_URL is required when ORION_DATASOURCES is set")
	}
	if v, err := getInt("ORION_EFFECT_WORKERS", 8); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 {
		problems = append(problems, "ORION_EFFECT_WORKERS must be > 0")
	} else {
		cfg.EffectWorkers = v
	}
	if v, err := getInt("ORION_EFFECT_QUEUE", 256); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 {
		problems = append(problems, "ORION_EFFECT_QUEUE must be > 0")
	} else {
		cfg.EffectQueue = v
	}

	// Per-stream egress budget (ADR Blue 009 §B / R3). 0 disables the
	// bound; the window must be > 0 whenever the budget is enabled.
	if v, err := getInt("ORION_EGRESS_BUDGET_PER_STREAM", 60); err != nil {
		problems = append(problems, err.Error())
	} else if v < 0 {
		problems = append(problems, "ORION_EGRESS_BUDGET_PER_STREAM must be >= 0")
	} else {
		cfg.EgressBudgetPerStream = v
	}
	if v, err := getInt("ORION_EGRESS_BUDGET_WINDOW_S", 10); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 {
		problems = append(problems, "ORION_EGRESS_BUDGET_WINDOW_S must be > 0")
	} else {
		cfg.EgressBudgetWindowS = v
	}

	// Viewer-credentials rotation interval (ADR Blue 009 §3.2 / R1). 0
	// disables the refresh ticker; a slot-assignment change still re-arms.
	if v, err := getInt("ORION_VIEWER_CREDS_REFRESH_S", 240); err != nil {
		problems = append(problems, err.Error())
	} else if v < 0 {
		problems = append(problems, "ORION_VIEWER_CREDS_REFRESH_S must be >= 0")
	} else {
		cfg.ViewerCredsRefreshS = v
	}

	// Scene-validation budgets (issue #87). 0 = use the ADR default
	// (1 M steps / 5 s). A campaign persist context defaults to 60 s.
	if v, err := getInt("ORION_VALIDATION_MAX_STEPS", 1_000_000); err != nil {
		problems = append(problems, err.Error())
	} else if v < 0 {
		problems = append(problems, "ORION_VALIDATION_MAX_STEPS must be >= 0")
	} else {
		cfg.ValidationMaxSteps = uint64(v)
	}
	if v, err := getInt("ORION_VALIDATION_MAX_WALL_S", 5); err != nil {
		problems = append(problems, err.Error())
	} else if v < 0 {
		problems = append(problems, "ORION_VALIDATION_MAX_WALL_S must be >= 0")
	} else {
		cfg.ValidationMaxWall = time.Duration(v) * time.Second
	}
	if v, err := getInt("ORION_VALIDATION_TIMEOUT_S", 60); err != nil {
		problems = append(problems, err.Error())
	} else if v <= 0 {
		problems = append(problems, "ORION_VALIDATION_TIMEOUT_S must be > 0")
	} else {
		cfg.ValidationTimeout = time.Duration(v) * time.Second
	}

	// Required-field punch list is profile-keyed (ADR 016 §3.2/§3.3, refined by
	// Amendment 1 / #246 and the full-prod pivot). antenne needs its Postgres
	// DSN + Canvas/Blue HTTP bases. embedded-local needs the local SQLite file +
	// the three base-URLs (Canvas/Blue/ZabGate) — pointed at PROD ZabGate under
	// the full-prod model (the httpFetcher reads prod artefacts directly; no
	// local mirror). The frozen scene bundle and the validation mirror root are
	// both OPTIONAL offline fallbacks. The pg DSN stays unused in embedded-local
	// (SQLite store). ZabAuth stays required in both (validator/service-token
	// plumbing is shared).
	if cfg.Profile.IsEmbeddedLocal() {
		if cfg.SQLitePath == "" {
			problems = append(problems, "ORION_SQLITE_PATH is required in the embedded-local profile")
		}
		// ORION_SCENE_BUNDLE_PATH is OPTIONAL here (offline fallback) — see #246.
		if cfg.CanvasBaseURL == "" {
			problems = append(problems, "ORION_CANVAS_BASE_URL is required in the embedded-local profile")
		}
		if cfg.BlueBaseURL == "" {
			problems = append(problems, "ORION_BLUE_BASE_URL is required in the embedded-local profile")
		}
		if cfg.ZabGateURL == "" {
			problems = append(problems, "ORION_ZABGATE_URL is required in the embedded-local profile")
		}
		// ORION_VALIDATION_MIRROR_ROOT is OPTIONAL (full-prod model): the local
		// push→validate→activate chain writes the `validated` record to the
		// store, which the air gate reads. Set it only to opt into a seeded
		// validated-record mirror as an offline fallback (see cmd/orion).
	} else {
		if cfg.DatabaseURL == "" {
			problems = append(problems, "ORION_DATABASE_URL is required")
		}
		if cfg.CanvasBaseURL == "" {
			problems = append(problems, "ORION_CANVAS_BASE_URL is required")
		}
		if cfg.BlueBaseURL == "" {
			problems = append(problems, "ORION_BLUE_BASE_URL is required")
		}
	}
	if cfg.ZabAuthValidateURL == "" {
		problems = append(problems, "ORION_ZABAUTH_VALIDATE_URL is required")
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
