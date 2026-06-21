package datasidecar

import (
	"bytes"
	"fmt"
	"strings"
)

// outputKeys reproduces the row-key derivation in
// ZabTruth/ZabRanking routes/internal.py:104-117 (identical both services):
// keys = descriptor.select, then per join in order join.select; on a name
// collision the WHOLE list is rebuilt qualifying the joined duplicate as
// "<join.table>.<col>" (the FROM column keeps its bare name).
func outputKeys(d *queryDescriptor) []string {
	keys := append([]string{}, d.Select...)
	for _, j := range d.Joins {
		keys = append(keys, j.Select...)
	}
	// collision?
	set := map[string]bool{}
	dup := false
	for _, k := range keys {
		if set[k] {
			dup = true
			break
		}
		set[k] = true
	}
	if !dup {
		return keys
	}
	seen := map[string]bool{}
	keys = keys[:0]
	for _, col := range d.Select {
		keys = append(keys, col)
		seen[col] = true
	}
	for _, j := range d.Joins {
		for _, col := range j.Select {
			if seen[col] {
				keys = append(keys, j.Table+"."+col)
			} else {
				keys = append(keys, col)
			}
			seen[col] = true
		}
	}
	return keys
}

// selectedColumn is a (table, column, type) tuple the compiler projects, in
// descriptor order. Its declared type drives the scalar coercion on the way
// out (the SQLite driver returns loosely-typed scalars; the type tells us
// whether to emit a JSON float, a bool, etc — contract §A.6 #1-3).
type selectedColumn struct {
	table  string
	column string
	typ    columnType
}

// compiled is the SQL + bind args + projection + output keys for one query.
type compiled struct {
	sql     string
	args    []any
	cols    []selectedColumn
	outKeys []string
}

// compile turns a validated descriptor into a single SQLite SELECT that is
// SEMANTICALLY identical to the SQLAlchemy Select queryme.compile_query
// produces against Postgres. The four parity gaps (contract §A.6):
//
//  1. scalar coercion: handled at serialise time from selectedColumn.typ.
//  2. LIKE: queryme emits plain SQL LIKE → Postgres LIKE is case-SENSITIVE.
//     SQLite LIKE is case-insensitive for ASCII unless case_sensitive_like
//     is ON — the sidecar sets PRAGMA case_sensitive_like = ON (see seed.go)
//     so the wire matches. No per-column ILIKE exists in queryme.
//  3. Numeric typing: score column is typed float in the catalog → emitted
//     as a JSON number, never a string.
//  4. NULL ordering: Postgres sorts NULLs LAST on asc / FIRST on desc;
//     SQLite is the opposite. For every ORDER over a NULLABLE column the
//     compiler prepends "<col> IS NULL [DESC]" so the ordering matches
//     Postgres exactly. (NOT NULL columns need no fix — no NULLs to order.)
func (c catalog) compile(d *queryDescriptor) (*compiled, error) {
	base, ok := c.table(d.Table)
	if !ok {
		return nil, fmt.Errorf("compile: unknown table %q", d.Table)
	}
	known := map[string]table{base.Name: base}
	for _, j := range d.Joins {
		jt, ok := c.table(j.Table)
		if !ok {
			return nil, fmt.Errorf("compile: unknown join table %q", j.Table)
		}
		known[jt.Name] = jt
	}

	// SELECT list, descriptor order: FROM cols first, then each join's.
	var cols []selectedColumn
	for _, col := range d.Select {
		cdef, _ := base.col(col)
		cols = append(cols, selectedColumn{table: base.Name, column: col, typ: cdef.Type})
	}
	for _, j := range d.Joins {
		jt := known[j.Table]
		for _, col := range j.Select {
			cdef, _ := jt.col(col)
			cols = append(cols, selectedColumn{table: j.Table, column: col, typ: cdef.Type})
		}
	}

	var b bytes.Buffer
	b.WriteString("SELECT ")
	for i, sc := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s.%s", qident(sc.table), qident(sc.column))
	}
	fmt.Fprintf(&b, " FROM %s", qident(base.Name))

	// JOINs (INNER) — local column resolves against accumulated tables.
	cumul := map[string]table{base.Name: base}
	for _, j := range d.Joins {
		jt := known[j.Table]
		localTbl, localCol := resolveColumn(j.On[0], cumul)
		fmt.Fprintf(&b, " JOIN %s ON %s.%s = %s.%s",
			qident(jt.Name),
			qident(localTbl), qident(localCol),
			qident(jt.Name), qident(j.On[1]))
		cumul[jt.Name] = jt
	}

	// WHERE — AND-joined.
	var args []any
	if len(d.Where) > 0 {
		b.WriteString(" WHERE ")
		for i, w := range d.Where {
			if i > 0 {
				b.WriteString(" AND ")
			}
			tbl, col := resolveColumn(w.Column, known)
			ref := qident(tbl) + "." + qident(col)
			switch w.Op {
			case "IS NULL":
				fmt.Fprintf(&b, "%s IS NULL", ref)
			case "IN":
				list, _ := w.Value.([]any)
				if len(list) == 0 {
					// SQLAlchemy col.in_([]) compiles to a false predicate
					// (1 != 1) — empty IN never matches. Match that.
					b.WriteString("0 = 1")
				} else {
					b.WriteString(ref)
					b.WriteString(" IN (")
					for k, v := range list {
						if k > 0 {
							b.WriteString(", ")
						}
						b.WriteString("?")
						args = append(args, bindValue(v))
					}
					b.WriteString(")")
				}
			case "LIKE":
				fmt.Fprintf(&b, "%s LIKE ?", ref)
				args = append(args, w.Value)
			default: // = != > < >= <=
				fmt.Fprintf(&b, "%s %s ?", ref, w.Op)
				args = append(args, bindValue(w.Value))
			}
		}
	}

	// ORDER BY — Postgres NULL ordering parity on nullable columns.
	if len(d.Order) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range d.Order {
			if i > 0 {
				b.WriteString(", ")
			}
			tbl, col := resolveColumn(o.Column, known)
			cdef, _ := known[tbl].col(col)
			ref := qident(tbl) + "." + qident(col)
			if cdef.Nullable {
				// asc: Postgres NULLs LAST → "col IS NULL ASC" puts non-null
				//      (IS NULL = 0) before null (= 1).
				// desc: Postgres NULLs FIRST → "col IS NULL DESC" puts null first.
				if o.Direction == "asc" {
					fmt.Fprintf(&b, "%s IS NULL, ", ref)
				} else {
					fmt.Fprintf(&b, "%s IS NULL DESC, ", ref)
				}
			}
			if o.Direction == "asc" {
				fmt.Fprintf(&b, "%s ASC", ref)
			} else {
				fmt.Fprintf(&b, "%s DESC", ref)
			}
		}
	}

	if d.Limit != nil {
		fmt.Fprintf(&b, " LIMIT %d", *d.Limit)
	}
	if d.Offset != nil {
		// SQLite requires LIMIT before OFFSET; queryme emits both
		// independently. Postgres allows OFFSET without LIMIT; SQLite does
		// not — supply the no-op LIMIT -1 when offset is set without limit.
		if d.Limit == nil {
			b.WriteString(" LIMIT -1")
		}
		fmt.Fprintf(&b, " OFFSET %d", *d.Offset)
	}

	return &compiled{
		sql:     b.String(),
		args:    args,
		cols:    cols,
		outKeys: outputKeys(d),
	}, nil
}

// resolveColumn mirrors queryme._resolve_column: "table.col" → that table;
// "col" → first known table that has it. The validator already proved it
// resolves, so a miss is an internal contract violation.
func resolveColumn(ref string, known map[string]table) (string, string) {
	if t, col, ok := splitQualified(ref); ok {
		return t, col
	}
	for name, t := range known {
		if _, ok := t.col(ref); ok {
			return name, ref
		}
	}
	return "", ref
}

func splitQualified(ref string) (table, col string, ok bool) {
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		return ref[:i], ref[i+1:], true
	}
	return "", ref, false
}

// qident quotes a SQLite identifier (catalog-sourced, but defensive).
func qident(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// bindValue normalises JSON-decoded scalars for SQLite binding. JSON numbers
// arrive as float64; integers passed as float64 bind fine to SQLite. Bools
// bind as 0/1 (matching the stored boolean affinity). Everything else
// (string, nil) passes through.
func bindValue(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return v
	}
}
