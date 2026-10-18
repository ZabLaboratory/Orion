package adapters

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Lumencast/lumencast-go/server"
)

// WSSubscribeConfig parameterises a WebSocket adapter. URL and Decode
// are required.
type WSSubscribeConfig struct {
	// URL is the upstream WebSocket endpoint (ws:// or wss://).
	URL string

	// Header is sent on the upgrade.
	Header http.Header

	// Subprotocols negotiated with the upstream.
	Subprotocols []string

	// Decode converts a single received frame's payload into a
	// patches map. Returning (nil, nil) skips the emit ; non-nil
	// error backs off.
	Decode func(messageType websocket.MessageType, data []byte) (map[string]any, error)

	// MaxBackoff caps the reconnect backoff after a transport error.
	// Defaults to 30 s.
	MaxBackoff time.Duration

	// OnError, if non-nil, surfaces transport / decode errors.
	OnError func(error)
}

// WSSubscribe connects to cfg.URL, decodes incoming frames via
// cfg.Decode, and emits the resulting patches on scene. Returns
// when ctx is cancelled. Reconnects with exponential backoff on
// disconnect.
func WSSubscribe(ctx context.Context, scene *server.Scene, cfg WSSubscribeConfig) error {
	if cfg.URL == "" {
		return fmt.Errorf("adapters: WSSubscribeConfig.URL is required")
	}
	if cfg.Decode == nil {
		return fmt.Errorf("adapters: WSSubscribeConfig.Decode is required")
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}

	var backoff time.Duration
	for {
		err := wsRunOnce(ctx, scene, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && cfg.OnError != nil {
			cfg.OnError(err)
		}
		backoff = nextBackoff(backoff, cfg.MaxBackoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
}

// wsRunOnce dials and pumps until the connection drops or ctx ends.
func wsRunOnce(ctx context.Context, scene *server.Scene, cfg WSSubscribeConfig) error {
	dialOpts := &websocket.DialOptions{
		HTTPHeader:   cfg.Header,
		Subprotocols: cfg.Subprotocols,
	}
	c, _, err := websocket.Dial(ctx, cfg.URL, dialOpts)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		patches, err := cfg.Decode(mt, data)
		if err != nil {
			if cfg.OnError != nil {
				cfg.OnError(fmt.Errorf("decode: %w", err))
			}
			continue
		}
		if len(patches) > 0 {
			if emitErr := scene.Emit(patches); emitErr != nil && cfg.OnError != nil {
				cfg.OnError(fmt.Errorf("emit: %w", emitErr))
			}
		}
	}
}
