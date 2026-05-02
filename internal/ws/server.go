// Package ws implements Orion's WebSocket surface (ADR 002).
//
// Two endpoints, both routed through ZabGate at /orion/api/v1/...:
//
//   - /show/stream — live show; viewer / operator / service.
//   - /scenes/{id}/test?session={uuid} — isolated scene preview;
//     operator only.
//
// Auth is trust-first: ZabGate validates the bearer JWT (or the
// show-token query string for Pulsar CEF) on the upgrade and injects
// X-Authenticated-User / X-Authenticated-Role on the upstream
// request. Orion never re-validates a JWT on its own — the gate did.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Server bundles the WS handlers. All state lives in the dependencies
// (Show, Inbox, TestSessionManager); the server itself is stateless.
type Server struct {
	Show    *runtime.Show
	Inbox   *adapters.Inbox
	Test    *runtime.TestSessionManager
	Logger  *slog.Logger
	Metrics *obs.Metrics
}

// ServeShowStream is the live show subscription handler.
func (s *Server) ServeShowStream(w http.ResponseWriter, r *http.Request) {
	id := auth.FromHeaders(r.Header)
	if !id.IsAuthenticated() {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	switch id.Role {
	case auth.RoleViewer, auth.RoleOperator, auth.RoleService, auth.RoleAdmin:
	default:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // ZabGate is the trust boundary
	})
	if err != nil {
		s.Logger.Warn("ws accept failed", "err", err)
		return
	}
	conn := newConnection(c, id, s.Logger, s.Metrics)
	defer conn.close(websocket.StatusNormalClosure, "")

	s.Metrics.WSConnections.WithLabelValues("show", string(id.Role)).Inc()
	defer s.Metrics.WSConnections.WithLabelValues("show", string(id.Role)).Dec()

	if err := s.runShowConnection(r.Context(), conn); err != nil {
		s.Logger.Warn("show ws session ended", "err", err)
	}
}

// ServeTestSession is the per-scene test mode handler.
func (s *Server) ServeTestSession(w http.ResponseWriter, r *http.Request) {
	id := auth.FromHeaders(r.Header)
	if !id.IsAuthenticated() {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if id.Role != auth.RoleOperator && id.Role != auth.RoleAdmin {
		http.Error(w, "operator role required", http.StatusForbidden)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	scene, err := s.Test.Connect(sessionID)
	if err != nil {
		writeWSError(w, http.StatusGone, protocol.CodeTestSessionExpired, err.Error())
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Logger.Warn("ws accept failed", "err", err)
		return
	}
	conn := newConnection(c, id, s.Logger, s.Metrics)
	defer conn.close(websocket.StatusNormalClosure, "")
	defer s.Test.Disconnect(sessionID)

	s.Metrics.WSConnections.WithLabelValues("test", string(id.Role)).Inc()
	defer s.Metrics.WSConnections.WithLabelValues("test", string(id.Role)).Dec()

	if err := s.runTestConnection(r.Context(), conn, scene); err != nil {
		s.Logger.Warn("test ws session ended", "err", err)
	}
}

func (s *Server) runShowConnection(ctx context.Context, conn *connection) error {
	// Wait for `subscribe` first; until then we send nothing.
	if err := conn.expectSubscribe(ctx); err != nil {
		return err
	}

	sub, snap, err := s.Show.SubscribeLive(256)
	if err != nil {
		_ = conn.sendError(ctx, protocol.CodeSceneNotFound, "no active scene", false)
		return err
	}
	defer s.Show.UnsubscribeLive(sub)
	defer sub.Close()

	if err := conn.sendMessage(ctx, snap); err != nil {
		return err
	}

	return conn.run(ctx, sub, func(ctx context.Context, msg *protocol.Input) error {
		// Reject __test.* on the live show endpoint (ADR 002 § 8).
		if strings.HasPrefix(msg.Path, "__test.") {
			return conn.sendError(ctx, protocol.CodeWriteForbidden, "__test.* not allowed on live", false)
		}
		err := s.Inbox.Write(ctx, adapters.Write{
			Identity:    conn.identity,
			Path:        msg.Path,
			Value:       msg.Value,
			Source:      composeSource(conn.identity, msg.Source),
			ClientMsgID: msg.ClientMsgID,
		})
		if errors.Is(err, adapters.ErrWriteForbidden) {
			return conn.sendError(ctx, protocol.CodeWriteForbidden, "scope denied", false)
		}
		return err
	})
}

func (s *Server) runTestConnection(ctx context.Context, conn *connection, scene *runtime.Scene) error {
	if err := conn.expectSubscribe(ctx); err != nil {
		return err
	}
	sub, snap := scene.Subscribe(256)
	defer sub.Close()
	if err := conn.sendMessage(ctx, snap); err != nil {
		return err
	}

	return conn.run(ctx, sub, func(ctx context.Context, msg *protocol.Input) error {
		// Test session accepts __test.* and live-style writes both
		// — adapters.Inbox would refuse __test.*, so we go direct
		// to the cloned scene's inbox here.
		scene.Input(runtime.InputMsg{
			Path:        msg.Path,
			Value:       msg.Value,
			Source:      composeSource(conn.identity, msg.Source),
			ClientMsgID: msg.ClientMsgID,
		})
		return nil
	})
}

func composeSource(id auth.Identity, suggested string) string {
	prefix := string(id.Role) + ":" + id.UserID
	if suggested == "" {
		return prefix
	}
	if strings.HasPrefix(suggested, prefix) {
		return suggested
	}
	// Spoofing of the prefix is rejected at the inbox; here we just
	// concat so the audit trail keeps both signals.
	return prefix + "/" + suggested
}

// writeWSError replies a JSON error envelope on the upgrade
// response, before the WS upgrade has actually happened.
func writeWSError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := map[string]any{
		"type":        "error",
		"v":           protocol.ProtocolVersion,
		"code":        code,
		"message":     message,
		"recoverable": false,
	}
	_ = json.NewEncoder(w).Encode(body)
}

// pingInterval is the server-initiated ping cadence per ADR 002 § 4
// (60 s of silence triggers a server ping). The 10 s pong deadline
// from the same section is enforced by the standard Read timeout in
// connection.run; we don't carry a separate constant for it.
const pingInterval = 60 * time.Second
