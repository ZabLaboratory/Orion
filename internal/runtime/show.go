package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// Show is the singleton process-wide live show. It owns the roster
// of pre-built scenes (every active scene with a non-null
// latest_pushed_version per ADR 004 § 4.4) and the active-scene
// pointer. Live-show subscribers attach to the *show* (not to a
// specific scene); on scene_changed, the show migrates them to the
// destination scene's subscriber list so the WS connection survives.
type Show struct {
	registry *ComputeRegistry
	logger   *slog.Logger

	mu       sync.RWMutex
	scenes   map[string]*Scene
	active   string
	liveSubs []*Subscription

	// sceneVersions mirrors each loaded scene's LSML/graph content address
	// (graph.SceneVersion, the same value handed to MirrorFor). It is the
	// feed for the scene_roster preload frame (Prism#230): the roster is
	// IDs × versions. Kept in lock-step with `scenes` — set on LoadExec,
	// removed on Unload — so a roster snapshot is always consistent with
	// the live scene set.
	sceneVersions map[string]string

	// streamRules maps each promoted stream-level rule id to its KIND
	// (ADR 009 §3.1). A promoted rule is a roster scene that runs ALWAYS —
	// never gated on the active pointer, never frozen at a scene switch. The
	// set is the second leg of RouteTargets' union (the active scene being the
	// first). The kind (scene-based vs blueprint-direct, #287) drives the
	// DURABILITY path only — the boot reseed strategy and which persistence
	// table the API cleans on demote — never the routing, which is identical
	// for both. Membership (``_, ok := streamRules[id]``) is unchanged.
	streamRules map[string]RuleKind

	// mirrors is the optional LSDP/1.1 wire (ADR 007 §C.3b). nil in
	// bespoke mode — the entire kit path is then dead weight that never
	// runs (no-op deploy). In dual/lsdp mode the Show asks it for a
	// per-scene SceneMirror at Load and tells it which scene is active
	// at SetActive, so the kit serves the same source as the bespoke
	// wire.
	mirrors MirrorRegistry

	// execMetrics, when non-nil, is handed to every loaded scene so
	// the exec layer (ADR 003 §3.1.6, issue #82) reports shed /
	// preempt / parked counts. Installed once at boot.
	execMetrics ExecMetrics

	// wsMetrics, when non-nil, is handed to every loaded scene so the
	// fanout back-pressure collapse/close is observed (Orion#274, ADR-
	// BLUE-012 §12/B8). Installed once at boot, before any scene loads.
	wsMetrics WSMetrics

	// effects is the shared async-effect executor bundle (ADR 003
	// §3.1.3 / R9 lift ADR 006 §3.4). Built once at boot from config +
	// the service-token manager, installed on a scene ONLY when that
	// scene loads with a non-empty exec program set — i.e. a validated,
	// exec-bearing version (see LoadExec). nil = effects unconfigured
	// (dev / a deploy without the effect env): every world-effect op
	// then fails to its error port, fail-closed. The R9 invariant lives
	// in LoadExec's `len(progs) > 0` guard, NOT here.
	effects *SceneEffects

	// emitter is the active-only `show.emit` injection sink (ADR 009 §3.6,
	// issue #155). nil until SetEmitter wires it (the adapters.Inbox, which
	// owns the audit ring + the system-write path). A nil emitter leaves
	// every scene's emit op a construction-safe no-op (it still fires
	// `then`). Set once at boot, before live traffic; read under RLock.
	emitter Emitter

	ctx    context.Context
	cancel context.CancelFunc
}

// Emitter is the active-only injection sink the `show.emit` op delivers
// through (ADR 009 §3.6). The adapters.Inbox implements it: it builds a
// SYSTEM write `__events.<topic>` = payload, targets show.Active() ONLY
// (never RouteTargets — anti-cascade), records ONE audit entry, and
// delivers it to the active scene's loop. Defined here so the runtime
// package owns the contract while the implementation stays in adapters
// (where the audit ring + system-write seam live).
type Emitter interface {
	EmitToActive(topic string, payload json.RawMessage)
}

// SetEmitter installs the active-only `show.emit` injection sink (ADR 009
// §3.6, issue #155). Called once at boot, after the inbox is built and
// before live traffic. Every scene's emit closure reads sh.emitter at CALL
// time, so a scene loaded before this is wired still emits correctly once
// it is set — the only requirement is that it is set before any rule fires
// show.emit live.
func (sh *Show) SetEmitter(e Emitter) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.emitter = e
}

// emitToActive is the closure each scene's `show.emit` op invokes. It
// reads the emitter under RLock at CALL time (so boot-load order is
// irrelevant) and delegates the active-only injection. A nil emitter
// drops the emission (the op still fires `then` — construction-safe).
func (sh *Show) emitToActive(topic string, payload json.RawMessage) {
	sh.mu.RLock()
	e := sh.emitter
	sh.mu.RUnlock()
	if e != nil {
		e.EmitToActive(topic, payload)
	}
}

// emitSlotAssignment is the closure each scene's `assign-slot` op invokes on
// a successful ZabCam upsert (ADR Blue 009 §3.3, issue #260). It forwards the
// stream-level `slot_ref → peer_label` binding to the LSDP wire (the derived
// cache), which stores it and emits the re-keying delta. A nil wire (bespoke
// mode) drops it — the upsert is already durable in ZabCam, the LSDP mirror
// is best-effort. Read under RLock at CALL time so boot-load order (SetMirrors
// before scene loads) is irrelevant.
func (sh *Show) emitSlotAssignment(slotRef, peerLabel string) {
	sh.mu.RLock()
	m := sh.mirrors
	sh.mu.RUnlock()
	if m != nil {
		m.EmitSlotAssignment(slotRef, peerLabel)
	}
}

// emitOverlayApp is the closure each scene's `overlay-app.set` op invokes to
// forward the stream-level overlay control state to the LSDP wire (ADR 016
// Prism §3.2, issue #283). running / on_air may be nil (dimension unchanged).
// A nil wire (bespoke mode) drops it — the app's durable state belongs to the
// app itself (RC #11), the LSDP mirror is the derived cache. Read under RLock
// at CALL time so boot-load order (SetMirrors before scene loads) is
// irrelevant.
func (sh *Show) emitOverlayApp(appID string, running, onAir *bool) {
	sh.mu.RLock()
	m := sh.mirrors
	sh.mu.RUnlock()
	if m != nil {
		m.EmitOverlayApp(appID, running, onAir)
	}
}

// SetExecMetrics installs the exec-layer metrics sink (implemented by
// *obs.Metrics). Called once at boot, before any scene is loaded.
func (sh *Show) SetExecMetrics(m ExecMetrics) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.execMetrics = m
}

// SetWSMetrics installs the fanout back-pressure metrics sink (Orion#274,
// ADR-BLUE-012 §12/B8). Called once at boot, before any scene loads —
// mirrors SetExecMetrics.
func (sh *Show) SetWSMetrics(m WSMetrics) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.wsMetrics = m
}

// SetEffects installs the shared async-effect executor bundle (R9 lift,
// ADR 006 §3.4). Called once at boot, before any scene is loaded. The
// bundle itself confers NO capability: a scene only ever registers the
// world-effect ops when it loads with a non-empty validated exec set
// (LoadExec's guard). A nil bundle keeps effects unconfigured.
func (sh *Show) SetEffects(e *SceneEffects) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.effects = e
}

// MirrorRegistry is the Show-side handle on the LSDP/1.1 wire
// (ADR 007 §C.3b). The lsdp package implements it over a
// lumencast-go server.Server. nil = bespoke mode.
type MirrorRegistry interface {
	// MirrorFor returns the SceneMirror for the given scene id,
	// registering a paired kit scene if needed. sceneVersion is the
	// LSML/graph content address echoed on snapshot/scene_changed
	// frames. bundle is the active scene's render bundle — the wire
	// reads its bindings to emit ONLY the renderable leaf surface
	// (ADR 007 §C.3b hygiene; the compute intermediates never leave the
	// tap). A nil bundle disables the bound-leaf gate (fail-open to the
	// scalar filter), so a passthrough/operator-only scene is never
	// blacked out.
	MirrorFor(sceneID, sceneVersion string, bundle *compiler.RenderBundle) SceneMirror
	// SetActive tells the wire which scene the live endpoint serves,
	// mirroring Show.SetActive so the kit migrates its live subscribers.
	SetActive(sceneID string)
	// Drop removes a scene's paired kit scene (on Unload).
	Drop(sceneID string)
	// EmitRoster publishes the show's full scene roster (id × version) on
	// the wire so a runtime can preload every scene bundle ahead of a swap
	// (additive scene_roster frame, Prism#230). The wire caches it and
	// replays it to each new 1.1 subscriber after its snapshot. entries may
	// be empty (an idle show). A nil MirrorRegistry (bespoke mode) is a
	// no-op — there is no wire to preload against.
	EmitRoster(entries []RosterEntry)
	// EmitSlotAssignment records a stream-level `slot_ref → peer_label`
	// binding and emits an LSDP delta re-keying the slot on the active wire
	// (ADR Blue 009 §3.3, issue #260). The binding is stream-level: it
	// persists across scene switches and is replayed onto the destination
	// scene at SetActive, so a `meet-peer` slot of any scene of the stream
	// resolves to its bound peer. The leaf rides a reserved namespace that
	// bypasses the per-scene bound-leaf gate (it is not a scene leaf).
	EmitSlotAssignment(slotRef, peerLabel string)
	// EmitOverlayApp records the stream-level desired `{running, on_air}`
	// control state of an operator-declared overlay app and publishes the
	// complete show-level `overlay_apps` frame on the wire (ADR 016 Prism §3.2,
	// issue #283; channel changed in #292). running / on_air may be nil (that
	// dimension unchanged). Stream-level: the frame is show metadata (not scene
	// leaves), so the kit caches + replays it on join and it is deliverable even
	// with no active scene — no per-SetActive replay. Memory only (RC #11).
	EmitOverlayApp(appID string, running, onAir *bool)
}

// RosterEntry is one scene of the show's preload roster (scene_roster
// frame): the scene id and the LSML/graph content address the runtime
// should preload the bundle at. Mirrors protocol.RosterEntry in the
// kit; the lsdp Wire maps between the two.
type RosterEntry struct {
	SceneID      string
	SceneVersion string
}

// SetMirrors installs the LSDP/1.1 wire. Called once at boot in
// dual/lsdp mode, before any scene is loaded. nil keeps bespoke mode.
func (sh *Show) SetMirrors(m MirrorRegistry) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.mirrors = m
}

// ErrSceneNotFound is the API-facing miss.
var ErrSceneNotFound = errors.New("show: scene not found")

// ErrSceneNotPushed is the API-facing reject for activating a scene
// that has never been pushed.
var ErrSceneNotPushed = errors.New("show: scene not pushed")

// NewShow constructs an empty show. Scenes are added via Load.
func NewShow(registry *ComputeRegistry, logger *slog.Logger) *Show {
	ctx, cancel := context.WithCancel(context.Background())
	return &Show{
		registry:      registry,
		logger:        logger.With("component", "show"),
		scenes:        map[string]*Scene{},
		sceneVersions: map[string]string{},
		streamRules:   map[string]RuleKind{},
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Stop cancels every scene goroutine and waits for them to drain.
func (sh *Show) Stop() {
	sh.cancel()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, s := range sh.scenes {
		s.Stop()
	}
}

// Load adds a scene to the roster, starting its goroutine. Idempotent:
// loading a scene already in the roster swaps the artefacts atomically
// (used by the push handler when a re-push lands on an already-live
// scene — ADR 004 § 7).
func (sh *Show) Load(id string, graph *compiler.Graph, bundle *compiler.RenderBundle) {
	sh.LoadExec(id, graph, bundle)
}

// LoadExec is Load with the scene's exec program SET attached (ADR 003
// §3.1, issue #83; multi-program lift, ADR 006 §3.3 / issue #105). A
// live scene hosts ALL of its blueprints' programs — `progs` is the set
// `ExecProgramsFromGraph` returns; InstallExec merges their trigger
// indexes under namespaced keys.
//
// R9 lift (ADR 006 §3.4, issue #106): the production push/boot/validate/
// rollback paths now pass the program set resolved by the `execForAir`
// seam — non-empty IFF the scene_version carries a `validated` record for
// the current harness_version (the normative invariant). A non-validated
// or pure-dataflow scene still passes nil (len(progs)==0 → all trigger
// wiring below is inert), so authoring is never blocked.
//
// Re-push semantics (§3.1.4): swapping an already-loaded scene STOPS
// the previous instance — its live tasks, parked continuations and
// timers die with it (cancellation by teardown) — and the new instance
// starts from declared defaults (restart-reseed). If the swapped scene
// is the ACTIVE one, `on-start` fires on the fresh instance.
func (sh *Show) LoadExec(id string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) {
	// Registered before the unlock defer, so (LIFO) it runs AFTER sh.mu is
	// released: emitRoster takes its own RLock. A load/re-push mutates the
	// roster (new id or changed version), so the wire is refreshed here.
	defer sh.emitRoster()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.sceneVersions[id] = graph.SceneVersion
	if existing, ok := sh.scenes[id]; ok {
		existing.Stop()
	}
	scene := NewScene(id, graph, bundle, sh.registry, sh.logger)
	if sh.execMetrics != nil {
		scene.SetExecMetrics(sh.execMetrics)
	}
	if sh.wsMetrics != nil {
		scene.SetWSMetrics(sh.wsMetrics)
	}
	scene.InstallExec(progs...)
	// `show.emit` active-only injection seam (ADR 009 §3.6, issue #155):
	// every loaded scene (active, rule, or roster) gets the same closure —
	// it routes to show.Active() at call time, so the TARGET is always the
	// active scene regardless of which instance emits. This is the distinct
	// active-only path; it never touches RouteTargets (anti-cascade).
	scene.SetEmitEvent(sh.emitToActive)
	// Stream-level slot-binding seam (ADR Blue 009 §3.3, issue #260): every
	// loaded scene gets the same closure — the `assign-slot` op routes the
	// `slot_ref → peer_label` binding to the LSDP wire after a durable ZabCam
	// upsert. Stream-level: the binding outlives any single scene.
	scene.SetSlotAssigner(sh.emitSlotAssignment)
	// Stream-level overlay-app control seam (ADR 016 Prism §3.2, issue #283):
	// every loaded scene gets the same closure — the `overlay-app.set` op routes
	// the desired `{running, on_air}` state to the LSDP overlay mirror.
	// Stream-level: the state outlives any single scene.
	scene.SetOverlayAppSetter(sh.emitOverlayApp)
	// R9 world-effect install (ADR 006 §3.4, load-bearing). The
	// world-touching ops (http.request / db.query / source.read) are
	// registered ONLY when this scene loads with a non-empty exec set —
	// which `execForAir` returns IFF the scene_version carries a
	// `validated` record for the current harness_version. This is the
	// SAME validation-keyed seam that gates InstallExec above (the
	// program set is the single source of truth). A non-validated or
	// pure-dataflow scene gets len(progs)==0 → SetEffects is never
	// called → the ops are not in the registry → an authored
	// http.request/db.query/source.read halts-at-node with ZERO egress
	// or query (ADR 006 §3.4 résidu, Bastion #106). An unconfigured
	// bundle (sh.effects == nil) likewise never registers anything.
	if len(progs) > 0 && sh.effects != nil {
		scene.SetEffects(sh.effects)
	}
	// Stream-rule scope (ADR 009 §3.4): a promoted rule NEVER enters the
	// active-pointer lifecycle. It is loaded UNGATED and seeded on air
	// permanently, so the dataflow/trigger gate (scene.go:645/914,
	// `triggersGated && !onAir`) never blocks it and it executes through
	// every scene switch. Because it is never the `from`/`dest` of a
	// SetActive, nothing ever calls CancelExec/SetOnAir(false) on it —
	// it cannot be frozen by construction. This is the same ungated path
	// a test/validation clone takes; we additionally seed onAir so a
	// rule with an on-air-keyed read stays consistent. A re-push of a
	// promoted rule (restart-reseed) lands here too and stays ungated.
	if _, isRule := sh.streamRules[id]; isRule {
		scene.SeedOnAir(true)
	} else {
		// Air-only trigger scope (ADR 006 §3.4, issue #106): every roster
		// instance gates its on-tick/on-event firing on the on-air flag, so
		// a loaded-but-off-air validated scene stays exec-quiescent backstage
		// (zero effects, criterion #6). A push-swap of the CURRENTLY ACTIVE
		// scene seeds the fresh instance on air, so its on-tick chain is live
		// immediately (it replaces an on-air instance — FireOnStart below
		// also fires). Pre-Run, so SeedOnAir is a plain assignment.
		scene.GateTriggers()
		if sh.active == id {
			scene.SeedOnAir(true)
		}
	}
	// ADR 007 §C.3b: in dual/lsdp mode, pair the scene with a kit
	// scene and tap its output port. The mirror is seeded with the
	// freshly-seeded snapshot inside SetMirror before Run starts.
	if sh.mirrors != nil {
		scene.SetMirror(sh.mirrors.MirrorFor(id, graph.SceneVersion, bundle))
	}
	sh.scenes[id] = scene
	go scene.Run(sh.ctx)
	if _, isRule := sh.streamRules[id]; isRule {
		// Re-push / boot-reload of a promoted rule: the fresh instance
		// reseeds from defaults and fires its on-start once (ADR 009 §3.4
		// — FireOnStart at promotion AND at boot-reload). A rule is never
		// the active scene, so the active-id branch below never applies.
		scene.FireOnStart("system:stream-rule-reload")
	} else if sh.active == id {
		// Push-swap of the live scene: the fresh instance becomes
		// live now → defaults + on-start (ADR 003 §3.1.3/§3.1.4).
		scene.FireOnStart("system:scene-activated")
	}
}

// Unload stops a scene and drops it from the roster.
func (sh *Show) Unload(id string) {
	// Runs after sh.mu is released (LIFO with the unlock defer below):
	// dropping a scene shrinks the roster, so refresh the wire.
	defer sh.emitRoster()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if existing, ok := sh.scenes[id]; ok {
		existing.Stop()
		delete(sh.scenes, id)
		delete(sh.sceneVersions, id)
		if sh.mirrors != nil {
			sh.mirrors.Drop(id)
		}
	}
	if sh.active == id {
		sh.active = ""
	}
}

// emitRoster snapshots the loaded scene set (id × version) and publishes
// it on the wire as the show's preload roster (scene_roster frame,
// Prism#230). Called after every roster mutation (Load / re-push /
// Unload) and after SetActive. A nil MirrorRegistry (bespoke mode) is a
// no-op. The mirror call is made OUTSIDE sh.mu — callers register it as a
// defer BEFORE their unlock defer, or invoke it after releasing the lock.
func (sh *Show) emitRoster() {
	sh.mu.RLock()
	m := sh.mirrors
	if m == nil {
		sh.mu.RUnlock()
		return
	}
	ids := make([]string, 0, len(sh.scenes))
	for id := range sh.scenes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]RosterEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, RosterEntry{SceneID: id, SceneVersion: sh.sceneVersions[id]})
	}
	sh.mu.RUnlock()
	m.EmitRoster(entries)
}

// Get returns the scene by id (or nil + ErrSceneNotFound).
func (sh *Show) Get(id string) (*Scene, error) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	s, ok := sh.scenes[id]
	if !ok {
		return nil, ErrSceneNotFound
	}
	return s, nil
}

// IDs returns the loaded scene ids — used by /show.
func (sh *Show) IDs() []string {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	out := make([]string, 0, len(sh.scenes))
	for id := range sh.scenes {
		out = append(out, id)
	}
	return out
}

// Active returns the currently active scene (or nil if none set yet).
func (sh *Show) Active() *Scene {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.active == "" {
		return nil
	}
	return sh.scenes[sh.active]
}

// ActiveID returns the id of the currently active scene ("" if none).
// Used by the inbox to tag a drop metric without re-deriving the id.
func (sh *Show) ActiveID() string {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.active
}

// RouteTargets returns the union {active} ∪ {promoted stream-rules}
// (ADR 009 §3.3) — the set of scenes a wire/tick write fans out to. The
// active scene comes first (if any), then the promoted rules sorted by
// id for a deterministic order, de-duplicated (a scene is never both
// active and a rule — promotion refuses the active scene, #154 — but the
// guard keeps the contract local). Read under a single RLock so both
// producers (inbox.go, tick.go) snapshot a consistent set; a write in
// flight during a (de)promotion lands on the set as of its read —
// accepted, events are live-only (ADR 009 §3.3 linearisation).
//
// Disjoint from the non-promoted roster BY CONSTRUCTION: a roster scene
// that is neither active nor a promoted rule is absent from this slice,
// so it receives nothing (criterion #1 ADR 008 stays vert — the dormant
// backstage is dormant because it is not routed to).
//
// This is NOT the ADR 004 §5 rule-4 fan-out: the multiplicity is bounded
// to the operator-promoted set (typically 0–3), not the whole roster.
//
// Reserved for issue #155: the `show.emit` rule→active injection is a
// DISTINCT active-only system path and deliberately does NOT go through
// RouteTargets — routing it via this union would cascade rule→rule
// (loops). Callers of the emit path must target Active() directly, never
// this slice. See ADR 009 §3.6.
func (sh *Show) RouteTargets() []*Scene {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	out := make([]*Scene, 0, 1+len(sh.streamRules))
	seen := make(map[string]struct{}, 1+len(sh.streamRules))
	if sh.active != "" {
		if s := sh.scenes[sh.active]; s != nil {
			out = append(out, s)
			seen[sh.active] = struct{}{}
		}
	}
	ruleIDs := make([]string, 0, len(sh.streamRules))
	for id := range sh.streamRules {
		if _, dup := seen[id]; dup {
			continue
		}
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)
	for _, id := range ruleIDs {
		if s := sh.scenes[id]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

// ErrRuleIsActiveScene rejects promoting the currently active scene or
// activating a promoted rule (ADR 009 §3.1 — a rule and the active scene
// are disjoint roles). The full lifecycle reject set (SCENE_NOT_VALIDATED,
// SCENE_IN_USE, dépromotion, restart reload) is the public API of issue
// #154; this guard is the minimal in-runtime invariant the routing relies
// on.
var ErrRuleIsActiveScene = errors.New("show: scene is/would be the active scene")

// RuleKind distinguishes how a promoted stream-level rule is made durable
// across a restart (#287). Both kinds route identically; only the boot-reseed
// strategy and the persistence table differ.
type RuleKind uint8

const (
	// RuleKindScene is a rule promoted from a pushed, R9-validated scene. It
	// reseeds from its stored pushed version (show_stream_rules), the path that
	// already existed. The zero value, so an un-tagged promotion is scene-kind.
	RuleKindScene RuleKind = iota
	// RuleKindBlueprint is a rule promoted directly from a Blue blueprint with
	// no carrier scene. It has no stored pushed version, so it reseeds by
	// re-fetching + recompiling from Blue (show_blueprint_stream_rules).
	RuleKindBlueprint
)

// PromoteStreamRule loads (or reloads) a scene as a stream-level rule
// (ADR 009 §3.1/§3.4). Internal hook for issue #153 — the public,
// persisted promote API (validation gate, SCENE_NOT_VALIDATED, etc.) is
// issue #154, which resolves the compiled artefacts and passes them here.
// It:
//   - refuses the active scene (ErrRuleIsActiveScene);
//   - records the id in the rule set, then LOADS the instance ungated +
//     on-air via the LoadExec seam so the dataflow/trigger gate never
//     blocks it and it can never be frozen at a switch;
//   - fires on-start ONCE (inside LoadExec's rule branch) — FireOnStart at
//     promotion only, not at every scene switch.
//
// Going through LoadExec (rather than mutating a running instance) is the
// clean way to make the rule ungated: triggersGated is a pre-Run field
// owned by the scene goroutine, so the instance is swapped, not mutated
// in flight (single-writer preserved). The caller hands the artefacts;
// for a scene already in the roster they are its current graph/bundle/
// progs (#154 supplies them — it owns the compiled set already).
func (sh *Show) PromoteStreamRule(id string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) error {
	return sh.promoteRule(id, RuleKindScene, graph, bundle, progs...)
}

// PromoteBlueprintStreamRule is PromoteStreamRule for a BLUEPRINT-DIRECT rule
// (#287): a rule promoted from a Blue blueprint with no carrier scene, keyed
// by blueprint_id. The only difference is the recorded RuleKind, which the
// durability layer reads to pick the boot-reseed strategy (a blueprint rule
// reseeds by re-fetching + recompiling from Blue, a scene rule from its stored
// pushed version) and the persistence table to clean on demote. Routing and
// lifecycle are identical to a scene rule.
func (sh *Show) PromoteBlueprintStreamRule(id string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) error {
	return sh.promoteRule(id, RuleKindBlueprint, graph, bundle, progs...)
}

func (sh *Show) promoteRule(id string, kind RuleKind, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) error {
	sh.mu.Lock()
	if sh.active == id {
		sh.mu.Unlock()
		return ErrRuleIsActiveScene
	}
	sh.streamRules[id] = kind
	sh.mu.Unlock()
	// LoadExec sees the id in streamRules and takes the ungated + on-air
	// branch, then fires on-start once.
	sh.LoadExec(id, graph, bundle, progs...)
	return nil
}

// StreamRuleKind returns the kind of a promoted rule (scene-based vs
// blueprint-direct, #287) and whether the id is currently a promoted rule.
// The API's demote path reads it to clean the matching persistence table.
func (sh *Show) StreamRuleKind(id string) (RuleKind, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	kind, ok := sh.streamRules[id]
	return kind, ok
}

// StreamRuleIDs returns the ids of every promoted stream-level rule (scene_id
// or blueprint_id keys), sorted for a deterministic listing. In-memory is the
// authority here: it includes blueprint-direct rules the store does not persist
// yet. Read by the cockpit "Blueprints Stream-level" tab (GET /show/stream-rules).
func (sh *Show) StreamRuleIDs() []string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	ids := make([]string, 0, len(sh.streamRules))
	for id := range sh.streamRules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DemoteStreamRule removes a scene from the rule set and cancels its live
// tasks (ADR 009 criterion #5 — dépromotion cancels live work). Internal
// hook for issue #153; the persisted API is #154. The instance stays in
// the roster as a plain (gated, off-air unless active) scene; a caller
// that wants it gone calls Unload.
func (sh *Show) DemoteStreamRule(id string) {
	sh.mu.Lock()
	_, wasRule := sh.streamRules[id]
	delete(sh.streamRules, id)
	scene := sh.scenes[id]
	sh.mu.Unlock()
	if wasRule && scene != nil {
		scene.CancelExec()
		// It is no longer routed to (absent from RouteTargets); make it a
		// dormant backstage member by clearing its on-air flag unless it
		// happens to be the active scene.
		if sh.ActiveID() != id {
			scene.SetOnAir(false)
		}
	}
}

// IsStreamRule reports whether the scene id is a promoted stream rule.
// Used by tests and (later, #154) the active-scene guard.
func (sh *Show) IsStreamRule(id string) bool {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	_, ok := sh.streamRules[id]
	return ok
}

// StreamRuleScene returns the loaded scene of the promoted stream-level rule
// with this id (scene_id or blueprint_id), or nil when the id is not a
// currently promoted rule (never promoted, or demoted since — its scene may
// linger in the roster, so membership in streamRules is the authority). The
// operator routes use it to resolve ?rule={rule_id}: nil → RULE_NOT_ACTIVE.
// Atomic read — set membership and roster load are checked under one lock so a
// racing demote is reflected consistently.
func (sh *Show) StreamRuleScene(id string) *Scene {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if _, ok := sh.streamRules[id]; !ok {
		return nil
	}
	return sh.scenes[id]
}

// StreamRuleScenes returns the promoted stream-level rule scenes, sorted by
// id for a deterministic order (ADR 009 §3.3). Distinct from RouteTargets:
// this excludes the active scene — it is purely the always-on rule set, the
// `stream`-scoped contributors to the cockpit contract (ADR 008 §3.5). A
// promoted id with no loaded scene is skipped.
func (sh *Show) StreamRuleScenes() []*Scene {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	ids := make([]string, 0, len(sh.streamRules))
	for id := range sh.streamRules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Scene, 0, len(ids))
	for _, id := range ids {
		if s := sh.scenes[id]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

// SetActive flips the active-scene pointer, migrates every live-show
// subscriber from the previous scene to the new one, and emits
// scene_changed + fresh snapshot on the destination. ADR 004 § 4.4 +
// ADR 002 § 11.
func (sh *Show) SetActive(id string, transition json.RawMessage) error {
	sh.mu.Lock()
	dest, ok := sh.scenes[id]
	if !ok {
		sh.mu.Unlock()
		return ErrSceneNotFound
	}
	// A promoted stream rule is never the active scene (ADR 009 §3.1):
	// the two roles are disjoint, so RouteTargets never double-counts and
	// no rule is ever subjected to the switch-away CancelExec/SetOnAir.
	if _, isRule := sh.streamRules[id]; isRule {
		sh.mu.Unlock()
		return ErrRuleIsActiveScene
	}
	from := sh.active
	sh.active = id
	prev, hadPrev := sh.scenes[from]
	migrating := append([]*Subscription{}, sh.liveSubs...)
	mirrors := sh.mirrors
	sh.mu.Unlock()

	// Exec-layer lifecycle (ADR 003 §3.1.3/§3.1.4, issue #83) — both
	// calls are inert no-ops on scenes without an installed exec
	// program, i.e. every prod scene until the phase-4 gate (R9):
	// switch-away cancels all live tasks of the previous scene
	// version; the destination becoming live fires `on-start`.
	if from != id {
		// Switch-away (A→B): the previous scene leaves the antenna. Cancel
		// its live tasks (ADR 003 §3.1.4) AND clear its on-air flag so its
		// on-tick/on-event triggers stop firing while it sits backstage in
		// the roster (ADR 006 §3.4, criterion #6). The flag flip travels the
		// inbox — single-writer; CancelExec travels its own coalescing
		// channel.
		if hadPrev {
			prev.CancelExec()
			prev.SetOnAir(false)
		}
	} else {
		// Re-activation (from == id): activation is the canonical verb for
		// (re)launching exec, so on-start refires even when the scene is
		// already live (ADR 008 §3.2/§3.4/R2, Amendment 1 §A1.2). Cancel the
		// scene's own in-flight exec so the refire starts from a clean task
		// slate — but NEVER SetOnAir(false) (prev == dest: a scene must not
		// turn itself off air). State is NOT reseeded: refire ≠ reseed (§A1.5).
		dest.CancelExec()
	}
	// Destination takes the antenna and (re)fires on-start in BOTH branches.
	// Flag it on air BEFORE the on-start fire (both inbox messages, FIFO
	// arrival order), so any on-tick that lands after activation observes
	// onAir == true. SetOnAir is idempotent when already on air.
	dest.SetOnAir(true)
	dest.FireOnStart("system:scene-activated")

	// ADR 007 §C.3b: switch the kit's active scene too, so LSDP/1.1
	// live subscribers get scene_changed + a fresh snapshot off the
	// kit's own migration path (server.SetActive). Done first so the
	// kit observes the switch before the next emit fans out on the
	// destination.
	if mirrors != nil {
		mirrors.SetActive(id)
		// Re-publish the roster after the switch (contract: emit after
		// SetActive). The id × version set is unchanged by an activation,
		// so this is an idempotent refresh — it keeps a wire that armed
		// mid-switch consistent and costs one cached fan-out.
		sh.emitRoster()
	}

	// Step 1: detach migrating subs from the previous scene (if any).
	if hadPrev {
		for _, sub := range migrating {
			prev.Detach(sub)
		}
	}
	// Step 2: attach to destination, sending scene_changed first then
	// the fresh snapshot per ADR 002 § 5/7.
	//
	// Exception — the detached writer (ADR 002 § 11 writer-vs-viewer):
	// a sub with no prior scene (scene == nil, e.g. a service writer that
	// connected with the show idle) is not *transitioning* from one scene
	// to another. `scene_changed` is the A→B viewer transition signal;
	// there is no "from" scene here (from == ""). Its first activation is
	// an initial BIND, so it receives only the fresh `snapshot` — emitting
	// a `scene_changed{from:""}` would be a phantom transition. A sub that
	// WAS on a previous scene keeps the full scene_changed + snapshot pair.
	//
	// Re-activation (from == id, ADR 008 Amendment 1 §A1.3/criterion #10):
	// there is no A→B viewer transition, so NO `scene_changed` is emitted —
	// emitting `scene_changed{from==to}` would be a phantom transition. The
	// subscriber still receives the fresh `snapshot` below so its values
	// resync after the on-start refire. The emit is gated strictly behind
	// `from != id` so the re-activation branch never reaches it.
	for _, sub := range migrating {
		wasDetached := sub.scene == nil
		snap := dest.AttachExisting(sub)
		if from != id && !wasDetached {
			// trySend (not a raw send): a subscriber can Close concurrently
			// with this migration (WS disconnect racing a scene switch) —
			// same close-vs-send race scene.go's fanout closes.
			sub.trySend(&protocol.SceneChanged{
				FromSceneID: from,
				ToSceneID:   id,
				Transition:  transition,
			})
		}
		// Reset the destination scene's sequence so the snapshot
		// reseeds it (ADR 002 § 7).
		sub.trySend(snap)
	}
	dest.state.ResetSequence()
	return nil
}

// SubscribeLive attaches a new live-show subscription to the
// currently active scene. Returns the subscription and the initial
// snapshot.
func (sh *Show) SubscribeLive(buf int) (*Subscription, *protocol.Snapshot, error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.active == "" {
		return nil, nil, ErrSceneNotFound
	}
	scene := sh.scenes[sh.active]
	if scene == nil {
		return nil, nil, ErrSceneNotFound
	}
	sub, snap := scene.Subscribe(buf)
	sh.liveSubs = append(sh.liveSubs, sub)
	return sub, snap, nil
}

// SubscribeLiveWriter attaches a live-show subscription for a
// scene-independent WRITER (a `service`-role client such as Quasar)
// that pushes input leaves continuously, outside any scene lifecycle.
//
// Contract (writer vs viewer on /show/stream): a viewer needs an
// active scene to receive deltas, so SubscribeLive returns
// ErrSceneNotFound when none is active. A service writer does NOT —
// platform events (Twitch chat/follow/sub) arrive continuously, hors
// de tout cycle de scène ; gating the writer on an active scene loses
// every event at show start / scene switch and makes the coupling
// fragile. So this never errors on an empty active pointer.
//
// When no scene is active the returned subscription is DETACHED
// (scene == nil): it carries a live Out channel but is not bound to
// any scene, so it receives no deltas (there is nothing to mirror) and
// no initial snapshot. It is registered in liveSubs, so the next
// SetActive migrates it onto the freshly-activated scene exactly like
// any other live subscriber (AttachExisting) — at which point the
// writer also starts receiving that scene's deltas. When a scene IS
// already active, this behaves like SubscribeLive but the caller may
// choose to ignore the snapshot (a writer-only client does).
//
// Writes are unaffected by attachment: the inbox routes every accepted
// write to the ACTIVE scene iff it declares the path (Inbox.Write,
// ADR 008 §3.1), independently of this subscription.
func (sh *Show) SubscribeLiveWriter(buf int) (*Subscription, *protocol.Snapshot) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.active != "" {
		if scene := sh.scenes[sh.active]; scene != nil {
			sub, snap := scene.Subscribe(buf)
			sh.liveSubs = append(sh.liveSubs, sub)
			return sub, snap
		}
	}
	// No active scene: hand back a detached, live subscription so the
	// writer stays connected and is migrated on the next SetActive.
	if buf < 16 {
		buf = 16
	}
	sub := &Subscription{Out: make(chan SubscriberMsg, buf)}
	sh.liveSubs = append(sh.liveSubs, sub)
	return sub, nil
}

// UnsubscribeLive detaches a live-show subscription. The caller is
// expected to call this from the WS handler's defer.
func (sh *Show) UnsubscribeLive(sub *Subscription) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for i, s := range sh.liveSubs {
		if s == sub {
			sh.liveSubs = append(sh.liveSubs[:i], sh.liveSubs[i+1:]...)
			break
		}
	}
}
