package datasidecar

import (
	"bytes"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// bytesReader avoids importing bytes at every call site for json.NewDecoder.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// serialiseScalar reproduces ZabTruth/ZabRanking _serialise (internal.py:56-70)
// applied to a SQLite-driver scalar, using the column's DECLARED type as the
// authority (the antenna coerces from the Python DB type; here we coerce from
// the catalog type, which is the same source of truth).
//
// Contract §A.6 #1-3:
//   - uuid   → canonical lowercase-hex string, no braces (stored as TEXT).
//   - float  → JSON number (Numeric(4,2) score must NOT be a string).
//   - bool   → JSON true/false (stored as 0/1 INTEGER affinity).
//   - int    → JSON integer.
//   - datetime/date → ISO-8601 string (stored as TEXT already in that form).
//   - text/string → passthrough.
//   - NULL   → JSON null (any nullable column).
func serialiseScalar(typ columnType, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case typeUUID:
		return strings.ToLower(asString(v)), nil
	case typeFloat:
		return asFloat(v)
	case typeInteger:
		return asInt(v)
	case typeBoolean:
		i, err := asInt(v)
		if err != nil {
			return nil, err
		}
		return i != 0, nil
	default:
		// string, text, datetime, date, json: stored as TEXT, emitted
		// verbatim (datetime/date are stored already ISO-8601 by the seed).
		return v, nil
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int64:
		return float64(t), nil
	case int:
		return float64(t), nil
	case string:
		return strconv.ParseFloat(t, 64)
	case []byte:
		return strconv.ParseFloat(string(t), 64)
	default:
		return 0, fmt.Errorf("cannot coerce %T to float", v)
	}
}

func asInt(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	case []byte:
		return strconv.ParseInt(string(t), 10, 64)
	default:
		return 0, fmt.Errorf("cannot coerce %T to int", v)
	}
}

// scanRows executes the compiled query and serialises every row into the
// {key: scalar} map shape the antenna returns, keyed by outputKeys.
func scanRows(rows *sql.Rows, c *compiled) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		raw := make([]any, len(c.cols))
		ptrs := make([]any, len(c.cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(c.cols))
		for i, sc := range c.cols {
			val, err := serialiseScalar(sc.typ, raw[i])
			if err != nil {
				return nil, err
			}
			row[c.outKeys[i]] = val
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// pyrepr renders a string the way Python's repr() does for the validator
// messages (single quotes), so error bodies are byte-identical to the
// antenna's. Only the simple ASCII case the catalog produces is needed.
func pyrepr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}
