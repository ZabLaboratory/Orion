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
	"crypto/subtle"
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

	// AuthSource is the seam through which the show WS endpoints derive
	// the caller Identity — the SAME seam the HTTP gates use (ADR 016
	// §3.2-2). Nil ⇒ HeaderAuthSource: byte-for-byte today's antenne
	// behaviour (read the X-Authenticated-* headers ZabGate injected after
	// validating the JWT / show-token). embedded-local supplies
	// localOperatorAuth so the loopback handshake header X-Orion-Local-Auth
	// is honoured on the WS, not just on HTTP. Only WHO derives the
	// Identity changes; the role checks below are identical either way.
	AuthSource auth.AuthSource
	// LocalViewerToken is accepted only on the embedded-local profile. It
	// exists for Solar browser WebSockets, which cannot send a custom header.
	LocalViewerToken string
	LocalViewerUser  string
}

// identityFrom derives the caller Identity through the configured
// AuthSource, defaulting to HeaderAuthSource so a Server constructed
// without an explicit source keeps the exact antenne header-trust
// behaviour (unit tests, and any antenne wiring that leaves it nil).
func (s *Server) identityFrom(h http.Header) auth.Identity {
	src := s.AuthSource
	if src == nil {
		src = auth.HeaderAuthSource{}
	}
	return src.FromHeaders(h)
}

func (s *Server) identityFromRequest(r *http.Request) auth.Identity {
	id := s.identityFrom(r.Header)
	if id.IsAuthenticated() || s.LocalViewerToken == "" {
		return id
	}
	provided := r.URL.Query().Get("local_viewer_token")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.LocalViewerToken)) != 1 {
		return id
	}
	user := s.LocalViewerUser
	if user == "" {
		user = "local-viewer"
	}
	return auth.Identity{UserID: user, Role: auth.RoleViewer}
}

// ServeShowStream is the live show subscription handler.
func (s *Server) ServeShowStream(w http.ResponseWriter, r *http.Request) {
	id := s.identityFromRequest(r)
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
	id := s.identityFrom(r.Header)
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

	// Writer-vs-viewer contract on /show/stream:
	//
	//   - A `service`-role client (Quasar) is a scene-independent
	//     WRITER: it pushes platform-event input leaves continuously,
	//     outside any scene cycle. It must stay connected even with no
	//     active scene, otherwise every event at show start / scene
	//     switch is lost and the coupling is fragile. SubscribeLiveWriter
	//     never errors on an empty active pointer; when a scene later
	//     activates, SetActive migrates this subscription onto it.
	//
	//   - A viewer / operator needs an active scene to receive deltas;
	//     with none, SubscribeLive returns ErrSceneNotFound and we close
	//     with SCENE_NOT_FOUND as before.
	var (
		sub  *runtime.Subscription
		snap *protocol.Snapshot
	)
	if conn.identity.Role == auth.RoleService {
		sub, snap = s.Show.SubscribeLiveWriter(256)
	} else {
		var err error
		sub, snap, err = s.Show.SubscribeLive(256)
		if err != nil {
			_ = conn.sendError(ctx, protocol.CodeSceneNotFound, "no active scene", false)
			return err
		}
	}
	defer s.Show.UnsubscribeLive(sub)
	defer sub.Close()

	// snap is nil for a writer that connected with no active scene —
	// there is no scene to snapshot yet. Skip the initial frame; the
	// writer receives its first snapshot when SetActive migrates it.
	if snap != nil {
		if err := conn.sendMessage(ctx, snap); err != nil {
			return err
		}
	}

	return conn.run(ctx, sub, func(inputCtx context.Context, msg *protocol.Input) error {
		// Reject __test.* on the live show endpoint (ADR 002 § 8).
		if strings.HasPrefix(msg.Path, "__test.") {
			return conn.sendError(inputCtx, protocol.CodeWriteForbidden, "__test.* not allowed on live", false)
		}
		err := s.Inbox.Write(inputCtx, adapters.Write{
			Identity:    conn.identity,
			Path:        msg.Path,
			Value:       msg.Value,
			Source:      composeSource(conn.identity, msg.Source),
			ClientMsgID: msg.ClientMsgID,
		})
		if errors.Is(err, adapters.ErrWriteForbidden) {
			return conn.sendError(inputCtx, protocol.CodeWriteForbidden, "scope denied", false)
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

	return conn.run(ctx, sub, func(_ context.Context, msg *protocol.Input) error {
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
