// Package adapters implements the unified adapter inbox + the
// external input adapters (HTTP poll, PG LISTEN/NOTIFY). Per ADR 004
// § 5, every write to scene state goes through the inbox before
// reaching a scene's per-goroutine event loop. The inbox is the
// single audit point for writes.
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	System      bool // true for tick / __system writes — bypass scope checks
}

// Inbox is the single write entry into the runtime.
type Inbox struct {
	show   *runtime.Show
	logger *slog.Logger
	audit  *Audit
}

// NewInbox builds an inbox bound to the show.
func NewInbox(show *runtime.Show, logger *slog.Logger) *Inbox {
	return &Inbox{
		show:   show,
		logger: logger.With("component", "inbox"),
		audit:  NewAudit(2048),
	}
}

// Write validates scope + fan-outs to every scene that has declared
// a binding on the target path. Returns ErrWriteForbidden if the
// caller's identity is not allowed at the target path; nil on success
// (including the case where no scene has a binding — silent drop).
func (in *Inbox) Write(_ context.Context, w Write) error {
	if !w.System {
		if !w.Identity.CanWritePath(w.Path) {
			in.logger.Debug("write forbidden", "path", w.Path, "user", w.Identity.UserID, "role", w.Identity.Role)
			return ErrWriteForbidden
		}
	}

	// Test-mode paths never reach the live show.
	if isTestNamespace(w.Path) && !w.System {
		return ErrWriteForbidden
	}

	msg := runtime.InputMsg{
		Path:        w.Path,
		Value:       w.Value,
		Source:      w.Source,
		ClientMsgID: w.ClientMsgID,
		IsSystem:    w.System,
	}

	// Fan-out to every loaded scene that declared a binding on the
	// path (ADR 004 § 5 rule 4).
	for _, id := range in.show.IDs() {
		scene, err := in.show.Get(id)
		if err != nil {
			continue
		}
		if !sceneAcceptsPath(scene, w.Path) {
			continue
		}
		scene.Input(msg)
	}

	in.audit.Record(AuditEntry{
		Source:    w.Source,
		Path:      w.Path,
		ValueHash: hashValue(w.Value),
		Timestamp: time.Now(),
	})
	return nil
}

// sceneAcceptsPath checks whether the scene's compiled graph
// declares the path in any of: defaults seed, operator_inputs, or
// external_adapter target_paths. v1 rule: this is the binding
// declaration that decides who receives the write (ADR 004 § 5).
func sceneAcceptsPath(scene *runtime.Scene, path string) bool {
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
	// __system.* fan-out unconditionally (tick lands on every scene).
	if strings.HasPrefix(path, "__system.") {
		return true
	}
	return false
}

func isTestNamespace(p string) bool {
	return strings.HasPrefix(p, "__test.")
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
