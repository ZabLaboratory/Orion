// Command datasidecar is the standalone embedded-local data sidecar
// (ADR 016 §3.2, issue #225). It serves the frozen _query contract on
// loopback against in-memory SQLite mirrors of ZabTruth/ZabRanking, so an
// embedded-local Orion can resolve db.query nodes against real-shaped data
// with zero outbound infra. Prism (#150) bundles this binary and points
// ORION_ZABGATE_URL at it (http://127.0.0.1:<port>).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ZabLaboratory/Orion/internal/datasidecar"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	addr := os.Getenv("DATASIDECAR_ADDR")
	if addr == "" {
		// Loopback-only by default: the sidecar lives inside Prism's trust
		// boundary and must NOT be network-reachable (contract §A.5).
		addr = "127.0.0.1:4097"
	}

	srv, err := datasidecar.NewServer(log)
	if err != nil {
		log.Error("datasidecar: failed to build mirrors", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("datasidecar listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("datasidecar: serve failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("datasidecar stopped")
}
