// Command orion is the Orion v2 service entry point.
// Wires config → runtime → adapters → ws → api into a single process.
// Per ADR 004 § 1, this is the only place global state lives; everything
// else is constructed and passed in. Stateless since #15/#331
// (ADR-BLUE-012): no store, the scene path is attestation-driven.
package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/lsdp"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/runtime"
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
		"profile", string(cfg.Profile),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Edge-implementation selection by execution profile (ADR 016 §3.3).
	// This is the ONLY place the profile is consulted — it picks the
	// AuthSource (and, below, the Store) impls at boot; the hot path
	// (requireOperator, db.query, tick, inbox) never branches on it.
	// Until the local impls land (#222 sqliteStore, #223 localOperatorAuth),
	// embedded-local wires the same antenne defaults, so it boots cleanly
	// and shares the exact production code path.
	// embedded-local (#223): localOperatorAuth grants operator on loopback
	// requests carrying the Prism↔Orion handshake secret. NEVER wired on
	// antenne — the profile branch keeps HeaderAuthSource there, so the
	// production path is byte-for-byte unchanged (RC-1, invariant).
	var authSource auth.AuthSource = auth.HeaderAuthSource{}
	authSourceKind := "header"
	if cfg.Profile.IsEmbeddedLocal() {
		local, err := auth.NewLocalOperatorAuth(cfg.LocalAuthSecret, cfg.LocalAuthUser)
		if err != nil {
			return err
		}
		authSource = local
		authSourceKind = "local-operator"
	}
	logger.Info("auth source selected", "profile", string(cfg.Profile), "source", authSourceKind)

	// Scene persistence and the validated-record store remain retired. The
	// scene path is driven by ZabCanvas's resolved-scene-ref attestation. The
	// Postgres connection opened below is only the encrypted service-token
	// state needed to keep Engine B's outbound identity alive across restarts.

	// Runtime: compute registry → show → tick → test sessions.
	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	// Exec-layer observability (ADR 003 §3.1.6, issue #82):
	// orion_event_shed_total / orion_task_preempt_total /
	// orion_parked_tasks land on the internal scrape endpoint.
	show.SetExecMetrics(metrics)
	// WS fanout back-pressure observability (Orion#274, ADR-BLUE-012
	// §12/B8): orion_ws_dropped_total{reason="collapse"|"stuck_timeout"}.
	show.SetWSMetrics(metrics)
	defer show.Stop()

	// LSDP/1.1 wire (ADR 007 §C.3b) — built and installed on the show
	// only in dual/lsdp mode, before cold-start so every loaded scene is
	// paired with a kit scene. In bespoke mode the wire is nil: the kit
	// is never constructed and the bespoke WS is the only wire (no-op
	// deploy). Gateway-first holds by construction — the wire derives
	// identity from the SAME AuthSource as the HTTP gates and the bespoke
	// WS (HeaderAuthSource on antenne, localOperatorAuth on embedded-local);
	// no JWT, no token.
	var lsdpHandler http.Handler
	var sessionWires runtime.SessionWireFactory
	var previewLSDPHandler http.Handler
	var previewSlot *runtime.PreviewSlot
	var antenneWire *lsdp.Wire
	var previewWire *lsdp.Wire
	if cfg.LSDPMode == config.LSDPModeDual || cfg.LSDPMode == config.LSDPModeLSDP {
		wire, err := lsdp.NewWire(logger, authSource)
		if err != nil {
			return err
		}
		// LSDP Snapshot identity-gap observability (ADR-BLUE-012 §16.1,
		// B3-R6-16-ORION-PGM): orion_lsdp_snapshot_identity_gap_total{scene_id}
		// counts a reseed that drops a KNOWN projection identity because the
		// Snapshot frame has no metadata field — never a fix, just makes the
		// wire-schema limitation observable instead of silent.
		wire.SetSnapshotMetrics(metrics)
		show.SetMirrors(wire)
		antenneWire = wire
		lsdpHandler = wire.Handler()
		// Per-session preview LSDP wire (preview/antenne split): each test
		// session gets its OWN isolated kit server so the preview Solar
		// runtime follows only the session clone, never the antenne's
		// active scene. Wired onto the TestSessionManager below.
		sessionWires = lsdp.NewSessionWireFactory(logger, authSource)

		// Persistent PREVIEW wire (preview/antenne split, the working model):
		// a SECOND lsdp.Wire beside the antenne's. The cockpit preview Solar
		// connects here ONCE (fixed /show/preview.lsdp); switching the previewed
		// scene swaps this wire's active clone via PreviewSlot.Activate →
		// previewWire.SetActive (scene_changed + snapshot over the existing
		// socket, no reload). The antenne wire above is never touched, so a
		// preview switch can never flip the live antenne.
		var pwErr error
		previewWire, pwErr = lsdp.NewWire(logger, authSource)
		if pwErr != nil {
			return pwErr
		}
		previewWire.SetSnapshotMetrics(metrics)
		previewLSDPHandler = previewWire.Handler()
		previewSlot = runtime.NewPreviewSlot(ctx, registry, previewWire, logger)
		defer previewSlot.Close()
		logger.Info("lsdp wire enabled", "mode", string(cfg.LSDPMode))
	}

	testMgr := runtime.NewTestSessionManager(registry, logger, 5*time.Minute)
	if sessionWires != nil {
		testMgr.SetSessionWires(sessionWires)
	}
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

	// Engine B egress uses a durable family only to obtain exact, short-lived
	// route tokens. The family bearer is never sent to a data route, and an
	// absent/expired exchange fails closed in the effect. In embedded-local,
	// the static token remains a test-only convenience; antenne never falls
	// back to ORION_SERVICE_TOKEN.
	staticServiceToken := ""
	if cfg.Profile.IsEmbeddedLocal() {
		staticServiceToken = cfg.ServiceToken
	}
	serviceTokens, tokenErr := auth.NewServiceTokenManager(
		ctx,
		cfg.DatabaseURL,
		auth.ServiceTokenRefreshURL(cfg.ZabAuthValidateURL),
		cfg.ServiceRefreshToken,
		cfg.EncryptionKey,
		staticServiceToken,
		logger,
	)
	if tokenErr != nil {
		logger.Error("service-token state unavailable; Engine B outbound calls fail closed", "err", tokenErr)
	}
	if serviceTokens != nil {
		if err := serviceTokens.Start(ctx); err != nil {
			logger.Error("service-token manager start failed; Engine B outbound calls fail closed", "err", err)
		}
		logger.Info("service-token manager", "state", string(serviceTokens.State()))
		defer serviceTokens.Stop()
	}
	familyTokenFn := func() string { return "" }
	if serviceTokens != nil {
		familyTokenFn = serviceTokens.Token
	}
	serviceTokenMinter := effects.NewServiceTokenExchangeMinterWithTokenFunc(cfg.ZabGateURL, familyTokenFn, nil)
	serviceTokenFn := serviceTokenMinter.Token
	// The runtime must resolve every curated service route against the same
	// Blue-published registry used by Orion's compiler. Loading this once at
	// boot keeps the hot path local while preserving Blue as the sole route
	// producer; a missing route remains fail-closed in bluehost.
	egressFetcher := compiler.NewHTTPFetcherWithTokenFunc(cfg.CanvasBaseURL, cfg.BlueBaseURL, func() string {
		return serviceTokenFn(cfg.ServicePaths)
	})
	egressRoutes, egressErr := egressFetcher.FetchEgressRoutes(ctx)
	if egressErr != nil {
		return fmt.Errorf("load Blue curated egress routes: %w", egressErr)
	}

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
	serviceCallClient := effects.NewServiceCallClient(cfg.ZabGateURL, serviceTokenFn, nil)
	// The direct bluehost ABI has no stream identity in EffectFunc, so its
	// conservative fallback budget is shared under one host key. Engine A's
	// scene runtime keeps the same limiter but supplies its per-stream key.
	egressBudget := effects.NewStreamEgressLimiter(cfg.EgressBudgetPerStream, cfg.EgressBudgetWindowS)
	// Build one dependency bundle for both Engine A's SceneEffects and the
	// scene-intent bluehost. The generic core.effect.invoke@1 adapter and the
	// direct EffectHandlers use different contracts, but must share the same
	// worker pool, egress policy, topology-A DB client, and datasource map.
	effectDeps := bluehost.EffectDeps{
		Runner:              effectRunner,
		Egress:              effects.NewEgressPolicy(cfg.HTTPEgressAllowHosts, cfg.HTTPEgressAllowHTTP),
		DB:                  effects.NewDBQueryClientWithPathTokenFunc(cfg.ZabGateURL, serviceTokenMinter.Token, nil),
		DataSources:         dataSources,
		ServiceCall:         serviceCallClient,
		ResolveServiceRoute: bluehostRouteResolver(egressRoutes),
		EgressBudget:        egressBudget,
		EgressBudgetKey:     "orion-bluehost",
		StreamID:            "live",
		Logger:              logger,
	}
	// Curated service-egress (ADR Blue 002 §3.3) no longer uses a standing
	// operator credential. The viewer armer below uses the durable service-token
	// manager to exchange for the exact ZabCam credentials-read scope, so it
	// remains least-privilege and fail-closed when the family token is absent.
	//
	// Stream-level Meet viewer-credentials arming on the antenne LSDP wire
	// (ADR Blue 009 §3.2, issue #261 — R1, Bastion-gated). Orion resolves the
	// armed `peer_label`s (the stream-level slot bindings) to their live room
	// receive-only viewer credentials via ZabCam and carries them on
	// `__cam.viewer` so Solar #28 can join the Meet room(s) on air. The token
	// is short-lived: re-fetched + re-emitted every ViewerCredsRefreshS. The
	// The meet_token rides a reserved leaf (off the
	// blueprint/_query surface) and is never logged. Antenne wire only —
	// preview keeps the Prism global.
	if antenneWire != nil {
		// core.overlay-app.set@1's real effector for Engine B
		// (ENGINE-B-PARITY-ORION, internal/bluehost/effect_overlay.go):
		// the SAME show-level wire Engine A's show.SetMirrors(wire) above
		// already drives. Assigned only inside this guard — antenneWire is
		// a *lsdp.Wire and assigning a nil one to the OverlayAppMirror
		// interface field would store a non-nil interface wrapping a nil
		// pointer, defeating dispatchOverlayAppSet's nil check.
		effectDeps.OverlayMirror = antenneWire
		effectDeps.SlotMirror = antenneWire
		credsFetcher := lsdp.NewZabCamCredsFetcher(cfg.ZabGateURL, serviceTokenMinter.Token, logger)
		antenneWire.EnableViewerCreds(ctx,
			credsFetcher, time.Duration(cfg.ViewerCredsRefreshS)*time.Second)
		logger.Info("viewer creds arming enabled", "refresh_s", cfg.ViewerCredsRefreshS)
	}
	// Per-stream egress budget (ADR Blue 009 §B / R3): the bound the G0
	// clearance requires before a WRITE route opens on the antenna path. A
	// runaway scene / chat feedback loop can spend at most
	// EgressBudgetPerStream curated egress calls per window per stream;
	// over budget the node fails closed to its `error` port. Surface
	// flagged for Bastion (R3).
	sceneEffects := &runtime.SceneEffects{
		Runner:       effectDeps.Runner,
		Egress:       effectDeps.Egress,
		DB:           effectDeps.DB,
		ServiceCall:  serviceCallClient,
		EgressBudget: egressBudget,
		DataSources:  effectDeps.DataSources,
		Metrics:      metrics,
	}
	show.SetEffects(sceneEffects)
	// The preview slot shares the SAME effects bundle (read-only): a preview
	// clone must run db.query / http.request just like the antenne, else its
	// on-call chain dies on the first world-effect op (unregistered exec op).
	if previewSlot != nil {
		previewSlot.SetEffects(sceneEffects)
	}
	logger.Info("async effects configured",
		"workers", cfg.EffectWorkers,
		"queue", cfg.EffectQueue,
		"datasources", len(dataSources),
		"egress_allow_hosts", len(cfg.HTTPEgressAllowHosts),
		"egress_budget_per_stream", cfg.EgressBudgetPerStream,
		"egress_budget_window_s", cfg.EgressBudgetWindowS,
	)
	if cfg.EgressBudgetPerStream <= 0 {
		logger.Warn("per-stream egress budget DISABLED (ORION_EGRESS_BUDGET_PER_STREAM<=0) — service.call is unbounded; R3 requires a positive bound before a WRITE route opens on the antenna path")
	}

	// Cold-start scene reseed — RETIRED (#15, #331): loadActiveScenes read
	// the persisted active-scene pointer + pushed versions from the store.
	// Orion boots with an empty roster now; the on-air/preview slots are
	// populated by the scene-intent surface (attestation-driven Prepare/
	// Take) below, never by a boot-time reseed.

	// Adapter inbox for authenticated platform/system writes.
	inbox := adapters.NewInbox(show, logger, metrics)
	// `show.emit` active-only injection sink (ADR 009 §3.6, issue #155):
	// the inbox owns the audit ring + the system-write path, so it is the
	// Emitter. Wired before live traffic; scenes loaded earlier read it at
	// call time.
	show.SetEmitter(inbox)
	// HTTP compiler fetcher / blueprint-direct stream-rule reseed — RETIRED
	// (#15, #331): both `selectFetcher` and `api.ReloadBlueprintStreamRules`
	// (deleted with internal/api/stream_rules.go) existed solely to compile
	// FROM the legacy push/store path. The scene-intent surface below
	// resolves scenes from the ZabCanvas attestation directly, with its own
	// fetch inside internal/api/scene_intent.go — no boot-time fetcher.

	wsServer := &ws.Server{
		Show:    show,
		Inbox:   inbox,
		Test:    testMgr,
		Logger:  logger,
		Metrics: metrics,
		// Same identity seam as the HTTP gates (ADR 016 §3.2-2): on antenne
		// this is HeaderAuthSource (byte-for-byte header-trust); on
		// embedded-local it is localOperatorAuth, so the loopback handshake
		// header X-Orion-Local-Auth is honoured on /show/stream too.
		AuthSource: authSource,
	}

	// Additive stateless-cutover surface (#331, ADR-BLUE-012). Dark by
	// default (nil) unless every ORION_WORKLOAD_*/ORION_CANVAS_TRUST_PATH
	// var is set — see cmd/orion/scene_intent_wiring.go. A config error
	// here is NOT a boot failure: the legacy path stays fully live either
	// way (Phase A of the #331 cutover plan).
	sceneIntent, sierr := wireSceneIntent(cfg, logger, effectDeps)
	if sierr != nil {
		logger.Error("scene-intent surface not wired; legacy path unaffected", "err", sierr)
	} else if sceneIntent != nil {
		logger.Info("scene-intent surface wired", "workload_zabgate_url", cfg.WorkloadZabGateURL)
		// Request-replay idempotence (§6.4/§6.7): a repeated intent with the
		// same dedup tuple + idempotency_key returns the prior typed result
		// instead of re-running Prepare/Take and restarting the bridge.
		// Unconditional — applies whether or not a bridge is wired below.
		sceneIntent.Idempotency = api.NewIdempotencyCacheWithLimits(
			time.Duration(cfg.IdempotencyTTLS)*time.Second,
			cfg.IdempotencyMaxEntries,
			metrics,
		)
		// Pair the bluehost instance with the SAME lsdp.Wire scene the
		// legacy Show-backed path already drives (B3-R6-12-ORION-PROJECTION,
		// Conduit's verdict on #331: internal/lsdp is the sole Solar
		// consumer and stays the wire, unmodified — only what feeds it
		// gains a second producer). Only wired when the antenne LSDP wire
		// actually exists (dual/lsdp mode); a bespoke-mode boot leaves
		// MirrorFor nil, so startBridge stays a no-op — Prepare/Take still
		// run and answer, nothing reaches Solar over this path yet.
		if antenneWire != nil {
			// sceneVersion is the real digest startBridge passes
			// (ORION-TAKE-SLOT-IDENTITY, Blue#345), not a hardcoded "" —
			// the LSDP kit tells Solar its scene_version is whatever value
			// lands here, and Solar echoes that back as ?v= on GET
			// .../render-bundle. A hardcoded "" told every client the wrong
			// value to send, independent of #401's own fix. sceneIntentMirrorFor
			// now threads it straight through to whichever wire it picks,
			// instead of hardcoding "" itself.
			//
			// The FLUX is a required parameter of the resolution itself
			// (#398), not an argument a call site can forget to consult:
			// sceneIntentMirrorFor closes over BOTH wires and the returned
			// function's own signature carries bluehost.Slot, so
			// startBridge's slot (already computed from the attestation
			// action) determines which wire gets the mirror — a
			// prepare-preview can never again land on the antenne wire the
			// way the prior 1-wire, sceneID-only signature allowed.
			// previewWire is guaranteed non-nil here: it is constructed in
			// the SAME cfg.LSDPMode dual/lsdp guard as antenneWire, above.
			//
			// bundle is the slot's LSML render-bundle bytes (deps.Host.
			// Bundle(slot), threaded by startBridge) — the only render-
			// bundle artefact this path ever holds. MirrorForLSML derives
			// the bound-leaf gate from it directly (#396): a hardcoded nil
			// here left boundLeafSet permanently disabled on the stateless
			// path even though the SAME mechanism is already proven safe on
			// the legacy Show-backed path.
			sceneIntent.MirrorFor = sceneIntentMirrorFor(lsdpWires{preview: previewWire, antenne: antenneWire})
			sceneIntent.Activate = sceneIntentActivate(lsdpWires{preview: previewWire, antenne: antenneWire})
			sceneIntent.EmitRoster = sceneIntentEmitRoster(lsdpWires{preview: previewWire, antenne: antenneWire})
			sceneIntent.Bridges = bluewire.NewRegistry()
			sceneIntent.Logger = logger
		}
		if sceneIntent.Host != nil {
			inbox.SetPlatformEventSink(func(path string, payload any) {
				for _, slot := range []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir} {
					if _, err := sceneIntent.Host.WritePlatformEvent(slot, path, payload); err != nil && !errors.Is(err, bluehost.ErrNotLoaded) {
						logger.Warn("stateless platform event delivery failed", "slot", slot, "path", path, "error", err)
					}
				}
			})
		}
	}

	// Global stream-rule runtime (ADR 009): one volatile Engine B plane,
	// independent of the preview/on-air scene Host. Prism remains the durable
	// intent owner and replays missing rules after restart. The plane receives
	// the same providers, capability policy and effect dependencies as scene
	// instances, including the show-level overlay mirror, but no Canvas
	// artefact or Orion store participates in promotion.
	rulePlane := bluehost.NewRulePlane(
		providers.Registry(),
		providers.Policy(len(cfg.HTTPEgressAllowHosts) > 0),
		effectDeps,
		cfg.TickHz,
		logger,
	)
	defer rulePlane.Stop()
	if sceneIntent != nil && sceneIntent.Host != nil {
		rulePlane.SetShowEmitSink(func(topic string, payload any) {
			for _, slot := range []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir} {
				if err := sceneIntent.Host.EmitEvent(slot, topic, payload); err != nil && !errors.Is(err, bluehost.ErrNotLoaded) {
					logger.Warn("stateless show.emit delivery failed", "slot", slot, "topic", topic, "error", err)
				}
			}
		})
	}
	inbox.SetStreamRulePlatformSink(rulePlane)

	// Public mux: HTTP + WS surface routed through ZabGate.
	publicMux := http.NewServeMux()
	api.RegisterPublic(publicMux, api.PublicDeps{
		Logger:        logger,
		Metrics:       metrics,
		Config:        cfg,
		Show:          show,
		Inbox:         inbox,
		Test:          testMgr,
		WSServer:      wsServer,
		Harness:       harness,
		StaticDir:     http.Dir(cfg.SolarRoot),
		QuasarBaseURL: cfg.QuasarBaseURL,
		LSDPHandler:   lsdpHandler,
		Preview:       previewSlot,
		PreviewLSDP:   previewLSDPHandler,
		AuthSource:    authSource,
		// Read-only DB catalog (ADR Blue 008 §3.4): same gateway as the
		// db.query client; both use the same exact-route exchange callback.
		SchemaClient: effects.NewSchemaClientWithPathTokenFunc(cfg.ZabGateURL, serviceTokenMinter.Token, nil),
		SceneIntent:  sceneIntent,
		StreamRules: &api.StreamRulesDeps{
			Plane:       rulePlane,
			BlueBaseURL: cfg.BlueBaseURL,
		},
	})

	// Internal-only HTTP surface for prom scrape + dev probes.
	internalMux := http.NewServeMux()
	internalMux.Handle("/orion/internal/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{Registry: metrics.Registry}))
	internalMux.HandleFunc("/orion/internal/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// embedded-local (#223, ADR 016 D4): in-process loopback guard, the
	// complement to the loopback-only listen addr. Even a mis-bind to a
	// routable interface would refuse off-host callers before any handler
	// (incl. the operator grant) runs. No-op on antenne — never wrapped.
	var publicHandler http.Handler = obs.Recover(logger, publicMux)
	if cfg.Profile.IsEmbeddedLocal() {
		publicHandler = auth.LoopbackOnly(publicHandler)
	}
	publicSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           publicHandler,
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
