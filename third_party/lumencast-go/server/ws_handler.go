package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Lumencast/lumencast-go/protocol"
)

// serveLSDP is the WebSocket subscribe handler. It implements the
// LSDP/1.x lifecycle :
//
//  1. Negotiate Sec-WebSocket-Protocol — `lsdp.v1.1` preferred,
//     `lsdp.v1` accepted for backward compat. Anything else → close
//     with policy violation.
//  2. Accept the upgrade.
//  3. Read the first Subscribe frame within SubscribeTimeout.
//  4. Authenticate the token ; pick a scene ; emit Snapshot.
//  5. Run the read/write loops until ctx end or peer close.
func (s *Server) serveLSDP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: protocol.SubProtocols,
	})
	if err != nil {
		s.logger.Warn("ws accept failed", "err", err)
		return
	}
	negotiated := c.Subprotocol()
	if negotiated != protocol.SubProtocolV1_1 && negotiated != protocol.SubProtocol {
		_ = c.Close(websocket.StatusPolicyViolation, "lsdp.v1 or lsdp.v1.1 subprotocol required")
		return
	}
	c.SetReadLimit(s.cfg.MaxFrameBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 1. Read Subscribe.
	subCtx, subCancel := context.WithTimeout(ctx, s.cfg.SubscribeTimeout)
	subFrame, err := readSubscribe(subCtx, c)
	subCancel()
	if err != nil {
		// Spec § 13 : a server that receives a subscribe with v != 1
		// while only supporting v = 1 MUST close with 1002.
		if errors.Is(err, protocol.ErrVersionMismatch) {
			_ = c.Close(websocket.StatusProtocolError, "version mismatch")
			return
		}
		_ = sendError(ctx, c, 0, protocol.CodeAuthDenied, err.Error(), false)
		_ = c.Close(websocket.StatusPolicyViolation, "subscribe expected")
		return
	}

	// 2. Authenticate. Two sources of identity, selected at config time :
	//   - header-trust seam (ADR 007 §C.3a) : a trusted front-proxy has
	//     already authenticated the caller, so we derive the Identity from
	//     the upgrade request and ignore the Subscribe token entirely ;
	//   - token path (default) : validate Subscribe.Token via Auth.
	// The downstream lifecycle (resolveScene, subscribeWithResume, write
	// loop, CanWrite) is identical regardless of source.
	var id Identity
	if s.cfg.IdentityFromRequest != nil {
		id, err = s.cfg.IdentityFromRequest(r)
	} else {
		id, err = s.cfg.Auth.Authenticate(ctx, subFrame.Token)
	}
	if err != nil || !id.IsAuthenticated() {
		_ = sendError(ctx, c, 0, protocol.CodeAuthDenied, "token invalid", false)
		_ = c.Close(websocket.StatusPolicyViolation, "auth denied")
		return
	}

	// 3. Resolve scene.
	scene, errCode, errMsg := s.resolveScene(subFrame, id)
	if scene == nil {
		_ = sendError(ctx, c, 0, errCode, errMsg, false)
		_ = c.Close(websocket.StatusPolicyViolation, errMsg)
		return
	}

	// 3b. A scene whose backing bundle failed validation is unservable :
	// serve the recorded error instead of a snapshot of a scene built
	// from a bundle we rejected.
	if rej := scene.Rejection(); rej != nil {
		_ = sendError(ctx, c, scene.seq.Current(), rej.Code, rej.Message, false)
		_ = c.Close(websocket.StatusPolicyViolation, "bundle rejected")
		return
	}

	// 4. Subscribe + ship initial frames. Honours LSDP/1.1
	// `since_sequence` resume — when the replay buffer covers the gap,
	// we ship a delta stream from since_sequence+1 forward instead of
	// a fresh snapshot (§4.1, §18).
	live := subFrame.Scene == ""
	proto11 := negotiated == protocol.SubProtocolV1_1
	sub, snap, replay := scene.subscribeWithResume(256, live, proto11, subFrame.SinceSequence)
	defer s.detach(sub)
	if snap != nil {
		if err := sendFrame(ctx, c, snap); err != nil {
			s.logger.Debug("snapshot send failed", "err", err)
			return
		}
	} else {
		// Replay path — ship the buffered deltas before entering the
		// main loop. Each carries its original (per-scene) seq.
		for _, r := range replay {
			md := r.projectionMetadata
			var (
				schemaVersion     string
				sceneDigest       string
				runtimeInstanceID string
				target            string
				renderRevision    string
				correlationID     string
			)
			if md != nil {
				schemaVersion = md.SchemaVersion
				sceneDigest = md.SceneDigest
				runtimeInstanceID = md.RuntimeInstanceID
				target = md.Target
				renderRevision = md.RenderRevision
				correlationID = md.CorrelationID
			}
			d := &protocol.Delta{
				Seq:               r.seq,
				Patches:           r.patches,
				Cause:             r.cause,
				SchemaVersion:     schemaVersion,
				SceneDigest:       sceneDigest,
				RuntimeInstanceID: runtimeInstanceID,
				Target:            target,
				RenderRevision:    renderRevision,
				CorrelationID:     correlationID,
			}
			if err := sendFrame(ctx, c, d); err != nil {
				s.logger.Debug("replay delta send failed", "err", err)
				return
			}
		}
	}

	// 4b. Replay the cached show roster to this subscriber right after
	// its initial snapshot, so a fresh 1.1 runtime can preload every
	// scene bundle without waiting for the next roster change (Prism#230).
	// Show-level metadata : live subscribers only, and only when the
	// connection negotiated 1.1.
	if live && proto11 {
		if roster, ok := s.rosterFrame(); ok {
			if err := sendFrame(ctx, c, roster); err != nil {
				s.logger.Debug("roster replay send failed", "err", err)
				return
			}
		}
		// 4c. Replay the cached show-level overlay-app state (overlay_apps),
		// so a fresh 1.1 consumer — including one that joined with NO active
		// scene (the holding path) — preloads the overlay control state.
		if overlay, ok := s.overlayAppsFrame(); ok {
			if err := sendFrame(ctx, c, overlay); err != nil {
				s.logger.Debug("overlay_apps replay send failed", "err", err)
				return
			}
		}
	}

	// 5. Loop. Reader and writer share the connection ; coder/websocket
	// supports the pattern as long as neither side exceeds its role.
	if err := s.runConnection(ctx, c, scene, sub, id); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!isCleanClose(err) {
		s.logger.Debug("ws session ended", "err", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// resolveScene picks the right Scene given a Subscribe frame. Live
// mode (no scene field) returns the active scene ; test mode demands
// (scene, session) and the role MUST be test or operator.
func (s *Server) resolveScene(sub *protocol.Subscribe, id Identity) (*Scene, protocol.ErrorCode, string) {
	if sub.Scene == "" {
		// Live mode. With no active scene, attach to the holding scene rather
		// than rejecting: a live 1.1 subscriber can then still receive
		// show-level frames (roster, overlay_apps) with an empty show and is
		// migrated onto the first scene that becomes active. This is what makes
		// the overlay_apps frame deliverable to a consumer with no scene.
		scene := s.ActiveScene()
		if scene == nil {
			return s.holding, "", ""
		}
		return scene, "", ""
	}
	// Test mode.
	if id.Role != protocol.RoleTest && id.Role != protocol.RoleOperator {
		return nil, protocol.CodeAuthDenied, "test mode requires test or operator role"
	}
	if sub.Session == "" {
		return nil, protocol.CodeSceneNotFound, "test mode requires session"
	}
	scene, ok := s.Scene(sub.Scene)
	if !ok {
		return nil, protocol.CodeSceneNotFound, "scene not registered"
	}
	return scene, "", ""
}

// runConnection drains the read side and pipes the subscription's
// outgoing channel to the wire. Returns when the read side errors,
// the writer encounters a fatal write, or ctx ends.
func (s *Server) runConnection(
	ctx context.Context,
	c *websocket.Conn,
	scene *Scene,
	sub *subscription,
	id Identity,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writerErr := make(chan error, 1)
	go func() {
		writerErr <- s.writerLoop(ctx, c, sub)
	}()

	// Reader loop runs on this goroutine.
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			cancel()
			<-writerErr
			return err
		}
		msg, derr := protocol.Decode(raw)
		if derr != nil {
			code := protocol.CodeInternal
			if errors.Is(derr, protocol.ErrVersionMismatch) {
				code = protocol.CodeVersionMismatch
			}
			// Error frames carry the current scene seq — they don't
			// advance it (per-scene seq, §18.1.1, errors are
			// connection-scoped not scene-scoped events).
			_ = sendError(ctx, c, scene.seq.Current(), code, derr.Error(), false)
			cancel()
			<-writerErr
			return derr
		}
		switch m := msg.(type) {
		case *protocol.Input:
			code, path, ierr := scene.applyInput(ctx, id, m)
			if ierr != nil {
				_ = sendPathError(ctx, c, scene.seq.Current(), code, ierr.Error(), code != protocol.CodeAuthDenied, path)
			}
		case *protocol.Ping:
			// LSDP/1.1 §3.5 : echo the nonce verbatim if present.
			if err := sendFrame(ctx, c, &protocol.Pong{Nonce: m.Nonce}); err != nil {
				cancel()
				<-writerErr
				return err
			}
		case *protocol.Pong:
			// Liveness reply ; nothing to do — the read deadline
			// resets on every received frame.
		case *protocol.Unsubscribe:
			// LSDP/1.1 §4.4 : clean teardown. No data flows after this
			// frame ; the server closes the WebSocket within 1 second.
			// Cancel writer, wait for it, return cleanly.
			cancel()
			<-writerErr
			return nil
		case *protocol.Subscribe:
			_ = sendError(ctx, c, scene.seq.Current(), protocol.CodeInternal, "duplicate subscribe", false)
			cancel()
			<-writerErr
			return errors.New("duplicate subscribe")
		}
	}
}

// writerLoop drains sub.out and the periodic ping timer.
func (s *Server) writerLoop(ctx context.Context, c *websocket.Conn, sub *subscription) error {
	t := time.NewTimer(s.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-sub.out:
			if !ok {
				return nil
			}
			if err := sendFrame(ctx, c, msg); err != nil {
				return err
			}
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(s.cfg.PingInterval)
		case <-t.C:
			if err := sendFrame(ctx, c, &protocol.Ping{}); err != nil {
				return err
			}
			t.Reset(s.cfg.PingInterval)
		}
	}
}

// sendFrame encodes and writes a single LSDP/1 text frame.
func sendFrame(ctx context.Context, c *websocket.Conn, msg any) error {
	raw, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, raw)
}

// sendError ships an Error frame with the given fields. Callers use
// this for both pre-subscribe errors (seq=0) and mid-flight errors.
func sendError(ctx context.Context, c *websocket.Conn, seq uint64, code protocol.ErrorCode, msg string, recoverable bool) error {
	return sendPathError(ctx, c, seq, code, msg, recoverable, "")
}

// sendPathError is sendError for the path-scoped codes : LSDP/1 §3.4.1
// makes `path` REQUIRED on WRITE_FORBIDDEN, UNKNOWN_PATH and
// INVALID_VALUE. An empty path encodes to no field at all, so this is
// also the safe form for the codes that carry none.
func sendPathError(ctx context.Context, c *websocket.Conn, seq uint64, code protocol.ErrorCode, msg string, recoverable bool, path string) error {
	return sendFrame(ctx, c, &protocol.Error{
		Seq:         seq,
		Code:        string(code),
		Message:     msg,
		Recoverable: recoverable,
		Path:        path,
	})
}

// readSubscribe waits for and parses the first frame ; rejects anything
// that is not a Subscribe.
func readSubscribe(ctx context.Context, c *websocket.Conn) (*protocol.Subscribe, error) {
	_, raw, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	msg, err := protocol.Decode(raw)
	if err != nil {
		return nil, err
	}
	sub, ok := msg.(*protocol.Subscribe)
	if !ok {
		return nil, errors.New("first frame must be subscribe")
	}
	return sub, nil
}

// isCleanClose hides the common "peer closed normally" errors so the
// logger doesn't shout on every disconnect.
func isCleanClose(err error) bool {
	if err == nil {
		return true
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure,
			websocket.StatusGoingAway,
			websocket.StatusNoStatusRcvd:
			return true
		}
	}
	return false
}
