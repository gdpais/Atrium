package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A refused delete is a ConflictError naming what refers to the entity,
// whichever code the server reports it with: PostgreSQL 18 says
// restrict_violation for `on delete restrict`, older releases
// foreign_key_violation. Other errors stay server errors.
func TestInUse(t *testing.T) {
	for _, code := range []string{restrictViolation, foreignKeyViolation} {
		err := inUse(fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, TableName: "database_targets"}), "environment", "disable it instead")
		var conflict *ConflictError
		if !errors.As(err, &conflict) || conflict.Detail != "the environment still has database targets, so it cannot be deleted: disable it instead" {
			t.Errorf("code %s: %v, want a ConflictError naming database targets", code, err)
		}
	}
	err := inUse(&pgconn.PgError{Code: restrictViolation, TableName: "future_things"}, "application", "disable it instead")
	if !strings.Contains(err.Error(), "still has future things") {
		t.Errorf("an unlisted table: %v", err)
	}
	err = inUse(&pgconn.PgError{Code: "57014"}, "environment", "") // query_canceled
	var conflict *ConflictError
	if errors.As(err, &conflict) || !strings.HasPrefix(err.Error(), "delete the environment: ") {
		t.Errorf("another error: %v, want it wrapped, not a conflict", err)
	}
}
