// Package migrations embeds Atrium's PostgreSQL migrations, the
// NNNN_name.up.sql and NNNN_name.down.sql files beside it, so the API binary
// carries the schema it was built for and `api migrate up` applies it with
// golang-migrate (ADR 0013).
//
// The files are golang-migrate's format: a zero-padded version, a name, and
// the direction. sqlc reads the same directory for the schema, parsing the
// .up.sql files only.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

// FS holds every migration file, at its root.
//
//go:embed *.sql
var FS embed.FS

// fileName matches a migration file: version, name, direction.
var fileName = regexp.MustCompile(`^(\d+)_[a-z0-9_]+\.(up|down)\.sql$`)

// Latest returns the highest version among the up migrations in fsys: the
// schema version a database has once every migration is applied. It fails
// when a .sql file is not named as golang-migrate expects, so a misnamed
// file is caught by tests instead of being skipped when migrating.
func Latest(fsys fs.FS) (uint64, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, err
	}
	var latest uint64
	for _, entry := range entries {
		m := fileName.FindStringSubmatch(entry.Name())
		if m == nil {
			return 0, fmt.Errorf("migration %s: want NNNN_name.up.sql or NNNN_name.down.sql", entry.Name())
		}
		version, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if m[2] == "up" && version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, fmt.Errorf("no up migrations")
	}
	return latest, nil
}
