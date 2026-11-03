package migrations

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// golang-migrate needs one up and one down file per version, and applies
// them in numeric order; sqlc reads the up files in lexical order. Every
// version is zero-padded to four digits so the two orders agree.
func TestEmbeddedMigrationsArePairedAndOrdered(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	ups := map[string]string{}
	downs := map[string]string{}
	for _, entry := range entries {
		m := fileName.FindStringSubmatch(entry.Name())
		if m == nil {
			t.Fatalf("%s is not named NNNN_name.up.sql or NNNN_name.down.sql", entry.Name())
		}
		if len(m[1]) != 4 {
			t.Errorf("%s: version %s is not four digits", entry.Name(), m[1])
		}
		name := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".up.sql"), ".down.sql")
		byVersion := ups
		if m[2] == "down" {
			byVersion = downs
		}
		if other, ok := byVersion[m[1]]; ok {
			t.Errorf("version %s has two %s migrations: %s and %s", m[1], m[2], other, name)
		}
		byVersion[m[1]] = name
	}
	for version, name := range ups {
		if downs[version] != name {
			t.Errorf("%s.up.sql has no matching %s.down.sql", name, name)
		}
	}
	for version, name := range downs {
		if ups[version] != name {
			t.Errorf("%s.down.sql has no matching %s.up.sql", name, name)
		}
	}
}

// golang-migrate applies only versions above the database's own, so a
// migration numbered below one already applied is skipped for good, with
// no error. Versions are therefore consecutive: a gap means a number was
// taken out of turn, and the migration that fills it later would be
// skipped on every database already past it. Renumber to the next free
// version before merging.
func TestEmbeddedMigrationVersionsAreConsecutive(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var versions []int
	for _, entry := range entries {
		m := fileName.FindStringSubmatch(entry.Name())
		if m == nil || m[2] != "up" {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	slices.Sort(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("up migration versions %v: want 1 to %d with no gap; renumber %04d to %04d", versions, len(versions), v, i+1)
		}
	}
}

func TestLatest(t *testing.T) {
	latest, err := Latest(FS)
	if err != nil {
		t.Fatal(err)
	}
	if latest < 1 {
		t.Fatalf("Latest(FS) = %d, want at least 1", latest)
	}

	for _, tc := range []struct {
		name  string
		files []string
		want  uint64
		err   string
	}{
		{"highest up version", []string{"0001_a.up.sql", "0001_a.down.sql", "0010_b.up.sql", "0010_b.down.sql", "0002_c.up.sql"}, 10, ""},
		{"a down file alone does not count", []string{"0001_a.up.sql", "0003_b.down.sql"}, 1, ""},
		{"misnamed file", []string{"0001_a.up.sql", "0002-b.up.sql"}, 0, "0002-b.up.sql"},
		{"no direction", []string{"0001_a.sql"}, 0, "0001_a.sql"},
		{"no migrations", nil, 0, "no up migrations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for _, name := range tc.files {
				fsys[name] = &fstest.MapFile{Data: []byte("select 1;")}
			}
			got, err := Latest(fsys)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Latest = %d, %v; want an error naming %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Latest = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}
