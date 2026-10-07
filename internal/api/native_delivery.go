package api

import (
	"context"
	"time"
)

// Success on a native-backed operator route means application by the receiver.
// Editable inputs first cross the scene inbox; flushing only the producer
// would otherwise overtake the scene goroutine and acknowledge stale state.
func flushNativeDelivery(ctx context.Context, deps PublicDeps, preview bool) error {
	if deps.NativeLSDPFlush == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if preview && deps.Preview != nil {
		if err := deps.Preview.Flush(ctx); err != nil {
			return err
		}
	}
	return deps.NativeLSDPFlush(ctx)
}
