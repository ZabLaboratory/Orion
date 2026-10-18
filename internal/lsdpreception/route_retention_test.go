package lsdpreception

import (
	"context"
	"encoding/json"
	"errors"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestProducerReleasesOnlyCompletedRoutesBeforeRetentionSaturates(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	records := map[string]bool{}
	routes, released := 0, 0
	p.dial = func(context.Context) (producerClient, error) {
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			value := request.(map[string]any)
			id, _ := value["id"].(string)
			if value["kind"] == "route.forget" {
				if value["force"] != false || !records[id] {
					t.Error("attempted to release an unconfirmed route")
				}
				delete(records, id)
				released++
				raw, _ := json.Marshal(map[string]any{"status": "forgotten", "id": id})
				return raw, nil
			}
			if len(records) >= 8 {
				return nil, &native.WireError{Code: "ROUTE_BYTES", Outcome: "rejected"}
			}
			records[id] = true
			routes++
			raw, _ := json.Marshal(map[string]any{"status": "completed", "id": id})
			return raw, nil
		}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 256; i++ {
		p.Replace("solar/preview", map[string]any{"revision": i})
		if err := p.Flush(ctx); err != nil {
			t.Fatalf("replacement %d exhausted retained route memory: %v", i, err)
		}
	}
	if routes != 256 || released != routes || len(records) != 0 {
		t.Fatalf("routes=%d released=%d retained=%d", routes, released, len(records))
	}
}

func TestProducerNeverReleasesAnUnconfirmedRoute(t *testing.T) {
	for _, receipt := range []string{`{"status":"incomplete"}`, `{"status":"completed","id":"another-route"}`, `{"status":"rejected"}`} {
		t.Run(receipt, func(t *testing.T) {
			p := NewProducer(context.Background(), &Reception{})
			defer p.Close()
			releases := 0
			p.dial = func(context.Context) (producerClient, error) {
				return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
					if request.(map[string]any)["kind"] == "route.forget" {
						releases++
					}
					return json.RawMessage(receipt), nil
				}}, nil
			}
			p.Replace("solar/preview", map[string]any{"revision": 1})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := p.Flush(ctx); err == nil || releases != 0 {
				t.Fatalf("unconfirmed receipt accepted: err=%v releases=%d", err, releases)
			}
		})
	}
}

// The harness owns a disposable Rust node with a 128 KiB routing budget. This
// must never target the shared application: the no-release control saturates it.
func TestRealNativeRouteRetention(t *testing.T) {
	address := os.Getenv("ORION_TEST_ROUTE_RETENTION_ADDRESS")
	if address == "" {
		t.Skip("requires disposable native receiver with bounded routing budget")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r := &Reception{Address: address, Resource: "orion/state"}
	c, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := map[string]any{"revision": 0, "padding": strings.Repeat("x", 8192)}
	var retained []string
	var rejection *native.WireError
	for i := 0; i < 32; i++ {
		id, err := native.NewID()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := c.SendPort(ctx, id, "orion/state", "orion.state/1", payload)
		if err != nil {
			if !errors.As(err, &rejection) || rejection.Code != "ROUTE_BYTES" {
				t.Fatalf("no-release control: %v", err)
			}
			break
		}
		if err := completedRoute(raw, id); err != nil {
			t.Fatal(err)
		}
		retained = append(retained, id)
	}
	if rejection == nil || len(retained) == 0 {
		t.Fatal("no-release control did not exhaust the bounded receiver")
	}
	for _, id := range retained {
		if err := forgetCompletedRoute(func(request any) (json.RawMessage, error) { return c.Exchange(ctx, request) }, id); err != nil {
			t.Fatal(err)
		}
	}
	p := NewProducer(ctx, r)
	defer p.Close()
	var durations []float64
	for i := 1; i <= 2048; i++ {
		payload["revision"] = i
		start := time.Now()
		p.Replace("orion/state", payload)
		if err := p.Flush(ctx); err != nil {
			t.Fatalf("replacement %d: %v", i, err)
		}
		durations = append(durations, float64(time.Since(start).Microseconds())/1000)
	}
	for i := 2049; i <= 2176; i++ {
		payload["revision"] = i
		if err := r.Replace(ctx, "orion/state", payload); err != nil {
			t.Fatalf("control replacement %d: %v", i, err)
		}
	}
	raw, err := c.Exchange(ctx, map[string]any{"kind": "state.read", "target": "orion/state"})
	if err != nil {
		t.Fatal(err)
	}
	var final struct{ State struct{ Revision int } }
	if json.Unmarshal(raw, &final) != nil || final.State.Revision != 2176 || p.Status().Terminal || p.Status().Delivered != 2048 {
		t.Fatalf("final state or health invalid: %s %+v", raw, p.Status())
	}
	sort.Float64s(durations)
	t.Logf("REAL_ROUTE_RETENTION_PROOF no_release_rejection=ROUTE_BYTES retained_before_rejection=%d producer_replacements=2048 control_replacements=128 final_revision=%d producer_p50_ms=%.3f producer_p95_ms=%.3f terminal=false", len(retained), final.State.Revision, durations[len(durations)/2], durations[len(durations)*95/100])
}

func TestProducerRetriesReleaseAfterLostReceiptWithoutReplayingCompletedRoute(t *testing.T) {
	p := NewProducer(context.Background(), &Reception{})
	defer p.Close()
	p.retryDelay = time.Millisecond
	routes, releases := 0, 0
	p.dial = func(context.Context) (producerClient, error) {
		return recoveryClient{exchange: func(request any) (json.RawMessage, error) {
			value := request.(map[string]any)
			if value["kind"] == "route.forget" {
				releases++
				if releases == 1 {
					return nil, io.EOF
				}
				return json.RawMessage(`{"status":"unknown"}`), nil
			}
			routes++
			raw, _ := json.Marshal(map[string]any{"status": "completed", "id": value["id"]})
			return raw, nil
		}}, nil
	}
	p.Replace("solar/preview", map[string]any{"revision": 1})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if routes != 1 || releases != 2 {
		t.Fatalf("completed route was replayed: routes=%d releases=%d", routes, releases)
	}
}
