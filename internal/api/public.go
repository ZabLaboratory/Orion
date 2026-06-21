// Package api wires the public HTTP and WebSocket surface onto a
// mux. Resource handlers live in adjacent files; this file is the
// single registration entry point cmd/orion/main.go calls.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
	"github.com/ZabLaboratory/Orion/internal/ws"
)

// PublicDeps groups every dependency the public router needs.
type PublicDeps struct {
	Logger        *slog.Logger
	Metrics       *obs.Metrics
	Config        config.Config
	Show          *runtime.Show
	Inbox         *adapters.Inbox
	Test          *runtime.TestSessionManager
	Store         store.Store
	Fetcher       compiler.Fetcher
	WSServer      *ws.Server

	// Harness runs scene-validation campaigns (ADR 003 §3.2, issue #87).
	// ValidationRunner serialises campaigns per (scene, version).
	Harness          *runtime.Harness
	ValidationRunner *validationRunner
	StaticDir     http.FileSystem // /static/solar/...
	QuasarBaseURL string          // e.g. http://zabgate:4000/quasar
	ServiceTokens *auth.ServiceTokenManager

	// SchemaClient fetches a datasource's read-only catalog (`_schema`)
	// for the DB-catalog surface (ADR Blue 008 §3.4). Nil ⇒ the catalog
	// routes degrade (503 / empty listing); no behaviour change otherwise.
	SchemaClient *effects.SchemaClient

	// LSDPHandler is the lumencast-go LSDP/1.1 WebSocket handler
	// (ADR 007 §C.3b). Non-nil only in dual/lsdp mode; in bespoke mode
	// it is nil and the LSDP route is not registered (no-op deploy).
	// The handler internally routes /lsdp.v1, so the public route
	// rewrites the path to it before delegating.
	LSDPHandler http.Handler

	// AuthSource is the seam through which requireOperator derives the
	// request Identity (ADR 016 §3.2-2). Nil ⇒ HeaderAuthSource (the
	// antenne default: read the X-Authenticated-* headers ZabGate
	// injected). embedded-local supplies localOperatorAuth instead. The
	// gate logic (role check) is byte-for-byte identical either way —
	// only WHO derives the Identity changes.
	AuthSource auth.AuthSource
}

// RegisterPublic wires every endpoint per ADR 004 § 2. Routes start
// at /api/v1/... — ZabGate strips the /orion prefix before forwarding.
//
// `/health` and `/ready` are also exposed bare (no `/api/v1/`)
// because ZabGate's upstream-health poller calls `{url}/health`
// per the workspace convention (`agents/_shared/conventions.md`).
// Same handler, two paths.
func RegisterPublic(mux *http.ServeMux, deps PublicDeps) {
	if deps.ValidationRunner == nil {
		deps.ValidationRunner = newValidationRunner()
	}
	// Select the identity source for the auth gates (ADR 016 §3.2-2).
	// Default = HeaderAuthSource: byte-for-byte today's antenne behaviour.
	if deps.AuthSource != nil {
		authSource = deps.AuthSource
	} else {
		authSource = auth.HeaderAuthSource{}
	}
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /ready", ready(deps))
	mux.HandleFunc("GET /api/v1/health", health)
	mux.HandleFunc("GET /api/v1/ready", ready(deps))

	mux.HandleFunc("POST /api/v1/scenes/{id}/push", pushScene(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/render-bundle", getRenderBundle(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/lsml-bundle", getLSMLBundle(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/operator-inputs", getOperatorInputs(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/graph", getGraph(deps))
	mux.HandleFunc("POST /api/v1/scenes/{id}/status", postSceneStatus(deps))
	// Scene-validation gate (ADR 003 §3.2.2, issue #87).
	mux.HandleFunc("POST /api/v1/scenes/{id}/validate", postValidate(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/validation", getValidation(deps))
	// External animation completion report (B-syswrite, issue #86).
	// R9: registered but inert until the phase-4 gate — exec dormant in
	// prod, so every report drops as an unknown wake key, 202.
	mux.HandleFunc("POST /api/v1/scenes/{id}/exec/completion", postExecCompletion(deps))

	// Service-scoped simulate (ADR 015, issues #194/#195/#196/#197): a
	// synchronous dry-run of a draft graph against a synthetic event for
	// the bluemcp agent. Separate /validate/* surface gated by exact scope
	// `orion.validate.session` — NOT under /show/* (operator-only path
	// untouched: regression-guard R2). No persistence, zero effect (B10).
	mux.HandleFunc("POST /api/v1/validate/simulate", postSimulate(deps))

	mux.HandleFunc("GET /api/v1/show", getShow(deps))
	mux.HandleFunc("POST /api/v1/show/active-scene", postActiveScene(deps))
	mux.HandleFunc("POST /api/v1/show/test-sessions", postTestSession(deps))
	// Stream-level Blue rules (ADR 009 §3.1, issue #154): operator-gated
	// promotion/demotion of a roster scene into an always-on rule.
	mux.HandleFunc("POST /api/v1/show/stream-rules", postStreamRule(deps))
	mux.HandleFunc("DELETE /api/v1/show/stream-rules/{id}", deleteStreamRule(deps))

	// DB catalog surface (ADR Blue 008 §3.4, issue #211): read-only
	// introspection of whitelisted datasources for cockpit selectors.
	mux.HandleFunc("GET /api/v1/db/datasources", listDatasources(deps))
	mux.HandleFunc("GET /api/v1/db/{service}/schema", getDBSchema(deps))

	// Operator runtime surface (Orion #209, Blue ADR 008 §3.2/§3.3):
	// operator-gated injection of values into the ACTIVE scene's live exec
	// graph — fire a named on-call entrypoint, list suspension points, and
	// resolve an await. Active-only (ADR 008): a dormant blueprint answers
	// 409 (call) / 410 (resolve) / empty (pending).
	mux.HandleFunc("POST /api/v1/operator/call/{blueprint_id}/{entrypoint_id}", postOperatorCall(deps))
	mux.HandleFunc("GET /api/v1/runtime/{blueprint_id}/pending", getRuntimePending(deps))
	mux.HandleFunc("POST /api/v1/operator/resolve/{blueprint_id}/{await_name}", postOperatorResolve(deps))

	// Cockpit contract aggregate (Orion #210, Blue ADR 008 §3.5): the single
	// derived read the cockpit uses to render the live operator UI of every
	// active rule — params + triggers + awaits over the active scene (scope
	// `scene`) and promoted stream-level rules (scope `stream`). Operator-gated
	// (reveals the live operator surface); read-only, never stored.
	mux.HandleFunc("GET /api/v1/cockpit/contracts", getCockpitContracts(deps))

	mux.HandleFunc("GET /api/v1/assets/{id}", getAsset(deps))
	mux.HandleFunc("GET /api/v1/credentials/{id}/stream-key", getStreamKey(deps))

	// WebSocket endpoints. coder/websocket lives behind these handlers.
	mux.HandleFunc("/api/v1/show/stream", deps.WSServer.ServeShowStream)
	mux.HandleFunc("/api/v1/scenes/{id}/test", deps.WSServer.ServeTestSession)

	// LSDP/1.1 wire (ADR 007 §C.3b) — distinct route, dual-served beside
	// the bespoke /show/stream above. Registered only in dual/lsdp mode
	// (LSDPHandler nil ⇒ bespoke ⇒ route absent, no behaviour change).
	// The kit handler routes its own /lsdp.v1 path, so we rewrite the
	// request path before delegating.
	if deps.LSDPHandler != nil {
		mux.Handle("/api/v1/show/stream.lsdp", lsdpRoute(deps.LSDPHandler))
	}

	// Static Solar bundle host (long-TTL immutable cache headers).
	mux.Handle("GET /static/solar/", staticSolarHandler(deps.StaticDir))
}

// lsdpRoute rewrites the public LSDP route onto the path the kit's Mux
// expects (/lsdp.v1) and delegates. The kit reads identity from the
// upgrade request via Config.IdentityFromRequest (header-trust); the
// rewrite preserves the request headers ZabGate injected.
func lsdpRoute(kit http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		kit.ServeHTTP(w, r2)
	})
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "orion",
	})
}

// ready reports DB ping + show roster status. Returns 503 if either
// fails so a load balancer can pull traffic during a degraded boot.
func ready(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status":  "ok",
			"service": "orion",
		}
		if deps.Store != nil {
			if err := deps.Store.Ping(r.Context()); err != nil {
				body["status"] = "degraded"
				body["database"] = "down"
				writeJSON(w, http.StatusServiceUnavailable, body)
				return
			}
			body["database"] = "ok"
		}
		body["scenes_loaded"] = len(deps.Show.IDs())
		writeJSON(w, http.StatusOK, body)
	}
}

// authSource is the package-level identity source the auth gates read
// through (ADR 016 §3.2-2). Set once by RegisterPublic from
// PublicDeps.AuthSource; defaults to HeaderAuthSource so the gate stays
// byte-for-byte the antenne behaviour when nothing is wired (e.g. in unit
// tests that construct requests with X-Authenticated-* headers directly).
var authSource auth.AuthSource = auth.HeaderAuthSource{}

// requireOperator + requireService are tiny wrappers around the
// authSource gate that every mutating handler reuses. The gate logic is
// unchanged from when it called auth.FromHeaders directly — only the
// identity SOURCE is now pluggable (HeaderAuthSource on antenne,
// localOperatorAuth on embedded-local).
func requireOperator(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := authSource.FromHeaders(r.Header)
		if !id.IsAuthenticated() || (id.Role != auth.RoleOperator && id.Role != auth.RoleAdmin) {
			http.Error(w, "operator role required", http.StatusForbidden)
			return
		}
		handler(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// codeFromError maps store errors to API error codes.
func codeFromError(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, runtime.ErrSceneNotFound):
		return http.StatusNotFound, "SCENE_NOT_FOUND"
	case errors.Is(err, runtime.ErrSceneNotPushed):
		return http.StatusConflict, "SCENE_NOT_PUSHED"
	case errors.Is(err, store.ErrSceneInUse):
		return http.StatusConflict, "SCENE_IN_USE"
	case errors.Is(err, runtime.ErrRuleIsActiveScene):
		// ADR 009 §3.1 criterion #5: a rule and the active scene are
		// disjoint roles — promoting the active scene is refused.
		return http.StatusConflict, "RULE_IS_ACTIVE_SCENE"
	default:
		return http.StatusInternalServerError, "INTERNAL"
	}
}
