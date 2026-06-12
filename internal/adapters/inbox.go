// Package adapters implements the unified adapter inbox + the
// external input adapters (HTTP poll, PG LISTEN/NOTIFY). Per ADR 004
// § 5, every write to scene state goes through the inbox before
// reaching a scene's per-goroutine event loop. The inbox is the
// single audit point for writes. Per ADR 008 §3.1 a write is routed to
// the ACTIVE scene only (no longer fanned out to the whole roster):
// the active scene executes, the rest of the roster is a frozen
// backstage that receives nothing.
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// ErrWriteForbidden is the inbox-level scope-check refusal.
var ErrWriteForbidden = errors.New("adapters: write forbidden")

// Write is the typed input the inbox accepts. Identity is set by
// upstream auth (WS handler / API handler).
type Write struct {
	Identity    auth.Identity
	Path        string
	Value       json.RawMessage
	Source      string // server-trusted source string per ADR 002 § 6
	ClientMsgID string

	// system marks a server-trusted internal write (tick fan-out,
	// declared-adapter poller/pg-listen) that bypasses the scope check.
	// UNEXPORTED ON PURPOSE (B-syswrite hardening, ADR 003 §3.1.3 /
	// issue #85): only this package can set it — no wire surface
	// (WS/REST handler) can ever construct a System write or smuggle a
	// `__system.*` state write past the gate. Async-effect completions
	// do NOT come through here at all: they travel intra-process as
	// InputMsg.ResumeExec (runtime/exec_effects.go).
	system bool
}

// systemWrite marks a Write as server-trusted. In-package adapters
// (poller, pg-listen) call this on values THEY constructed — it is
// deliberately not reachable from request-handling packages.
func systemWrite(w Write) Write {
	w.system = true
	return w
}

// InboxMetrics is the observability seam the inbox reports drops on
// (ADR 003 §3.3 E2, issue #84): one increment per write a scene's
// event loop refused (full inbox channel). *obs.Metrics implements it
// (`orion_inbox_dropped_total`); tests substitute a counter double.
type InboxMetrics interface {
	InboxDropped(sceneID string)
}

// dropWarnInterval rate-limits the inbox-drop warn log: under a flood
// (the exact condition that produces drops) one warn per interval is
// signal, one warn per drop is its own incident. The metric counts
// every drop regardless.
const dropWarnInterval = time.Second

// Inbox is the single write entry into the runtime.
type Inbox struct {
	show    *runtime.Show
	logger  *slog.Logger
	audit   *Audit
	metrics InboxMetrics // nil-safe: nil disables the drop counter

	// lastDropWarn is the unix-nano stamp of the last drop warn, used
	// to rate-limit logging (never the metric).
	lastDropWarn atomic.Int64
}

// NewInbox builds an inbox bound to the show. metrics may be nil
// (drops are then logged but not counted).
func NewInbox(show *runtime.Show, logger *slog.Logger, metrics InboxMetrics) *Inbox {
	return &Inbox{
		show:    show,
		logger:  logger.With("component", "inbox"),
		audit:   NewAudit(2048),
		metrics: metrics,
	}
}

// Write validates scope + routes the write to the ACTIVE scene only,
// iff it declares a binding on the target path (ADR 008 §3.1). Returns
// ErrWriteForbidden if the caller's identity is not allowed at the
// target path; nil on success (including the case where no scene is
// active, or the active scene has no binding — silent absorb).
func (in *Inbox) Write(_ context.Context, w Write) error {
	if !w.system {
		// B-syswrite hardening (issue #85): the `__system.*` namespace
		// is never writable from the wire, for ANY identity — operator
		// and admin included. Internal producers (tick, declared
		// adapters) carry the in-package system mark; async-effect
		// completions never come through the inbox-as-write at all
		// (they are intra-process InputMsg.ResumeExec messages).
		if isSystemNamespace(w.Path) {
			in.logger.Warn("write forbidden: __system.* is not wire-writable",
				"path", w.Path, "user", w.Identity.UserID, "role", w.Identity.Role)
			return ErrWriteForbidden
		}
		if !w.Identity.CanWritePath(w.Path) {
			in.logger.Debug("write forbidden", "path", w.Path, "user", w.Identity.UserID, "role", w.Identity.Role)
			return ErrWriteForbidden
		}
	}

	// Test-mode paths never reach the live show.
	if isTestNamespace(w.Path) && !w.system {
		return ErrWriteForbidden
	}

	msg := runtime.InputMsg{
		Path:        w.Path,
		Value:       w.Value,
		Source:      w.Source,
		ClientMsgID: w.ClientMsgID,
		IsSystem:    w.system,
	}

	// Route to the union {active} ∪ {promoted stream-rules} (ADR 009 §3.3,
	// extending ADR 008 §3.1). The active scene executes its blues AND
	// each promoted rule that declares the path; sceneAcceptsPath gates
	// every target individually. This is NOT the ADR 004 §5 rule-4 roster
	// fan-out: RouteTargets is bounded to the operator-promoted set
	// (typically 0–3), so the rest of the roster — a scene neither active
	// nor promoted — is absent from the slice and receives NOTHING, a
	// frozen backstage by non-routage (criterion #1 ADR 008 stays vert).
	// The set is snapshotted once under the show RLock; a write in flight
	// during a (de)activation/(de)promotion lands on the set as of that
	// read — accepted, events are live-only (ADR 009 §3.3).
	//
	// Reserved for issue #155: the `show.emit` rule→active injection is a
	// distinct active-only system path that must NOT reuse this union
	// (routing it here would cascade rule→rule). #155 targets
	// in.show.Active() directly for that path. See ADR 009 §3.6.
	targets := in.show.RouteTargets()
	if len(targets) == 0 {
		// No active scene and no promoted rule: nothing to receive the
		// write. Absorbed, exactly as a path no scene declares is absorbed.
		in.audit.Record(AuditEntry{
			Source:    w.Source,
			Path:      w.Path,
			ValueHash: hashValue(w.Value),
			Timestamp: time.Now(),
		})
		return nil
	}
	for _, scene := range targets {
		if !sceneAcceptsPath(scene, w.Path, w.system) {
			continue
		}
		// scene.Input's return is consumed, not discarded (ADR 003 §3.3
		// E2, issue #84): false means the scene's event loop refused the
		// write (full channel) — the value is LOST, which must be
		// observable (`orion_inbox_dropped_total`) without log-spamming
		// under the very flood that causes it. Drop metric is per scene id.
		if !scene.Input(msg) {
			in.noteDrop(scene.ID(), w.Path)
		}
	}

	// One audit record per accepted write (ADR 009 §3.3 — the inbox stays
	// the single point of audit regardless of fan-out cardinality).
	in.audit.Record(AuditEntry{
		Source:    w.Source,
		Path:      w.Path,
		ValueHash: hashValue(w.Value),
		Timestamp: time.Now(),
	})
	return nil
}

// noteDrop records one refused write: the metric counts EVERY drop;
// the warn is rate-limited to one per dropWarnInterval via a CAS on
// the last-warn stamp (losing the race just means another goroutine
// owns this interval's warn).
func (in *Inbox) noteDrop(sceneID, path string) {
	if in.metrics != nil {
		in.metrics.InboxDropped(sceneID)
	}
	now := time.Now().UnixNano()
	last := in.lastDropWarn.Load()
	if now-last < int64(dropWarnInterval) {
		return
	}
	if in.lastDropWarn.CompareAndSwap(last, now) {
		in.logger.Warn("scene inbox full — write dropped", "scene_id", sceneID, "path", path)
	}
}

// sceneAcceptsPath checks whether the scene's compiled graph
// declares the path in any of: defaults seed, operator_inputs, or
// external_adapter target_paths (the latter now also covers synthesized
// `platform-stream` and `event-topic` bindings). It is evaluated on the
// ACTIVE scene only (ADR 008 §3.1): the binding declaration decides
// whether the active scene receives the write (ADR 004 § 5 acceptance,
// preserved; the fan-out of rule 4 is superseded by active-only routing).
func sceneAcceptsPath(scene *runtime.Scene, path string, system bool) bool {
	g := scene.Graph()
	if _, ok := g.Defaults[path]; ok {
		return true
	}
	for _, in := range g.OperatorInputs {
		if in.Path == path {
			return true
		}
	}
	for _, b := range g.Bindings {
		for _, tp := range b.TargetPaths {
			if tp == path {
				return true
			}
		}
	}
	// __system.* fan-out (tick lands on every scene) — for INTERNAL
	// system writes only (B-syswrite hardening, issue #85): an external
	// `__system.*` write is already rejected at the scope gate, and
	// this guard keeps the fan-out shortcut unreachable for it even if
	// a future caller misroutes one.
	if system && isSystemNamespace(path) {
		return true
	}
	return false
}

func isTestNamespace(p string) bool {
	return strings.HasPrefix(p, "__test.")
}

func isSystemNamespace(p string) bool {
	return p == "__system" || strings.HasPrefix(p, "__system.")
}

// AuditEntry is one row in the in-memory audit ring (per ADR 002
// § 10: "Live audit is not persisted to disk by Orion").
type AuditEntry struct {
	Source    string
	Path      string
	ValueHash string
	Timestamp time.Time
}

// Audit is a fixed-size ring of recent writes. Used for /show
// inspection and for tracing during a live broadcast. Not persisted.
type Audit struct {
	mu       sync.Mutex
	entries  []AuditEntry
	capacity int
	next     int
	full     bool
}

// NewAudit builds a ring of given capacity.
func NewAudit(capacity int) *Audit {
	if capacity <= 0 {
		capacity = 256
	}
	return &Audit{entries: make([]AuditEntry, capacity), capacity: capacity}
}

// Record appends an entry, evicting the oldest if full.
func (a *Audit) Record(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[a.next] = e
	a.next = (a.next + 1) % a.capacity
	if a.next == 0 {
		a.full = true
	}
}

// Snapshot returns the ring contents in chronological order.
func (a *Audit) Snapshot() []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.full {
		out := make([]AuditEntry, a.next)
		copy(out, a.entries[:a.next])
		return out
	}
	out := make([]AuditEntry, 0, a.capacity)
	out = append(out, a.entries[a.next:]...)
	out = append(out, a.entries[:a.next]...)
	return out
}

// hashValue produces a short hex tag for an audit entry's value
// without ballooning RAM. SHA-256 first 8 bytes is plenty to
// distinguish values for human-eyes triage.
func hashValue(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	// Inline a tiny FNV — sufficient for audit, no crypto requirement.
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	var h uint64 = offset64
	for _, b := range v {
		h ^= uint64(b)
		h *= prime64
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[15-i] = hex[h&0xF]
		h >>= 4
	}
	return string(out)
}
