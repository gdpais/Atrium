// Package auth logs local users in and out and keeps their sessions (ADR
// 0013 §7–8).
//
// [Service.Login] checks an email and password against the user's argon2id
// hash, upgrading a hash made with old parameters, and starts a session in
// Redis ([Sessions]); failed logins are limited per account and per client
// address ([Limits]). [Service.Authenticate] turns a session token into the
// user and their roles on each request, and refuses a disabled user's.
// Logins, logouts and failed logins are audited (ADR 0016 §1), never with a
// password, a hash or a token.
//
// Package auth knows nothing of HTTP: package httpapi reads the cookie,
// checks the Origin header and each operation's role (see [Allows]).
package auth

import "slices"

// Role is one of the roles in the roles table.
type Role string

// Roles, from the least to the most allowed (ADR 0013 §8). Each API
// operation declares the least role it needs; a user may call what any of
// their roles allows.
const (
	// Viewer reads everything; for wallboards.
	Viewer Role = "viewer"
	// Operator changes inventory, telemetry sources, database targets and
	// probes.
	Operator Role = "operator"
	// Admin does everything, users and roles included.
	Admin Role = "admin"
)

// Roles lists every role, from the least allowed.
var Roles = []Role{Viewer, Operator, Admin}

// Allows reports whether a user with roles may call an operation that needs
// need. An unknown role allows nothing.
func Allows(roles []string, need Role) bool {
	least := slices.Index(Roles, need)
	if least < 0 {
		return false
	}
	for _, r := range roles {
		if slices.Index(Roles, Role(r)) >= least {
			return true
		}
	}
	return false
}
