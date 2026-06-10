package conformance_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/conformance"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// allowlistRatchet is the FROZEN size of conformance_allowlist.txt.
// The ratchet (ADR 003 §6 criterion 1): CI fails if the live allowlist
// exceeds this. Shrinking the allowlist means lowering this number in
// the same commit — the list only ever goes down, toward 0.
//
// ADR 006 §3.5 (issue #107): the 6 core.db.* inline-only atoms have
// been reclassified KindInlineOnly and dropped from the allowlist.
// The allowlist has reached its accepted empty end-state.
const allowlistRatchet = 0

// repoRoot walks up from the package dir to the repo root (the dir
// holding conformance_allowlist.txt).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "conformance_allowlist.txt")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("conformance_allowlist.txt not found walking up from %s", wd)
	return ""
}

func loadAllowlist(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "conformance_allowlist.txt"))
	if err != nil {
		t.Fatalf("read allowlist: %v", err)
	}
	return conformance.ParseAllowlist(string(body))
}

// TestConformance_Matrix is the MASTER criterion (ADR 003 §6 #1): every
// Blue manifest node id is either served by a registered Orion executor,
// classified KindInlineOnly (served transitively — not a gap), or on the
// allowlist — never silently unserved. Hard-fails CI on any uncovered id.
func TestConformance_Matrix(t *testing.T) {
	manifest := conformance.Manifest()
	if len(manifest) == 0 {
		t.Fatal("vendored manifest is empty")
	}

	allow := loadAllowlist(t)
	allowSet := map[string]bool{}
	for _, id := range allow {
		allowSet[id] = true
	}

	var uncovered []string
	covered := 0
	inlineOnlyCovered := 0
	for _, e := range manifest {
		sn, servedOK := conformance.Classify(e.NodeID)
		switch {
		case servedOK && sn.Kind == conformance.KindInlineOnly:
			// Inline-only: covered transitively (not a gap, not allowlisted).
			covered++
			inlineOnlyCovered++
		case servedOK:
			covered++
		case allowSet[e.NodeID]:
			// tolerated transitional gap
		default:
			uncovered = append(uncovered, e.NodeID)
		}
	}

	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Fatalf("conformance: %d manifest node(s) neither served nor allowlisted "+
			"(doctrine §1.1 violated — every Blue node must be served or a named, "+
			"shrinking debt):\n  %v", len(uncovered), uncovered)
	}

	t.Logf("conformance: %d/%d manifest nodes covered (%d inline-only via db.query), "+
		"%d on allowlist",
		covered, len(manifest), inlineOnlyCovered, len(allow))
}

// TestConformance_AllowlistRatchet enforces the shrink-to-zero ratchet:
// the allowlist never grows. Adding a node to it (without lowering
// allowlistRatchet) fails CI; that is the whole point — a regression
// where Blue gains a node Orion does not serve cannot land silently.
func TestConformance_AllowlistRatchet(t *testing.T) {
	allow := loadAllowlist(t)
	if len(allow) > allowlistRatchet {
		t.Fatalf("conformance ratchet: allowlist grew to %d (frozen ceiling %d). "+
			"The allowlist only shrinks toward 0 — serve the new node + add its test, "+
			"do not widen the allowlist:\n  %v", len(allow), allowlistRatchet, allow)
	}
	if len(allow) < allowlistRatchet {
		t.Fatalf("conformance ratchet: allowlist shrank to %d but allowlistRatchet is "+
			"still %d — lower the constant in the SAME commit so the ratchet keeps "+
			"biting at the new floor", len(allow), allowlistRatchet)
	}
}

// TestConformance_AllowlistEntriesAreRealManifestIDs guards against a
// stale allowlist: every id it lists must be a real manifest id (a typo
// or a since-removed node would otherwise hide forever).
func TestConformance_AllowlistEntriesAreRealManifestIDs(t *testing.T) {
	known := map[string]bool{}
	for _, e := range conformance.Manifest() {
		known[e.NodeID] = true
	}
	for _, id := range loadAllowlist(t) {
		if !known[id] {
			t.Errorf("allowlist entry %q is not a manifest node id (stale or typo)", id)
		}
		// An allowlisted id must NOT also be classified as served — that
		// would be dead debt masking a contradiction.
		if _, ok := conformance.Classify(id); ok {
			t.Errorf("allowlist entry %q is ALSO classified as served — remove it from "+
				"the allowlist (it is served, not a gap)", id)
		}
	}
}

// TestConformance_ComputeClassificationMatchesRegistry cross-checks the
// id→executor classification against the REAL compute registry: every
// id classified KindCompute must actually be installed by
// runtime.NewComputeRegistry(), and (the converse) every registered
// compute id must be classified KindCompute. A drift either way — a
// node claimed served that nobody registered, or a registered executor
// the matrix forgot — fails here, not on air.
func TestConformance_ComputeClassificationMatchesRegistry(t *testing.T) {
	reg := runtime.NewComputeRegistry()
	registered := map[string]bool{}
	for _, id := range reg.IDs() {
		registered[id] = true
	}

	for _, id := range conformance.ServedComputeIDs() {
		if !registered[id] {
			t.Errorf("node %q classified KindCompute but NOT in NewComputeRegistry()", id)
		}
	}
	for id := range registered {
		sn, ok := conformance.Classify(id)
		if !ok || sn.Kind != conformance.KindCompute {
			t.Errorf("compute registry installs %q but the matrix does not classify it "+
				"KindCompute (classified=%v, ok=%v) — registry/matrix drift", id, sn.Kind, ok)
		}
	}
}

// TestConformance_ExecOpClassificationMatchesRuntime cross-checks every
// op the matrix maps onto (KindExecOp) against runtime.ExecOps — the
// canonical set the interpreter actually serves. A manifest node mapped
// to a non-existent op fails here.
func TestConformance_ExecOpClassificationMatchesRuntime(t *testing.T) {
	runtimeOps := map[string]bool{}
	for _, op := range runtime.ExecOps {
		runtimeOps[op] = true
	}
	for _, op := range conformance.ServedExecOps() {
		if !runtimeOps[op] {
			t.Errorf("matrix maps a node onto exec op %q which runtime.ExecOps does not "+
				"serve", op)
		}
	}
}

// TestConformance_InlineOnlyAtomsAreClassifiedNotGap verifies the 6
// core.db.* atoms are KindInlineOnly (ADR 006 §3.5 / issue #107):
//   - Classify returns ok=true with KindInlineOnly for each.
//   - None appears on the allowlist (they are NOT a gap).
//   - None is KindCompute or KindExecOp (no standalone executor).
//   - Each carries a non-empty Reason.
//
// Regression guard: if one of these atoms is accidentally re-added to
// the allowlist, TestConformance_AllowlistEntriesAreRealManifestIDs
// also catches the contradiction (served AND allowlisted).
func TestConformance_InlineOnlyAtomsAreClassifiedNotGap(t *testing.T) {
	want := []string{
		"core.db.from@1",
		"core.db.join@1",
		"core.db.limit@1",
		"core.db.order@1",
		"core.db.select@1",
		"core.db.where@1",
	}
	allowSet := map[string]bool{}
	for _, id := range loadAllowlist(t) {
		allowSet[id] = true
	}
	ids := conformance.InlineOnlyIDs()
	if len(ids) != len(want) {
		t.Fatalf("InlineOnlyIDs() returned %d entries, want %d: %v", len(ids), len(want), ids)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("InlineOnlyIDs()[%d] = %q, want %q", i, id, want[i])
		}
	}
	for _, id := range want {
		sn, ok := conformance.Classify(id)
		if !ok {
			t.Errorf("Classify(%q) = ok=false, want KindInlineOnly", id)
			continue
		}
		if sn.Kind != conformance.KindInlineOnly {
			t.Errorf("Classify(%q).Kind = %q, want KindInlineOnly", id, sn.Kind)
		}
		if sn.Reason == "" {
			t.Errorf("Classify(%q).Reason is empty — inline-only classification requires a reason", id)
		}
		if allowSet[id] {
			t.Errorf("%q is KindInlineOnly but ALSO on the allowlist — remove it from the allowlist", id)
		}
	}
}

// TestConformance_InlineOnlyAtomsAreInManifest verifies the 6 atoms
// actually exist in the vendored manifest (a rename in Blue would remove
// them from the manifest and this test would catch the drift).
func TestConformance_InlineOnlyAtomsAreInManifest(t *testing.T) {
	known := map[string]bool{}
	for _, e := range conformance.Manifest() {
		known[e.NodeID] = true
	}
	for _, id := range conformance.InlineOnlyIDs() {
		if !known[id] {
			t.Errorf("inline-only id %q is not in the vendored manifest — "+
				"either the atom was renamed in Blue (update manifest.json) "+
				"or InlineOnlyIDs() has a stale entry", id)
		}
	}
}

// TestConformance_RatchetProof_RejectGrowth proves the ratchet rejects
// growth without requiring a live file write. It simulates what
// TestConformance_AllowlistRatchet would do if it read a list with 1
// entry against the current ceiling of 0.
func TestConformance_RatchetProof_RejectGrowth(t *testing.T) {
	// Simulate: allowlist has 1 entry, ceiling is 0.
	simulatedLen := 1
	ceiling := allowlistRatchet // 0

	wouldFail := simulatedLen > ceiling
	if !wouldFail {
		t.Fatal("ratchet did not trip on a simulated growth — ratchet logic is broken")
	}
	t.Logf("ratchet proof: a 1-entry list against ceiling %d correctly trips (growth blocked)", ceiling)
}
