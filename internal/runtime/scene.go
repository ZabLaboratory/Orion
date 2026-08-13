package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// InputMsg is what an adapter, operator, or service produces. The
// Inbox is where these land before reaching a scene's loop.
type InputMsg struct {
	Path        string
	Value       json.RawMessage
	Source      string // server-trusted identity prefix per ADR 002 § 6
	ClientMsgID string
	IsSystem    bool // true for tick / __system writes — bypass scope checks

	// FireExec / ResumeExec route exec-layer control through the SAME
	// inbox as state writes (ADR 003 §3.1, issue #82): arrival order
	// between fires, resumes and writes is the processing order, and
	// the budget check / task creation happen on the scene goroutine —
	// single-writer by construction. INTERNAL ONLY: no wire surface
	// (ws/adapters) ever populates these; the phase-3 authenticated
	// completion contract (B-syswrite) is the only future external
	// producer of resumes, behind its own role+token checks.
	FireExec   string // exec entrypoint id to fire
	ResumeExec string // wake key of a parked continuation to resume
	// SetOnAir toggles the scene instance's on-air flag (ADR 006 §3.4,
	// issue #106). It routes through the SAME inbox as fires and state
	// writes so the flag is owned by the scene goroutine alone
	// (single-writer): the Show flips it true on the destination and
	// false on the previous scene at SetActive, ordered against the
	// on-start fire and the tick stream by inbox arrival. nil = not an
	// on-air control message. The on-air flag gates on-tick/on-event
	// firing of a live roster instance so an off-air, validated, loaded
	// scene runs ZERO exec effects backstage (criterion #6).
	SetOnAir *bool
	// ResumeEnv carries the completion bindings of an async effect
	// (phase 3, issue #85): merged into the parked continuation's
	// environment on the scene goroutine, just before re-enqueue.
	// Ownership transfers with the message — the producer (worker
	// pool) never touches the map after Input.
	ResumeEnv map[string]json.RawMessage

	// FireEnv carries the data-out bindings seeded into a FireExec task's
	// environment (Orion #209): the operator-call route binds the request
	// `payload` under the on-call node's pin here, parity with on-event's
	// `<node>.payload`. Empty for an unparametrised fire.
	FireEnv map[string]json.RawMessage

	// Control runs an arbitrary read/mutate closure ON the scene goroutine
	// (Orion #209): the operator routes (resolve / pending list) need a
	// synchronous result computed under single-writer, so they enqueue a
	// closure that touches scene state and signals its own reply channel.
	// Mutually exclusive with the other fields; nil for a plain input.
	Control func(*Scene)
}

// SubscriberMsg is the union of messages a subscription receives.
// Concrete types: *protocol.Snapshot, *protocol.Delta,
// *protocol.SceneChanged, *protocol.Error. The WS layer encodes them
// onto the wire; tests assert on the typed values directly.
type SubscriberMsg = any

// SceneMirror is an optional, write-only observer of a scene's
// outbound stream (ADR 007 §C.3b, the LSDP/1.1 wire seam). Every
// message the bespoke fan-out produces is *also* handed to the mirror,
// so a second wire (the lumencast-go LSDP/1.1 server) can be driven
// from the **same** reactive source without the engine knowing which
// wires exist.
//
// The reactive loop never moves: the mirror is a tap on the output
// port (`fanout`), not a replacement for it. In `bespoke` mode the
// mirror is nil and the tap is a no-op — a deploy with the flag unset
// changes nothing.
//
// Forward is called on the scene goroutine while the subscriber list
// lock is NOT held; implementations must not block (the kit's Emit is
// non-blocking with snapshot-collapse back-pressure).
type SceneMirror interface {
	Forward(msg SubscriberMsg)
}

// Subscription is one client's lease on a scene's outbound stream.
// The scene loop pushes onto Out; the WS layer drains it. A bounded
// channel + the per-connection Drop policy implements the
// backpressure rule from ADR 002 § 9.
type Subscription struct {
	Out    chan SubscriberMsg
	closed atomic.Bool
	scene  *Scene
	// mu serializes Close's close(Out) against every send on Out. fanout
	// (and Show's scene-switch migration) take a snapshot of the
	// subscriber list OUTSIDE any per-subscription lock, so a concurrent
	// Close can run between that snapshot and a send already headed for
	// this subscription — a real send-on-a-closing-channel race, not a
	// -race false positive (caught in CI on #331, TestOperator_
	// CallRuleSelectorFires). Every direct Out send/drain goes through
	// trySend/drainAndSeed below instead of touching the channel raw.
	mu sync.Mutex
}

// Close drains the subscriber and removes it from the scene. Idempotent.
func (s *Subscription) Close() {
	if s.closed.Swap(true) {
		return
	}
	if s.scene != nil {
		s.scene.unsubscribe(s)
	}
	s.mu.Lock()
	close(s.Out)
	s.mu.Unlock()
}

// trySend attempts a non-blocking delivery to Out, mutually exclusive
// with Close. Returns false if the subscription is already closed
// (never touches Out) or its queue is full (Out untouched, caller
// decides — e.g. collapse to a fresh snapshot).
func (s *Subscription) trySend(msg SubscriberMsg) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	select {
	case s.Out <- msg:
		return true
	default:
		return false
	}
}

// drainAndSeed empties Out then delivers snap, waiting up to timeout —
// same mutual exclusion with Close as trySend. Returns false if the
// subscription was already closed (Out untouched) or the send timed out
// (the caller then closes the stuck subscriber, outside this lock to
// avoid Close's own lock acquisition deadlocking against this one).
func (s *Subscription) drainAndSeed(snap SubscriberMsg, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
drain:
	for {
		select {
		case <-s.Out:
		default:
			break drain
		}
	}
	select {
	case s.Out <- snap:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Scene is one live scene instance — a graph + state + a goroutine
// that drives the drain-then-compute loop.
type Scene struct {
	id string
	// streamKey is the per-stream egress-budget bucket key (ADR Blue 009
	// §B / R3). Empty resolves to the singleton live show (defaultStreamKey)
	// — every live roster scene shares ONE budget. An isolated execution
	// context (preview slot, test session) sets its own key via SetStreamKey
	// so its egress is metered independently and can never drain the live
	// budget (nor be drained by it). Pre-Run only; read on the scene goroutine.
	streamKey string
	graph     *compiler.Graph
	bundle    *compiler.RenderBundle
	state     *State
	cmpReg    *ComputeRegistry
	logger    *slog.Logger

	inbox chan InputMsg

	subsMu sync.Mutex
	subs   []*Subscription

	// ctx and cancel are bound at NewScene so Stop can safely fire
	// before Run has had a chance to start. Running both Run and
	// Stop concurrently used to race on cancel; pre-binding the
	// context closes that window.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// computeOrder is the topological compute list, cached at scene
	// load. Each entry is a runtime view of a graph node that the
	// recompute loop walks.
	computeOrder []computeEntry

	// O(1) load-time indexes (issue #80, ADR 003 §3.1.5). Built once
	// in NewScene, read-only afterwards (scene goroutine only).
	//
	// nodePath maps a node id to the state path its value lives at
	// (GraphNode.Path, or the node id for unnamed intermediates) —
	// replaces the per-lookup linear scan of graph.Nodes.
	nodePath map[string]string
	// consumers maps a state path to the computeOrder indexes of the
	// nodes that read it as an upstream. This is the reverse-adjacency
	// index the dirty-cone walk follows downstream.
	consumers map[string][]int
	// pending accumulates the state paths whose value actually changed
	// since the last recompute (applyInput writes that Set accepted).
	// They seed the dirty cone; recompute consumes and clears them.
	pending map[string]struct{}

	// mirror, when non-nil, taps every outbound message for a second
	// wire (LSDP/1.1 via lumencast-go — ADR 007 §C.3b). nil = bespoke
	// mode, the tap is inert.
	mirror SceneMirror

	// nodeIdx maps a node id to its computeOrder index — the demand-
	// evaluation entry point of the exec layer's data pulls (issue
	// #82). Built once in NewScene, read-only afterwards.
	nodeIdx map[string]int

	// --- exec layer (ADR 003 §3.1, issue #82) -------------------------
	// All of the following is owned by the scene goroutine after Run
	// starts; the Set*/Install* mutators are pre-Run only.
	//
	// execProgs holds every installed exec program of this scene, keyed
	// by blueprint_key (shared, read-only after InstallExec). A live
	// scene hosts ALL of its blueprints' programs (issue #105); a
	// validation clone hosts exactly one. nil/empty = no exec layer (every
	// prod scene until the phase-4 activation gate — exec stays dormant).
	execProgs map[string]*ExecProgram
	// execEntries resolves a namespaced trigger key `<blueprint_key>/<id>`
	// to the owning program and the program-local entrypoint. Built by
	// InstallExec, read-only after; the firing paths look entries up here
	// so a task is born bound to the right program (its node ids and
	// blueprint key are program-local). Issue #105.
	execEntries map[string]execEntryRef
	// execQueue is the FIFO of runnable tasks: round-robin via
	// runExecSlice (pop head, re-enqueue at tail on preemption) —
	// deterministic order, never map-driven.
	execQueue []*execTask
	// execParked holds suspended continuations by wake key. Issue #83
	// adds the B8 cap + timer wheel on top of this map.
	execParked map[string]*execTask
	// execTaskSeq numbers tasks deterministically.
	execTaskSeq uint64
	// execBudget is the B5 per-scene concurrent-task budget; <= 0
	// disables it. New fires beyond it are shed (counted), running
	// tasks are never touched.
	execBudget int
	// execSliceSteps / execSliceDur bound one time slice (yield +
	// re-enqueue, never kill).
	execSliceSteps int
	execSliceDur   time.Duration
	// effector is the single effect seam (live: sceneEffector;
	// phase 4 swaps a validation-mode implementation).
	effector Effector
	// execOps holds extension ops (issue #83 delay, phase-3 effects,
	// test latents).
	execOps map[string]execOpFn
	// execMetrics is the observability sink (nil = disabled).
	execMetrics ExecMetrics
	// effects bundles the phase-3 async-effect executors (issue #85).
	// Installed by SetEffects pre-Run only; nil = effects unconfigured
	// (every async-effect op then fails to its error port, fail-closed).
	// R9: no production path installs it before the phase-4 gate.
	effects *SceneEffects

	// emit is the `show.emit` rule→antenna injection seam (ADR 009 §3.6,
	// issue #155). Set by the Show at Load to a closure that delivers a
	// system `__events.<topic>` write to show.Active() ONLY — a distinct
	// active-only path, never RouteTargets, so a rule's emission can never
	// cascade rule→rule (anti-loop by construction). nil = unwired (no
	// active scene reachable / not yet wired): the executor then no-ops the
	// injection and still fires `then` (construction-safe, no error pin —
	// Blue#73 contract). Read on the scene goroutine; the closure itself is
	// concurrency-safe (it routes through the audited inbox).
	emitEvent func(topic string, payload json.RawMessage)

	// assignSlot is the stream-level slot-binding seam (ADR Blue 009 §3.3,
	// issue #260). Set by the Show at Load to a closure that records
	// `slot_ref → peer_label` at stream level and emits an LSDP delta re-keying
	// the slot (the derived cache). The `assign-slot` op invokes it ONLY after
	// a 2xx ZabCam upsert, on the scene goroutine (finishEffect). nil = unwired
	// (bespoke mode / no mirror): the op still binds ok and fires `then`, the
	// mirror is simply absent — never an error. Read on the scene goroutine.
	assignSlot func(slotRef, peerLabel string)

	// overlayAppSet is the stream-level overlay-app control seam (ADR 016 Prism
	// §3.2, issue #283). Set by the Show at Load to a closure that forwards the
	// desired `{running, on_air}` state (either may be nil = unchanged) to the
	// LSDP overlay mirror. The `overlay-app.set` op invokes it on the scene
	// goroutine. nil = unwired (bespoke mode / no mirror): the op still fires
	// `then`, the mirror is simply absent — never an error.
	overlayAppSet func(appID string, running, onAir *bool)

	// --- timer wheel / triggers / cancellation (issue #83) ------------
	// clock is the injectable time source the wheel runs on
	// (systemClock in prod, fake clock in tests).
	clock Clock
	// wheel holds the armed timer entries (min-heap by deadline +
	// park order); wheelTimer/wheelC is the single Timer pointed at
	// the earliest deadline, consumed in Run's select. nil wheelC
	// blocks forever — a scene that never delays pays nothing.
	wheel      wheelHeap
	wheelTimer Timer
	wheelC     <-chan time.Time
	// execWheelSeq orders same-deadline entries deterministically.
	execWheelSeq uint64
	// execEpoch is the cancellation epoch wake keys are stamped with
	// (with the scene version): cancelExecTasks bumps it, so any
	// in-flight resume minted before the cut arrives stale and is
	// dropped + counted (ADR 003 §3.1.4).
	execEpoch uint64
	// execWakeSeq numbers minted wake keys.
	execWakeSeq uint64
	// animGeneration is the per-scene monotone `animation.play`
	// generation counter (issue #86) — scene goroutine only, never
	// derived from map order (deterministic).
	animGeneration uint64
	// execParkCap is the B8 cap on parked timers/continuations;
	// <= 0 disables it. A NEW park beyond it is shed (counted) —
	// parked/running tasks are never touched.
	execParkCap int
	// execCancelCh delivers CancelExec requests to the loop on a
	// dedicated 1-buffered channel (coalescing, never lost to a full
	// inbox).
	execCancelCh chan struct{}
	// execLastTickMs is the previous global-tick timestamp this scene
	// observed, for the on-tick `delta_seconds` binding (-1 = none).
	execLastTickMs int64
	// execOnStart/execOnTick/execOnEvent/execOnPlatform are the trigger
	// indexes built by InstallExec, in sorted entry-key order
	// (deterministic firing, never map iteration). Pre-Run only; read-only
	// after. execOnPlatform is keyed by the full `__inputs.platform.*` leaf
	// an on-platform-event entry observes (ADR 013) — the platform-namespace
	// twin of execOnEvent (keyed by the `__events.` topic).
	execOnStart    []string
	execOnTick     []string
	execOnEvent    map[string][]string
	execOnPlatform map[string][]string

	// --- validation mode (ADR 003 §3.2, issue #87) -------------------
	// validationMode makes the effect seam STRUCTURALLY inert (B10): a
	// world-touching async op is routed through validationEffect in
	// execNode and its I/O closure is never invoked. Set pre-Run only by
	// the validation harness; false for every live/test scene.
	validationMode bool
	// validation captures per-entrypoint observations (leaves, effects,
	// node coverage) while a validation campaign fires entrypoints.
	// nil outside a campaign — every record* call is then a no-op.
	validation *validationCapture

	// --- on-air trigger scope (ADR 006 §3.4, issue #106) -------------
	// The global tick fans out to EVERY loaded scene (tick.go), and an
	// off-air scene may be loaded, validated and carrying exec programs
	// (it sits in the roster awaiting activation). Without gating, its
	// `on-tick`/`on-event` chains would fire REAL effects backstage —
	// the franchissement leak risk R-4. So a LIVE ROSTER instance fires
	// those triggers only while on air.
	//
	// triggersGated marks an instance whose on-tick/on-event firing is
	// gated on onAir. The Show sets it on every roster instance at Load.
	// A test-session / validation clone leaves it false: authors iterate
	// freely (ADR §3.4 — test sessions untouched), so their triggers
	// always fire. `on-start` is NEVER gated here: it is fired explicitly
	// only at activation (Show.SetActive / FireOnStart) — an off-air
	// scene's on-start is never requested.
	//
	// Both fields are owned by the scene goroutine: triggersGated is set
	// pre-Run (like the other Install/Set mutators); onAir is flipped
	// ONLY via the SetOnAir inbox message, applied on the scene goroutine
	// — single-writer, race-free under -race with no concurrent read.
	triggersGated bool
	onAir         bool

	// pendingAwaits holds the live `operator.await-value` suspension points
	// of this scene instance, keyed `<blueprint_key>/<await_name>` (Orion
	// #209, Blue ADR 008 §3.3). Each entry carries the wake key of the
	// parked continuation plus the metadata the operator surface publishes
	// (value_type, ui). Scene-goroutine only — registered when a chain
	// reaches an await node, consumed (and deleted) by a resolve, and
	// cleared wholesale by cancelExecTasks: a switch-away / re-push / archive
	// invalidates every await of the leaving scene version (ADR 008
	// invariant 7), so a late resolve finds no entry and the route answers
	// 410. nil until the first await parks.
	pendingAwaits map[string]*pendingAwait
}

type computeEntry struct {
	node     compiler.GraphNode
	upstream []string // upstream node ids — from node.Inputs when carried (issue #79), else node.Upstream
}

// NewScene constructs a Scene from compiled artefacts and seeds its
// state from the graph defaults. The goroutine starts only after Run
// is called by the Show.
func NewScene(id string, graph *compiler.Graph, bundle *compiler.RenderBundle, registry *ComputeRegistry, logger *slog.Logger) *Scene {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scene{
		id:     id,
		graph:  graph,
		bundle: bundle,
		state:  NewState(),
		cmpReg: registry,
		logger: logger.With("scene_id", id),
		inbox:  make(chan InputMsg, 256),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),

		execParked:     map[string]*execTask{},
		execBudget:     defaultExecTaskBudget,
		execSliceSteps: defaultExecSliceSteps,
		execSliceDur:   defaultExecSliceDuration,
		execParkCap:    defaultExecParkCap,
		execCancelCh:   make(chan struct{}, 1),
		execLastTickMs: -1,
		clock:          systemClock{},
	}
	s.effector = &sceneEffector{s}
	// `delay` is a built-in latent op, registered through the same
	// extension seam phase 3's async effects use (issue #83).
	s.registerExecOp(OpDelay, execDelay)
	// `animation.play` is a built-in latent op too (issue #86): pure
	// scene-state machinery (state write + park + timer fallback), no
	// external dependency — unlike the SetEffects ops. R9 holds because
	// no production path installs an ExecProgram before the phase-4
	// gate (#87); without a program the op can never fire.
	s.registerExecOp(OpAnimationPlay, execAnimationPlay)
	// `operator.await` (Orion #209, Blue ADR 008 §3.3): the suspend twin of
	// `delay` — parks the chain awaiting an external operator value, no
	// timer. Pure scene-state machinery (park + registry), no dependency.
	s.registerExecOp(OpOperatorAwait, execOperatorAwait)
	// `show.emit` (ADR 009 §3.6, issue #155): the rule→antenna bridge. Pure
	// scene machinery (read config/input + inject through the Show's emit
	// seam), no external dependency — like animation.play. R9 holds because
	// no production path installs an ExecProgram before the phase-4 gate
	// (#87); without a program the op can never fire. The active-only
	// injection seam (s.emitEvent) is wired by the Show at Load.
	s.registerExecOp(OpShowEmit, execShowEmit)
	// `overlay-app.set` (ADR 016 Prism §3.2, issue #283): the stream-level
	// overlay-app control primitive. Pure scene machinery (read app_id +
	// running/on_air, forward through the Show's overlay-mirror seam), no
	// external dependency — like show.emit. R9 holds (no ExecProgram before
	// the phase-4 gate). The mirror seam (s.overlayAppSet) is wired at Load.
	s.registerExecOp(OpOverlayAppSet, execOverlayAppSet)
	s.state.Seed(graph.Defaults)
	// O(1) node-id → state-path index (issue #80): one pass, then
	// every upstreamPath call is a map hit instead of an O(N) scan.
	s.nodePath = make(map[string]string, len(graph.Nodes))
	for _, n := range graph.Nodes {
		if n.Path != "" {
			s.nodePath[n.ID] = n.Path
		} else {
			s.nodePath[n.ID] = n.ID
		}
	}
	// O(1) node-id → computeOrder index, for the exec layer's
	// demand-driven data pulls (issue #82).
	s.nodeIdx = make(map[string]int, len(graph.Nodes))
	for _, n := range graph.Nodes {
		// The dirty-check upstream set derives from the NAMED wiring when
		// the artefact carries it (issue #79) — Inputs is authoritative;
		// the compiler keeps Upstream zipped 1:1 with it, so for compiled
		// graphs the two are identical. Pre-#79 artefacts carry only
		// Upstream and keep working unchanged.
		up := n.Upstream
		if len(n.Inputs) > 0 {
			up = make([]string, len(n.Inputs))
			for i, in := range n.Inputs {
				up[i] = in.From
			}
		}
		s.computeOrder = append(s.computeOrder, computeEntry{node: n, upstream: up})
		s.nodeIdx[n.ID] = len(s.computeOrder) - 1
	}
	// Reverse-adjacency index (issue #80, ADR 003 §3.1.5): for each
	// recomputable node, register it as a consumer of every upstream
	// state path. The dirty-cone walk seeds from the changed paths and
	// follows this index downstream — cost proportional to the affected
	// cone, never the scene. Input-kind nodes never recompute, so they
	// take no consumer entry.
	s.consumers = make(map[string][]int)
	s.pending = make(map[string]struct{})
	for idx, ce := range s.computeOrder {
		if ce.node.Kind == "input" {
			continue
		}
		seen := make(map[string]struct{}, len(ce.upstream))
		for _, up := range ce.upstream {
			p := s.upstreamPath(up)
			if _, dup := seen[p]; dup {
				continue // two ports off the same upstream — index once
			}
			seen[p] = struct{}{}
			s.consumers[p] = append(s.consumers[p], idx)
		}
	}
	// Cold-start compute: Seed only fills constant/input leaves, so without
	// an initial forced pass every COMPUTED leaf (math/compare/logic/output)
	// would be absent from the first snapshot — a blueprint-backed scene
	// would render blank until the first input arrived. Run the topo-sorted
	// graph once now (force, ignoring dirtiness) so the scene is fully
	// evaluated before the first Subscribe. Synchronous + pre-Run, so no
	// subscriber can observe the un-evaluated state.
	s.recompute(true)
	return s
}

// ID returns the scene's id.
func (s *Scene) ID() string { return s.id }

// defaultStreamKey is the egress-budget bucket every live roster scene
// shares: Orion runs a single live show and "the show IS the stream"
// (api/cockpit.go). A scene with no explicit key meters its service.call
// egress against this one live budget (ADR Blue 009 §B / R3).
const defaultStreamKey = "live"

// SetStreamKey overrides the per-stream egress-budget bucket key for an
// ISOLATED execution context (preview slot, test session). Pre-Run only,
// like the other exec mutators. An empty key (the live roster default)
// resolves to defaultStreamKey at the call site. Giving each preview /
// test session its own key keeps its egress metered independently of the
// live show — neither can drain the other's budget (the isolation RC).
func (s *Scene) SetStreamKey(key string) { s.streamKey = key }

// egressStreamKey resolves the bucket key the per-stream egress budget
// charges this scene's service.call against.
func (s *Scene) egressStreamKey() string {
	if s.streamKey != "" {
		return s.streamKey
	}
	return defaultStreamKey
}

// GateTriggers marks this instance a LIVE ROSTER member whose
// on-tick/on-event firing is gated on the on-air flag (ADR 006 §3.4,
// issue #106). Pre-Run only, like the other exec mutators — the Show
// calls it at Load before scene.Run starts. A scene left ungated (test
// session / validation clone) fires its triggers freely. on-start is
// never affected (it is fired explicitly only at activation).
func (s *Scene) GateTriggers() { s.triggersGated = true }

// SeedOnAir sets the on-air flag directly, pre-Run only (the scene
// goroutine is not yet running, so this is a plain assignment with no
// concurrent reader — same contract as InstallExec/SetMirror). The Show
// uses it for a push-swap of the ACTIVE scene: the fresh instance
// replaces an on-air one, so it must START on air or its on-tick chain
// would stay dead until the next SetActive. The runtime swap path
// guarantees Run has not started when this is called.
func (s *Scene) SeedOnAir(onAir bool) { s.onAir = onAir }

// SetOnAir requests the on-air flag flip through the inbox so the scene
// goroutine is the sole writer (single-writer, ADR 006 §3.4). Returns
// false if the inbox is full. Safe from any goroutine; the Show calls it
// at SetActive (true on the destination, false on the previous scene).
func (s *Scene) SetOnAir(onAir bool) bool {
	return s.Input(InputMsg{SetOnAir: &onAir})
}

// Graph exposes the compiled graph artefact (used by adapters to
// read declared bindings).
func (s *Scene) Graph() *compiler.Graph { return s.graph }

// Bundle exposes the render bundle artefact (served by the API).
func (s *Scene) Bundle() *compiler.RenderBundle { return s.bundle }

// SnapshotState returns a deep copy of this scene's live state — the
// preview→air hand-off export seam (ADR Prism 005 Amendment 2 §A2.2.d).
// It delegates to State.Snapshot (lock-guarded deep-copy already used to
// seed WS subscribers), so the returned map is the caller's to mutate
// freely without touching live state. The version travels alongside so
// the import side can enforce a strict match (R11): it is the compiled
// graph's own scene_version, the version actually running on the sidecar.
func (s *Scene) SnapshotState() (version string, seq uint64, state map[string]json.RawMessage) {
	seq, state = s.state.Snapshot()
	return s.graph.SceneVersion, seq, state
}

// SeedState replaces values at the given paths through State.Seed
// (lock-guarded; safe to call concurrently with the running scene
// goroutine, which takes the same lock for every state access). It is the
// import side of the hand-off (ADR Prism 005 Amendment 2 §A2.2.d): the
// caller MUST have validated `state` fail-closed BEFORE calling this — Seed
// itself writes whatever it is handed. The Show calls it on the DESTINATION
// scene strictly before SetOnAir/FireOnStart, so the antenna takes the
// seeded state, never a virgin one.
func (s *Scene) SeedState(state map[string]json.RawMessage) {
	s.state.Seed(state)
}

// DeclaredKeyspace returns the set of author-declared leaf paths of this
// scene's compiled graph — the union of every node's declared state path
// and every seedable default. It is the fail-closed whitelist the import
// seam checks each snapshot path against (ADR Prism 005 Amendment 2,
// Bastion VETO #1): a path absent from this set is engine/platform-internal
// or forged, never author state, and the whole snapshot is rejected. Built
// from the graph, so it is exactly the keyspace of the TARGET version.
func (s *Scene) DeclaredKeyspace() map[string]struct{} {
	ks := make(map[string]struct{}, len(s.graph.Defaults)+len(s.graph.Nodes))
	for p := range s.graph.Defaults {
		ks[p] = struct{}{}
	}
	for _, n := range s.graph.Nodes {
		if n.Path != "" {
			ks[n.Path] = struct{}{}
		}
	}
	return ks
}

// SetMirror attaches (or clears, with nil) the LSDP/1.1 output tap
// (ADR 007 §C.3b). It must be called before Run starts, while no
// subscriber is attached — the Show wires it at Load time. Passing a
// non-nil mirror also seeds it with the scene's current snapshot so a
// late-attached kit scene starts from the same state.
func (s *Scene) SetMirror(m SceneMirror) {
	s.mirror = m
	if m == nil {
		return
	}
	seq, state := s.state.Snapshot()
	// Through tapMirror so a panic in the kit seed is recover-isolated
	// too (a faulty mirror must not crash Show.Load).
	s.tapMirror(&protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	})
}

// Run starts the scene goroutine. Blocks until the scene's context
// is cancelled (via Stop or via the parent ctx going down).
//
// parentCtx links the scene's lifecycle to the show's: when the show
// is stopped, every scene's parentCtx fires Done and the watcher
// goroutine below cancels each scene in turn.
func (s *Scene) Run(parentCtx context.Context) {
	defer close(s.done)
	// Shutdown is a cancellation path (ADR 003 §3.1.4): every live
	// task of this scene version is dropped, the wheel purged, the
	// gauges zeroed. Runs before close(done) — Stop returns to a
	// fully torn-down exec layer.
	defer s.cancelExecTasks()
	go func() {
		select {
		case <-parentCtx.Done():
			s.cancel()
		case <-s.ctx.Done():
		}
	}()

	for {
		// Hybrid scheduler (ADR 003 §3.1, issue #82). With no exec
		// work pending, the loop blocks on the inbox exactly as the
		// dataflow-only engine did. With runnable tasks, the loop
		// stays reactive by FAIRNESS, not by kill: poll the inbox
		// first (inputs are never starved by exec work — the ≤ 50 ms
		// input→delta guarantee), then run ONE time slice of the head
		// task, recompute the dirty cone its effects seeded, and emit
		// — deltas batch at yield/completion points (§3.1.3). A task
		// that outlives its slice is re-enqueued, never killed.
		//
		// Two more sources join the select (issue #83), both consumed
		// here on the scene goroutine — single-writer holds: the
		// timer wheel's channel (due `delay` continuations resume in
		// deterministic deadline order) and the dedicated cancel
		// channel (ADR 003 §3.1.4 lifecycle cancellation).
		if len(s.execQueue) == 0 {
			select {
			case <-s.ctx.Done():
				return
			case <-s.execCancelCh:
				s.cancelExecTasks()
			case <-s.wheelC:
				s.fireDueTimers()
				s.drainNonBlocking()
				s.recompute(false)
				s.emit(nil)
			case msg := <-s.inbox:
				s.applyInput(msg)
				s.drainNonBlocking()
				s.recompute(false)
				s.emit(&msg)
			}
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.execCancelCh:
			s.cancelExecTasks()
		case <-s.wheelC:
			s.fireDueTimers()
			s.recompute(false)
			s.emit(nil)
		case msg := <-s.inbox:
			s.applyInput(msg)
			s.drainNonBlocking()
			s.recompute(false)
			s.emit(&msg)
		default:
			s.runExecSlice()
			s.recompute(false)
			s.emit(nil)
		}
	}
}

// Stop signals the loop to exit and waits for it to drain. Safe to
// call before Run starts (the context is bound at NewScene time).
func (s *Scene) Stop() {
	s.cancel()
	<-s.done
}

// Input enqueues a write. Returns false if the inbox is full.
func (s *Scene) Input(msg InputMsg) bool {
	select {
	case s.inbox <- msg:
		return true
	default:
		return false
	}
}

// Subscribe registers a new subscriber and returns a snapshot to seed it.
// The caller is expected to push the snapshot onto its WS as the
// first frame, then forward Out.
func (s *Scene) Subscribe(buf int) (*Subscription, *protocol.Snapshot) {
	if buf < 16 {
		buf = 16
	}
	sub := &Subscription{
		Out:   make(chan SubscriberMsg, buf),
		scene: s,
	}
	seq, state := s.state.Snapshot()
	snap := &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
	s.subsMu.Lock()
	s.subs = append(s.subs, sub)
	s.subsMu.Unlock()
	return sub, snap
}

// AttachExisting hooks an existing subscription to this scene. Used
// by Show.SetActive to migrate live-show subscribers between scenes
// without forcing the WS to reconnect (ADR 002 § 11).
func (s *Scene) AttachExisting(sub *Subscription) *protocol.Snapshot {
	s.subsMu.Lock()
	s.subs = append(s.subs, sub)
	s.subsMu.Unlock()
	sub.scene = s
	seq, state := s.state.Snapshot()
	return &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
}

// Detach removes a subscription from this scene without closing it.
// Pair with AttachExisting on the destination scene.
func (s *Scene) Detach(sub *Subscription) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for i, x := range s.subs {
		if x == sub {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			return
		}
	}
}

func (s *Scene) unsubscribe(sub *Subscription) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for i, x := range s.subs {
		if x == sub {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			return
		}
	}
}

// SubscriberCount is exposed for tests / metrics.
func (s *Scene) SubscriberCount() int {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	return len(s.subs)
}

// applyInput writes the input to state, marking the leaf dirty if
// the value actually changed. A changed path is also recorded as a
// dirty-cone seed for the next recompute (issue #80); an idempotent
// write (Set returned false) seeds nothing — exactly the writes the
// previous IsDirty walk would have ignored.
func (s *Scene) applyInput(msg InputMsg) {
	// Exec-layer control messages (issue #82): a fire creates (or
	// sheds, B5) a task; a resume wakes a parked continuation. Both
	// execute here, on the scene goroutine — single-writer holds.
	if msg.FireExec != "" {
		s.enqueueFireEnv(msg.FireExec, msg.FireEnv)
		return
	}
	if msg.ResumeExec != "" {
		s.resumeParkedWith(msg.ResumeExec, msg.ResumeEnv)
		return
	}
	if msg.Control != nil {
		// Scene-goroutine closure (Orion #209): operator resolve / pending
		// list. Runs under single-writer; the closure owns its own reply.
		msg.Control(s)
		return
	}
	if msg.SetOnAir != nil {
		// On-air toggle (ADR 006 §3.4): applied on the scene goroutine so
		// the flag the tick-firing path below reads is never racing a
		// writer. Going off air does NOT cancel tasks here — switch-away
		// cancellation is a separate, explicit CancelExec from the Show
		// (ADR 003 §3.1.4); this flag only governs FUTURE on-tick/on-event
		// fires of this instance.
		s.onAir = *msg.SetOnAir
		return
	}
	// Dataflow gate (ADR 008 §3.1, issue #149). A gated roster instance
	// off air does NO dataflow recompute: the leaf is still written
	// (state is single-source-of-truth and a future reactivation reads
	// it), but the dirty cone is NOT seeded, so no recompute pass runs and
	// no delta is emitted backstage. This is the dataflow counterpart of
	// the on-tick/on-event firing gate below — both layer 2 (defence in
	// depth) atop active-only routing, which already keeps genuine writes
	// from reaching a dormant scene at all (only system/test writes can).
	// A test-session / validation clone is never gated (triggersGated ==
	// false), so its dataflow is unaffected. State writes performed BY
	// exec tasks go through the effector on the scene goroutine, not this
	// path, so a freshly reactivated scene's on-start chain mutates state
	// and recomputes normally.
	gatedOffAir := s.triggersGated && !s.onAir
	if s.state.Set(msg.Path, msg.Value) && !gatedOffAir {
		s.pending[msg.Path] = struct{}{}
	}
	if gatedOffAir {
		return
	}
	// Trigger hooks (issue #83, ADR 003 §3.1.3). After the state
	// write, so a fired task's data pulls observe the new value. Both
	// fire on the WRITE, not on the value change: an event carrying
	// the same payload twice is two events.
	if len(s.execProgs) == 0 {
		return
	}
	// Air-only trigger scope (ADR 006 §3.4, issue #106, criterion #6) is
	// now subsumed by the dataflow gate above (ADR 008 §3.1): a gated
	// off-air instance returns before reaching either the state write or
	// these trigger hooks, so on-tick/on-event cannot fire backstage. The
	// two layers agree — active-only routing means a dormant scene never
	// receives a write at all; this firing path only runs for the active
	// (on-air) instance or an ungated clone.
	if len(s.execOnTick) > 0 && msg.Path == tickPath {
		s.fireOnTick(msg.Value)
		return
	}
	if len(s.execOnEvent) > 0 && strings.HasPrefix(msg.Path, eventsPrefix) {
		for _, k := range s.execOnEvent[msg.Path[len(eventsPrefix):]] {
			// Bind the triggering event value under the entry node's
			// `payload` data-out pin — parity with on-platform-event and
			// on-tick's `delta_seconds`. on-event is an exec node, not a
			// dataflow node, so without this a downstream `payload` read
			// (demandValue) finds no state leaf at `<node>` and resolves to
			// null (the live finale null-text bug: `show.emit → on-event →
			// get-field(payload.text)`). msg.Value is the value EmitToActive
			// wrote at `__events.<topic>`.
			var env map[string]json.RawMessage
			if ref, ok := s.execEntries[k]; ok {
				if node := ref.entry.Node; node != "" {
					env = map[string]json.RawMessage{node + ".payload": msg.Value}
				}
			}
			s.enqueueFireEnv(k, env)
		}
	}
	// on-platform-event (ADR 013): the arming twin of on-event, but indexed
	// by the FULL `__inputs.platform.*` leaf (no `__events.` topic
	// shortening — Quasar writes the canonical leaf verbatim). Fires on the
	// WRITE, like every trigger above (a write carrying the same payload
	// twice is two events) — one write, one fire per observing entry. The
	// dataflow recompute for this same write already ran via the state Set
	// + pending seed above, so a blueprint carrying both the quasar.* input
	// and an on-platform-event sees BOTH on one write (coexistence, §3.6).
	if len(s.execOnPlatform) > 0 && strings.HasPrefix(msg.Path, platformLeafPrefix) {
		for _, k := range s.execOnPlatform[msg.Path] {
			// Bind the triggering leaf value under the entry node's
			// `payload` data-out pin, mirroring on-tick's `delta_seconds`
			// binding (fireOnTick) — and parity with on-event, whose
			// `payload` pin resolves to the same node-scoped value. msg.Value
			// is the canonical event Quasar wrote (`{type, payload:{...}}`).
			// Without this the data-out pin resolves to null: the on-platform
			// node is an exec node, not a dataflow node, so demandValue finds
			// no state leaf at `<node>` and a downstream `payload` read is
			// empty (the live finale null-text bug, ADR 013).
			var env map[string]json.RawMessage
			if ref, ok := s.execEntries[k]; ok {
				if node := ref.entry.Node; node != "" {
					env = map[string]json.RawMessage{node + ".payload": msg.Value}
				}
			}
			s.enqueueFireEnv(k, env)
		}
	}
}

// eventsPrefix namespaces the operator/service-dispatched event topics
// `on-event` listens to (ADR 003 §3.1.3).
const eventsPrefix = "__events."

// platformLeafPrefix is the namespace Quasar writes platform events into
// (`__inputs.platform.<platform>.<channel>.last_<type>`). on-platform-event
// entries arm on a write under it (ADR 013). Mirrors the compiler's
// platformLeafPrefix (compile.go) — the compiler cannot import the runtime
// (cycle), so the literal is pinned on both sides.
const platformLeafPrefix = "__inputs.platform."

// fireOnTick fires every on-tick entrypoint with `delta_seconds`
// bound in the task environment under the event node's id. The first
// observed tick binds 0 (no previous instant to diff against).
func (s *Scene) fireOnTick(value json.RawMessage) {
	var nowMs int64
	if err := json.Unmarshal(value, &nowMs); err != nil {
		s.logger.Warn("on-tick: tick payload not a number", "value", string(value))
		return
	}
	delta := 0.0
	if s.execLastTickMs >= 0 {
		delta = float64(nowMs-s.execLastTickMs) / 1000.0
	}
	s.execLastTickMs = nowMs
	raw := json.RawMessage(strconv.FormatFloat(delta, 'g', -1, 64))
	for _, k := range s.execOnTick {
		var env map[string]json.RawMessage
		if ref, ok := s.execEntries[k]; ok {
			if node := ref.entry.Node; node != "" {
				env = map[string]json.RawMessage{node + ".delta_seconds": raw}
			}
		}
		s.enqueueFireEnv(k, env)
	}
}

// drainNonBlocking pulls every input already sitting in the inbox.
// ADR 004 § 4.2: this is the natural batching primitive — under
// burst, many writes coalesce into one recompute pass.
func (s *Scene) drainNonBlocking() {
	for {
		select {
		case msg := <-s.inbox:
			s.applyInput(msg)
		default:
			return
		}
	}
}

// recompute re-evaluates the graph. force=true (cold start only)
// walks the full topo-sorted compute list once. force=false walks the
// DIRTY CONE only (issue #80, ADR 003 §3.1.5): the paths applyInput
// changed seed a topo-index-ordered queue; each recomputed node whose
// value changed pushes its own consumers. Cost is proportional to the
// affected subgraph, not the scene — the 20 k enabler. Popping in
// ascending topo index guarantees every producer runs before its
// consumers, so the semantics are identical to the previous full walk
// with per-node IsDirty checks (a node recomputes iff some upstream
// value actually changed; an unchanged computed value — Set returns
// false — stops the propagation exactly as before).
func (s *Scene) recompute(force bool) {
	if force {
		for i := range s.computeOrder {
			s.computeAt(i)
		}
		clear(s.pending)
		return
	}
	if len(s.pending) == 0 {
		return
	}
	var queue topoQueue
	queued := make(map[int]struct{})
	push := func(idx int) {
		if _, dup := queued[idx]; dup {
			return
		}
		queued[idx] = struct{}{}
		queue.push(idx)
	}
	for p := range s.pending {
		for _, idx := range s.consumers[p] {
			push(idx)
		}
	}
	clear(s.pending)
	for queue.len() > 0 {
		idx := queue.pop()
		leaf, changed := s.computeAt(idx)
		if !changed {
			continue
		}
		for _, j := range s.consumers[leaf] {
			push(j)
		}
	}
}

// computeAt evaluates one computeOrder entry and persists its result.
// Returns the state path written and whether the value changed (an
// input-kind node, an unknown compute or a compute error all report
// unchanged — same skip semantics as the previous loop body).
func (s *Scene) computeAt(i int) (string, bool) {
	ce := s.computeOrder[i]
	if ce.node.Kind == "input" {
		// inputs are written directly by adapters; nothing to
		// recompute here.
		return "", false
	}
	// Gather upstream values under their DECLARED port names — the
	// artefact carries each edge's to_port (issue #79, ADR 003
	// §3.1.1), so wiring is edge-order-independent. Pre-#79
	// artefacts (no Inputs) fall back to positional `a..d`.
	args := s.gatherInputs(ce)
	fn, err := s.cmpReg.Get(ce.node.Compute)
	if err != nil {
		s.logger.Error("unknown compute", "compute", ce.node.Compute, "node", ce.node.ID, "err", err)
		return "", false
	}
	val, err := fn(args, ce.node.Config)
	if err != nil {
		s.logger.Warn("compute error", "compute", ce.node.Compute, "node", ce.node.ID, "err", err)
		return "", false
	}
	// Persist the result so downstream nodes can read it. A named
	// sink/leaf (output/input/literal → Path) writes its public leaf;
	// an INTERMEDIATE compute (core.math.*/compare/logic, Path=="")
	// writes to its node id — the exact address upstreamPath falls
	// back to, so a multi-stage graph (literal→add→output) actually
	// chains. Without this an intermediate's value vanished and every
	// downstream node read null (the real compiler gives intermediates
	// Path==""; only hand-built test graphs assigned them a Path).
	leaf := ce.node.Path
	if leaf == "" {
		leaf = ce.node.ID
	}
	return leaf, s.state.Set(leaf, val)
}

// topoQueue is a binary min-heap over computeOrder indexes. Popping in
// ascending index order IS topological order (computeOrder is the
// compiler's topo sort), which is what makes the dirty-cone walk
// correct: every producer is evaluated before any of its consumers.
// O(cone · log cone) total; ints only, zero allocations beyond the
// backing slice.
type topoQueue struct{ h []int }

func (q *topoQueue) len() int { return len(q.h) }

func (q *topoQueue) push(x int) {
	q.h = append(q.h, x)
	i := len(q.h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if q.h[parent] <= q.h[i] {
			break
		}
		q.h[parent], q.h[i] = q.h[i], q.h[parent]
		i = parent
	}
}

func (q *topoQueue) pop() int {
	root := q.h[0]
	n := len(q.h) - 1
	q.h[0] = q.h[n]
	q.h = q.h[:n]
	i := 0
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		small := l
		if r := l + 1; r < n && q.h[r] < q.h[l] {
			small = r
		}
		if q.h[i] <= q.h[small] {
			break
		}
		q.h[i], q.h[small] = q.h[small], q.h[i]
		i = small
	}
	return root
}

// gatherInputs assembles the port-name → value map a compute reads.
// Named wiring (issue #79, ADR 003 §3.1.1): when the artefact carries
// the edges' to_port names (GraphNode.Inputs), each upstream value is
// delivered under its DECLARED port name, so shuffled edge order wires
// correctly. The positional `a..d` convention survives only as the
// fallback for pre-#79 persisted artefacts (no Inputs field), which
// re-mint named wiring at their next push. A carried entry with an
// empty port name (malformed authoring Blue should have validated)
// still receives its positional name so the value is never dropped.
func (s *Scene) gatherInputs(ce computeEntry) map[string]json.RawMessage {
	portNames := []string{"a", "b", "c", "d"}
	if ins := ce.node.Inputs; len(ins) > 0 {
		out := make(map[string]json.RawMessage, len(ins))
		for i, in := range ins {
			name := in.Port
			if name == "" {
				name = portNames[i%len(portNames)]
			}
			if v, ok := s.state.Get(s.upstreamPath(in.From)); ok {
				out[name] = v
			}
		}
		return out
	}
	out := make(map[string]json.RawMessage, len(ce.upstream))
	for i, up := range ce.upstream {
		name := portNames[i%len(portNames)]
		if v, ok := s.state.Get(s.upstreamPath(up)); ok {
			out[name] = v
		}
	}
	return out
}

// upstreamPath maps an upstream node id back to a state path: the
// node's Path, or the node's own id when it has none (inputs and
// unnamed intermediates). O(1) via the load-time index (issue #80,
// ADR 003 §3.1.5) — this used to linear-scan graph.Nodes per lookup,
// O(N²) per recompute pass at scale.
func (s *Scene) upstreamPath(nodeID string) string {
	if p, ok := s.nodePath[nodeID]; ok {
		return p
	}
	return nodeID
}

// emit assembles a Delta from the dirty set and fans it out.
func (s *Scene) emit(cause *InputMsg) {
	// Dormant gate (ADR 008 §3.1, issue #149). A gated off-air roster
	// instance emits NO delta backstage: it flushes and DISCARDS any dirty
	// leaves (a stray state write still clears so it cannot accumulate and
	// leak on reactivation — the SetActive snapshot reseeds the full state
	// anyway). Active-only routing already keeps genuine writes from
	// reaching a dormant scene; this is the emit-side layer 2, matching the
	// recompute/trigger gates in applyInput. Test-session / validation
	// clones are never gated (triggersGated == false).
	if s.triggersGated && !s.onAir {
		s.state.FlushDirty()
		return
	}
	dirty := s.state.FlushDirty()
	if len(dirty) == 0 && cause != nil && cause.ClientMsgID != "" {
		// ADR 002 § 6: zero-patch delta confirms an idempotent input
		// so optimistic UI clears its pending state.
		seq := s.state.AdvanceSequence()
		msg := &protocol.Delta{
			SceneID:  s.id,
			Sequence: seq,
			Patches:  []protocol.Patch{},
			Cause:    causeFrom(cause),
		}
		s.fanout(msg)
		return
	}
	if len(dirty) == 0 {
		return
	}
	patches := make([]protocol.Patch, 0, len(dirty))
	for _, p := range dirty {
		v, ok := s.state.Get(p)
		if !ok {
			continue
		}
		patches = append(patches, protocol.Patch{Path: p, Value: v})
	}
	if len(patches) == 0 {
		return
	}
	seq := s.state.AdvanceSequence()
	msg := &protocol.Delta{
		SceneID:  s.id,
		Sequence: seq,
		Patches:  patches,
		Cause:    causeFrom(cause),
	}
	s.fanout(msg)
}

func causeFrom(in *InputMsg) *protocol.Cause {
	if in == nil || (in.Source == "" && in.ClientMsgID == "") {
		return nil
	}
	return &protocol.Cause{Source: in.Source, InputID: in.ClientMsgID}
}

// tapMirror forwards the message to the LSDP mirror (if any), isolated
// by a recover: a panic in the kit-side wire (dual/lsdp mode) MUST NOT
// take down the scene goroutine or the bespoke fan-out — the bespoke
// wire is the source of truth (ADR 007 §C.3b). On a panic the emit is
// dropped for the LSDP wire only and logged; bespoke is unaffected.
// A nil mirror (bespoke mode) is a no-op.
func (s *Scene) tapMirror(msg SubscriberMsg) {
	if s.mirror == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("lsdp mirror tap panicked; degraded to bespoke for this emit", "panic", r)
		}
	}()
	s.mirror.Forward(msg)
}

// fanout pushes a server message onto every active subscription.
// Backpressure: drops to the slowest subscriber by collapsing into
// a fresh snapshot — but the v1 implementation is simpler: a full
// queue causes the subscription to receive a snapshot reset on the
// next emit. The WS layer's Connection.sendQ tightens this further.
func (s *Scene) fanout(msg SubscriberMsg) {
	// ADR 007 §C.3b: tap the output port for the LSDP/1.1 wire, before
	// the bespoke fan-out so both wires observe the same ordering. The
	// tap is recover-isolated (tapMirror) so a kit-side panic degrades
	// to bespoke-only rather than killing the scene goroutine.
	s.tapMirror(msg)
	s.subsMu.Lock()
	subs := make([]*Subscription, len(s.subs))
	copy(subs, s.subs)
	s.subsMu.Unlock()
	for _, sub := range subs {
		if sub.trySend(msg) {
			continue
		}
		// Already closed (a concurrent Close raced this fanout — trySend
		// is a no-op then, correctly) or queue full. Only the full case
		// needs a collapse; a closed subscription has nothing left to
		// collapse into.
		if !sub.closed.Load() {
			s.collapseToSnapshot(sub)
		}
	}
}

func (s *Scene) collapseToSnapshot(sub *Subscription) {
	seq, state := s.state.Snapshot()
	snap := &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
	if !sub.drainAndSeed(snap, 50*time.Millisecond) {
		// Either already closed (no-op) or *very* stuck — Close is
		// idempotent either way; the WS layer will reconnect a stuck one.
		sub.Close()
	}
}

// EmitSceneChanged sends a SceneChanged to every subscriber and
// resets the per-scene sequence (ADR 002 § 7). The Show calls this
// when the operator switches scenes; the destination scene's
// snapshot reseeds the sequence on the next subscribe call.
func (s *Scene) EmitSceneChanged(from, to string, transition json.RawMessage) {
	s.state.ResetSequence()
	msg := &protocol.SceneChanged{
		FromSceneID: from,
		ToSceneID:   to,
		Transition:  transition,
	}
	s.fanout(msg)
}

// EmitFreshSnapshot pushes a new snapshot to every subscriber
// (used after a re-push or a scene switch).
func (s *Scene) EmitFreshSnapshot() {
	seq, state := s.state.Snapshot()
	if seq == 0 {
		seq = s.state.AdvanceSequence()
	}
	snap := &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
	s.fanout(snap)
}
