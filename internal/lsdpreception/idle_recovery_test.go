package lsdpreception

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestRecoveryRetriesClosedConnectionImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := NewProducer(ctx, &Reception{})
	defer p.Close()
	p.retryDelay = time.Hour // waiting for backoff would outlive this test
	request := map[string]any{"id": "same-id", "format": "lsdp.apply/1", "operations": []any{map[string]any{"op": "add", "path": "/items/0", "value": 1}}}
	wanted, _ := json.Marshal(request)
	var first []byte
	client := producerClient(recoveryClient{exchange: func(request any) (json.RawMessage, error) { first, _ = json.Marshal(request); return nil, io.EOF }})
	p.dial = func(context.Context) (producerClient, error) {
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			raw, _ := json.Marshal(request)
			if string(raw) != string(first) || string(raw) != string(wanted) {
				t.Error("recovery changed transaction identity/payload")
			}
			return json.RawMessage(`{"level":"applied"}`), nil
		}}, nil
	}
	_, replayed, err := p.exchange(&client, request)
	if err != nil || !replayed || p.Status().Reconnects != 1 || p.Status().Recovering {
		t.Fatal("healthy replacement connection waited for backoff", err, p.Status())
	}
}

func TestImmediateRecoveryStillBacksOffAfterFailedReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := NewProducer(ctx, &Reception{})
	defer p.Close()
	p.retryDelay = time.Hour
	attempts := make(chan struct{}, 4)
	p.dial = func(context.Context) (producerClient, error) { attempts <- struct{}{}; return nil, io.EOF }
	client := producerClient(recoveryClient{exchange: func(any) (json.RawMessage, error) { return nil, io.EOF }})
	done := make(chan error, 1)
	go func() { _, _, err := p.exchange(&client, map[string]any{"id": "same-id"}); done <- err }()
	select {
	case <-attempts:
	case <-ctx.Done():
		t.Fatal("first reconnect was not immediate")
	}
	select {
	case <-attempts:
		t.Fatal("failed reconnect spun without backoff")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled recovery succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("recovery did not stop on shutdown")
	}
}
