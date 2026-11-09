// Package users manages Atrium's local users and their roles (ADR 0013
// §7–8). [Service] serves the API's admin operations; [SetPassword] also
// serves a user changing their own password; [CreateAdmin] makes admins for
// `api users create-admin`: the first admin, since there is no default
// password, and a way back in for whoever can run a command in an API
// container.
package users

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/password"
	"atrium/apps/api/internal/store"
)

// RoleAdmin is the role an admin has; migration 0002 creates it.
const RoleAdmin = "admin"

// Limits on what a user's fields hold.
const (
	maxEmailBytes       = 254
	maxDisplayNameRunes = 200
)

// ErrEmailTaken reports an email that already belongs to a user.
var ErrEmailTaken = errors.New("a user with this email already exists")

// NewAdmin is an admin to create.
type NewAdmin struct {
	// Email is the login, compared without regard to case: it is stored in
	// lower case, and a unique index on lower(email) holds for any writer.
	Email string
	// DisplayName defaults to the email's local part.
	DisplayName string
	// Password is checked with [password.Check] and stored only as its
	// argon2id hash.
	Password string
}

// Created is the admin CreateAdmin made, and the record of it.
type Created struct {
	UserID      string
	Email       string
	AuditRecord audit.Record
}

// User is a user. Its JSON is how audit records show it: never the
// password hash, and without the times, which a change does not make.
type User struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name"`
	Status      string   `json:"status"`
	Roles       []string `json:"roles"`

	LastLoginAt          *time.Time `json:"-"`
	CreatedAt, UpdatedAt time.Time  `json:"-"`
}

// CreateAdmin creates a user with the admin role, and the audit record of
// it, in one transaction on db: either both are written or neither. It
// fails with [ErrEmailTaken] when the email belongs to a user already, and
// before hashing on an email, a name or a password that is not acceptable.
func CreateAdmin(ctx context.Context, db audit.Beginner, hasher *password.Hasher, admin NewAdmin, actor audit.Actor, requestID string) (Created, error) {
	email, err := NormalizeEmail(admin.Email)
	if err != nil {
		return Created{}, err
	}
	name, err := displayName(admin.DisplayName, email)
	if err != nil {
		return Created{}, err
	}
	if err := password.Check(admin.Password); err != nil {
		return Created{}, err
	}
	hash, err := hasher.Hash(ctx, admin.Password)
	if err != nil {
		return Created{}, err
	}

	var created Created
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := q.CreateUser(ctx, store.CreateUserParams{
			Email: email, DisplayName: name, PasswordHash: pgtype.Text{String: hash, Valid: true},
		})
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("create the user: %w", err)
		}
		_, err = q.GrantRole(ctx, store.GrantRoleParams{UserID: user.ID, RoleName: RoleAdmin})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("grant the %s role: no such role; run api migrate up", RoleAdmin)
		}
		if err != nil {
			return fmt.Errorf("grant the %s role: %w", RoleAdmin, err)
		}
		id := user.ID.String()
		record, err := audit.Write(ctx, q, audit.Change{
			RequestID:  requestID,
			Actor:      actor,
			Action:     audit.ActionCreate,
			EntityType: audit.EntityUser,
			EntityID:   id,
			After: User{
				ID: id, Email: user.Email, DisplayName: user.DisplayName, Status: user.Status,
				Roles: []string{RoleAdmin},
			},
		})
		if err != nil {
			return err
		}
		created = Created{UserID: id, Email: user.Email, AuditRecord: record}
		return nil
	})
	if err != nil {
		return Created{}, err
	}
	return created, nil
}

// NormalizeEmail returns address trimmed and in lower case, or an error when
// it is not a bare address such as admin@example.com.
func NormalizeEmail(address string) (string, error) {
	address = strings.TrimSpace(address)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Name != "" || parsed.Address != address || len(address) > maxEmailBytes {
		return "", fmt.Errorf("email %q: want an address such as admin@example.com, at most %d bytes", address, maxEmailBytes)
	}
	return strings.ToLower(address), nil
}

// displayName returns name trimmed, or the email's local part when it is
// empty.
func displayName(name, email string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxDisplayNameRunes || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("display name: want at most %d characters, without control characters", maxDisplayNameRunes)
	}
	return name, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" // unique_violation
}
