package lsdpreception

import (
	"context"
	"encoding/json"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type lostNativeReceipt struct {
	producerClient
	lost *atomic.Bool
}

func (c lostNativeReceipt) Exchange(ctx context.Context, request any) (json.RawMessage, error) {
	result, err := c.producerClient.Exchange(ctx, request)
	if value, ok := request.(map[string]any); ok && value["format"] == "lsdp.apply/1" && err == nil && c.lost.CompareAndSwap(false, true) {
		c.Close() // actual Rust commit happened; its receipt never reaches Producer.
		return nil, io.EOF
	}
	return result, err
}
func TestRealNativeTransportRecovery(t *testing.T) {
	address := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS")
	if address == "" {
		t.Skip("requires actual native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": "orion/state"})
	if err != nil {
		t.Fatal(err)
	}
	var original struct{ State any }
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := client.SendPort(context.Background(), "", "orion/state", "orion.state/1", original.State); err != nil {
			t.Error(err)
		}
	}()
	reception, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProducer(ctx, reception)
	defer p.Close()
	var lost atomic.Bool
	p.dial = func(ctx context.Context) (producerClient, error) {
		c, e := native.Dial(ctx, address, "", nil)
		if e != nil {
			return nil, e
		}
		return lostNativeReceipt{c, &lost}, nil
	}
	p.Replace("orion/state", map[string]any{"items": []any{}})
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	p.Apply("orion/state", []map[string]any{{"op": "add", "path": "/items/-", "value": "one"}})
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err = client.Exchange(ctx, map[string]any{"kind": "state.read", "target": "orion/state"})
	if err != nil {
		t.Fatal(err)
	}
	var observed struct{ State struct{ Items []string } }
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	if len(observed.State.Items) != 1 || observed.State.Items[0] != "one" || !lost.Load() || p.Status().Reconnects != 1 {
		t.Fatalf("committed insertion replayed: %s %+v", raw, p.Status())
	}
	t.Logf("REAL_RECOVERY_PROOF committed_insertion=1 receipt_lost=true reconnects=%d healthy=true", p.Status().Reconnects)
}
