package users

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/store"
)

// Statuses of a user.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Audit actions of the changes besides creating a user.
const (
	ActionUpdate      = "update"
	ActionSetPassword = "set_password"
	ActionGrantRole   = "grant_role"
	ActionRevokeRole  = "revoke_role"
)

// Errors of the user operations; package httpapi maps them to problems.
var (
	// ErrNotFound is an ID that names no user.
	ErrNotFound = errors.New("users: no user has this ID")
	// ErrLastAdmin is a change that would leave no active admin.
	ErrLastAdmin = errors.New("the last active admin cannot be disabled or lose the admin role")
	// ErrUnknownRole is a role that does not exist.
	ErrUnknownRole = errors.New("users: no such role")
	// ErrStalePassword is a password change whose expected hash is no
	// longer the user's: the password changed meanwhile.
	ErrStalePassword = errors.New("users: the password changed meanwhile")
)

// InvalidError is a field whose value is not acceptable.
type InvalidError struct {
	Field  string
	Reason string
}

func (e *InvalidError) Error() string { return e.Field + ": " + e.Reason }

// DB is PostgreSQL: a pool, which runs queries and begins transactions.
type DB interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Sessions ends a user's sessions: *auth.Sessions.
type Sessions interface {
	DeleteUser(ctx context.Context, userID string) error
}

// Caller is who asks for a change, for its audit record.
type Caller struct {
	Actor audit.Actor
	// RequestID identifies the request that makes the change.
	RequestID string
}

// Service reads and changes users and their roles, for admins.
//
// Each change runs in one transaction with its audit record, whose before
// and after are the user as [User]'s JSON shows them, never a password or a
// hash; a change that changes nothing writes and records nothing.
// Disabling a user or setting their password ends their sessions in that
// transaction, once the user's row is locked, so no login can start a
// session the change misses (store's RecordLogin). Every change that could
// leave no active admin takes the admin role's lock first, then counts the
// others. It is safe for concurrent use.
type Service struct {
	db       DB
	hasher   *password.Hasher
	sessions Sessions
}

// NewService returns a Service on db, hashing new passwords with hasher and
// ending users' sessions through sessions.
func NewService(db DB, hasher *password.Hasher, sessions Sessions) *Service {
	return &Service{db: db, hasher: hasher, sessions: sessions}
}

// NewUser is a user to create.
type NewUser struct {
	Email       string
	DisplayName string
	Password    string
	// Roles are the user's roles; at least one.
	Roles []string
}

// Change holds the fields to change; nil keeps one.
type Change struct {
	Email       *string
	DisplayName *string
	Status      *string
}

// List returns every user, by email; only those with status unless it is
// empty.
func (s *Service) List(ctx context.Context, status string) ([]User, error) {
	rows, err := store.New(s.db).ListUsers(ctx, pgtype.Text{String: status, Valid: status != ""})
	if err != nil {
		return nil, fmt.Errorf("list the users: %w", err)
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = user(store.GetUserRow(r))
	}
	return out, nil
}

// Get returns the user id names, or [ErrNotFound].
func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) {
	return get(ctx, store.New(s.db), id)
}

// Create creates an active user with a password and roles, and returns
// them. It fails with [ErrEmailTaken], an [*InvalidError], or
// [ErrUnknownRole] for a role that does not exist; on a field it can check
// alone, before hashing.
func (s *Service) Create(ctx context.Context, caller Caller, in NewUser) (User, error) {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return User{}, &InvalidError{Field: "email", Reason: err.Error()}
	}
	name, err := displayName(in.DisplayName, email)
	if err != nil {
		return User{}, &InvalidError{Field: "display_name", Reason: err.Error()}
	}
	if err := password.Check(in.Password); err != nil {
		return User{}, &InvalidError{Field: "password", Reason: err.Error()}
	}
	if len(in.Roles) == 0 {
		return User{}, &InvalidError{Field: "roles", Reason: "want at least one role"}
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return User{}, err
	}
	var created User
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.CreateUser(ctx, store.CreateUserParams{
			Email: email, DisplayName: name, PasswordHash: pgtype.Text{String: hash, Valid: true},
		})
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("create the user: %w", err)
		}
		for _, role := range in.Roles {
			if err := roleExists(ctx, q, role); err != nil {
				return err
			}
			if _, err := q.GrantRoleIfMissing(ctx, store.GrantRoleIfMissingParams{UserID: row.ID, RoleName: role}); err != nil {
				return fmt.Errorf("grant the %s role: %w", role, err)
			}
		}
		if created, err = get(ctx, q, uuid.UUID(row.ID.Bytes)); err != nil {
			return err
		}
		return record(ctx, q, caller, audit.ActionCreate, created.ID, nil, created)
	})
	if err != nil {
		return User{}, err
	}
	return created, nil
}

// Update changes the fields change holds and returns the user after it.
// Disabling a user ends their sessions; the last active admin cannot be
// disabled ([ErrLastAdmin]). An email another user has is [ErrEmailTaken].
func (s *Service) Update(ctx context.Context, caller Caller, id uuid.UUID, change Change) (User, error) {
	var email, name string
	var err error
	if change.Email != nil {
		if email, err = NormalizeEmail(*change.Email); err != nil {
			return User{}, &InvalidError{Field: "email", Reason: err.Error()}
		}
	}
	if change.DisplayName != nil {
		if strings.TrimSpace(*change.DisplayName) == "" {
			return User{}, &InvalidError{Field: "display_name", Reason: fmt.Sprintf("want 1 to %d characters", maxDisplayNameRunes)}
		}
		if name, err = displayName(*change.DisplayName, ""); err != nil {
			return User{}, &InvalidError{Field: "display_name", Reason: err.Error()}
		}
	}
	if change.Status != nil && *change.Status != StatusActive && *change.Status != StatusDisabled {
		return User{}, &InvalidError{Field: "status", Reason: fmt.Sprintf("%q: want %s or %s", *change.Status, StatusActive, StatusDisabled)}
	}
	disabling := change.Status != nil && *change.Status == StatusDisabled
	var after User
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if disabling {
			// Before the user's row, as every change that can leave no
			// admin does, so two such changes never wait for each other.
			if _, err := q.LockAdminRole(ctx); err != nil {
				return fmt.Errorf("lock the admin role: %w", err)
			}
		}
		before, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		after = before
		if change.Email != nil {
			after.Email = email
		}
		if change.DisplayName != nil {
			after.DisplayName = name
		}
		if change.Status != nil {
			after.Status = *change.Status
		}
		if after.Email == before.Email && after.DisplayName == before.DisplayName && after.Status == before.Status {
			return nil
		}
		ending := after.Status == StatusDisabled && before.Status == StatusActive
		if ending && slices.Contains(before.Roles, RoleAdmin) {
			if err := lastAdmin(ctx, q, id); err != nil {
				return err
			}
		}
		err = q.UpdateUser(ctx, store.UpdateUserParams{ID: pgUUID(id), Email: after.Email, DisplayName: after.DisplayName, Status: after.Status})
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("update the user: %w", err)
		}
		if after, err = get(ctx, q, id); err != nil {
			return err
		}
		if err := record(ctx, q, caller, ActionUpdate, after.ID, before, after); err != nil {
			return err
		}
		if ending {
			return s.sessions.DeleteUser(ctx, after.ID)
		}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return after, nil
}

// SetPassword replaces the password of the user id names and ends their
// sessions, as for a user who has forgotten theirs.
func (s *Service) SetPassword(ctx context.Context, caller Caller, id uuid.UUID, secret string) error {
	return SetPassword(ctx, s.db, s.hasher, s.sessions, caller, PasswordChange{UserID: id, Password: secret})
}

// PasswordChange is a new password for a user.
type PasswordChange struct {
	UserID   uuid.UUID
	Password string
	// IfHash, when set, is the hash the user must still have: the one their
	// current password was checked against. If it is not, the change is
	// [ErrStalePassword].
	IfHash string
}

// SetPassword sets change's password, as an admin does for a user and a
// user for themselves: it hashes it, replaces the user's hash, audits it
// and ends every session of the user, in one transaction on db. The user's
// row is locked first, so a login that checked the old password can neither
// record itself nor start a session after the sessions end.
func SetPassword(ctx context.Context, db DB, hasher *password.Hasher, sessions Sessions, caller Caller, change PasswordChange) error {
	if err := password.Check(change.Password); err != nil {
		return &InvalidError{Field: "password", Reason: err.Error()}
	}
	hash, err := hasher.Hash(ctx, change.Password)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := lock(ctx, q, change.UserID)
		if err != nil {
			return err
		}
		n, err := q.SetPasswordHash(ctx, store.SetPasswordHashParams{
			ID: pgUUID(change.UserID), NewHash: pgtype.Text{String: hash, Valid: true},
			IfHash: pgtype.Text{String: change.IfHash, Valid: change.IfHash != ""},
		})
		if err != nil {
			return fmt.Errorf("set the password hash: %w", err)
		}
		if n == 0 {
			return ErrStalePassword
		}
		err = record(ctx, q, caller, ActionSetPassword, user.ID, nil,
			passwordSet{ID: user.ID, Email: user.Email, SessionsEnded: true})
		if err != nil {
			return err
		}
		return sessions.DeleteUser(ctx, user.ID)
	})
}

// passwordSet is a password change as its audit record shows it: that the
// password changed and the sessions ended, never the password or a hash.
type passwordSet struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	SessionsEnded bool   `json:"sessions_ended"`
}

// Grant gives the user id names role and returns the user after it. A role
// they have changes nothing.
func (s *Service) Grant(ctx context.Context, caller Caller, id uuid.UUID, role string) (User, error) {
	return s.role(ctx, caller, id, role, true)
}

// Revoke takes role from the user id names and returns the user after it.
// A role they lack changes nothing; admin cannot be taken from the last
// active admin ([ErrLastAdmin]).
func (s *Service) Revoke(ctx context.Context, caller Caller, id uuid.UUID, role string) (User, error) {
	return s.role(ctx, caller, id, role, false)
}

func (s *Service) role(ctx context.Context, caller Caller, id uuid.UUID, role string, grant bool) (User, error) {
	var after User
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := roleExists(ctx, q, role); err != nil {
			return err
		}
		revokingAdmin := !grant && role == RoleAdmin
		if revokingAdmin {
			if _, err := q.LockAdminRole(ctx); err != nil {
				return fmt.Errorf("lock the admin role: %w", err)
			}
		}
		before, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		after = before
		if slices.Contains(before.Roles, role) == grant {
			return nil
		}
		action := ActionGrantRole
		var n int64
		if grant {
			n, err = q.GrantRoleIfMissing(ctx, store.GrantRoleIfMissingParams{UserID: pgUUID(id), RoleName: role})
		} else {
			action = ActionRevokeRole
			if revokingAdmin && before.Status == StatusActive {
				if err := lastAdmin(ctx, q, id); err != nil {
					return err
				}
			}
			n, err = q.RevokeRole(ctx, store.RevokeRoleParams{UserID: pgUUID(id), RoleName: role})
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", action, role, err)
		}
		if n != 1 {
			return fmt.Errorf("%s %s: %d rows changed, want 1", action, role, n)
		}
		if err := q.TouchUser(ctx, pgUUID(id)); err != nil {
			return fmt.Errorf("%s %s: %w", action, role, err)
		}
		if after, err = get(ctx, q, id); err != nil {
			return err
		}
		return record(ctx, q, caller, action, after.ID, before, after)
	})
	if err != nil {
		return User{}, err
	}
	return after, nil
}

// record audits a change of the user id; before and after are nil, a User,
// or another object without secrets.
func record(ctx context.Context, q *store.Queries, caller Caller, action, id string, before, after any) error {
	_, err := audit.Write(ctx, q, audit.Change{
		RequestID:  caller.RequestID,
		Actor:      caller.Actor,
		Action:     action,
		EntityType: audit.EntityUser,
		EntityID:   id,
		Before:     before,
		After:      after,
	})
	return err
}

// lastAdmin fails with [ErrLastAdmin] unless an active admin other than id
// exists. The caller holds the admin role's lock (LockAdminRole).
func lastAdmin(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	n, err := q.CountOtherActiveAdmins(ctx, pgUUID(id))
	if err != nil {
		return fmt.Errorf("count the active admins: %w", err)
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

func roleExists(ctx context.Context, q *store.Queries, role string) error {
	ok, err := q.RoleExists(ctx, role)
	if err != nil {
		return fmt.Errorf("look up the %s role: %w", role, err)
	}
	if !ok {
		return ErrUnknownRole
	}
	return nil
}

// lock locks the user's row for the transaction q runs in and returns the
// user, or [ErrNotFound].
func lock(ctx context.Context, q *store.Queries, id uuid.UUID) (User, error) {
	_, err := q.LockUser(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("lock the user: %w", err)
	}
	return get(ctx, q, id)
}

func get(ctx context.Context, q *store.Queries, id uuid.UUID) (User, error) {
	row, err := q.GetUser(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("read the user: %w", err)
	}
	return user(row), nil
}

func user(r store.GetUserRow) User {
	u := User{
		ID: r.ID.String(), Email: r.Email, DisplayName: r.DisplayName, Status: r.Status, Roles: r.Roles,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.LastLoginAt.Valid {
		t := r.LastLoginAt.Time
		u.LastLoginAt = &t
	}
	return u
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
