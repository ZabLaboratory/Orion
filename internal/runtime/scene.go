package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
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
}

// Close drains the subscriber and removes it from the scene. Idempotent.
func (s *Subscription) Close() {
	if s.closed.Swap(true) {
		return
	}
	if s.scene != nil {
		s.scene.unsubscribe(s)
	}
	close(s.Out)
}

// Scene is one live scene instance — a graph + state + a goroutine
// that drives the drain-then-compute loop.
type Scene struct {
	id     string
	graph  *compiler.Graph
	bundle *compiler.RenderBundle
	state  *State
	cmpReg *ComputeRegistry
	logger *slog.Logger

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
	}
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

// Graph exposes the compiled graph artefact (used by adapters to
// read declared bindings).
func (s *Scene) Graph() *compiler.Graph { return s.graph }

// Bundle exposes the render bundle artefact (served by the API).
func (s *Scene) Bundle() *compiler.RenderBundle { return s.bundle }

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
	go func() {
		select {
		case <-parentCtx.Done():
			s.cancel()
		case <-s.ctx.Done():
		}
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.inbox:
			s.applyInput(msg)
			s.drainNonBlocking()
			s.recompute(false)
			s.emit(&msg)
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
	if s.state.Set(msg.Path, msg.Value) {
		s.pending[msg.Path] = struct{}{}
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
		select {
		case sub.Out <- msg:
		default:
			// Slow consumer — collapse into a snapshot (ADR 002 § 9).
			s.collapseToSnapshot(sub)
		}
	}
}

func (s *Scene) collapseToSnapshot(sub *Subscription) {
	// Drain the existing queue.
	for {
		select {
		case <-sub.Out:
		default:
			goto seed
		}
	}
seed:
	seq, state := s.state.Snapshot()
	snap := &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
	select {
	case sub.Out <- snap:
	case <-time.After(50 * time.Millisecond):
		// Subscriber is *very* stuck — close it; the WS layer will
		// reconnect.
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
