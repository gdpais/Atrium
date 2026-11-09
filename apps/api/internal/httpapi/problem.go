package httpapi

import (
	"encoding/json"
	"net/http"

	"atrium/apps/api/internal/openapi"
)

// problem is an RFC 9457 problem whose status says what went wrong.
func problem(status int, detail string) openapi.Problem {
	p := openapi.Problem{Type: "about:blank", Title: http.StatusText(status), Status: status}
	if detail != "" {
		p.Detail = &detail
	}
	return p
}

// writeProblem answers with a problem, for errors found before an
// operation's handler runs.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem(status, detail))
}
