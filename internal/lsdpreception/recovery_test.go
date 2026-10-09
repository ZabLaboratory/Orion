package lsdpreception

import (
	"context"
	"encoding/json"
	"errors"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"io"
	"sync"
	"testing"
	"time"
)

type recoveryClient struct {
	exchange func(any) (json.RawMessage, error)
}

func (c recoveryClient) Exchange(_ context.Context, request any) (json.RawMessage, error) {
	return c.exchange(request)
}
func (c recoveryClient) Close() error { return nil }

func TestProducerRecoversWithoutDroppingOrChangingTransaction(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	p.retryDelay = time.Millisecond
	var mu sync.Mutex
	var attempts []string
	offline := make(chan struct{})
	attemptsSeen := make(chan struct{}, 1)
	p.dial = func(context.Context) (producerClient, error) {
		select {
		case <-offline:
		default:
			select {
			case attemptsSeen <- struct{}{}:
			default:
			}
			return nil, io.EOF
		}
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			raw, _ := json.Marshal(request)
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, string(raw))
			// Simulate a commit whose receipt was lost on the first connection.
			if len(attempts) == 1 {
				return nil, io.EOF
			}
			return json.RawMessage(`{"status":"completed"}`), nil
		}}, nil
	}
	p.Replace("solar/program", map[string]any{"value": "one"})
	<-attemptsSeen
	deadline := time.Now().Add(time.Second)
	for p.Error() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := p.Error(); err == nil {
		t.Fatal("outage did not invalidate readiness")
	}
	// Every accepted delivery stays ordered behind the unavailable receiver.
	p.Replace("solar/preview", map[string]any{"value": "two"})
	close(offline)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 || attempts[0] != attempts[1] {
		t.Fatalf("lost or changed replay: %v", attempts)
	}
	if p.Error() != nil {
		t.Fatal("recovery remained permanently unready", p.Error())
	}
	if status := p.Status(); status.Delivered != 2 || status.Reconnects < 2 || status.Recovering || status.Terminal || status.Pending != 0 {
		t.Fatalf("recovery status: %+v", status)
	}
}

func TestProducerDoesNotRebaseUnknownCommittedInsertion(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	p.retryDelay = time.Millisecond
	attempts := 0
	p.dial = func(context.Context) (producerClient, error) {
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			value := request.(map[string]any)
			if value["kind"] == "state.read" {
				return json.RawMessage(`{"state":{"items":[]}}`), nil
			}
			attempts++
			if attempts == 1 {
				return nil, io.EOF
			}
			return nil, &native.WireError{Code: "BASE_MISMATCH", Outcome: "rejected"}
		}}, nil
	}
	p.Apply("solar/program", []map[string]any{{"op": "add", "path": "/items/-", "value": "one"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err == nil || !errors.Is(err, p.Error()) {
		t.Fatal("unknown mutation outcome was hidden", err)
	}
	if attempts != 2 {
		t.Fatal("insertion was rebased after an unknown commit")
	}
}

func TestProducerCloseCancelsReconnect(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	attempted := make(chan struct{}, 1)
	p.dial = func(context.Context) (producerClient, error) { attempted <- struct{}{}; return nil, io.EOF }
	p.Replace("solar/program", nil)
	<-attempted
	stopped := make(chan struct{})
	go func() { p.Close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck in retry")
	}
}

func TestProducerProtocolFailureIsTerminal(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	p.dial = func(context.Context) (producerClient, error) { return nil, errors.New("INCOMPATIBLE_HELLO") }
	p.Replace("solar/program", nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err == nil || !p.Status().Terminal || p.Status().Reconnects != 0 {
		t.Fatal("protocol failure was retried", err, p.Status())
	}
}

func TestProducerRebasesRepeatedProvenConcurrentRejections(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	attempts := 0
	p.dial = func(context.Context) (producerClient, error) {
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			value := request.(map[string]any)
			if value["kind"] == "state.read" {
				return json.RawMessage(`{"state":{"external":"preserved"}}`), nil
			}
			attempts++
			if attempts <= 3 {
				return nil, &native.WireError{Code: "BASE_MISMATCH", Outcome: "rejected"}
			}
			raw, _ := json.Marshal(map[string]any{"level": "applied", "target": value["target"], "transactionId": value["id"]})
			return raw, nil
		}}, nil
	}
	p.Apply("orion/state", []map[string]any{{"op": "add", "path": "/own", "value": "one"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil || attempts != 4 {
		t.Fatalf("bounded conflict recovery failed: %v attempts=%d", err, attempts)
	}
}
