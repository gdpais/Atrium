// Package oslog writes the log rows the OutSystems 11 platform would write for
// a request into PatientZero's logging database, into the cycle table the
// current week's view of each log type (oslog_<TYPE>) reads, so the weekly
// rotation only has to move the views. Rows go through the view, except for
// a log type whose view joins a detail table (the error log): its rows go
// into the cycle table the view reads, and a value longer than that table
// keeps into the detail table too.
package oslog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"atrium/tools/patientzero/internal/logschema"
)

// Row is one log row: the log type (the fixture's name, such as screen) and
// its column values. Columns are the view's own names; a nil value is NULL.
type Row struct {
	Type    string
	Columns map[string]any
}

// Writer stores the log rows of one request.
type Writer interface {
	Write(ctx context.Context, rows []Row) error
}

// Schema is the logging schema by log type.
type Schema map[string]logschema.LogType

// NewSchema indexes the log types by name.
func NewSchema(types []logschema.LogType) Schema {
	s := Schema{}
	for _, t := range types {
		s[t.Name] = t
	}
	return s
}

// Check reports the first row that names a log type or column the schema
// does not have. The logging schema follows the collector's fixtures, so a
// renamed column shows up here instead of as a failed insert.
func (s Schema) Check(rows []Row) error {
	for _, r := range rows {
		t, ok := s[r.Type]
		if !ok {
			return fmt.Errorf("log type %s is not in the logging schema", r.Type)
		}
		for c := range r.Columns {
			if !t.HasColumn(c) {
				return fmt.Errorf("log type %s: view dbo.%s has no column %s", r.Type, t.View, c)
			}
		}
	}
	return nil
}

// SQLWriter inserts rows into the logging database.
type SQLWriter struct {
	DB     *sql.DB
	Schema Schema
}

// Write inserts the rows in one transaction: the request's rows across log
// types are committed together, as one unit the collector sees or does not.
func (w *SQLWriter) Write(ctx context.Context, rows []Row) error {
	if err := w.Schema.Check(rows); err != nil {
		return err
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	cycles := map[string]int{}
	for _, r := range rows {
		t := w.Schema[r.Type]
		c, ok := cycles[r.Type]
		if !ok && t.Detail != nil {
			if c, err = currentCycle(ctx, tx, t); err != nil {
				return err
			}
			cycles[r.Type] = c
		}
		stmts, err := w.inserts(r, c)
		if err != nil {
			return err
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.query, st.args...); err != nil {
				return fmt.Errorf("insert into %s: %w", st.into, err)
			}
		}
	}
	return tx.Commit()
}

// currentCycle is the cycle whose table the log type's current view reads,
// looked up on every write: whatever moved the views, the rows follow them.
func currentCycle(ctx context.Context, tx *sql.Tx, t logschema.LogType) (int, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT referenced_entity_name FROM sys.sql_expression_dependencies WHERE referencing_id = OBJECT_ID(@p1)", "dbo."+t.View)
	if err != nil {
		return 0, fmt.Errorf("tables behind dbo.%s: %w", t.View, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		names = append(names, name)
		for c := 0; c < logschema.Cycles; c++ {
			if strings.EqualFold(name, t.Table(c)) {
				return c, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("view dbo.%s reads none of %s to %s: %v", t.View, t.Table(0), t.Table(logschema.Cycles-1), names)
}

// insert is one INSERT statement and the object it inserts into.
type insert struct {
	into  string
	query string
	args  []any
}

// inserts are the statements that store the row: one through the current
// view, or, for a log type with a detail table, one into cycle table c and
// one into its detail table when the overridden value is longer than the
// cycle table keeps. The cycle table then keeps the value's start.
func (w *SQLWriter) inserts(r Row, c int) ([]insert, error) {
	t := w.Schema[r.Type]
	cols := make(map[string]any, len(r.Columns))
	for name, v := range r.Columns {
		cols[t.CycleColumn(name)] = v
	}
	if t.Detail == nil {
		return []insert{insertInto(t.View, cols)}, nil
	}
	d := *t.Detail
	full, ok := cols[d.Fallback].(string)
	if !ok || utf8.RuneCountInString(full) <= d.Limit {
		return []insert{insertInto(t.Table(c), cols)}, nil
	}
	key := lookup(r.Columns, d.On)
	if key == nil {
		return nil, fmt.Errorf("log type %s: a %s longer than %d characters goes to dbo.%s, which needs %s", r.Type, d.Column, d.Limit, d.Table(c), d.On)
	}
	cols[d.Fallback] = string([]rune(full)[:d.Limit])
	return []insert{
		insertInto(t.Table(c), cols),
		insertInto(d.Table(c), map[string]any{d.Key: key, d.Value: full}),
	}, nil
}

// lookup is the row's value of a column, named case-insensitively.
func lookup(cols map[string]any, name string) any {
	for c, v := range cols {
		if strings.EqualFold(c, name) {
			return v
		}
	}
	return nil
}

func insertInto(object string, values map[string]any) insert {
	cols := make([]string, 0, len(values))
	for c := range values {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	names := make([]string, len(cols))
	params := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		names[i] = "[" + c + "]"
		params[i] = fmt.Sprintf("@p%d", i+1)
		args[i] = value(values[c])
	}
	return insert{into: object, query: fmt.Sprintf("INSERT INTO dbo.%s (%s) VALUES (%s);", object,
		strings.Join(names, ", "), strings.Join(params, ", ")), args: args}
}

// value binds times in UTC, the zone the logging database stores Instant
// in (the collector's default log_timezone), cut to the millisecond its
// datetime2(3) columns keep (Stored), so the Ledger counts each row in the
// second the database has it in.
func value(v any) any {
	if t, ok := v.(time.Time); ok {
		return Stored(t)
	}
	return v
}

// Rotate moves every log type's views one cycle on, as the platform's
// weekly rotation does every Friday at 23:45: the previous view to the cycle
// the current view read, the current view to the next cycle table. Only the
// views move, all in one transaction; the rows stay where they are, and new
// rows follow the current view. It returns the cycle each log type's
// current view reads now.
func (w *SQLWriter) Rotate(ctx context.Context) (map[string]int, error) {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	names := make([]string, 0, len(w.Schema))
	for name := range w.Schema {
		names = append(names, name)
	}
	sort.Strings(names)
	now := map[string]int{}
	for _, name := range names {
		t := w.Schema[name]
		c, err := currentCycle(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		next := (c + 1) % logschema.Cycles
		for _, stmt := range t.RotateStatements(next, c) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return nil, fmt.Errorf("rotating %s: %w", name, err)
			}
		}
		now[name] = next
	}
	return now, tx.Commit()
}
