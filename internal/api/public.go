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
	Store         *store.Store
	Fetcher       compiler.Fetcher
	WSServer      *ws.Server
	StaticDir     http.FileSystem // /static/solar/...
	QuasarBaseURL string          // e.g. http://zabgate:4000/quasar
	ServiceTokens *auth.ServiceTokenManager
}

// RegisterPublic wires every endpoint per ADR 004 § 2. Routes start
// at /api/v1/... — ZabGate strips the /orion prefix before forwarding.
//
// `/health` and `/ready` are also exposed bare (no `/api/v1/`)
// because ZabGate's upstream-health poller calls `{url}/health`
// per the workspace convention (`agents/_shared/conventions.md`).
// Same handler, two paths.
func RegisterPublic(mux *http.ServeMux, deps PublicDeps) {
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /ready", ready(deps))
	mux.HandleFunc("GET /api/v1/health", health)
	mux.HandleFunc("GET /api/v1/ready", ready(deps))

	mux.HandleFunc("POST /api/v1/scenes/{id}/push", pushScene(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/render-bundle", getRenderBundle(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/operator-inputs", getOperatorInputs(deps))
	mux.HandleFunc("GET /api/v1/scenes/{id}/graph", getGraph(deps))
	mux.HandleFunc("POST /api/v1/scenes/{id}/status", postSceneStatus(deps))

	mux.HandleFunc("GET /api/v1/show", getShow(deps))
	mux.HandleFunc("POST /api/v1/show/active-scene", postActiveScene(deps))
	mux.HandleFunc("POST /api/v1/show/test-sessions", postTestSession(deps))

	mux.HandleFunc("GET /api/v1/assets/{id}", getAsset(deps))
	mux.HandleFunc("GET /api/v1/credentials/{id}/stream-key", getStreamKey(deps))

	// WebSocket endpoints. coder/websocket lives behind these handlers.
	mux.HandleFunc("/api/v1/show/stream", deps.WSServer.ServeShowStream)
	mux.HandleFunc("/api/v1/scenes/{id}/test", deps.WSServer.ServeTestSession)

	// Static Solar bundle host (long-TTL immutable cache headers).
	mux.Handle("GET /static/solar/", staticSolarHandler(deps.StaticDir))
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

// requireOperator + requireService are tiny wrappers around the
// auth.FromHeaders gate that every mutating handler reuses.
func requireOperator(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := auth.FromHeaders(r.Header)
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
	default:
		return http.StatusInternalServerError, "INTERNAL"
	}
}
