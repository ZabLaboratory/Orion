package runtime

// core.db.* descriptor-builder tranche — ADR 007 §3.1, issues #140/#141.
//
// The six clause atomics core.db.{from,where,join,select,order,limit}@1
// are PURE descriptor builders: each takes a `plan` (the accumulating
// QueryMe QueryDescriptor JSON) and returns a refined plan. The chain's
// final `plan` wires verbatim into core.db.query@1's `descriptor` input
// — there is no intermediate format and no conversion step (ADR 007
// §3.1). The emitted shape is QueryMe's canonical
// `{table, where:[], joins:[], select:[], order:[], limit?, offset?}`
// (QueryMe/src/queryme/descriptor.py) so the owning service's validator/
// compiler accepts it byte-for-byte.
//
// Totality rule (ADR 007 §3.1 / §5.4): these are TOTAL pure functions —
// they NEVER error. A malformed upstream `plan` (non-object, or absent)
// is replaced by the empty base shape before the clause is applied. ALL
// semantic validation (unknown table/column, operator/value shape, type
// issues, cross-service joins) stays server-side in the QueryMe
// validator/compiler on the owning service and surfaces on
// core.db.query@1's `error` output — Orion does not re-implement the
// whitelist. One validation boundary, not two.
//
// Config/port names mirror Blue's seeded signatures verbatim
// (stdlib_seeder.py "DB query atomics"):
//   from   — config table
//   where  — input plan, value?; config column, op (default "=")
//   join   — input plan; config table, local_column, foreign_column, select
//   select — input plan; config columns
//   order  — input plan; config column, direction (default "asc")
//   limit  — input plan, n?; config n (default 100)

import (
	"encoding/json"
)

// queryDescriptor is the canonical QueryMe QueryDescriptor wire shape
// (QueryMe/src/queryme/descriptor.py::QueryDescriptor). Field ORDER and
// tags are pinned to that model: `table` first, then where/joins/select/
// order, then the optional limit/offset. `where`, `joins`, `select` and
// `order` always serialise as arrays (never null / never omitted) so the
// base shape emitted by `from` matches QueryMe's default_factory=list
// fields; `limit`/`offset` are pointers with omitempty so an unset clause
// emits no key (QueryMe: `None` = no clause).
type queryDescriptor struct {
	Table  string        `json:"table"`
	Where  []whereClause `json:"where"`
	Joins  []joinClause  `json:"joins"`
	Select []string      `json:"select"`
	Order  []orderClause `json:"order"`
	Limit  *int          `json:"limit,omitempty"`
	Offset *int          `json:"offset,omitempty"`
}

// whereClause mirrors QueryMe WhereClause. `value` is always present
// (QueryMe Field default None → JSON null) so the wire shape is
// predictable; the consumer normalises IS NULL anyway.
type whereClause struct {
	Column string          `json:"column"`
	Op     string          `json:"op"`
	Value  json.RawMessage `json:"value"`
}

// joinClause mirrors QueryMe JoinClause: on is the [local, foreign] pair,
// select is the joined-table projection (empty list = filter-only join).
type joinClause struct {
	Table  string    `json:"table"`
	On     [2]string `json:"on"`
	Select []string  `json:"select"`
}

// orderClause mirrors QueryMe OrderClause.
type orderClause struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

// registerDBTranche adds the six core.db.* descriptor builders to the
// registry (ADR 007 §3.1). Called from NewComputeRegistry alongside the
// pure tranche — same seam, same total-pure contract.
func (r *ComputeRegistry) registerDBTranche() {
	r.fns["core.db.from@1"] = dbFromFn
	r.fns["core.db.where@1"] = dbWhereFn
	r.fns["core.db.join@1"] = dbJoinFn
	r.fns["core.db.select@1"] = dbSelectFn
	r.fns["core.db.order@1"] = dbOrderFn
	r.fns["core.db.limit@1"] = dbLimitFn
}

// emptyDescriptor is the base shape `from` emits and the normaliser
// substitutes for a malformed/absent plan: a descriptor with an empty
// table and the four list clauses initialised (matching QueryMe's
// default_factory=list), no limit/offset.
func emptyDescriptor() queryDescriptor {
	return queryDescriptor{
		Where:  []whereClause{},
		Joins:  []joinClause{},
		Select: []string{},
		Order:  []orderClause{},
	}
}

// pullPlan decodes the `plan` input into a queryDescriptor. Per the
// totality rule, ANY decode failure or absence yields the empty base
// shape — never an error. A plan that decodes but lacks a clause list
// (e.g. a partial object) has its nil slices re-initialised to empty so
// the output keeps QueryMe's always-array wire shape.
func pullPlan(inputs map[string]json.RawMessage) queryDescriptor {
	raw, ok := inputs["plan"]
	if !ok {
		return emptyDescriptor()
	}
	var d queryDescriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return emptyDescriptor()
	}
	if d.Where == nil {
		d.Where = []whereClause{}
	}
	if d.Joins == nil {
		d.Joins = []joinClause{}
	}
	if d.Select == nil {
		d.Select = []string{}
	}
	if d.Order == nil {
		d.Order = []orderClause{}
	}
	return d
}

// configStrings reads a JSON-array-of-strings config key (join.select,
// select.columns). Anything that is not a clean string array — missing,
// null, a non-array, or an array with a non-string element — yields an
// empty slice (totality; the server validator rejects bad shapes).
func configStrings(config map[string]json.RawMessage, key string) []string {
	raw, ok := config[key]
	if !ok {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// dbFromFn — core.db.from@1. Config `table`. Emits the base descriptor
// `{table, where:[], joins:[], select:[], order:[]}` (ADR 007 §3.1).
// No `plan` input: from is the chain head.
func dbFromFn(_, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := emptyDescriptor()
	d.Table = configStr(config, "table")
	return json.Marshal(d)
}

// dbWhereFn — core.db.where@1. Input `plan`, optional input `value`;
// config `column`, `op` (default "="). Appends {column, op, value} to
// where. For op "IS NULL", value is normalised to null (matching
// QueryMe WhereClause._check_value_shape, so the wire shape never
// surprises the consumer). A missing value input is null (Python None).
func dbWhereFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := pullPlan(inputs)
	op := configStr(config, "op")
	if op == "" {
		op = "=" // seeded default
	}
	value := json.RawMessage(`null`)
	if op != "IS NULL" {
		if raw, ok := inputs["value"]; ok {
			value = raw
		}
	}
	d.Where = append(d.Where, whereClause{
		Column: configStr(config, "column"),
		Op:     op,
		Value:  value,
	})
	return json.Marshal(d)
}

// dbJoinFn — core.db.join@1. Input `plan`; config `table`,
// `local_column`, `foreign_column`, `select`. Appends
// {table, on:[local,foreign], select} to joins (ADR 007 §3.1, §3.4
// single-datasource — cross-service joins fail the server validator,
// not Orion). `select` defaults to an empty list (filter-only join).
func dbJoinFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := pullPlan(inputs)
	d.Joins = append(d.Joins, joinClause{
		Table:  configStr(config, "table"),
		On:     [2]string{configStr(config, "local_column"), configStr(config, "foreign_column")},
		Select: configStrings(config, "select"),
	})
	return json.Marshal(d)
}

// dbSelectFn — core.db.select@1. Input `plan`; config `columns`. SETS
// (overwrites) select to the FROM-table projection. Joined-table columns
// project via each join's own select (ADR 007 §3.1).
func dbSelectFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := pullPlan(inputs)
	d.Select = configStrings(config, "columns")
	return json.Marshal(d)
}

// dbOrderFn — core.db.order@1. Input `plan`; config `column`,
// `direction` (default "asc"). Appends {column, direction} to order;
// stacking is the tie-break order (ADR 007 §3.1).
func dbOrderFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := pullPlan(inputs)
	dir := configStr(config, "direction")
	if dir == "" {
		dir = "asc" // seeded default
	}
	d.Order = append(d.Order, orderClause{
		Column:    configStr(config, "column"),
		Direction: dir,
	})
	return json.Marshal(d)
}

// dbLimitFn — core.db.limit@1. Input `plan`, optional input `n`; config
// `n` (default 100). Sets limit; the `n` INPUT overrides the config
// default when wired (ADR 007 §3.1). The value is read through pyNum
// (the shared numeric coercion) and truncated to an int — QueryMe's
// limit is `int | None, ge=0`; the server validator rejects a negative.
func dbLimitFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	d := pullPlan(inputs)
	// Config default first (seeded 100), then the input n overrides if wired.
	n := 100
	if raw, ok := config["n"]; ok {
		n = int(pyNumRaw(raw, 100))
	}
	if raw, ok := inputs["n"]; ok {
		n = int(pyNumRaw(raw, float64(n)))
	}
	d.Limit = &n
	return json.Marshal(d)
}
