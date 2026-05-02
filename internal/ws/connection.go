package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// connection is the per-WS state. One reader goroutine and one
// writer goroutine drain messages concurrently; coder/websocket
// supports this pattern provided neither exceeds its own role.
type connection struct {
	c        *websocket.Conn
	identity auth.Identity
	logger   *slog.Logger
	metrics  *obs.Metrics

	closed atomic.Bool
}

func newConnection(c *websocket.Conn, id auth.Identity, logger *slog.Logger, metrics *obs.Metrics) *connection {
	return &connection{
		c:        c,
		identity: id,
		logger:   logger.With("role", id.Role, "user", id.UserID),
		metrics:  metrics,
	}
}

// expectSubscribe waits for the first frame and verifies it is a
// `subscribe` envelope. Anything else closes the connection.
func (cc *connection) expectSubscribe(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, raw, err := cc.c.Read(rctx)
	if err != nil {
		return err
	}
	msg, err := protocol.Decode(raw)
	if err != nil {
		_ = cc.sendError(ctx, protocol.CodeInternal, err.Error(), false)
		return err
	}
	if _, ok := msg.(*protocol.Subscribe); !ok {
		_ = cc.sendError(ctx, protocol.CodeInternal, "first message must be subscribe", false)
		return errors.New("ws: expected subscribe")
	}
	cc.metrics.WSMessagesIn.WithLabelValues(protocol.TypeSubscribe).Inc()
	return nil
}

// run is the main message loop. Reader: one goroutine in this method.
// Writer: another goroutine drains sub.Out. The provided onInput
// closure is called once per `input` frame so the show / test handler
// can route through the right inbox.
func (cc *connection) run(
	ctx context.Context,
	sub *runtime.Subscription,
	onInput func(context.Context, *protocol.Input) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writerErr := make(chan error, 1)
	go cc.writeLoop(ctx, sub, writerErr)

	for {
		_, raw, err := cc.c.Read(ctx)
		if err != nil {
			cancel()
			<-writerErr
			return err
		}
		msg, derr := protocol.Decode(raw)
		if derr != nil {
			_ = cc.sendError(ctx, protocol.CodeInternal, derr.Error(), false)
			continue
		}
		switch m := msg.(type) {
		case *protocol.Input:
			cc.metrics.WSMessagesIn.WithLabelValues(protocol.TypeInput).Inc()
			if err := onInput(ctx, m); err != nil {
				cc.logger.Warn("input rejected", "err", err)
			}
		case *protocol.Ping:
			cc.metrics.WSMessagesIn.WithLabelValues(protocol.TypePing).Inc()
			if err := cc.sendMessage(ctx, protocol.Pong{Nonce: m.Nonce}); err != nil {
				return err
			}
		case *protocol.Unsubscribe:
			cc.metrics.WSMessagesIn.WithLabelValues(protocol.TypeUnsubscribe).Inc()
			cancel()
			<-writerErr
			return nil
		case *protocol.Subscribe:
			_ = cc.sendError(ctx, protocol.CodeInternal, "duplicate subscribe", false)
		case *protocol.Pong:
			cc.metrics.WSMessagesIn.WithLabelValues(protocol.TypePong).Inc()
		}
	}
}

func (cc *connection) writeLoop(ctx context.Context, sub *runtime.Subscription, errCh chan<- error) {
	defer func() { errCh <- nil }()
	pingTimer := time.NewTimer(pingInterval)
	defer pingTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.Out:
			if !ok {
				return
			}
			if err := cc.sendMessage(ctx, msg); err != nil {
				return
			}
			if !pingTimer.Stop() {
				select {
				case <-pingTimer.C:
				default:
				}
			}
			pingTimer.Reset(pingInterval)
		case <-pingTimer.C:
			_ = cc.sendMessage(ctx, protocol.Ping{Nonce: nonce()})
			pingTimer.Reset(pingInterval)
		}
	}
}

// sendMessage marshals through the protocol package and writes a
// single WS text frame.
func (cc *connection) sendMessage(ctx context.Context, msg any) error {
	raw, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	cc.metrics.WSMessagesOut.WithLabelValues(typeOf(msg)).Inc()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return cc.c.Write(wctx, websocket.MessageText, raw)
}

func (cc *connection) sendError(ctx context.Context, code, message string, recoverable bool) error {
	return cc.sendMessage(ctx, protocol.Error{
		Code:        code,
		Message:     message,
		Recoverable: recoverable,
	})
}

func (cc *connection) close(status websocket.StatusCode, reason string) {
	if cc.closed.Swap(true) {
		return
	}
	_ = cc.c.Close(status, reason)
}

func typeOf(msg any) string {
	switch msg.(type) {
	case *protocol.Snapshot, protocol.Snapshot:
		return protocol.TypeSnapshot
	case *protocol.Delta, protocol.Delta:
		return protocol.TypeDelta
	case *protocol.SceneChanged, protocol.SceneChanged:
		return protocol.TypeSceneChanged
	case *protocol.Error, protocol.Error:
		return protocol.TypeError
	case *protocol.Pong, protocol.Pong:
		return protocol.TypePong
	case *protocol.Ping, protocol.Ping:
		return protocol.TypePing
	default:
		return "unknown"
	}
}

// nonce produces a small random hex string for ping frames. Quality
// doesn't matter — it's a correlation id, not a security primitive.
func nonce() string {
	const hex = "0123456789abcdef"
	now := time.Now().UnixNano()
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = hex[now&0xF]
		now >>= 4
	}
	return string(out)
}

// writeJSON is used by some tests to confirm bytes-on-wire match
// envelope shape; kept for parity with api/public.go.
func writeJSON(_ context.Context, w *websocket.Conn, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.Write(context.Background(), websocket.MessageText, raw)
}

var _ = writeJSON
