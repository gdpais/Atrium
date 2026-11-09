package app

import (
	"context"
	"database/sql"
	"fmt"

	"atrium/tools/patientzero/internal/logschema"
	"atrium/tools/patientzero/internal/shop"
)

// Migrate creates the business schema and the logging schema where they do
// not exist yet.
func Migrate(ctx context.Context, business, logging *sql.DB, types []logschema.LogType, reader string) error {
	for _, stmt := range shop.Schema {
		if _, err := business.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("business schema: %w", err)
		}
	}
	if reader != "" {
		if _, err := logging.ExecContext(ctx, logschema.ReaderRoleStatement(reader)); err != nil {
			return fmt.Errorf("log reader role: %w", err)
		}
	}
	for _, t := range types {
		for _, stmt := range t.Statements(reader) {
			if _, err := logging.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("logging schema, %s: %w", t.Name, err)
			}
		}
	}
	return nil
}
