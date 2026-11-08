package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/store"
	"heartbeat/apps/api/internal/users"
)

// createAdminCommand names the command in the audit records it writes.
const createAdminCommand = "api users create-admin"

// createAdmin runs `api users create-admin --email ADDRESS [--name NAME]`:
// it reads the password from stdin and creates the admin and its audit
// record in one transaction (ADR 0013 §7). It checks what it was given
// before connecting, and fails on an email that belongs to a user already.
func createAdmin(ctx context.Context, args []string, logger *slog.Logger, stdin io.Reader) error {
	flags := flag.NewFlagSet("users create-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	email := flags.String("email", "", "")
	name := flags.String("name", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || *email == "" {
		return errUsage
	}
	if _, err := users.NormalizeEmail(*email); err != nil {
		return err
	}
	secret, err := readPassword(stdin)
	if err != nil {
		return err
	}
	cfg, resolver, err := loadMigrateConfig()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	conn, err := store.Connect(ctx, cfg, resolver)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	want, err := store.LatestVersion()
	if err != nil {
		return err
	}
	if err := (store.SchemaChecker{Queries: store.New(conn), Want: want}).Check(ctx); err != nil {
		return err
	}
	requestID, err := audit.NewID()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	created, err := users.CreateAdmin(ctx, conn, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: *email, DisplayName: *name, Password: secret},
		audit.CLIActor(createAdminCommand, host), requestID.String())
	if err != nil {
		return fmt.Errorf("users create-admin: %w", err)
	}
	logger.Info("users: admin created", "user_id", created.UserID, "email", created.Email,
		"audit_record_id", created.AuditRecord.ID, "request_id", created.AuditRecord.RequestID)
	return nil
}

// readPassword reads the password from stdin: its first line, which must be
// its only one. A terminal is refused, since what is typed there is echoed.
func readPassword(stdin io.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("standard input is a terminal or a device: pipe the password in, such as with " +
				`read -rs PASSWORD && printf '%s\n' "$PASSWORD" | api users create-admin --email <address>`)
		}
	}
	// Room for the longest password, a line end, and one byte to tell that
	// it is longer.
	data, err := io.ReadAll(io.LimitReader(stdin, password.MaxBytes+3))
	if err != nil {
		return "", fmt.Errorf("read the password from standard input: %w", err)
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("standard input holds more than one line: pipe in the password alone")
	}
	if err := password.Check(secret); err != nil {
		return "", err
	}
	return secret, nil
}
