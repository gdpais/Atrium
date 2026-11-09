package scale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	mssql "github.com/microsoft/go-mssqldb"

	"atrium/tools/patientzero/internal/logschema"
)

// DatabasePrefix starts the name of every database this package creates or
// drops, so it can never drop PatientZero's own logging database or a real
// one.
const DatabasePrefix = "patientzero_scale"

func checkName(name string) error {
	if !strings.HasPrefix(name, DatabasePrefix) || strings.ContainsAny(name, "]'\" ;") {
		return fmt.Errorf("database %q: the name must start with %s and hold no quotes, brackets, spaces or semicolons", name, DatabasePrefix)
	}
	return nil
}

// Open returns a pool for database on server, a sqlserver:// URL whose
// own database, if any, is replaced.
func Open(server, database string) (*sql.DB, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "sqlserver" {
		return nil, errors.New("the SQL Server URL is not a sqlserver:// URL")
	}
	q := u.Query()
	q.Set("database", database)
	q.Set("app name", "PatientZero scale")
	u.RawQuery = q.Encode()
	return sql.Open("sqlserver", u.String())
}

// Drop drops the database, if it exists. master is a connection to any
// other database of the server.
func Drop(ctx context.Context, master *sql.DB, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	_, err := master.ExecContext(ctx, fmt.Sprintf(`IF DB_ID(N'%[1]s') IS NOT NULL
BEGIN
    ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE [%[1]s];
END`, name))
	return err
}

// Create drops and creates the database, in the simple recovery model (a
// test database: bulk loads stay minimally logged).
func Create(ctx context.Context, master *sql.DB, name string) error {
	if err := Drop(ctx, master, name); err != nil {
		return err
	}
	for _, stmt := range []string{
		fmt.Sprintf("CREATE DATABASE [%s];", name),
		fmt.Sprintf("ALTER DATABASE [%s] SET RECOVERY SIMPLE;", name),
	} {
		if _, err := master.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// Schema creates the logging schema in db (ten cycle tables per log type,
// the error detail tables, the two views each) and the reader role with
// SELECT on the views. A readerLogin, such as the Atrium collector's,
// becomes a member of the role, so a collector can read the database as it
// reads PatientZero's.
func Schema(ctx context.Context, db *sql.DB, types []logschema.LogType, reader, readerLogin string) error {
	var stmts []string
	if reader != "" {
		stmts = append(stmts, logschema.ReaderRoleStatement(reader))
	}
	for _, t := range types {
		stmts = append(stmts, t.Statements(reader)...)
	}
	if reader != "" && readerLogin != "" {
		user := "[" + strings.ReplaceAll(readerLogin, "]", "]]") + "]"
		stmts = append(stmts,
			fmt.Sprintf("IF DATABASE_PRINCIPAL_ID(N'%s') IS NULL CREATE USER %s FOR LOGIN %s;", strings.ReplaceAll(readerLogin, "'", "''"), user, user),
			fmt.Sprintf("ALTER ROLE [%s] ADD MEMBER %s;", strings.ReplaceAll(reader, "]", "]]"), user))
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("logging schema: %w", err)
		}
	}
	return nil
}

// BulkSink bulk-copies rows into the cycle tables: one bulk load per table
// between two flushes. A stack trace longer than the error cycle table's
// Stack is cut there and stored whole in the cycle's detail table, as
// PatientZero's writer does.
type BulkSink struct {
	DB    *sql.DB
	Types map[string]logschema.LogType
	Ctx   context.Context
	// Rows counts the rows copied per table.
	Rows map[string]int

	open  map[string]*bulk
	names map[string]map[string]string // table -> upper-case column -> its name
}

type bulk struct {
	conn  *sql.Conn
	tx    *sql.Tx
	stmt  *sql.Stmt
	count int
}

// NewBulkSink returns a sink into db for the given log types.
func NewBulkSink(ctx context.Context, db *sql.DB, types []logschema.LogType) *BulkSink {
	byName := map[string]logschema.LogType{}
	for _, t := range types {
		byName[t.Name] = t
	}
	return &BulkSink{DB: db, Types: byName, Ctx: ctx, Rows: map[string]int{}, open: map[string]*bulk{}, names: map[string]map[string]string{}}
}

// Row implements Sink.
func (s *BulkSink) Row(logType string, cycle int, values []any) error {
	t, ok := s.Types[logType]
	if !ok {
		return fmt.Errorf("log type %s is not in the logging schema", logType)
	}
	cols := Columns(logType)
	if d := t.Detail; d != nil {
		i := indexFold(cols, d.Fallback)
		if trace, ok := valueAt(values, i).(string); ok && utf8.RuneCountInString(trace) > d.Limit {
			key := valueAt(values, indexFold(cols, d.On))
			values = append([]any(nil), values...)
			values[i] = string([]rune(trace)[:d.Limit])
			if err := s.copy(d.Table(cycle), []string{d.Key, d.Value}, []any{key, trace}); err != nil {
				return err
			}
		}
	}
	return s.copy(t.Table(cycle), cols, values)
}

func valueAt(values []any, i int) any {
	if i < 0 || i >= len(values) {
		return nil
	}
	return values[i]
}

func indexFold(cols []string, name string) int {
	for i, c := range cols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

func (s *BulkSink) copy(table string, cols []string, values []any) error {
	b := s.open[table]
	if b == nil {
		names, err := s.columnNames(table, cols)
		if err != nil {
			return err
		}
		b = &bulk{}
		if b.conn, err = s.DB.Conn(s.Ctx); err != nil {
			return err
		}
		if b.tx, err = b.conn.BeginTx(s.Ctx, nil); err != nil {
			b.conn.Close()
			return err
		}
		if b.stmt, err = b.tx.PrepareContext(s.Ctx, mssql.CopyIn("dbo."+table, mssql.BulkOptions{Tablock: true}, names...)); err != nil {
			b.tx.Rollback() //nolint:errcheck // already failing
			b.conn.Close()
			return fmt.Errorf("bulk copy into %s: %w", table, err)
		}
		s.open[table] = b
	}
	if _, err := b.stmt.ExecContext(s.Ctx, values...); err != nil {
		return fmt.Errorf("bulk copy into %s: %w", table, err)
	}
	b.count++
	return nil
}

// columnNames maps cols to the table's own column names: bulk copy matches
// names exactly, the fixtures' spelling varies (Executed_by, REQUEST_KEY).
func (s *BulkSink) columnNames(table string, cols []string) ([]string, error) {
	names := s.names[table]
	if names == nil {
		rows, err := s.DB.QueryContext(s.Ctx, "SELECT name FROM sys.columns WHERE object_id = OBJECT_ID(@p1)", "dbo."+table)
		if err != nil {
			return nil, err
		}
		names = map[string]string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			names[strings.ToUpper(name)] = name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		s.names[table] = names
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		name, ok := names[strings.ToUpper(c)]
		if !ok {
			return nil, fmt.Errorf("table dbo.%s has no column %s", table, c)
		}
		out[i] = name
	}
	return out, nil
}

// Flush implements Sink: it ends every open bulk load and commits it.
func (s *BulkSink) Flush() error {
	var first error
	for table, b := range s.open {
		_, err := b.stmt.ExecContext(s.Ctx)
		b.stmt.Close()
		if err == nil {
			err = b.tx.Commit()
		} else {
			b.tx.Rollback() //nolint:errcheck // already failing
		}
		b.conn.Close()
		if err != nil && first == nil {
			first = fmt.Errorf("bulk copy into %s: %w", table, err)
		}
		s.Rows[table] += b.count
		delete(s.open, table)
	}
	return first
}
