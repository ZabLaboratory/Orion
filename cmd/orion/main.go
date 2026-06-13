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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/lsdp"
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
	// Exec-layer observability (ADR 003 §3.1.6, issue #82):
	// orion_event_shed_total / orion_task_preempt_total /
	// orion_parked_tasks land on the internal scrape endpoint.
	show.SetExecMetrics(metrics)
	defer show.Stop()

	// LSDP/1.1 wire (ADR 007 §C.3b) — built and installed on the show
	// only in dual/lsdp mode, before cold-start so every loaded scene is
	// paired with a kit scene. In bespoke mode the wire is nil: the kit
	// is never constructed and the bespoke WS is the only wire (no-op
	// deploy). Gateway-first holds by construction — the wire's only
	// identity source is auth.FromHeaders (no JWT, no token).
	var lsdpHandler http.Handler
	if cfg.LSDPMode == config.LSDPModeDual || cfg.LSDPMode == config.LSDPModeLSDP {
		wire, err := lsdp.NewWire(logger)
		if err != nil {
			return err
		}
		show.SetMirrors(wire)
		lsdpHandler = wire.Handler()
		logger.Info("lsdp wire enabled", "mode", string(cfg.LSDPMode))
	}

	testMgr := runtime.NewTestSessionManager(registry, logger, 5*time.Minute)
	defer testMgr.Close()

	// Scene-validation harness (ADR 003 §3.2, issue #87). CPU-bound and
	// off the live path — it builds its own isolated clone scenes per
	// campaign and never touches the show. The budget bounds the PROOF,
	// never the engine (a divergent logic fails validation, never airs).
	harness := runtime.NewHarness(registry, logger,
		runtime.ValidationBudgetFrom(cfg.ValidationMaxSteps, cfg.ValidationMaxWall))

	tick := runtime.NewTick(cfg.TickHz, show)
	tick.Run()
	defer tick.Stop()

	_ = auth.NewValidator(cfg.ZabAuthValidateURL, cfg.ServiceToken, cfg.AuthCacheTTL)

	// Service-token manager — mints + rotates the Bearer token Orion
	// presents on outbound calls through ZabGate (the stream-key proxy,
	// the compiler fetcher, and the `db.query` `_query` delegation).
	// Static mode (no operator token) keeps backward-compat with the
	// existing `ORION_SERVICE_TOKEN` env-only pattern. Built BEFORE
	// cold-start because the effect bundle the show installs on validated
	// scenes reads its `_query` bearer live from it.
	authBase := strings.TrimSuffix(cfg.ZabAuthValidateURL, "/tokens")
	serviceTokens := &auth.ServiceTokenManager{
		MintURL:       authBase + "/service-tokens",
		RefreshURL:    authBase + "/service-tokens/refresh",
		OperatorToken: cfg.OperatorToken,
		StaticToken:   cfg.ServiceToken,
		ServiceName:   "orion",
		Paths:         cfg.ServicePaths,
		Logger:        logger,
	}
	if err := serviceTokens.Start(ctx); err != nil {
		logger.Warn("service token manager start failed; falling back to static mode", "err", err)
	}
	defer serviceTokens.Stop()

	// Async-effect bundle (ADR 003 §3.1.3 / R9 lift ADR 006 §3.4).
	// Installed on the show so a VALIDATED, exec-bearing scene registers
	// the world-effect ops on load (the show's len(progs) > 0 seam). The
	// bundle confers no capability on its own. `db.query` rides topology A
	// (POST $ZABGATE/<svc>/api/v1/_query, service token via serviceTokens
	// — zero DB credential in Orion); `http.request` is bounded by the
	// fail-closed egress policy (anti-SSRF, post-DNS). The worker pool
	// starts here and drains on shutdown.
	effectRunner := effects.NewRunner(cfg.EffectWorkers, cfg.EffectQueue, logger)
	effectRunner.Start()
	defer effectRunner.Stop()
	dataSources := map[string]effects.DataSource{}
	for name, svc := range cfg.DataSources {
		dataSources[name] = effects.DataSource{Name: name, Svc: svc}
	}
	sceneEffects := &runtime.SceneEffects{
		Runner:      effectRunner,
		Egress:      effects.NewEgressPolicy(cfg.HTTPEgressAllowHosts, cfg.HTTPEgressAllowHTTP),
		DB:          effects.NewDBQueryClientWithTokenFunc(cfg.ZabGateURL, serviceTokens.Token, nil),
		DataSources: dataSources,
		Metrics:     metrics,
	}
	show.SetEffects(sceneEffects)
	logger.Info("async effects configured",
		"workers", cfg.EffectWorkers,
		"queue", cfg.EffectQueue,
		"datasources", len(dataSources),
		"egress_allow_hosts", len(cfg.HTTPEgressAllowHosts),
	)

	// Cold-start: enumerate every active scene with a non-null
	// latest_pushed_version and load its compiled artefacts into
	// the show. ADR 004 § 4.4. Runs AFTER SetEffects so a validated exec
	// scene reseeded here arms its effects on boot (criterion #7).
	if err := loadActiveScenes(ctx, st, show, logger); err != nil {
		logger.Error("scene cold start failed", "err", err)
	}

	// Adapter inbox + HTTP poller + PG LISTEN/NOTIFY.
	inbox := adapters.NewInbox(show, logger, metrics)
	// `show.emit` active-only injection sink (ADR 009 §3.6, issue #155):
	// the inbox owns the audit ring + the system-write path, so it is the
	// Emitter. Wired before live traffic; scenes loaded earlier read it at
	// call time.
	show.SetEmitter(inbox)
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

	// HTTP compiler fetcher — its outbound service token is read LIVE
	// from the manager on every Canvas/Blue fetch (Bastion C1), so a
	// token rotation is reflected immediately. Wiring it to the static
	// cfg.ServiceToken (the old NewHTTPFetcher path) froze the boot
	// placeholder and 401'd every fetch → 422 push. Token() collapses
	// to the static token in static mode, so dev/test posture is
	// unchanged. Both bases stay ZabGate-fronted (C5/C6): no direct
	// service-to-service path is introduced.
	fetcher := compiler.NewHTTPFetcherWithTokenFunc(cfg.CanvasBaseURL, cfg.BlueBaseURL, serviceTokens.Token)

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
		Logger:        logger,
		Metrics:       metrics,
		Config:        cfg,
		Show:          show,
		Inbox:         inbox,
		Test:          testMgr,
		Store:         st,
		Fetcher:       fetcher,
		WSServer:      wsServer,
		Harness:       harness,
		StaticDir:     http.Dir(cfg.SolarRoot),
		QuasarBaseURL: cfg.QuasarBaseURL,
		ServiceTokens: serviceTokens,
		LSDPHandler:   lsdpHandler,
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
//
// R9 boot reseed (ADR 006 §3.4 path 3, criterion #7): a validated exec
// scene must come back with its exec INSTALLED after a restart, or it
// would air with its logic silently dead. So each scene is loaded through
// execForAir: a validated version reinstalls its programs; an unvalidated
// one loads pure-dataflow only (the same invariant every other path
// holds). Fail-closed on a per-scene error — one bad scene never aborts
// the whole cold start; it loads dataflow-only and is logged.
func loadActiveScenes(ctx context.Context, st *store.Store, show *runtime.Show, logger *slog.Logger) error {
	scenes, err := st.ListActiveScenesWithPush(ctx)
	if err != nil {
		return err
	}
	loaded := map[string]bool{}
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
		progs := api.ExecForBoot(ctx, st, sc.ID, pv.SceneVersion, &graph, logger)
		show.LoadExec(sc.ID.String(), &graph, &bundle, progs...)
		loaded[sc.ID.String()] = true
	}

	// Re-activate the persisted antenna pointer (migrations/0004).
	//
	// The bug this fixes: loading the scenes above only fills the roster —
	// the show's `active` pointer stays empty after a restart, so every
	// viewer on /show/stream is closed with `scene not found` until an
	// operator re-pushes (the workaround we retire). The scene SELECTION is
	// persisted broadcast config (POST /show/active-scene writes it), so the
	// antenna must come back on the same scene after a redeploy. Leaf VALUES
	// are NOT restored — they reseed from declared defaults via LoadExec
	// above (criterion #11): only the selection is durable, the live state
	// stays volatile.
	activeID, err := st.GetActiveSceneID(ctx)
	if err != nil {
		// Fail-soft: a persistence read failure must not abort an otherwise
		// healthy cold start. The antenna comes back dark (the pre-fix
		// behaviour) rather than crash-looping the process; it is logged so
		// the degraded boot is visible.
		logger.Error("cold start: read active scene pointer failed; antenna stays dark", "err", err)
		return nil
	}
	if activeID == nil {
		return nil // no scene was on air — nothing to re-activate
	}
	if !loaded[activeID.String()] {
		// The persisted active scene is not in the active+pushed roster
		// (archived since, or its push pointer was cleared). The FK is
		// ON DELETE SET NULL, so a deleted scene already nulls the pointer;
		// this guards the archived-but-not-deleted case. Leave the antenna
		// dark rather than SetActive a scene that was never loaded.
		logger.Warn("cold start: persisted active scene not loadable; antenna stays dark",
			"scene_id", activeID.String())
		return nil
	}
	if err := show.SetActive(activeID.String(), nil); err != nil {
		logger.Error("cold start: re-activate persisted scene failed",
			"scene_id", activeID.String(), "err", err)
	}
	reloadStreamRules(ctx, st, show, logger)
	return nil
}

// reloadStreamRules reseeds the persisted stream-level Blue rule set into
// the roster after a restart (ADR 009 §3.1, issue #154, criterion #11).
// Mirrors the active-pointer restore: only the SELECTION is durable — each
// rule reseeds from declared defaults and fires on-start once (ADR 009
// §3.4, the FireOnStart-at-reload branch in Show.LoadExec). A rule is
// resolved through the SAME validated-exec seam (ExecForBoot) as every
// other roster instance: a rule promoted while validated comes back with
// its exec programs. Fail-soft per rule — one bad rule never aborts boot.
// SetActive ran already, so PromoteStreamRule's active-scene guard is
// authoritative (a scene that is both persisted-active and persisted-rule —
// which the API prevents — would simply be refused as a rule here, never
// double-routed).
func reloadStreamRules(ctx context.Context, st *store.Store, show *runtime.Show, logger *slog.Logger) {
	ruleIDs, err := st.ListStreamRules(ctx)
	if err != nil {
		logger.Error("cold start: read stream rule set failed; rules stay dormant", "err", err)
		return
	}
	for _, id := range ruleIDs {
		pv, err := st.GetLatestPushedVersion(ctx, id)
		if err != nil {
			logger.Warn("cold start: stream rule has no pushed version; skipped",
				"scene_id", id.String(), "err", err)
			continue
		}
		var graph compiler.Graph
		var bundle compiler.RenderBundle
		if err := json.Unmarshal(pv.GraphJSON, &graph); err != nil {
			logger.Warn("cold start: stream rule bad graph json; skipped", "scene_id", id.String(), "err", err)
			continue
		}
		if err := json.Unmarshal(pv.BundleJSON, &bundle); err != nil {
			logger.Warn("cold start: stream rule bad bundle json; skipped", "scene_id", id.String(), "err", err)
			continue
		}
		progs := api.ExecForBoot(ctx, st, id, pv.SceneVersion, &graph, logger)
		if err := show.PromoteStreamRule(id.String(), &graph, &bundle, progs...); err != nil {
			logger.Warn("cold start: stream rule promotion refused; skipped",
				"scene_id", id.String(), "err", err)
		}
	}
}
