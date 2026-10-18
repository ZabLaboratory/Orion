package adapters

import (
	"context"
	"fmt"

	"github.com/Lumencast/lumencast-go/server"
)

// PgNotification is the minimal abstraction over a Postgres
// LISTEN/NOTIFY payload. It is library-agnostic so the kit does not
// depend on pgx (or any specific driver). Wire it up like :
//
//	notifications := make(chan adapters.PgNotification, 32)
//	go pumpFromPgxConn(notifications, conn)  // your code
//	adapters.PgListen(ctx, scene, notifications, decode)
//
// See examples/basic-scoreboard for a working pgx integration.
type PgNotification struct {
	Channel string
	Payload string
}

// PgListenDecode converts a raw LISTEN/NOTIFY payload into a patches
// map. The kit ships no opinionated default ; the caller chooses
// JSON, line-delimited fields, or any other shape.
type PgListenDecode func(channel, payload string) (map[string]any, error)

// PgListen consumes notifications from the supplied channel and emits
// the resulting patches on scene. Returns when ctx is cancelled or
// the notifications channel is closed.
func PgListen(
	ctx context.Context,
	scene *server.Scene,
	notifications <-chan PgNotification,
	decode PgListenDecode,
	onError func(error),
) error {
	if decode == nil {
		return fmt.Errorf("adapters: PgListen decode is required")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case n, ok := <-notifications:
			if !ok {
				return nil
			}
			patches, err := decode(n.Channel, n.Payload)
			if err != nil {
				if onError != nil {
					onError(fmt.Errorf("decode %s: %w", n.Channel, err))
				}
				continue
			}
			if len(patches) == 0 {
				continue
			}
			if err := scene.Emit(patches); err != nil && onError != nil {
				onError(fmt.Errorf("emit: %w", err))
			}
		}
	}
}
