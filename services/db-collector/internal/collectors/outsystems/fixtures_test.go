package outsystems

import (
	"os"
	"path/filepath"
	"testing"

	catalogoutsystems "atrium/services/db-collector/internal/probes/outsystems"
)

// fixtureDir holds the SQL Server fixtures, two files per log type: <log
// type>.sql creates the oslog tables and views its query reads, and <log
// type>.rows.sql fills them with representative rows.
const fixtureDir = "testdata/oslog"

// Every log type's query runs against SQL Server in CI (make
// test-sqlserver), so every log type needs its fixture tables and rows.
// This check runs without SQL Server, so a missing fixture fails make test
// too.
func TestEveryLogTypeHasASQLServerFixture(t *testing.T) {
	for _, logType := range catalogoutsystems.DefaultCatalog().Names() {
		for _, file := range []struct{ suffix, content string }{
			{".sql", "the CREATE TABLE and CREATE VIEW statements of the oslog objects"},
			{".rows.sql", "INSERT statements with representative rows for the oslog tables"},
		} {
			path := filepath.Join(fixtureDir, logType+file.suffix)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("log type %q has no SQL Server fixture: add services/db-collector/internal/collectors/outsystems/%s "+
					"with %s queries/%s.sql reads (%v)", logType, path, file.content, logType, err)
			}
		}
	}
}
