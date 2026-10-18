package lsdpreception

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

// The receiver remains alive: only this test's producer socket is closed.
// This distinguishes recovery backoff from cold state/hash processing.
func TestIdleSocketRecoveryNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires disposable native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProducer(ctx, r)
	defer p.Close()
	var mu sync.Mutex
	var socket *native.Client
	p.dial = func(ctx context.Context) (producerClient, error) {
		c, err := native.Dial(ctx, address, "", nil)
		mu.Lock()
		socket = c
		mu.Unlock()
		return c, err
	}
	p.Replace("solar/lane-b", map[string]any{"lsml": "1.2", "scene_id": "idle-recovery", "scene_version": "test", "layout": map[string]any{"kind": "shape"}, "defaults": map[string]any{"opacity": 1}})
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	err = socket.Close()
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		start := time.Now()
		p.Apply("solar/lane-b", []map[string]any{{"op": "add", "path": "/defaults/opacity", "value": float64(i+1) / 10}})
		if err = p.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		state, err := r.Read(ctx, "solar/lane-b")
		if err != nil || state.(map[string]any)["defaults"].(map[string]any)["opacity"] != float64(i+1)/10 {
			t.Fatal("mutation missing after recovered ACK", err)
		}
		status := p.Status()
		if status.Reconnects != 1 || status.Terminal || status.Recovering {
			t.Fatal(status)
		}
		proof, _ := json.Marshal(map[string]any{"step": i, "producer_ack_ms": float64(elapsed.Microseconds()) / 1000, "configured_backoff_ms": p.retryDelay.Milliseconds(), "status": status, "native_value_verified": true})
		t.Log("CLOSED_SOCKET_TIMING " + string(proof))
	}
}
