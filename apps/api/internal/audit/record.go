// Package audit records every admin and operator change (ADR 0016).
//
// [Write] inserts the change's record into the audit_outbox table, in the
// transaction that makes the change: a change that commits always has its
// record, and one that rolls back has none. A [Drainer] in each API replica
// then sends the oldest rows to the OTel Collector as OTLP log records and
// deletes them once the Collector accepts them, so delivery is at least once
// and a record's id identifies a duplicate. The record's shape is
// packages/telemetry-contracts/src/audit_record.schema.json.
//
// Records hold credential references, never values: an entity's before and
// after leave out passwords, password hashes, session IDs and tokens, and a
// test changes each audited entity and fails if one shows up.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/store"
)

// Entity types: what a record is about. Write accepts no other, so a new
// audited entity is added here, and the secrets test fails until it changes
// that entity too.
const (
	EntityUser = "user"
	// EntitySession is a login session. Its ID is the session's ID, which
	// identifies it but cannot log in, never its token.
	EntitySession = "session"
	// EntityEnvironment, EntityApplication and EntityComponent are the
	// inventory (package inventory); before and after are the entity as the
	// API shows it.
	EntityEnvironment = "environment"
	EntityApplication = "application"
	EntityComponent   = "application_component"
)

// EntityTypes lists every entity type Write accepts.
var EntityTypes = []string{EntityUser, EntitySession, EntityEnvironment, EntityApplication, EntityComponent}

// Actions.
const (
	ActionCreate = "create"
	// ActionUpdate changes an entity; its record has both before and after.
	ActionUpdate = "update"
	// ActionDelete removes one; its record's before is what was removed.
	ActionDelete = "delete"
	// ActionLogin starts a session; its record's after is the session.
	ActionLogin = "login"
	// ActionLogout ends one; its record's before is the session.
	ActionLogout = "logout"
	// ActionLoginFailed is a refused login (ADR 0016 §1): its entity ID
	// identifies the attempt, since no session exists, and its after says
	// which email was tried and why it was refused, never the password.
	ActionLoginFailed = "login_failed"
)

// Actor types.
const (
	// ActorUser is a logged-in user, by ID and email.
	ActorUser = "user"
	// ActorCLI is an administrative command run in an API container, such
	// as `api users create-admin`: whoever could run it, by the command and
	// the host (the pod) it ran on.
	ActorCLI = "cli"
	// ActorClient is a client that is not logged in, by its address, such
	// as one whose login failed.
	ActorClient = "client"
)

// Actor is who made a change.
type Actor struct {
	Type    string `json:"type"`
	UserID  string `json:"user_id,omitempty"`
	Email   string `json:"email,omitempty"`
	Command string `json:"command,omitempty"`
	Host    string `json:"host,omitempty"`
	Address string `json:"address,omitempty"`
}

// UserActor is a logged-in user.
func UserActor(userID, email string) Actor {
	return Actor{Type: ActorUser, UserID: userID, Email: email}
}

// CLIActor is an administrative command, run on host.
func CLIActor(command, host string) Actor {
	return Actor{Type: ActorCLI, Command: command, Host: host}
}

// ClientActor is a client that is not logged in, at address.
func ClientActor(address string) Actor {
	return Actor{Type: ActorClient, Address: address}
}

func (a Actor) validate() error {
	switch {
	case a.Type == ActorUser && uuid.MatchString(a.UserID) && a.Email != "" && a.Command == "" && a.Host == "" && a.Address == "":
	case a.Type == ActorCLI && a.Command != "" && a.UserID == "" && a.Email == "" && a.Address == "":
	case a.Type == ActorClient && a.Address != "" && a.UserID == "" && a.Email == "" && a.Command == "" && a.Host == "":
	default:
		return fmt.Errorf("actor %+v: want a user with a UUID and an email, a command, or a client address", a)
	}
	return nil
}

// Change is one audited change, as its caller describes it.
type Change struct {
	// RequestID identifies the request, or the command run, that made the
	// change.
	RequestID string
	Actor     Actor
	// Action is what was done, such as ActionCreate.
	Action string
	// EntityType is one of EntityTypes; EntityID identifies the entity.
	EntityType string
	EntityID   string
	// Before and After are the entity's state before and after the change,
	// encoded as JSON objects; nil for none, as Before of a creation. They
	// must leave out every secret value.
	Before, After any
	// ConfigVersion is the runtime config version the change concerns, if
	// any.
	ConfigVersion string
}

// Record is one audit record, as stored in the outbox and delivered.
type Record struct {
	ID            string          `json:"id"`
	Time          time.Time       `json:"time"`
	RequestID     string          `json:"request_id"`
	Actor         Actor           `json:"actor"`
	Action        string          `json:"action"`
	EntityType    string          `json:"entity_type"`
	EntityID      string          `json:"entity_id"`
	Before        json.RawMessage `json:"before"`
	After         json.RawMessage `json:"after"`
	ConfigVersion string          `json:"config_version,omitempty"`
}

// name matches an action or an entity type.
var name = regexp.MustCompile(`^[a-z][a-z_]*$`)

// uuid matches a user actor's ID in the hyphenated form the schema's uuid
// format asserts.
var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Write records change in the outbox through q, which must run in the
// transaction that makes the change, and returns the record. It fails on a
// change it cannot record as the schema describes, which fails the change.
func Write(ctx context.Context, q *store.Queries, change Change) (Record, error) {
	id, err := NewID()
	if err != nil {
		return Record{}, err
	}
	record := Record{
		ID:            id.String(),
		Time:          time.Now().UTC(),
		RequestID:     change.RequestID,
		Actor:         change.Actor,
		Action:        change.Action,
		EntityType:    change.EntityType,
		EntityID:      change.EntityID,
		ConfigVersion: change.ConfigVersion,
	}
	if record.Before, err = object(change.Before); err != nil {
		return Record{}, fmt.Errorf("audit: before: %w", err)
	}
	if record.After, err = object(change.After); err != nil {
		return Record{}, fmt.Errorf("audit: after: %w", err)
	}
	if err := record.validate(); err != nil {
		return Record{}, fmt.Errorf("audit: %w", err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return Record{}, fmt.Errorf("audit: %w", err)
	}
	if err := q.InsertAuditRecord(ctx, store.InsertAuditRecordParams{ID: id, Record: encoded}); err != nil {
		return Record{}, fmt.Errorf("audit: write the record: %w", err)
	}
	return record, nil
}

func (r Record) validate() error {
	switch {
	case r.RequestID == "":
		return errors.New("no request ID")
	case !name.MatchString(r.Action):
		return fmt.Errorf("action %q: want lower-case letters and underscores", r.Action)
	case !slices.Contains(EntityTypes, r.EntityType):
		return fmt.Errorf("entity type %q: want one of %v", r.EntityType, EntityTypes)
	case r.EntityID == "":
		return errors.New("no entity ID")
	case r.Before == nil && r.After == nil:
		return errors.New("neither before nor after")
	}
	return r.Actor.validate()
}

// object encodes v as a JSON object, or returns nil for nil, a nil pointer
// or a nil map.
func object(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(encoded) == "null" {
		return nil, nil
	}
	if encoded[0] != '{' {
		return nil, fmt.Errorf("%T does not encode as a JSON object", v)
	}
	return encoded, nil
}

// NewID returns a random (version 4) UUID.
func NewID() (pgtype.UUID, error) {
	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		return pgtype.UUID{}, fmt.Errorf("audit: new id: %w", err)
	}
	id.Bytes[6] = id.Bytes[6]&0x0f | 0x40
	id.Bytes[8] = id.Bytes[8]&0x3f | 0x80
	id.Valid = true
	return id, nil
}
