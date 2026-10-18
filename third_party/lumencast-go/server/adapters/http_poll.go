// Package adapters ships server-side adapters that pump external data
// into a Lumencast Scene. Adapters are user-driven goroutines : the
// kit hands them a Scene reference and they call Scene.Emit when new
// values arrive.
//
// Three reference adapters live here :
//
//   - HTTPPoll  — periodic GET against a JSON endpoint
//   - WSSubscribe — WebSocket consumer that forwards JSON messages
//   - PgNotify — Postgres LISTEN/NOTIFY consumer (interface-based,
//     no pgx dependency)
//
// Each adapter is a stand-alone goroutine ; cancel via context. The
// kit is opinion-free about retries and error reporting beyond the
// minimal exponential backoff for transient transport failures.
package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Lumencast/lumencast-go/server"
)

// HTTPPollConfig parameterises an HTTPPoll adapter. URL and Decode
// are required.
type HTTPPollConfig struct {
	// URL is the endpoint polled at every Interval.
	URL string

	// Interval is the steady-state poll cadence. Defaults to 200 ms.
	Interval time.Duration

	// Header optionally injects request headers on every poll
	// (Authorization, etc.).
	Header http.Header

	// Decode converts a successful response body to a patches map.
	// Returning a non-nil error skips the emit for this iteration.
	Decode func(body []byte) (map[string]any, error)

	// Client is the HTTP client. Defaults to http.DefaultClient with
	// a 10 s timeout if nil.
	Client *http.Client

	// MaxBackoff caps the exponential backoff applied to consecutive
	// transport errors. Defaults to 30 s.
	MaxBackoff time.Duration

	// OnError, if non-nil, is invoked on every transport / decode
	// error. Use it to feed your observability stack ; the adapter
	// keeps retrying regardless of what you return.
	OnError func(error)
}

// HTTPPoll polls cfg.URL at cfg.Interval, decodes each response via
// cfg.Decode, and emits the resulting patches on scene.
//
// The function returns when ctx is cancelled. Transient HTTP / decode
// errors trigger exponential backoff capped at cfg.MaxBackoff.
func HTTPPoll(ctx context.Context, scene *server.Scene, cfg HTTPPollConfig) error {
	if cfg.URL == "" {
		return fmt.Errorf("adapters: HTTPPollConfig.URL is required")
	}
	if cfg.Decode == nil {
		return fmt.Errorf("adapters: HTTPPollConfig.Decode is required")
	}
	if cfg.Interval == 0 {
		cfg.Interval = 200 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	var backoff time.Duration
	for {
		patches, err := pollOnce(ctx, client, cfg)
		switch {
		case err == nil:
			backoff = 0
			if len(patches) > 0 {
				if emitErr := scene.Emit(patches); emitErr != nil && cfg.OnError != nil {
					cfg.OnError(fmt.Errorf("emit: %w", emitErr))
				}
			}
		default:
			if cfg.OnError != nil {
				cfg.OnError(err)
			}
			backoff = nextBackoff(backoff, cfg.MaxBackoff)
		}

		wait := cfg.Interval
		if backoff > wait {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// pollOnce performs a single HTTP request and returns the decoded
// patches. Returns (nil, nil) when the body decodes to an empty map.
func pollOnce(ctx context.Context, client *http.Client, cfg HTTPPollConfig) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, vs := range cfg.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, body)
	}
	patches, err := cfg.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return patches, nil
}

// JSONFlatDecode is a convenience cfg.Decode that flattens a JSON
// object's top-level keys into patches under writePrefix. Nested
// objects become dotted leaf paths ; arrays are passed verbatim
// (clients can use repeat() over them).
func JSONFlatDecode(writePrefix string) func([]byte) (map[string]any, error) {
	return func(body []byte) (map[string]any, error) {
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		out := make(map[string]any, len(v))
		flatten(writePrefix, v, out)
		return out, nil
	}
}

func flatten(prefix string, src map[string]any, out map[string]any) {
	for k, v := range src {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flatten(key, sub, out)
			continue
		}
		out[key] = v
	}
}

// nextBackoff doubles the previous duration with a 250 ms floor and a
// caller-supplied cap. Pure function, easy to unit-test.
func nextBackoff(prev, max time.Duration) time.Duration {
	const floor = 250 * time.Millisecond
	if prev < floor {
		return floor
	}
	next := prev * 2
	if next > max {
		return max
	}
	return next
}
