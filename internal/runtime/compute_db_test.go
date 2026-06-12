package runtime

// Tests for the core.db.* descriptor-builder tranche (ADR 007 §3.1,
// issues #140/#141). Reference shape: QueryMe QueryDescriptor
// (QueryMe/src/queryme/descriptor.py). Every builder gets golden
// plan-in → plan-out coverage including the totality contract (a
// malformed/absent plan is normalised to the empty base shape, never an
// error); the from→where→join→select→order→limit chain is asserted to
// produce a descriptor matching the canonical QueryMe shape byte-for-byte
// (the parity fixture shared with Blue per §6.1).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestQueryDescriptor_GoldenParity is the Orion arm of the cross-repo
// QueryDescriptor contract test (§6.1). It decodes the shared golden
// fixture (internal/conformance/querydescriptor_golden.json) into Orion's
// queryDescriptor struct and re-encodes it, asserting the round-trip is
// byte-stable against the golden re-canonicalised the same way. The
// QueryMe arm (QueryMe/tests/test_descriptor_contract.py) and the Blue
// preview arm decode the SAME golden into their models — so all three
// QueryDescriptor representations are pinned to one fixture. A field
// rename or reorder on any side breaks its arm loudly.
//
// This proves the `on` JOIN pair stays a 2-tuple, the four clause lists
// stay arrays, limit stays omitempty, and the field NAMES match across the
// Go struct, the QueryMe Pydantic model, and Blue's preview emitter.
func TestQueryDescriptor_GoldenParity(t *testing.T) {
	root := goldenRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "conformance", "querydescriptor_golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var d queryDescriptor
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a stray/renamed field fails here
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("golden does not decode into Orion queryDescriptor (field drift?): %v", err)
	}
	// Re-encode and compare semantically against the golden — proves the
	// struct emits the same field set the consumers (QueryMe validator)
	// expect, with limit present and no spurious offset.
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonEq(t, string(out), string(raw))
	// Spot-check the load-bearing shape invariants explicitly.
	if d.Table != "players" || len(d.Where) != 2 || len(d.Joins) != 1 || d.Joins[0].On != [2]string{"id", "player_id"} {
		t.Fatalf("golden decoded to unexpected shape: %+v", d)
	}
	if d.Limit == nil || *d.Limit != 5 || d.Offset != nil {
		t.Fatalf("limit/offset contract drift: limit=%v offset=%v", d.Limit, d.Offset)
	}
}

// goldenRoot walks up to the Orion repo root (the dir holding go.mod).
func goldenRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("repo root (go.mod) not found from %s", wd)
	return ""
}

// runDB executes a db builder and returns the raw plan JSON. It is a thin
// alias over runPure so these tests read against the shared helper.
func runDB(t *testing.T, id string, inputs, config map[string]json.RawMessage) string {
	t.Helper()
	return runPure(t, id, inputs, config)
}

// jsonEq compares two JSON strings semantically (key order independent),
// so a Go-map-ordered descriptor can be checked against a hand-written
// canonical fixture without depending on serialisation order.
func jsonEq(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not valid JSON: %v (%s)", err, want)
	}
	gc, _ := json.Marshal(g)
	wc, _ := json.Marshal(w)
	if string(gc) != string(wc) {
		t.Fatalf("descriptor mismatch:\n got:  %s\n want: %s", gc, wc)
	}
}

// ---------------------------------------------------------------------------
// from
// ---------------------------------------------------------------------------

func TestPure_DBFrom(t *testing.T) {
	// from emits the base shape with the four list clauses initialised
	// (matching QueryMe default_factory=list), no limit/offset.
	got := runDB(t, "core.db.from@1", nil, in("table", `"players"`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[]}`)

	// Missing config table → empty table string (totality; the server
	// validator rejects an empty/unknown table).
	got = runDB(t, "core.db.from@1", nil, nil)
	jsonEq(t, got, `{"table":"","where":[],"joins":[],"select":[],"order":[]}`)
}

// ---------------------------------------------------------------------------
// where
// ---------------------------------------------------------------------------

func TestPure_DBWhere(t *testing.T) {
	base := `{"table":"players","where":[],"joins":[],"select":[],"order":[]}`

	// Comparison op carries the wired value.
	got := runDB(t, "core.db.where@1",
		in("plan", base, "value", `"GIDEON"`),
		in("column", `"summoner_name"`, "op", `"="`))
	jsonEq(t, got, `{"table":"players","where":[{"column":"summoner_name","op":"=","value":"GIDEON"}],"joins":[],"select":[],"order":[]}`)

	// Missing op → seeded default "=".
	got = runDB(t, "core.db.where@1",
		in("plan", base, "value", `5`),
		in("column", `"score"`))
	jsonEq(t, got, `{"table":"players","where":[{"column":"score","op":"=","value":5}],"joins":[],"select":[],"order":[]}`)

	// IS NULL → value normalised to null even if one was wired
	// (QueryMe WhereClause._check_value_shape parity).
	got = runDB(t, "core.db.where@1",
		in("plan", base, "value", `"ignored"`),
		in("column", `"deleted_at"`, "op", `"IS NULL"`))
	jsonEq(t, got, `{"table":"players","where":[{"column":"deleted_at","op":"IS NULL","value":null}],"joins":[],"select":[],"order":[]}`)

	// Missing value input → null (Python None).
	got = runDB(t, "core.db.where@1",
		in("plan", base),
		in("column", `"x"`, "op", `">"`))
	jsonEq(t, got, `{"table":"players","where":[{"column":"x","op":">","value":null}],"joins":[],"select":[],"order":[]}`)

	// IN op with a list value passes through verbatim.
	got = runDB(t, "core.db.where@1",
		in("plan", base, "value", `[1,2,3]`),
		in("column", `"id"`, "op", `"IN"`))
	jsonEq(t, got, `{"table":"players","where":[{"column":"id","op":"IN","value":[1,2,3]}],"joins":[],"select":[],"order":[]}`)
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

func TestPure_DBJoin(t *testing.T) {
	base := `{"table":"player_scores","where":[],"joins":[],"select":[],"order":[]}`

	got := runDB(t, "core.db.join@1",
		in("plan", base),
		in("table", `"splits"`, "local_column", `"split_id"`,
			"foreign_column", `"id"`, "select", `["name"]`))
	jsonEq(t, got, `{"table":"player_scores","where":[],"joins":[{"table":"splits","on":["split_id","id"],"select":["name"]}],"select":[],"order":[]}`)

	// Missing select → empty list (filter-only join).
	got = runDB(t, "core.db.join@1",
		in("plan", base),
		in("table", `"splits"`, "local_column", `"split_id"`, "foreign_column", `"id"`))
	jsonEq(t, got, `{"table":"player_scores","where":[],"joins":[{"table":"splits","on":["split_id","id"],"select":[]}],"select":[],"order":[]}`)
}

// ---------------------------------------------------------------------------
// select
// ---------------------------------------------------------------------------

func TestPure_DBSelect(t *testing.T) {
	base := `{"table":"players","where":[],"joins":[],"select":[],"order":[]}`

	got := runDB(t, "core.db.select@1",
		in("plan", base),
		in("columns", `["id","summoner_name"]`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":["id","summoner_name"],"order":[]}`)

	// select OVERWRITES (sets) rather than appends.
	withSel := `{"table":"players","where":[],"joins":[],"select":["old"],"order":[]}`
	got = runDB(t, "core.db.select@1",
		in("plan", withSel),
		in("columns", `["new"]`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":["new"],"order":[]}`)
}

// ---------------------------------------------------------------------------
// order
// ---------------------------------------------------------------------------

func TestPure_DBOrder(t *testing.T) {
	base := `{"table":"players","where":[],"joins":[],"select":[],"order":[]}`

	got := runDB(t, "core.db.order@1",
		in("plan", base),
		in("column", `"score"`, "direction", `"desc"`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[{"column":"score","direction":"desc"}]}`)

	// Missing direction → seeded default "asc".
	got = runDB(t, "core.db.order@1",
		in("plan", base),
		in("column", `"name"`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[{"column":"name","direction":"asc"}]}`)

	// Stacking: a second order entry appends (tie-break order).
	oneOrder := `{"table":"players","where":[],"joins":[],"select":[],"order":[{"column":"score","direction":"desc"}]}`
	got = runDB(t, "core.db.order@1",
		in("plan", oneOrder),
		in("column", `"name"`, "direction", `"asc"`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[{"column":"score","direction":"desc"},{"column":"name","direction":"asc"}]}`)
}

// ---------------------------------------------------------------------------
// limit
// ---------------------------------------------------------------------------

func TestPure_DBLimit(t *testing.T) {
	base := `{"table":"players","where":[],"joins":[],"select":[],"order":[]}`

	// Config n applies when the n input is unwired.
	got := runDB(t, "core.db.limit@1",
		in("plan", base),
		in("n", `25`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[],"limit":25}`)

	// No config and no input → seeded default 100.
	got = runDB(t, "core.db.limit@1", in("plan", base), nil)
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[],"limit":100}`)

	// The n INPUT overrides the config default when wired.
	got = runDB(t, "core.db.limit@1",
		in("plan", base, "n", `10`),
		in("n", `100`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[],"limit":10}`)
}

// ---------------------------------------------------------------------------
// Totality — a malformed/absent plan is normalised to the base shape
// ---------------------------------------------------------------------------

func TestPure_DBTotality_MalformedPlan(t *testing.T) {
	// A non-object plan (a string) is replaced by the empty base shape
	// before the clause applies — the builder never errors (ADR 007 §3.1
	// totality rule).
	got := runDB(t, "core.db.where@1",
		in("plan", `"not an object"`, "value", `1`),
		in("column", `"x"`, "op", `"="`))
	jsonEq(t, got, `{"table":"","where":[{"column":"x","op":"=","value":1}],"joins":[],"select":[],"order":[]}`)

	// An absent plan input on a non-head builder also starts from base.
	got = runDB(t, "core.db.select@1", nil, in("columns", `["a"]`))
	jsonEq(t, got, `{"table":"","where":[],"joins":[],"select":["a"],"order":[]}`)

	// A partial object (only a table) keeps its table and gets the missing
	// list clauses re-initialised to empty arrays.
	got = runDB(t, "core.db.order@1",
		in("plan", `{"table":"players"}`),
		in("column", `"score"`, "direction", `"desc"`))
	jsonEq(t, got, `{"table":"players","where":[],"joins":[],"select":[],"order":[{"column":"score","direction":"desc"}]}`)
}

// ---------------------------------------------------------------------------
// Parity — a full chain produces a canonical QueryMe QueryDescriptor
// ---------------------------------------------------------------------------

// dbChainDescriptor is the SHARED parity fixture (ADR 007 §6.1): the exact
// QueryDescriptor a from→where→join→select→order→limit chain must yield —
// the same payload QueryMe v0.2.2 accepts byte-for-byte. Mirror in Blue's
// preview-executor parity test (#59/#60/#61).
const dbChainDescriptor = `{
  "table": "player_scores",
  "where": [{"column": "score", "op": ">=", "value": 5}],
  "joins": [{"table": "splits", "on": ["split_id", "id"], "select": ["name"]}],
  "select": ["player_id", "score"],
  "order": [{"column": "score", "direction": "desc"}],
  "limit": 10
}`

// TestPure_DBChain_ParityWithQueryMeShape composes the six builders in
// the canonical clause order and asserts the accumulating plan equals the
// QueryMe QueryDescriptor fixture — proving the chain's final `plan` wires
// verbatim into core.db.query@1's `descriptor` input with no conversion
// step (ADR 007 §3.1, §6.1 parity).
func TestPure_DBChain_ParityWithQueryMeShape(t *testing.T) {
	// from player_scores
	plan := runDB(t, "core.db.from@1", nil, in("table", `"player_scores"`))
	// where score >= 5
	plan = runDB(t, "core.db.where@1",
		in("plan", plan, "value", `5`),
		in("column", `"score"`, "op", `">="`))
	// join splits on (split_id, id) select [name]
	plan = runDB(t, "core.db.join@1",
		in("plan", plan),
		in("table", `"splits"`, "local_column", `"split_id"`,
			"foreign_column", `"id"`, "select", `["name"]`))
	// select [player_id, score]
	plan = runDB(t, "core.db.select@1",
		in("plan", plan),
		in("columns", `["player_id","score"]`))
	// order score desc
	plan = runDB(t, "core.db.order@1",
		in("plan", plan),
		in("column", `"score"`, "direction", `"desc"`))
	// limit 10
	plan = runDB(t, "core.db.limit@1",
		in("plan", plan),
		in("n", `10`))

	jsonEq(t, plan, dbChainDescriptor)

	// The final plan must also decode into the canonical field set with no
	// extra keys (QueryMe model_config extra="forbid"): table, where,
	// joins, select, order, limit (offset omitted since unset).
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(plan), &keys); err != nil {
		t.Fatalf("final plan undecodable: %v", err)
	}
	allowed := map[string]bool{
		"table": true, "where": true, "joins": true,
		"select": true, "order": true, "limit": true, "offset": true,
	}
	for k := range keys {
		if !allowed[k] {
			t.Errorf("final plan carries unknown key %q (QueryMe forbids extras)", k)
		}
	}
	if _, ok := keys["offset"]; ok {
		t.Error("final plan emitted an offset key though no builder set it")
	}
}
