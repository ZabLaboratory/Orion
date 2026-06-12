// Package conformance is the total-conformance matrix (ADR 003 §3.4
// phase 5 / §6 criterion 1, the MASTER criterion). It proves that every
// node id in Blue's compute manifest — the ~70 `core.*` plus the 14
// `quasar.twitch.*` — has a registered Orion executor AND a passing
// execution test through the real engine. The transitional gap set lives
// in `conformance_allowlist.txt` and only ever shrinks (CI ratchet); the
// accepted end state is empty.
//
// Doctrine (ADR 003 §1.1): Orion serves the entirety of Blue — no node
// is rejected for being unserved. A node not yet served is a named,
// shrinking debt, never a capability restriction.
//
// The matrix runs OFFLINE (no Blue service in CI): the manifest is
// vendored at `internal/conformance/manifest.json` (regenerated from
// Blue's source of truth by `scripts/gen_conformance_manifest.py`). The
// id→executor classification below is cross-checked against the REAL
// runtime registries in conformance_test.go, so a node claimed "served"
// that no registry actually installs fails CI.
package conformance

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed manifest.json
var manifestJSON []byte

// ManifestEntry mirrors one Blue compute-manifest row, vendored.
type ManifestEntry struct {
	NodeID    string `json:"node_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Category  string `json:"category"`
	IsPure    bool   `json:"is_pure"`
	IsBounded bool   `json:"is_bounded"`
	Source    string `json:"source"`
	Platform  *struct {
		Name string `json:"name"`
	} `json:"platform"`
}

type manifestFile struct {
	Count   int             `json:"count"`
	Entries []ManifestEntry `json:"entries"`
}

// Manifest returns the vendored Blue compute manifest (sorted by
// node_id). It is the canonical set the conformance matrix iterates.
func Manifest() []ManifestEntry {
	var f manifestFile
	if err := json.Unmarshal(manifestJSON, &f); err != nil {
		panic(fmt.Sprintf("conformance: vendored manifest.json is corrupt: %v", err))
	}
	out := append([]ManifestEntry(nil), f.Entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// ExecutorKind classifies HOW Orion serves a manifest node id.
type ExecutorKind string

const (
	// KindCompute — a pure data node served by the runtime compute
	// registry (runtime.NewComputeRegistry; compute.go + compute_pure.go).
	// Cross-checked: its id MUST be in the registry's IDs().
	KindCompute ExecutorKind = "compute"
	// KindExecOp — served by the exec-layer interpreter, identified by a
	// runtime exec op (runtime.ExecOps). Cross-checked: the mapped op
	// MUST be in ExecOps.
	KindExecOp ExecutorKind = "exec-op"
	// KindEntry — an event entrypoint (on-start/on-tick/on-event):
	// compiled to an ExecEntry and FIRED by a trigger source, not a
	// value/effect executor. Served by the trigger wiring.
	KindEntry ExecutorKind = "entry"
	// KindLeafBound — bound to a state leaf by the compiler, no runtime
	// executor needed: core.input (adapter-written leaf), core.literal
	// (seeded default), core.variable.get (pure leaf read).
	KindLeafBound ExecutorKind = "leaf-bound"
	// KindPlatformBound — a quasar.* platform-event node: the compiler
	// binds it to a `__inputs.platform.*` leaf + a platform-stream
	// binding (ADR 003 §3.3); no runtime executor goroutine.
	KindPlatformBound ExecutorKind = "platform-bound"
)

// ServedNode describes how Orion serves one manifest id.
type ServedNode struct {
	Kind ExecutorKind
	// Op is the runtime exec op for KindExecOp (empty otherwise),
	// cross-checked against runtime.ExecOps.
	Op string
	// Test is a representative real-engine test for this node's
	// executor — documentary.
	Test string
}

// served is the classification table for standalone-executable nodes.
// Keep it sorted by id for review.
//
// The 6 core.db.* clause atoms are KindCompute (ADR 007 §3.1, issues
// #140/#141): pure descriptor builders in the runtime compute registry,
// composable in the main graph as ordinary dataflow. They USED to be
// "inline-only" (Plan C, never implemented); ADR 007 retired that.
//
// For each entry, `Test` documents a representative execution test that
// exercises the executor through the real engine. The "+ a passing test
// through the real engine" half of criterion 1 is enforced
// EXECUTABLY by TestConformance_EveryServedNodeExecutes (runtime
// package), which drives every KindCompute id and every KindExecOp op
// through a real Scene — `Test` here is a documentary pointer to the
// richer behavioural test, not the conformance proof itself.
var served = map[string]ServedNode{
	// --- Exec entrypoints (trigger-fired) -----------------------------
	"core.event.on-start@1": {Kind: KindEntry, Test: "TestExec_OnStart_ActivationAndRePush"},
	"core.event.on-tick@1":  {Kind: KindEntry, Test: "TestExec_OnTick_DeltaSeconds"},
	"core.event.on-event@1": {Kind: KindEntry, Test: "TestExec_OnEvent_FiresPerWrite"},

	// --- Leaf-bound (compiler binds, no runtime executor) -------------
	"core.input@1":        {Kind: KindLeafBound, Test: "TestCompile_InputLeafBound"},
	"core.literal@1":      {Kind: KindLeafBound, Test: "TestCompile_LiteralSeededDefault"},
	"core.variable.get@1": {Kind: KindLeafBound, Test: "TestExec_B9_VarsIsolationBetweenInstances"},

	// --- Exec-layer ops -----------------------------------------------
	"core.flow.branch@1":    {Kind: KindExecOp, Op: "branch", Test: "TestExec_BranchBothArms"},
	"core.flow.sequence@1":  {Kind: KindExecOp, Op: "sequence", Test: "TestExec_SequenceOrder"},
	"core.flow.gate@1":      {Kind: KindExecOp, Op: "gate", Test: "TestExec_GateStartClosedThenOpened"},
	"core.flow.for-loop@1":  {Kind: KindExecOp, Op: "for-loop", Test: "TestExec_ForLoop_ContinuationResumesAcrossSlices"},
	"core.flow.for-each@1":  {Kind: KindExecOp, Op: "for-each", Test: "TestExec_ForEach"},
	"core.flow.while@1":     {Kind: KindExecOp, Op: "while", Test: "TestExec_WhileTerminatesOnCondition"},
	"core.flow.delay@1":     {Kind: KindExecOp, Op: "delay", Test: "TestExecDelay_FiresAtDeadline_FakeClock"},
	"core.variable.set@1":   {Kind: KindExecOp, Op: "variable.set", Test: "TestExec_B9_VarsIsolationBetweenInstances"},
	"core.print@1":          {Kind: KindExecOp, Op: "print", Test: "TestExec_Print_WritesDebugRing"},
	"core.animation.play@1": {Kind: KindExecOp, Op: "animation.play", Test: "TestAnim_EmitsCommandAndThenImmediate"},
	"core.http.request@1":   {Kind: KindExecOp, Op: "http.request", Test: "TestEffects_HTTPRequestResumesContinuation"},
	"core.http-request@1":   {Kind: KindExecOp, Op: "http.request", Test: "TestEffects_HTTPRequestResumesContinuation"},
	"core.db.query@1":       {Kind: KindExecOp, Op: "db.query", Test: "TestEffects_DBQueryTopologyA"},
	"core.source.read@1":    {Kind: KindExecOp, Op: "source.read", Test: "TestEffects_SourceReadDeclaredBinding"},

	// --- Compute registry (pure data layer) ---------------------------
	"core.output@1": {Kind: KindCompute, Test: "TestPure_OutputPassthrough"},

	"core.math.add@1":   {Kind: KindCompute, Test: "TestCompute_Arithmetic"},
	"core.math.sub@1":   {Kind: KindCompute, Test: "TestCompute_Arithmetic"},
	"core.math.mul@1":   {Kind: KindCompute, Test: "TestCompute_Arithmetic"},
	"core.math.div@1":   {Kind: KindCompute, Test: "TestCompute_Arithmetic"},
	"core.math.mod@1":   {Kind: KindCompute, Test: "TestCompute_Arithmetic"},
	"core.math.abs@1":   {Kind: KindCompute, Test: "TestPure_MathAbs"},
	"core.math.min@1":   {Kind: KindCompute, Test: "TestPure_MathMinMax"},
	"core.math.max@1":   {Kind: KindCompute, Test: "TestPure_MathMinMax"},
	"core.math.clamp@1": {Kind: KindCompute, Test: "TestPure_MathClamp"},
	"core.math.lerp@1":  {Kind: KindCompute, Test: "TestPure_MathLerp"},
	"core.math.round@1": {Kind: KindCompute, Test: "TestPure_MathRound"},
	"core.math.floor@1": {Kind: KindCompute, Test: "TestPure_MathFloorCeil"},
	"core.math.ceil@1":  {Kind: KindCompute, Test: "TestPure_MathFloorCeil"},

	"core.compare.equal@1":         {Kind: KindCompute, Test: "TestCompute_Comparator"},
	"core.compare.not-equal@1":     {Kind: KindCompute, Test: "TestCompute_Comparator"},
	"core.compare.less-than@1":     {Kind: KindCompute, Test: "TestCompute_Comparator"},
	"core.compare.less-equal@1":    {Kind: KindCompute, Test: "TestCompute_Comparator"},
	"core.compare.greater-than@1":  {Kind: KindCompute, Test: "TestCompute_Comparator"},
	"core.compare.greater-equal@1": {Kind: KindCompute, Test: "TestCompute_Comparator"},

	"core.logic.and@1": {Kind: KindCompute, Test: "TestPure_Logic"},
	"core.logic.or@1":  {Kind: KindCompute, Test: "TestPure_Logic"},
	"core.logic.xor@1": {Kind: KindCompute, Test: "TestPure_Logic"},
	"core.logic.not@1": {Kind: KindCompute, Test: "TestCompute_Not"},

	"core.flow.select@1": {Kind: KindCompute, Test: "TestCompute_Select"},

	"core.string.concat@1": {Kind: KindCompute, Test: "TestPure_StringConcat"},
	"core.string.format@1": {Kind: KindCompute, Test: "TestPure_StringFormat"},
	"core.string.length@1": {Kind: KindCompute, Test: "TestPure_StringLength"},
	"core.string.split@1":  {Kind: KindCompute, Test: "TestPure_StringSplit"},
	"core.string.upper@1":  {Kind: KindCompute, Test: "TestPure_StringUpperLower"},
	"core.string.lower@1":  {Kind: KindCompute, Test: "TestPure_StringUpperLower"},

	"core.cast.to-string@1":  {Kind: KindCompute, Test: "TestPure_CastToString"},
	"core.cast.to-integer@1": {Kind: KindCompute, Test: "TestPure_CastToInteger"},
	"core.cast.to-float@1":   {Kind: KindCompute, Test: "TestPure_CastToFloat"},
	"core.cast.to-boolean@1": {Kind: KindCompute, Test: "TestPure_CastToBoolean"},

	"core.data.get-field@1":   {Kind: KindCompute, Test: "TestPure_DataGetField"},
	"core.data.set-field@1":   {Kind: KindCompute, Test: "TestPure_DataSetField"},
	"core.data.list-length@1": {Kind: KindCompute, Test: "TestPure_DataListLength"},
	"core.data.list-at@1":     {Kind: KindCompute, Test: "TestPure_DataListAt"},
	"core.data.list-append@1": {Kind: KindCompute, Test: "TestPure_DataListAppend"},
	"core.data.aggregate@1":   {Kind: KindCompute, Test: "TestPure_DataAggregate"},

	// core.db.* descriptor builders — pure QueryDescriptor builders
	// (ADR 007 §3.1, issues #140/#141). compute_db.go.
	"core.db.from@1":   {Kind: KindCompute, Test: "TestPure_DBFrom"},
	"core.db.where@1":  {Kind: KindCompute, Test: "TestPure_DBWhere"},
	"core.db.join@1":   {Kind: KindCompute, Test: "TestPure_DBJoin"},
	"core.db.select@1": {Kind: KindCompute, Test: "TestPure_DBSelect"},
	"core.db.order@1":  {Kind: KindCompute, Test: "TestPure_DBOrder"},
	"core.db.limit@1":  {Kind: KindCompute, Test: "TestPure_DBLimit"},
}

// quasarPlatformPrefix identifies the platform-event family the compiler
// binds (KindPlatformBound) rather than listing all 14 by hand — they
// are uniform input leaves and adding a 15th in Blue must surface here
// as a deliberate matrix event, so the family rule is explicit.
const quasarPlatformPrefix = "quasar.twitch."

// Classify returns how a manifest id is served, or ok=false if it is
// not in the served set (then it must be in the allowlist).
func Classify(nodeID string) (ServedNode, bool) {
	if sn, ok := served[nodeID]; ok {
		return sn, true
	}
	if strings.HasPrefix(nodeID, quasarPlatformPrefix) {
		return ServedNode{
			Kind: KindPlatformBound,
			Test: "TestCompile_PlatformBindingBoundAndAccepted",
		}, true
	}
	return ServedNode{}, false
}

// ServedExecOps returns the distinct runtime op names the served table
// maps onto — the cross-check set against runtime.ExecOps.
func ServedExecOps() []string {
	seen := map[string]struct{}{}
	for _, sn := range served {
		if sn.Kind == KindExecOp && sn.Op != "" {
			seen[sn.Op] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for op := range seen {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// ServedComputeIDs returns the manifest ids classified KindCompute — the
// cross-check set against runtime.NewComputeRegistry().IDs().
func ServedComputeIDs() []string {
	out := []string{}
	for id, sn := range served {
		if sn.Kind == KindCompute {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
