package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Lumencast/lumencast-go/protocol"
)

// ErrEmptyPatches is returned by Scene.Emit / Scene.Set when called
// with an empty map.
var ErrEmptyPatches = errors.New("server: empty patches map")

// Scene is the server-side abstraction of a Lumencast scene : an
// id, a current state, and a fan-out of subscribers. Adapter
// goroutines call Set (initial seed) and Emit (live deltas). The
// fan-out runs on a single goroutine per subscriber ; back-pressured
// subscribers get coalesced into a fresh snapshot rather than a
// growing delta queue.
//
// A Scene is safe for concurrent use.
type Scene struct {
	id      string
	version string
	store   *store

	mu sync.Mutex
	// seq is the per-scene monotonic counter (LSDP/1.1 §18.1.1). All
	// concurrent subscribers see the same seq value on a given delta ;
	// late-joining subscribers receive a snapshot whose seq matches
	// this counter, NOT seq=1. The first emission on a fresh scene
	// returns seq=1 from NextServer.
	seq *protocol.SequenceTracker
	// replay holds the recent (seq, patches, cause, projection metadata)
	// emissions so a 1.1
	// client reconnecting with `since_sequence` can resume without a
	// fresh snapshot (LSDP/1.1 §18.1).
	replay      *replayBuffer
	subscribers map[*subscription]struct{}

	// lastMetadata is the most recently known projection metadata from
	// an EmitWithCauseAndMetadata call — the scene's last known
	// correlation identity. Every Snapshot construction site (collapse
	// recovery, refreshAll, late-join subscribe, live migration) stamps
	// it onto the outgoing frame so a subscriber that never saw the
	// originating Delta still gets a correlatable identity. Nil until
	// the first EmitWithCauseAndMetadata call ; Snapshot fields are then
	// omitted entirely rather than fabricated.
	lastMetadata *protocol.ProjectionMetadata

	// declaredInputs is the subset of __inputs.* paths the scene
	// accepts, keyed by path. Empty means "accept anything under
	// __inputs.*". The CLI's lumencast validate would normally
	// populate this from an LSML bundle ; for code-driven scenes the
	// user calls DeclareInputs / WithOperatorInputs explicitly.
	declaredInputs map[string]InputSpec

	// rejection, when non-nil, marks the scene unservable : every
	// subscriber gets this error frame instead of a snapshot. Set by
	// Reject when the backing bundle failed validation.
	rejection *SceneRejection
}

// SceneRejection is the error a rejected scene serves in place of its
// snapshot.
type SceneRejection struct {
	Code    protocol.ErrorCode
	Message string
}

// SceneOption configures a Scene at creation time.
type SceneOption func(*Scene)

// WithSceneVersion sets the scene_version field that flows on
// snapshot / scene_changed frames. Defaults to "sha256:0..." (a
// sentinel) so test code does not need to compute a hash.
func WithSceneVersion(v string) SceneOption {
	return func(s *Scene) { s.version = v }
}

// WithDeclaredInputs restricts the set of __inputs.* paths the scene
// accepts. Inputs to undeclared paths are rejected with UNKNOWN_PATH.
// Equivalent to WithOperatorInputs with no constraint metadata.
func WithDeclaredInputs(paths []string) SceneOption {
	return func(s *Scene) {
		if s.declaredInputs == nil {
			s.declaredInputs = make(map[string]InputSpec, len(paths))
		}
		for _, p := range paths {
			s.declaredInputs[p] = InputSpec{Path: p}
		}
	}
}

// WithOperatorInputs declares operator-controllable paths AND their
// constraints. Inputs that violate a constraint (string longer than
// MaxLength, number out of [Min, Max], enum not in Values) are rejected
// with INVALID_VALUE atomically — the entire frame is dropped.
func WithOperatorInputs(specs []InputSpec) SceneOption {
	return func(s *Scene) {
		if s.declaredInputs == nil {
			s.declaredInputs = make(map[string]InputSpec, len(specs))
		}
		for _, spec := range specs {
			s.declaredInputs[spec.Path] = spec
		}
	}
}

// InputSpec describes one operator-controllable path and the type /
// constraint metadata the server enforces. Mirrors LSML 1.0 § 8.
//
// Type values : "string", "number", "boolean", "enum", "color",
// "date", "time", "path-ref", "image-ref". The empty string skips
// type checking but still enforces declaredness.
type InputSpec struct {
	Path string

	Type string

	// MaxLength applies to strings (Type == "string").
	MaxLength int

	// Min and Max apply to numbers (Type == "number").
	Min *float64
	Max *float64

	// Values is the enum domain for Type == "enum".
	Values []string
}

// newScene constructs a fresh Scene. Call NewScene on a Server to
// register one with the kit.
func newScene(id string, opts ...SceneOption) *Scene {
	seq := protocol.NewSequenceTracker()
	// Pre-seed the scene seq to 1 so the very first subscriber's
	// snapshot ships at seq=1 (matching the LSDP/1.0 baseline that all
	// existing conformance scenarios assume). Subsequent deltas
	// increment to 2, 3, etc. Late-joining subscribers see the
	// current value, not 1.
	seq.NextServer()
	s := &Scene{
		id:          id,
		version:     defaultSceneVersion,
		store:       newStore(),
		seq:         seq,
		replay:      newReplayBuffer(DefaultReplayBufferSize),
		subscribers: make(map[*subscription]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

const defaultSceneVersion = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// ID returns the scene's identifier.
func (s *Scene) ID() string { return s.id }

// SnapshotForConformance returns a defensive copy of the current
// authoritative state. Exposed for the conformance harness's
// expect-server-state step ; ordinary callers use Set / Emit.
func (s *Scene) SnapshotForConformance() map[string]json.RawMessage {
	return s.store.snapshot()
}

// Version returns the current scene_version (content hash).
func (s *Scene) Version() string { return s.version }

// SetVersion updates the scene_version. Triggering a SceneChanged
// frame is the caller's responsibility (Server.SwitchScene).
func (s *Scene) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// Reject marks the scene unservable : every subscriber receives code /
// message as a non-recoverable error frame in place of the snapshot.
// Used when the bundle backing the scene fails validation.
func (s *Scene) Reject(code protocol.ErrorCode, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejection = &SceneRejection{Code: code, Message: message}
}

// Rejection returns the rejection recorded by Reject, or nil.
func (s *Scene) Rejection() *SceneRejection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejection
}

// Set seeds initial state. Existing subscribers receive a fresh
// Snapshot reflecting the new state ; new subscribers see the same.
// Call Set before subscribers attach when bootstrapping ; for live
// updates use Emit.
func (s *Scene) Set(patches map[string]any) error {
	if len(patches) == 0 {
		return ErrEmptyPatches
	}
	if _, err := s.store.applyPatches(patches); err != nil {
		return err
	}
	// Notify subscribers with a fresh snapshot so they re-base.
	return s.refreshAll()
}

// Emit applies a leaf-grain delta : updates the store and fans out
// a Delta frame to every subscriber. Subscribers whose buffer is
// full collapse to a snapshot to avoid unbounded growth.
func (s *Scene) Emit(patches map[string]any) error {
	return s.emitWithCause(patches, nil, nil)
}

// EmitWithCause is the LSDP/1.1 entry point — same as Emit, but the
// resulting Delta carries the supplied Cause as provenance metadata
// (§3.2.3). Adapters and operator-input pipelines use this to thread
// origin info through to the wire. 1.0 callers stay on Emit and
// produce cause-less deltas.
func (s *Scene) EmitWithCause(patches map[string]any, cause *protocol.Cause) error {
	return s.emitWithCause(patches, cause, nil)
}

// EmitWithCauseAndMetadata is LSDP/1.1 entry point for caller-provided
// projection metadata, in addition to Cause.
// Solar/Orion can pass the six projection fields without forcing any
// caller-side contract changes outside the 1.1 wire surface.
func (s *Scene) EmitWithCauseAndMetadata(patches map[string]any, cause *protocol.Cause, metadata *protocol.ProjectionMetadata) error {
	return s.emitWithCause(patches, cause, metadata)
}

func (s *Scene) emitWithCause(patches map[string]any, cause *protocol.Cause, metadata *protocol.ProjectionMetadata) error {
	if len(patches) == 0 {
		return ErrEmptyPatches
	}
	raw, err := s.store.applyPatches(patches)
	if err != nil {
		return err
	}
	wirePatches := make([]protocol.Patch, len(raw))
	for i, p := range raw {
		wirePatches[i] = protocol.Patch{Path: p.Path, Value: p.Value}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if metadata != nil {
		// Defensive copy — same rationale as replayBuffer.push : a caller
		// mutating the metadata it just passed in must not race with a
		// later Snapshot construction reading s.lastMetadata.
		md := *metadata
		s.lastMetadata = &md
	}
	// LSDP/1.1 §18.1.1 — per-scene monotonic seq. All concurrent
	// subscribers receive the same delta with the same seq.
	seq := s.seq.NextServer()
	s.replay.push(seq, wirePatches, cause, metadata)
	for sub := range s.subscribers {
		s.sendDelta(sub, seq, wirePatches, cause, metadata)
	}
	return nil
}

// stampMetadata copies the scene's last known projection metadata (if
// any) onto snap. Caller MUST hold s.mu. No-op when no metadata is
// known yet — omission, never fabrication.
func (s *Scene) stampMetadata(snap *protocol.Snapshot) {
	if s.lastMetadata == nil {
		return
	}
	snap.SchemaVersion = s.lastMetadata.SchemaVersion
	snap.SceneDigest = s.lastMetadata.SceneDigest
	snap.RuntimeInstanceID = s.lastMetadata.RuntimeInstanceID
	snap.Target = s.lastMetadata.Target
	snap.RenderRevision = s.lastMetadata.RenderRevision
	snap.CorrelationID = s.lastMetadata.CorrelationID
}

// sendDelta enqueues a Delta on a subscription, falling back to a
// fresh Snapshot if the buffer is full (back-pressure protection).
// Caller MUST hold s.mu.
func (s *Scene) sendDelta(
	sub *subscription,
	seq uint64,
	patches []protocol.Patch,
	cause *protocol.Cause,
	metadata *protocol.ProjectionMetadata,
) {
	d := &protocol.Delta{
		Seq:               seq,
		Patches:           patches,
		Cause:             cause,
		SchemaVersion:     "",
		SceneDigest:       "",
		RuntimeInstanceID: "",
		Target:            "",
		RenderRevision:    "",
		CorrelationID:     "",
	}
	if metadata != nil {
		d.SchemaVersion = metadata.SchemaVersion
		d.SceneDigest = metadata.SceneDigest
		d.RuntimeInstanceID = metadata.RuntimeInstanceID
		d.Target = metadata.Target
		d.RenderRevision = metadata.RenderRevision
		d.CorrelationID = metadata.CorrelationID
	}
	select {
	case sub.out <- d:
	default:
		// Buffer full : drop into snapshot recovery. Under the per-scene
		// seq model (§18.1.1), the snapshot ships at the current scene
		// seq — the subscriber rebases via ObserveSnapshot and continues
		// from there. We do NOT reset the scene seq (other subscribers
		// keep advancing fine).
		snap := &protocol.Snapshot{
			Seq:          s.seq.Current(),
			SceneID:      s.id,
			SceneVersion: s.version,
			State:        s.store.snapshot(),
		}
		s.stampMetadata(snap)
		// Best-effort send — if even the snapshot can't be enqueued,
		// the subscription is dead and the writer goroutine will close.
		select {
		case sub.out <- snap:
		default:
			sub.markStale()
		}
	}
}

// refreshAll sends a fresh Snapshot to every subscriber. Used after
// a Set() or after a SceneChanged inside the same Scene.
func (s *Scene) refreshAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.store.snapshot()
	curSeq := s.seq.Current()
	for sub := range s.subscribers {
		snap := &protocol.Snapshot{
			Seq:          curSeq,
			SceneID:      s.id,
			SceneVersion: s.version,
			State:        state,
		}
		s.stampMetadata(snap)
		select {
		case sub.out <- snap:
		default:
			sub.markStale()
		}
	}
	return nil
}

// applyInput is the server-internal entry point for an Input frame.
// It runs the scope check, the declared-input check, the constraint
// check, then commits the patches via Emit.
//
// The second return value is the offending leaf path : LSDP/1 §3.4.1
// makes `path` REQUIRED on the WRITE_FORBIDDEN, UNKNOWN_PATH and
// INVALID_VALUE error frames, so the rejection site — the only place
// that knows which patch failed — hands it back to the caller. It is
// empty for the codes that carry no path (and on success).
func (s *Scene) applyInput(_ context.Context, id Identity, frame *protocol.Input) (protocol.ErrorCode, string, error) {
	if len(frame.Patches) == 0 {
		return protocol.CodeInvalidValue, "", errors.New("input: empty patches")
	}

	// Pre-validate every patch : either ALL apply or NONE.
	for _, p := range frame.Patches {
		if !id.CanWrite(p.Path) {
			return protocol.CodeWriteForbidden, p.Path, errors.New("write forbidden: " + p.Path)
		}
		if !s.acceptsInputPath(id.Role, p.Path) {
			return protocol.CodeUnknownPath, p.Path, errors.New("unknown path: " + p.Path)
		}
		if !json.Valid(p.Value) {
			return protocol.CodeInvalidValue, p.Path, errors.New("invalid JSON value at " + p.Path)
		}
		if err := s.checkConstraint(p.Path, p.Value); err != nil {
			return protocol.CodeInvalidValue, p.Path, fmt.Errorf("invalid value at %s: %w", p.Path, err)
		}
	}

	patches := make(map[string]any, len(frame.Patches))
	for _, p := range frame.Patches {
		patches[p.Path] = p.Value
	}

	// LSDP/1.1 §4.2 + §3.2.3 : when the input carries a client_msg_id,
	// echo it verbatim into the resulting delta's cause.input_id so
	// optimistic-UI clients can correlate the echo with their predicted
	// state. Subject convention is "<role>:<subject>" — falling back to
	// the role alone when no subject claim is on the token.
	var cause *protocol.Cause
	if frame.ClientMsgID != "" {
		subject := id.Subject
		if subject == "" {
			subject = string(id.Role)
		}
		cause = &protocol.Cause{
			Source:  string(id.Role) + ":" + subject,
			InputID: frame.ClientMsgID,
		}
	}
	if err := s.emitWithCause(patches, cause, nil); err != nil {
		return protocol.CodeInternal, "", err
	}
	return "", "", nil
}

// acceptsInputPath enforces the per-namespace policy :
//   - __inputs.* : if declaredInputs is non-empty, the path MUST be
//     listed.
//   - __test.* : accepted only on test sessions (Role == test).
//   - other reserved namespaces : rejected.
//   - non-reserved : rejected (no implicit-creation).
func (s *Scene) acceptsInputPath(role protocol.Role, path string) bool {
	p := protocol.LeafPath(path)
	if p.HasPrefix("__inputs") {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.declaredInputs) == 0 {
			return true
		}
		_, ok := s.declaredInputs[path]
		return ok
	}
	if p.HasPrefix("__test") {
		return role == protocol.RoleTest
	}
	// __system, __schema, anything else → no client-side writes.
	return false
}

// checkConstraint validates an input value against the InputSpec
// declared for path, if any. Returns nil for paths without a spec
// (e.g. __test.* writes). The check decodes the raw JSON value once
// and matches it against the declared Type / MaxLength / Min / Max /
// Values.
func (s *Scene) checkConstraint(path string, raw json.RawMessage) error {
	s.mu.Lock()
	spec, ok := s.declaredInputs[path]
	s.mu.Unlock()
	if !ok || spec.Type == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	switch spec.Type {
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
		if spec.MaxLength > 0 && len(str) > spec.MaxLength {
			return fmt.Errorf("string length %d exceeds maxLength %d", len(str), spec.MaxLength)
		}
	case "number":
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("expected number, got %T", v)
		}
		if spec.Min != nil && n < *spec.Min {
			return fmt.Errorf("number %v below min %v", n, *spec.Min)
		}
		if spec.Max != nil && n > *spec.Max {
			return fmt.Errorf("number %v above max %v", n, *spec.Max)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
	case "enum":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected enum string, got %T", v)
		}
		for _, allowed := range spec.Values {
			if str == allowed {
				return nil
			}
		}
		return fmt.Errorf("value %q not in enum domain %v", str, spec.Values)
	}
	return nil
}

// subscribeWithResume is the LSDP/1.1 entry point — replaces the old
// per-subscription `subscribe` helper.
// but honours the optional `since_sequence` field (§4.1, §18). When the
// replay buffer covers the gap, the returned `replay` slice contains
// the deltas to ship in lieu of a snapshot. Otherwise the returned
// snapshot carries the current scene seq and `replay` is nil.
//
// The caller MUST send EITHER the snapshot OR the replay deltas — never
// both. snap is non-nil iff replay is nil.
func (s *Scene) subscribeWithResume(buffer int, live, proto11 bool, sinceSequence uint64) (*subscription, *protocol.Snapshot, []replayRecord) {
	if buffer < 1 {
		buffer = 64
	}
	sub := &subscription{
		out:     make(chan any, buffer),
		live:    live,
		proto11: proto11,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[sub] = struct{}{}
	curSeq := s.seq.Current()

	// If the client requested resume AND the buffer covers the gap, the
	// server SHOULD ship a delta stream from sinceSequence+1 forward
	// instead of a fresh snapshot (§18.1.1 + §18.2).
	if sinceSequence > 0 && sinceSequence <= curSeq {
		if records, covered := s.replay.since(sinceSequence); covered {
			return sub, nil, records
		}
	}

	state := s.store.snapshot()
	snap := &protocol.Snapshot{
		Seq:          curSeq,
		SceneID:      s.id,
		SceneVersion: s.version,
		State:        state,
	}
	s.stampMetadata(snap)
	return sub, snap, nil
}

// unsubscribe detaches a subscriber. Idempotent.
func (s *Scene) unsubscribe(sub *subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subscribers[sub]; !ok {
		return
	}
	delete(s.subscribers, sub)
	sub.close()
}

// subscription is the per-WS pipe between a Scene's fan-out goroutine
// and the connection's writer.
//
// live indicates the subscription was opened in live mode (no scene
// field on the Subscribe frame) and therefore follows whatever scene
// is currently active on the server. SetActive migrates these.
type subscription struct {
	// out is the per-subscriber outgoing queue. The scene-level seq
	// counter is shared across all subscribers (LSDP/1.1 §18.1.1) ;
	// subscriptions don't carry their own counter.
	out    chan any
	closed bool
	stale  bool
	live   bool
	// proto11 is true when the connection negotiated the lsdp.v1.1
	// subprotocol. Gates additive 1.1-only frames (scene_roster) so 1.0
	// subscribers never receive them.
	proto11 bool
	mu      sync.Mutex
}

func (s *subscription) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.out)
}

func (s *subscription) markStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = true
}

// IsStale reports whether the subscription was abandoned because the
// fan-out could not deliver even a snapshot.
func (s *subscription) IsStale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stale
}
