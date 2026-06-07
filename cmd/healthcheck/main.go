// Command healthcheck is a zero-dependency liveness probe for Orion's
// container. The runtime image is distroless/static:nonroot — it ships no
// shell and no curl/wget — so Docker's HEALTHCHECK cannot use a shell form.
// This tiny binary is compiled alongside /orion and invoked by the compose
// healthcheck (`["CMD", "/healthcheck"]`).
//
// It issues GET {scheme}://{host}/health against the public listener and
// exits 0 only on HTTP 200. The target is derived from ORION_LISTEN_ADDR
// (default 0.0.0.0:4007, the same default config.Load applies), so the
// probe tracks the real listen port if it is overridden. A 0.0.0.0 host is
// rewritten to 127.0.0.1 for the loopback dial.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run())
}

// run performs the liveness probe and returns the process exit code. It is
// split out from main so that every deferred call (the context cancel and the
// response body close) runs before the process exits: os.Exit skips deferred
// functions, so the single os.Exit lives in main and run only ever returns.
func run() int {
	addr := os.Getenv("ORION_LISTEN_ADDR")
	if addr == "" {
		addr = "0.0.0.0:4007"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// ORION_LISTEN_ADDR may be a bare port or host; fall back sanely.
		host, port = "", strings.TrimPrefix(addr, ":")
		if port == "" {
			port = "4007"
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := "http://" + net.JoinHostPort(host, port) + "/health"

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
