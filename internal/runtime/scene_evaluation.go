package runtime

import (
	"encoding/json"
	"strconv"
	"strings"
)

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
