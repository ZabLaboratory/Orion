package datasidecar

import (
	"encoding/json"
	"fmt"
)

// queryDescriptor mirrors queryme.QueryDescriptor (extra="forbid"). The
// canonical shape: from → (where|join)* → select → (order|limit)?.
// Decoded from the verbatim _query request body Orion's DBQueryClient POSTs.
type queryDescriptor struct {
	Table  string        `json:"table"`
	Where  []whereClause `json:"where"`
	Joins  []joinClause  `json:"joins"`
	Select []string      `json:"select"`
	Order  []orderClause `json:"order"`
	Limit  *int          `json:"limit"`
	Offset *int          `json:"offset"`
}

type whereClause struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  any    `json:"value"`
}

type joinClause struct {
	Table  string    `json:"table"`
	On     [2]string `json:"on"`
	Select []string  `json:"select"`
}

type orderClause struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

// operators is queryme's closed WHERE operator set (descriptor.py).
var operators = map[string]bool{
	"=": true, "!=": true, ">": true, "<": true, ">=": true, "<=": true,
	"IN": true, "LIKE": true, "IS NULL": true,
}

// decodeDescriptor parses the request body with extra="forbid" semantics
// (queryme rejects unknown keys) and applies the WhereClause value-shape
// normalisation queryme's model_validator does (IS NULL → value nil; IN
// requires list; LIKE requires string). A shape error becomes a 422-class
// validation failure, surfaced exactly like FastAPI/pydantic would: a 400
// detail.issues envelope (the antenna returns 422 for pydantic body errors,
// but Orion treats any non-200 identically — the contract §A.4 only locks
// the 400 issues envelope and the 200 path).
func decodeDescriptor(body []byte) (*queryDescriptor, error) {
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	var d queryDescriptor
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("invalid query descriptor: %w", err)
	}
	if d.Table == "" {
		return nil, fmt.Errorf("descriptor.table is required")
	}
	for i := range d.Where {
		w := &d.Where[i]
		if !operators[w.Op] {
			return nil, fmt.Errorf("where[%d].op %q is not a permitted operator", i, w.Op)
		}
		switch w.Op {
		case "IS NULL":
			w.Value = nil
		case "IN":
			if _, ok := w.Value.([]any); !ok {
				return nil, fmt.Errorf("where[%d]: IN requires a list value", i)
			}
		case "LIKE":
			if _, ok := w.Value.(string); !ok {
				return nil, fmt.Errorf("where[%d]: LIKE requires a string value", i)
			}
		}
	}
	for i := range d.Order {
		if d.Order[i].Direction == "" {
			d.Order[i].Direction = "asc"
		}
		if d.Order[i].Direction != "asc" && d.Order[i].Direction != "desc" {
			return nil, fmt.Errorf("order[%d].direction %q must be asc|desc", i, d.Order[i].Direction)
		}
	}
	return &d, nil
}

// validationIssue mirrors queryme.validator.ValidationIssue (the 400
// detail.issues element shape: code/message/path).
type validationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// validate reproduces queryme.validate_against_schema verbatim: it walks
// every table/column reference against the catalog and emits the SAME
// structured issues (same codes, same path grammar). Type compatibility is
// delegated to the compiler/DB, exactly as on the antenna.
func (c catalog) validate(d *queryDescriptor) []validationIssue {
	var issues []validationIssue

	base, ok := c.table(d.Table)
	if !ok {
		return []validationIssue{{
			Code:    "unknown_table",
			Message: fmt.Sprintf("Table %s is not exposed by service %s", pyrepr(d.Table), pyrepr(c.Service)),
			Path:    "table",
		}}
	}

	known := map[string]table{base.Name: base}

	for idx, j := range d.Joins {
		joined, ok := c.table(j.Table)
		if !ok {
			issues = append(issues, validationIssue{
				Code:    "unknown_join_table",
				Message: fmt.Sprintf("Joined table %s is not exposed", pyrepr(j.Table)),
				Path:    fmt.Sprintf("joins[%d].table", idx),
			})
			continue
		}
		local, foreign := j.On[0], j.On[1]
		if !columnResolves(local, known) {
			issues = append(issues, validationIssue{
				Code:    "unknown_join_column",
				Message: fmt.Sprintf("Local join column %s not found on FROM/joined tables", pyrepr(local)),
				Path:    fmt.Sprintf("joins[%d].on[0]", idx),
			})
		}
		if _, ok := joined.col(foreign); !ok {
			issues = append(issues, validationIssue{
				Code:    "unknown_join_column",
				Message: fmt.Sprintf("Foreign join column %s not on table %s", pyrepr(foreign), pyrepr(joined.Name)),
				Path:    fmt.Sprintf("joins[%d].on[1]", idx),
			})
		}
		for jIdx, col := range j.Select {
			if _, ok := joined.col(col); !ok {
				issues = append(issues, validationIssue{
					Code:    "unknown_select_column",
					Message: fmt.Sprintf("Selected column %s not on joined table %s", pyrepr(col), pyrepr(joined.Name)),
					Path:    fmt.Sprintf("joins[%d].select[%d]", idx, jIdx),
				})
			}
		}
		known[joined.Name] = joined
	}

	for idx, w := range d.Where {
		if !columnResolves(w.Column, known) {
			issues = append(issues, validationIssue{
				Code:    "unknown_column",
				Message: fmt.Sprintf("WHERE column %s not found on FROM/joined tables", pyrepr(w.Column)),
				Path:    fmt.Sprintf("where[%d].column", idx),
			})
		}
	}

	anyJoinSelect := false
	for _, j := range d.Joins {
		if len(j.Select) > 0 {
			anyJoinSelect = true
			break
		}
	}
	if len(d.Select) == 0 && !anyJoinSelect {
		issues = append(issues, validationIssue{
			Code:    "empty_select",
			Message: "Query selects no column — at least one column on the FROM table or a join is required",
			Path:    "select",
		})
	}
	for idx, col := range d.Select {
		if _, ok := base.col(col); !ok {
			issues = append(issues, validationIssue{
				Code:    "unknown_select_column",
				Message: fmt.Sprintf("Selected column %s not on FROM table %s", pyrepr(col), pyrepr(base.Name)),
				Path:    fmt.Sprintf("select[%d]", idx),
			})
		}
	}

	for idx, o := range d.Order {
		if !columnResolves(o.Column, known) {
			issues = append(issues, validationIssue{
				Code:    "unknown_order_column",
				Message: fmt.Sprintf("ORDER BY column %s not found on FROM/joined tables", pyrepr(o.Column)),
				Path:    fmt.Sprintf("order[%d].column", idx),
			})
		}
	}

	return issues
}

// columnResolves mirrors queryme._column_resolves: "col" matches any known
// table, "table.col" matches that specific table.
func columnResolves(ref string, known map[string]table) bool {
	if t, col, ok := splitQualified(ref); ok {
		tbl, present := known[t]
		if !present {
			return false
		}
		_, has := tbl.col(col)
		return has
	}
	for _, t := range known {
		if _, ok := t.col(ref); ok {
			return true
		}
	}
	return false
}
