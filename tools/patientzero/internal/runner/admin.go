package runner

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"atrium/tools/patientzero/internal/journal"
)

// adminAuth authorizes the admin endpoints with the bearer token from
// PATIENTZERO_ADMIN_TOKEN, the db-collector's pattern
// (services/db-collector/internal/app/admin.go): it is the only
// authorization check, and every /admin/ route goes through it.
type adminAuth struct {
	// enabled is false when no token is configured; every request is then
	// refused.
	enabled bool
	// digest is the SHA-256 of the token. Comparing fixed-length digests
	// keeps the comparison constant time in the token's length too:
	// subtle.ConstantTimeCompare returns early when lengths differ.
	digest [sha256.Size]byte
}

// newAdminAuth hashes token. Surrounding whitespace is ignored, so a Secret
// written with a trailing newline works; a blank token disables the admin
// endpoints.
func newAdminAuth(token string) adminAuth {
	token = strings.TrimSpace(token)
	if token == "" {
		return adminAuth{}
	}
	return adminAuth{enabled: true, digest: sha256.Sum256([]byte(token))}
}

// authorized reports whether r carries exactly one Authorization header
// "Bearer <token>" (scheme case-insensitive) with the configured token.
func (a adminAuth) authorized(r *http.Request) bool {
	if !a.enabled {
		return false
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	presented := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], a.digest[:]) == 1
}

// Handler serves the admin endpoints under /admin/, behind the bearer token:
//
//	GET  /admin/scenarios                 the scenarios and fault types, and which can run here
//	POST /admin/scenarios/{name}/start    start a scenario (202; 409 while another runs); ?agent=host attaches a host agent
//	GET  /admin/run                       the active run's status, or the last run's
//	POST /admin/run/faults/{id}           the host agent's answer to a cluster fault request
//	POST /admin/run/stop                  stop the active run and wait for its end; ?detach=1 detaches the host agent first
//	GET  /admin/runs                      the journals kept on disk, newest first
//	GET  /admin/runs/{run}/journal        one run's journal, JSONL
//
// An unauthenticated request gets 401 before routing, so it learns nothing
// about the routes or the runner's state. With no token configured every
// request gets 401. Responses are never cached.
func (r *Runner) Handler(token string) http.Handler {
	auth := newAdminAuth(token)
	if !auth.enabled {
		r.cfg.Log.Warn("PATIENTZERO_ADMIN_TOKEN is not set: the admin endpoints answer 401", "warning", "admin_token_unset")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/scenarios", r.listScenarios)
	mux.HandleFunc("POST /admin/scenarios/{name}/start", r.startScenario)
	mux.HandleFunc("GET /admin/run", r.runStatus)
	mux.HandleFunc("POST /admin/run/faults/{id}", r.answerFault)
	mux.HandleFunc("POST /admin/run/stop", r.stopRun)
	mux.HandleFunc("GET /admin/runs", r.listRuns)
	mux.HandleFunc("GET /admin/runs/{run}/journal", r.runJournal)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !auth.authorized(req) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patientzero"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// No admin request has a body worth reading.
		req.Body = http.MaxBytesReader(w, req.Body, 1<<10)
		mux.ServeHTTP(w, req)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (r *Runner) listScenarios(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios":   r.Scenarios(),
		"fault_types": r.FaultTypes(),
	})
}

func (r *Runner) startScenario(w http.ResponseWriter, req *http.Request) {
	st, err := r.StartWith(req.PathValue("name"), req.URL.Query().Get("agent") == "host")
	switch {
	case err == nil:
		r.cfg.Log.Info("scenario start requested", "run", st.Run, "scenario", st.Scenario, "remote", req.RemoteAddr)
		writeJSON(w, http.StatusAccepted, st)
	case errors.Is(err, ErrUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "run": st})
	case errors.Is(err, ErrNotRunnable):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		r.cfg.Log.Error("starting a scenario failed", "scenario", req.PathValue("name"), "err", err)
		writeError(w, http.StatusInternalServerError, "starting the scenario failed")
	}
}

func (r *Runner) runStatus(w http.ResponseWriter, _ *http.Request) {
	st, ok := r.Status()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"state": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// answerFault takes the host agent's answer to a cluster fault request:
// {"error": ""} when it was applied, or the reason it was not.
func (r *Runner) answerFault(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `the body must be {"error": "..."}`)
		return
	}
	if r.cfg.Delegate == nil || r.cfg.Delegate.Complete(req.PathValue("id"), body.Error) != nil {
		writeError(w, http.StatusNotFound, ErrNoSuchRequest.Error())
		return
	}
	r.cfg.Log.Info("cluster fault answered", "request", req.PathValue("id"), "error", body.Error)
	w.WriteHeader(http.StatusNoContent)
}

func (r *Runner) stopRun(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("detach") == "1" && r.cfg.Delegate != nil {
		r.cfg.Delegate.Detach()
	}
	st, err := r.Stop()
	if errors.Is(err, ErrNotRunning) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	r.cfg.Log.Info("scenario stopped", "run", st.Run, "remote", req.RemoteAddr)
	writeJSON(w, http.StatusOK, st)
}

func (r *Runner) listRuns(w http.ResponseWriter, _ *http.Request) {
	list, err := journal.List(r.cfg.JournalDir)
	if err != nil {
		r.cfg.Log.Error("listing the journals failed", "err", err)
		writeError(w, http.StatusInternalServerError, "listing the journals failed")
		return
	}
	if list == nil {
		list = []journal.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list})
}

// runJournal serves a journal as it is on disk; while its run is active
// that is everything written so far.
func (r *Runner) runJournal(w http.ResponseWriter, req *http.Request) {
	f, err := journal.Open(r.cfg.JournalDir, req.PathValue("run"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such run journal")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		r.cfg.Log.Warn("sending a journal failed", "run", req.PathValue("run"), "err", err)
	}
}
