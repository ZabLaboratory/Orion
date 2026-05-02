// Command orion is the Orion v2 service entry point.
// Wires config → store → runtime → adapters → ws → api into a single
// process. Per ADR 004 § 1, this is the only place global state lives;
// everything else is constructed and passed in.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
	"github.com/ZabLaboratory/Orion/internal/ws"
)

func main() {
	if err := run(); err != nil {
		_, _ = os.Stderr.WriteString("orion: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// run holds the actual main body so deferred cleanup runs on every
// exit path. main() only handles the final os.Exit, sidestepping the
// "exit-after-defer" trap.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := obs.NewLogger(cfg)
	slog.SetDefault(logger)
	metrics := obs.NewMetrics()

	logger.Info("orion starting",
		"listen", cfg.ListenAddr,
		"internal", cfg.InternalAddr,
		"public_base_url", cfg.PublicBaseURL,
		"tick_hz", cfg.TickHz,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Persistence.
	dbCtx, dbCancel := context.WithTimeout(ctx, 30*time.Second)
	st, err := store.Open(dbCtx, cfg.DatabaseURL)
	dbCancel()
	if err != nil {
		return err
	}
	defer st.Close()

	// Runtime: compute registry → show → tick → test sessions.
	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	defer show.Stop()

	testMgr := runtime.NewTestSessionManager(registry, logger, 5*time.Minute)
	defer testMgr.Close()

	tick := runtime.NewTick(cfg.TickHz, show)
	tick.Run()
	defer tick.Stop()

	// Cold-start: enumerate every active scene with a non-null
	// latest_pushed_version and load its compiled artefacts into
	// the show. ADR 004 § 4.4.
	if err := loadActiveScenes(ctx, st, show, logger); err != nil {
		logger.Error("scene cold start failed", "err", err)
	}

	// Adapter inbox + HTTP poller + PG LISTEN/NOTIFY.
	inbox := adapters.NewInbox(show, logger)
	poller := adapters.NewPoller(inbox, logger, cfg.HTTPPollUserAgent)
	defer poller.StopAll()

	pgListen := adapters.NewPGListener(st.Pool(), inbox, logger)
	defer pgListen.StopAll()

	// Wire pollers + listeners on every loaded scene.
	for _, id := range show.IDs() {
		if scene, err := show.Get(id); err == nil {
			poller.Start(ctx, scene)
			pgListen.Start(ctx, scene)
		}
	}

	// HTTP compiler fetcher + auth validator.
	fetcher := compiler.NewHTTPFetcher(cfg.CanvasBaseURL, cfg.BlueBaseURL, cfg.ServiceToken)
	_ = auth.NewValidator(cfg.ZabAuthValidateURL, cfg.ServiceToken, cfg.AuthCacheTTL)

	wsServer := &ws.Server{
		Show:    show,
		Inbox:   inbox,
		Test:    testMgr,
		Logger:  logger,
		Metrics: metrics,
	}

	// Public mux: HTTP + WS surface routed through ZabGate.
	publicMux := http.NewServeMux()
	api.RegisterPublic(publicMux, api.PublicDeps{
		Logger:    logger,
		Metrics:   metrics,
		Config:    cfg,
		Show:      show,
		Inbox:     inbox,
		Test:      testMgr,
		Store:     st,
		Fetcher:   fetcher,
		WSServer:  wsServer,
		StaticDir: http.Dir(cfg.SolarRoot),
	})

	// Internal-only HTTP surface for prom scrape + dev probes.
	internalMux := http.NewServeMux()
	internalMux.Handle("/orion/internal/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{Registry: metrics.Registry}))
	internalMux.HandleFunc("/orion/internal/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	publicSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           obs.Recover(logger, publicMux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	internalSrv := &http.Server{
		Addr:              cfg.InternalAddr,
		Handler:           obs.Recover(logger, internalMux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 2)
	go func() {
		logger.Info("public server listening", "addr", cfg.ListenAddr)
		if err := publicSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	go func() {
		logger.Info("internal server listening", "addr", cfg.InternalAddr)
		if err := internalSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errs:
		logger.Error("server error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("public shutdown error", "err", err)
	}
	if err := internalSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("internal shutdown error", "err", err)
	}
	logger.Info("orion stopped")
	return nil
}

// loadActiveScenes brings every active+pushed scene into the runtime
// roster on cold start. Per ADR 004 § 4.4.
func loadActiveScenes(ctx context.Context, st *store.Store, show *runtime.Show, logger *slog.Logger) error {
	scenes, err := st.ListActiveScenesWithPush(ctx)
	if err != nil {
		return err
	}
	for _, sc := range scenes {
		pv, err := st.GetLatestPushedVersion(ctx, sc.ID)
		if err != nil {
			logger.Warn("cold start: latest pushed version missing", "scene_id", sc.ID, "err", err)
			continue
		}
		var graph compiler.Graph
		var bundle compiler.RenderBundle
		if err := json.Unmarshal(pv.GraphJSON, &graph); err != nil {
			logger.Warn("cold start: bad graph json", "scene_id", sc.ID, "err", err)
			continue
		}
		if err := json.Unmarshal(pv.BundleJSON, &bundle); err != nil {
			logger.Warn("cold start: bad bundle json", "scene_id", sc.ID, "err", err)
			continue
		}
		show.Load(sc.ID.String(), &graph, &bundle)
	}
	return nil
}
