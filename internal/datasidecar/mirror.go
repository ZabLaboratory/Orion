package datasidecar

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// sqliteType maps a catalog columnType to a SQLite column affinity. Affinity
// is advisory in SQLite (we coerce on the way out from the catalog type), but
// declaring it keeps the mirror readable and the stored form sane:
//   - uuid/datetime/date/string/text/json → TEXT
//   - integer/boolean → INTEGER
//   - float → REAL (Numeric(4,2) score stored as REAL → emitted JSON float)
func sqliteType(t columnType) string {
	switch t {
	case typeInteger, typeBoolean:
		return "INTEGER"
	case typeFloat:
		return "REAL"
	default:
		return "TEXT"
	}
}

// applyMirror creates the catalog's tables in the SQLite mirror and sets the
// LIKE case-sensitivity pragma to match Postgres (queryme emits plain LIKE →
// PG LIKE is case-sensitive; SQLite defaults to case-insensitive — contract
// §A.6 #2).
func applyMirror(ctx context.Context, db *sql.DB, cat catalog) error {
	if _, err := db.ExecContext(ctx, "PRAGMA case_sensitive_like = ON;"); err != nil {
		return err
	}
	for _, t := range cat.Tables {
		var b strings.Builder
		fmt.Fprintf(&b, "CREATE TABLE %s (", qident(t.Name))
		for i, c := range t.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s %s", qident(c.Name), sqliteType(c.Type))
			if c.Primary {
				b.WriteString(" PRIMARY KEY")
			}
			if !c.Nullable && !c.Primary {
				b.WriteString(" NOT NULL")
			}
		}
		b.WriteString(");")
		if _, err := db.ExecContext(ctx, b.String()); err != nil {
			return fmt.Errorf("create %s: %w", t.Name, err)
		}
	}
	return nil
}

// insertRow inserts one row described as column→value into a mirror table.
// Values are pre-coerced to SQLite storage forms (UUIDs lowercase-hex TEXT,
// datetimes ISO-8601 TEXT, booleans 0/1, scores REAL).
func insertRow(ctx context.Context, db *sql.DB, t string, row map[string]any) error {
	cols := make([]string, 0, len(row))
	ph := make([]string, 0, len(row))
	args := make([]any, 0, len(row))
	for k, v := range row {
		cols = append(cols, qident(k))
		ph = append(ph, "?")
		args = append(args, v)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		qident(t), strings.Join(cols, ", "), strings.Join(ph, ", "))
	_, err := db.ExecContext(ctx, q, args...)
	return err
}
