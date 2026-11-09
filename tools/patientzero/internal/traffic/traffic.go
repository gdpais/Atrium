// Package traffic counts the requests PatientZero serves during a scenario
// run, per interval and over the run: requests, failures and latency per
// entry point, and distinct users and sessions. The run journal records
// these summaries, the known answer for the users, sessions and requests
// Atrium reports. It counts only while a run is active, and its distinct
// sets are capped, so memory stays bounded however long the load runs.
package traffic

import (
	"sync"
	"time"
)

// Caps on the distinct users and sessions remembered per interval and per
// run. Past a cap the count stops growing and the summary says so.
const (
	MaxDistinctPerInterval = 50_000
	MaxDistinctPerRun      = 200_000
)

// Request is one served request.
type Request struct {
	EntryPoint string
	User       string // empty for anonymous callers
	Session    string
	Duration   time.Duration
	Failed     bool // the request wrote an error row
	Injected   bool // a scenario fault failed it
	SlowSQL    int  // slow-query rows (SLOWSQL) the request wrote
}

// EntryPoint is the summary of one entry point.
type EntryPoint struct {
	Requests       int64 `json:"requests"`
	Failed         int64 `json:"failed"`
	Injected       int64 `json:"injected"`
	DurationMsSum  int64 `json:"duration_ms_sum"`
	DurationMsMax  int64 `json:"duration_ms_max"`
	AnonymousCalls int64 `json:"anonymous"`
	SlowSQL        int64 `json:"slow_sql"`
	Shed           int64 `json:"shed"`
}

// Summary is the traffic between From and To.
type Summary struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Requests int64     `json:"requests"`
	Failed   int64     `json:"failed"`
	Injected int64     `json:"injected"`
	SlowSQL  int64     `json:"slow_sql"`
	// Shed are the requests turned away with 503 at the in-flight limit.
	// They wrote no log rows and are not among the Requests.
	Shed int64 `json:"shed"`
	// Rows are the log rows committed in the interval, per log type, held
	// ones when they were committed.
	Rows           map[string]int64      `json:"rows"`
	Users          int                   `json:"users"`
	Sessions       int                   `json:"sessions"`
	UsersCapped    bool                  `json:"users_capped,omitempty"`
	SessionsCapped bool                  `json:"sessions_capped,omitempty"`
	EntryPoints    map[string]EntryPoint `json:"entry_points"`
}

type window struct {
	from     time.Time
	limit    int
	sum      Summary
	users    map[string]struct{}
	sessions map[string]struct{}
}

func newWindow(from time.Time, limit int) *window {
	return &window{
		from: from, limit: limit,
		sum:      Summary{From: from, EntryPoints: map[string]EntryPoint{}, Rows: map[string]int64{}},
		users:    map[string]struct{}{},
		sessions: map[string]struct{}{},
	}
}

func (w *window) add(r Request) {
	s := &w.sum
	s.Requests++
	ep := s.EntryPoints[r.EntryPoint]
	ep.Requests++
	if r.Failed {
		s.Failed++
		ep.Failed++
	}
	if r.Injected {
		s.Injected++
		ep.Injected++
	}
	s.SlowSQL += int64(r.SlowSQL)
	ep.SlowSQL += int64(r.SlowSQL)
	ms := r.Duration.Milliseconds()
	ep.DurationMsSum += ms
	if ms > ep.DurationMsMax {
		ep.DurationMsMax = ms
	}
	if r.User == "" {
		ep.AnonymousCalls++
	} else {
		s.UsersCapped = insert(w.users, r.User, w.limit) || s.UsersCapped
	}
	if r.Session != "" {
		s.SessionsCapped = insert(w.sessions, r.Session, w.limit) || s.SessionsCapped
	}
	s.EntryPoints[r.EntryPoint] = ep
}

func (w *window) shed(entryPoint string) {
	w.sum.Shed++
	ep := w.sum.EntryPoints[entryPoint]
	ep.Shed++
	w.sum.EntryPoints[entryPoint] = ep
}

// insert adds v unless the set is full; it reports a full set.
func insert(set map[string]struct{}, v string, limit int) bool {
	if _, ok := set[v]; ok {
		return false
	}
	if len(set) >= limit {
		return true
	}
	set[v] = struct{}{}
	return false
}

func (w *window) close(to time.Time) Summary {
	s := w.sum
	s.To = to
	s.Users = len(w.users)
	s.Sessions = len(w.sessions)
	return s
}

// Counter counts requests while a run is active. A nil *Counter counts
// nothing.
type Counter struct {
	mu       sync.Mutex
	active   bool
	interval *window
	run      *window
}

// Observe counts one request, if a run is active.
func (c *Counter) Observe(r Request) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.interval.add(r)
	c.run.add(r)
}

// Shed counts a request turned away at the in-flight limit, if a run is
// active.
func (c *Counter) Shed(entryPoint string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.interval.shed(entryPoint)
	c.run.shed(entryPoint)
}

// Committed counts log rows committed, per log type, if a run is active.
func (c *Counter) Committed(rows map[string]int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	for t, n := range rows {
		c.interval.sum.Rows[t] += n
		c.run.sum.Rows[t] += n
	}
}

// Begin starts counting a run at now, forgetting any earlier one.
func (c *Counter) Begin(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.interval = newWindow(now, MaxDistinctPerInterval)
	c.run = newWindow(now, MaxDistinctPerRun)
}

// Flush returns the interval that ends at now and starts the next.
func (c *Counter) Flush(now time.Time) Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return Summary{From: now, To: now, EntryPoints: map[string]EntryPoint{}, Rows: map[string]int64{}}
	}
	s := c.interval.close(now)
	c.interval = newWindow(now, MaxDistinctPerInterval)
	return s
}

// End stops counting at now and returns the last interval and the whole
// run.
func (c *Counter) End(now time.Time) (last, run Summary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		empty := Summary{From: now, To: now, EntryPoints: map[string]EntryPoint{}, Rows: map[string]int64{}}
		return empty, empty
	}
	last, run = c.interval.close(now), c.run.close(now)
	c.active, c.interval, c.run = false, nil, nil
	return last, run
}
