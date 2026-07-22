package runtime

import (
	"encoding/json"
	"errors"
	"sort"
)

// The operator runtime (Orion #209, Blue ADR 008 §3.2/§3.3 — the
// operator-ui-contract). Two manifest nodes are materialised here:
//
//   - `core.operator.on-call@1` → an exec ENTRYPOINT of a new kind,
//     EntryOnCall. It is armed at InstallExec like every other entry, but
//     fired NOT by a state write — by an explicit operator request landing
//     on POST /operator/call/{blueprint_id}/{entrypoint_id}. Firing pushes
//     the wired `then` chain with the request `payload` bound under the
//     node's data-out pin (parity with on-event/on-platform-event).
//
//   - `core.operator.await-value@1` → a latent exec OP (OpOperatorAwait),
//     the suspend twin of `delay`: it parks the chain's continuation under
//     a version-stamped wake key WITHOUT a timer, publishes the suspension
//     point in the scene's pending-await registry, and resumes only when an
//     operator supplies a value through POST /operator/resolve/{bp}/{name}.
//     On resume the supplied value is bound under the node's `value`
//     data-out pin.
//
// Active-only (ADR 008 invariant 7) is inherited, not re-implemented: the
// registry lives on the Scene and is cleared by cancelExecTasks alongside
// the parked map, so a switch-away invalidates every await of the leaving
// version. A resolve arriving after the cut finds no entry (410) and, even
// if it carried the wake key, the epoch stamp would drop the resume.

// EntryOnCall fires on an explicit operator dispatch to a named operator
// entrypoint (Blue ADR 008 §3.2). Unlike the write-armed kinds
// (on-event/on-platform-event) it is NOT indexed by a leaf; it is fired by
// FireExec from the operator-call route, addressed by its namespaced entry
// key `<blueprint_key>/<entry_id>`. ExecEntry.Node is the on-call node id,
// so the route's payload binds under `<node>.payload` (same pin discipline
// as the other event kinds).
const EntryOnCall = "on-call"

// OpOperatorAwait is the runtime exec op `core.operator.await-value@1`
// materialises to. Latent like OpDelay: it parks (suspends) the chain and
// registers a pending await; resume is external (operator resolve), never a
// timer.
const OpOperatorAwait = "operator.await"

// Await config keys (Blue ADR 008 §3.3). The await node carries the
// operator-facing name it is addressed by and the value type a resolve is
// checked against; both are mirrored from Blue's seed and read verbatim.
const (
	awaitNameConfigKey = "await_name"
	awaitTypeConfigKey = "value_type"
	awaitUIConfigKey   = "ui"
	// awaitValuePort is the await node's data-out pin the resolved value is
	// bound under (Blue ADR 008 §3.3 declares it `value`). The resumed
	// continuation reads it through demandValue → t.env[`<node>.value`].
	awaitValuePort = "value"
)

// PendingAwait is the operator-facing view of one live suspension point —
// the shape GET /runtime/{blueprint_id}/pending publishes. It is a copy:
// the route never holds a pointer into the scene-goroutine-owned registry.
type PendingAwait struct {
	BlueprintKey string          `json:"blueprint_id"`
	AwaitName    string          `json:"await_name"`
	ValueType    string          `json:"value_type"`
	UI           json.RawMessage `json:"ui,omitempty"`
}

// pendingAwait is the scene-goroutine-owned registry entry: the public
// metadata plus the wake key of the continuation parked for this await and
// the await node id (the namespace its resolved `value` data-out pin is
// bound under in the resumed task env, parity with on-tick's
// `<node>.delta_seconds`).
type pendingAwait struct {
	view    PendingAwait
	wakeKey string
	nodeID  string
}

// awaitRegistryKey is the registry key for an await — namespaced exactly
// like an entry key so two blueprints sharing an `await_name` never collide
// (the slash separator is reserved, entryKey discipline).
func awaitRegistryKey(blueprintKey, awaitName string) string {
	return blueprintKey + "/" + awaitName
}

// execOperatorAwait implements `operator.await-value`: park the chain's
// continuation under a version-stamped wake key (no timer — the resume is
// external) and register the suspension point so the operator surface can
// list it and resolve it. Latent/fork semantics (issue #82): the
// surrounding task (an enclosing sequence/loop) continues; only the awaiting
// chain suspends.
//
// A node with no wired `then` has no continuation to park, so it registers
// nothing and ends the chain (parity with execDelay's unwired guard) — a
// resolve would have nothing to resume.
//
// A node with no `await_name` cannot be addressed by a resolve; it is logged
// and the chain ends rather than parking an unreachable continuation (the
// authoring gap is the author's — the compiler's await materialisation
// rejects a missing name at push, this is runtime defence in depth).
func execOperatorAwait(s *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
	resume, ok := node.next("then")
	if !ok {
		return execOpOutcome{}
	}
	name := configString(node.Config, awaitNameConfigKey)
	if name == "" {
		s.logger.Error("exec: operator.await without await_name — chain ended", "node", node.ID)
		return execOpOutcome{}
	}
	key := awaitRegistryKey(s.execProgKey(node), name)
	if s.pendingAwaits == nil {
		s.pendingAwaits = map[string]*pendingAwait{}
	}
	if _, dup := s.pendingAwaits[key]; dup {
		// Two live suspensions on the same name would make a resolve
		// ambiguous. The first holder keeps the name; the second ends its
		// chain loudly rather than parking an unaddressable continuation.
		s.logger.Error("exec: operator.await duplicate await_name — chain ended",
			"node", node.ID, "await", key)
		return execOpOutcome{}
	}
	wakeKey := s.nextWakeKey()
	s.pendingAwaits[key] = &pendingAwait{
		view: PendingAwait{
			BlueprintKey: s.execProgKey(node),
			AwaitName:    name,
			ValueType:    configString(node.Config, awaitTypeConfigKey),
			UI:           rawConfig(node.Config, awaitUIConfigKey),
		},
		wakeKey: wakeKey,
		nodeID:  node.ID,
	}
	return execOpOutcome{
		park:    true,
		parkKey: wakeKey,
		resume:  resume,
		// No timer: the resume is an external operator resolve.
	}
}

// execProgKey returns the blueprint key the node's program is namespaced
// under. The interpreter does not pass the program to an op, so the await op
// recovers it from the entry/registry context: every await node belongs to
// exactly one installed program, and a scene hosting it knows the key. We
// resolve it by scanning the installed programs for the one owning this node
// id — O(programs), bounded and scene-goroutine-only. The result is the same
// `BlueprintKey` execVariableSet uses for `__vars`.
func (s *Scene) execProgKey(node *ExecNode) string {
	for key, p := range s.execProgs {
		if _, ok := p.Nodes[node.ID]; ok {
			return key
		}
	}
	return ""
}

// Errors the operator routes map to HTTP statuses.
var (
	// errAwaitUnknown — no live suspension under this (blueprint, name):
	// never armed, already resolved, or invalidated by a switch-away
	// (invariant 7). The route answers 410 Gone.
	errAwaitUnknown = errors.New("operator await not found")
	// errAwaitTypeMismatch — the supplied value failed the value_type
	// check. The route answers 422.
	errAwaitTypeMismatch = errors.New("operator await value type mismatch")
)

// ListPendingAwaits returns a snapshot of this scene's live suspension
// points for the named blueprint key (empty key = all), sorted by registry
// key for a deterministic listing. Routes call it through the scene inbox
// gate (snapshotPending) — never directly, to stay off the scene goroutine.
func (s *Scene) listPendingAwaits(blueprintKey string) []PendingAwait {
	out := make([]PendingAwait, 0, len(s.pendingAwaits))
	keys := make([]string, 0, len(s.pendingAwaits))
	for k := range s.pendingAwaits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pa := s.pendingAwaits[k]
		if blueprintKey != "" && pa.view.BlueprintKey != blueprintKey {
			continue
		}
		out = append(out, pa.view)
	}
	return out
}

// resolveAwait validates the supplied value against the await's value_type,
// then resumes the parked continuation with the value bound under the
// node's `value` pin and removes the registry entry. Scene goroutine only
// (called via the resolve inbox message). Returns errAwaitUnknown when no
// live suspension matches (410) and errAwaitTypeMismatch on a type failure
// (422); both leave the continuation untouched.
func (s *Scene) resolveAwait(blueprintKey, awaitName string, value json.RawMessage) error {
	key := awaitRegistryKey(blueprintKey, awaitName)
	pa, ok := s.pendingAwaits[key]
	if !ok {
		return errAwaitUnknown
	}
	if !valueMatchesType(value, pa.view.ValueType) {
		return errAwaitTypeMismatch
	}
	delete(s.pendingAwaits, key)
	// Bind the resolved value under the await node's `value` data-out pin:
	// the resumed continuation reads it through demandValue's task-env path
	// (`<node_id>.value`), exactly as on-tick binds `<node>.delta_seconds`
	// and on-event binds `<node>.payload`. resumeParkedWith merges the env
	// into the parked task before re-enqueue.
	s.resumeParkedWith(pa.wakeKey, map[string]json.RawMessage{
		pa.nodeID + "." + awaitValuePort: value,
	})
	return nil
}

// valueMatchesType reports whether raw is a valid instance of the
// `core.primitive.*` type tag (Blue ADR 008 §3.3 value_type). Orion has no
// general type system at runtime — type-checking is an authoring (Blue /
// compiler) concern — so this is the minimal primitive validator the resolve
// path needs to reject a malformed operator value with a clear 4xx:
//
//   - core.primitive.string  → JSON string
//   - core.primitive.integer → JSON number with no fractional part
//   - core.primitive.float   → JSON number
//   - core.primitive.boolean → JSON true/false
//   - core.primitive.json    → any well-formed JSON value (the await's
//     declared static upper bound — accepts anything parseable)
//   - "" / unknown tag       → accept any well-formed JSON (no narrower
//     contract to enforce; the authoring layer owns refinement)
//
// In every case the bytes must be well-formed JSON.
func valueMatchesType(raw json.RawMessage, valueType string) bool {
	if !json.Valid(raw) {
		return false
	}
	switch valueType {
	case "core.primitive.string":
		var v string
		return json.Unmarshal(raw, &v) == nil
	case "core.primitive.boolean":
		var v bool
		return json.Unmarshal(raw, &v) == nil
	case "core.primitive.float":
		var v float64
		return json.Unmarshal(raw, &v) == nil
	case "core.primitive.integer":
		var v float64
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		return v == float64(int64(v))
	default:
		// core.primitive.json, empty, or an unrecognised tag: accept any
		// well-formed JSON value.
		return true
	}
}

// --- public operator surface (called by the API routes) ---------------

// HostsBlueprint reports whether this scene has an installed exec program
// for the given scene-local blueprint key (ADR 001 §3.2). The operator
// routes use it to answer 409 when a blueprint is not part of the active
// scene (dormant — ADR 008 active-only). Read of the install-time index.
func (s *Scene) HostsBlueprint(blueprintKey string) bool {
	_, ok := s.execProgs[blueprintKey]
	return ok
}

// HasOnCallEntry reports whether the scene has an armed `on-call`
// entrypoint addressable by the namespaced key `<blueprint_key>/<entry_id>`
// OR an unambiguous bare entry id (resolveEntry's convenience). Read of the
// install-time index — safe from any goroutine (execEntries is read-only
// after InstallExec). The route uses it to answer 409 for an unknown
// entrypoint before firing.
func (s *Scene) HasOnCallEntry(entrypointID string) bool {
	ref, _, ok := s.resolveEntry(entrypointID)
	return ok && ref.entry.Kind == EntryOnCall
}

// FireOnCall fires a named on-call entrypoint with the operator `payload`
// bound under the on-call node's `payload` data-out pin (parity with
// on-event). Returns false if the inbox is full (the route maps that to a
// 503). The entrypoint must be on-call (checked by the route via
// HasOnCallEntry first). Safe from any goroutine — travels the inbox.
func (s *Scene) FireOnCall(entrypointID string, payload json.RawMessage) bool {
	ref, fireKey, ok := s.resolveEntry(entrypointID)
	if !ok || ref.entry.Kind != EntryOnCall {
		return false
	}
	var env map[string]json.RawMessage
	if node := ref.entry.Node; node != "" && payload != nil {
		env = map[string]json.RawMessage{node + ".payload": payload}
	}
	// Fire under the canonical namespaced key resolveEntry matched — the
	// entry's INDEX key, which is `config.entrypoint` for a named on-call
	// and no longer equals the graph node id. The payload still binds under
	// the node id (its data-out namespace), kept separately above.
	return s.Input(InputMsg{
		FireExec: fireKey,
		FireEnv:  env,
		Source:   "operator:on-call",
	})
}

// PendingAwaits returns a snapshot of the scene's live operator awaits for
// blueprintKey (empty = all), computed ON the scene goroutine via the
// Control seam so the registry is read under single-writer. Returns nil if
// the scene loop is gone (inbox full / stopped).
func (s *Scene) PendingAwaits(blueprintKey string) []PendingAwait {
	reply := make(chan []PendingAwait, 1)
	if !s.Input(InputMsg{Control: func(sc *Scene) {
		reply <- sc.listPendingAwaits(blueprintKey)
	}}) {
		return nil
	}
	return <-reply
}

// ResolveAwait supplies a value to a live operator await, type-checked
// against its value_type, resuming the parked continuation. Runs on the
// scene goroutine via Control. Returns errAwaitUnknown (→410),
// errAwaitTypeMismatch (→422), or nil. A full inbox yields errAwaitUnknown
// (the scene cannot service it).
func (s *Scene) ResolveAwait(blueprintKey, awaitName string, value json.RawMessage) error {
	reply := make(chan error, 1)
	if !s.Input(InputMsg{Control: func(sc *Scene) {
		reply <- sc.resolveAwait(blueprintKey, awaitName, value)
	}}) {
		return errAwaitUnknown
	}
	return <-reply
}

// ErrAwaitUnknown / ErrAwaitTypeMismatch are the exported sentinels the API
// layer maps to HTTP statuses.
var (
	ErrAwaitUnknown      = errAwaitUnknown
	ErrAwaitTypeMismatch = errAwaitTypeMismatch
)

// rawConfig returns the raw JSON config value under key, or nil when absent
// — preserving omitempty on the published `ui`.
func rawConfig(cfg map[string]json.RawMessage, key string) json.RawMessage {
	if cfg == nil {
		return nil
	}
	if v, ok := cfg[key]; ok {
		return v
	}
	return nil
}
