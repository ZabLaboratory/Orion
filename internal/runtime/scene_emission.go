package runtime

import (
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"time"
)

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
	// Observability first (Orion#274 / ADR-BLUE-012 §12/B8): a collapse IS
	// the backpressure event — count/log it here, not only on the terminal
	// stuck-close, so a subscriber that recovers on every collapse (never
	// hits the drainAndSeed deadline) still shows up in the metric.
	if s.metrics != nil {
		s.metrics.WSCollapsed(s.id)
	}
	s.logger.Warn("ws fanout backpressure: collapsed subscriber to fresh snapshot", "scene_id", s.id)
	seq, state := s.state.Snapshot()
	snap := &protocol.Snapshot{
		SceneID:      s.id,
		SceneVersion: s.graph.SceneVersion,
		Sequence:     seq,
		State:        state,
	}
	if !sub.drainAndSeed(snap, 50*time.Millisecond) {
		// Either already closed (no-op, not counted twice — Close from a
		// concurrent disconnect is not a backpressure incident) or *very*
		// stuck. Close is idempotent either way; the WS layer will
		// reconnect a stuck one.
		if !sub.closed.Load() {
			if s.metrics != nil {
				s.metrics.WSStuckClosed(s.id)
			}
			s.logger.Warn("ws subscriber stuck through collapse drain deadline; closing", "scene_id", s.id)
		}
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
