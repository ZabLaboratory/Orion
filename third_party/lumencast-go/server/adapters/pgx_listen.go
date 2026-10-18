package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Lumencast/lumencast-go/server"
)

// PgxListenConfig parameterises a PgxListen adapter that connects to
// Postgres directly and pumps LISTEN/NOTIFY events into a Scene.
type PgxListenConfig struct {
	// DSN is a libpq-style connection string accepted by pgx.Connect.
	// Required.
	DSN string

	// Channels names every Postgres LISTEN target. Required, non-empty.
	Channels []string

	// Decode converts a (channel, payload) pair into a patches map.
	// Returning a non-nil error skips the emit ; (nil, nil) skips
	// silently.
	Decode PgListenDecode

	// MaxBackoff caps the reconnect backoff. Defaults to 30 s.
	MaxBackoff time.Duration

	// OnError surfaces transport / decode errors.
	OnError func(error)
}

// PgxListen is a turn-key LISTEN/NOTIFY adapter. It opens a pgx
// connection, runs LISTEN on every configured channel, and emits the
// decoded patches on scene. Reconnects with exponential backoff on
// transport failures. Returns when ctx is cancelled.
//
// Internally it pipes into PgListen (the library-agnostic core) so
// the same Decode signature is reused.
func PgxListen(ctx context.Context, scene *server.Scene, cfg PgxListenConfig) error {
	if cfg.DSN == "" {
		return errors.New("adapters: PgxListenConfig.DSN is required")
	}
	if len(cfg.Channels) == 0 {
		return errors.New("adapters: PgxListenConfig.Channels is required")
	}
	if cfg.Decode == nil {
		return errors.New("adapters: PgxListenConfig.Decode is required")
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}

	var backoff time.Duration
	for {
		err := pgxRunOnce(ctx, scene, cfg)
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

func pgxRunOnce(ctx context.Context, scene *server.Scene, cfg PgxListenConfig) error {
	conn, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("pgx connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	for _, ch := range cfg.Channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgxQuoteIdent(ch)); err != nil {
			return fmt.Errorf("LISTEN %q: %w", ch, err)
		}
	}

	notifications := make(chan PgNotification, 64)
	pumpErr := make(chan error, 1)
	go func() {
		defer close(notifications)
		for {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				pumpErr <- err
				return
			}
			select {
			case notifications <- PgNotification{Channel: n.Channel, Payload: n.Payload}:
			case <-ctx.Done():
				pumpErr <- ctx.Err()
				return
			}
		}
	}()

	listenErr := PgListen(ctx, scene, notifications, cfg.Decode, cfg.OnError)
	if listenErr != nil {
		return listenErr
	}
	select {
	case err := <-pumpErr:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("notification pump: %w", err)
	default:
		return nil
	}
}

// pgxQuoteIdent quotes a Postgres identifier per the rules pgx itself
// uses : double-quote, escape interior double-quotes by doubling. We
// implement it here to avoid pulling pgx/internal helpers.
func pgxQuoteIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}
