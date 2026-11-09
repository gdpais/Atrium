package scale

import (
	"sort"
	"time"
)

// Expected are the known answers about a generated log, for checking what
// Atrium shows against it: the dashboards' active users and sessions (a
// distinct count in Loki), error rows, and error kinds (one fingerprint
// each) first seen. Users and sessions count where a request started; rows
// and errors where they were logged (Instant). Times are UTC.
type Expected struct {
	Seed    uint64  `json:"seed"`
	Profile Profile `json:"profile"`
	// Start is the first instant generated, Rotation the start of the
	// current week's cycle table, End the last instant.
	Start    time.Time `json:"start"`
	Rotation time.Time `json:"rotation"`
	End      time.Time `json:"end"`
	// Weeks are the previous and the current cycle week.
	Weeks []Bucket `json:"weeks"`
	Days  []Bucket `json:"days"`
	Hours []Bucket `json:"hours"`
	// PeakMinute is the minute with the most rows, across log types.
	PeakMinute struct {
		Minute time.Time `json:"minute"`
		Rows   int       `json:"rows"`
	} `json:"peak_minute"`
	// ErrorKinds are every error kind, in the order first seen.
	ErrorKinds []KindSeen `json:"error_kinds"`

	weeks, days, hours map[time.Time]*bucket
	minutes            map[time.Time]int
	seen               map[string]bool
}

// Bucket is one week, day or hour.
type Bucket struct {
	From          time.Time      `json:"from"`
	To            time.Time      `json:"to"`
	Requests      int            `json:"requests"`
	Rows          map[string]int `json:"rows"`
	Users         int            `json:"users"`
	Sessions      int            `json:"sessions"`
	ErrorRows     int            `json:"error_rows"`
	ErrorKinds    int            `json:"error_kinds"`
	NewErrorKinds int            `json:"new_error_kinds"`
}

// KindSeen is one error kind and when its first row was logged.
type KindSeen struct {
	Name      string    `json:"name"`
	FirstSeen time.Time `json:"first_seen"`
	LongTrace bool      `json:"long_trace"`
}

type bucket struct {
	Bucket
	users    map[int]bool
	sessions map[string]bool
	kinds    map[string]bool
}

func newExpected(p Profile, seed uint64, start, rotation, end time.Time) *Expected {
	return &Expected{
		Seed: seed, Profile: p, Start: start.UTC(), Rotation: rotation.UTC(), End: end.UTC(),
		weeks: map[time.Time]*bucket{}, days: map[time.Time]*bucket{}, hours: map[time.Time]*bucket{},
		minutes: map[time.Time]int{}, seen: map[string]bool{},
	}
}

// buckets are the week, day and hour of t.
func (e *Expected) buckets(t time.Time) []*bucket {
	week := e.Rotation
	if t.Before(e.Rotation) {
		week = e.Start
	}
	get := func(m map[time.Time]*bucket, from time.Time, length time.Duration) *bucket {
		b := m[from]
		if b == nil {
			b = &bucket{Bucket: Bucket{From: from, To: from.Add(length), Rows: map[string]int{}},
				users: map[int]bool{}, sessions: map[string]bool{}, kinds: map[string]bool{}}
			m[from] = b
		}
		return b
	}
	t = t.UTC()
	return []*bucket{
		get(e.weeks, week, Week),
		get(e.days, t.Truncate(24*time.Hour), 24*time.Hour),
		get(e.hours, t.Truncate(time.Hour), time.Hour),
	}
}

func (e *Expected) request(t time.Time, userID int, sessionID string) {
	for _, b := range e.buckets(t) {
		b.Requests++
		b.users[userID] = true
		b.sessions[sessionID] = true
	}
}

func (e *Expected) row(logType string, t time.Time, _ int) {
	for _, b := range e.buckets(t) {
		b.Rows[logType]++
	}
	e.minutes[t.UTC().Truncate(time.Minute)]++
}

func (e *Expected) error(t time.Time, k *errorKind) {
	first := !e.seen[k.Name]
	if first {
		e.seen[k.Name] = true
		e.ErrorKinds = append(e.ErrorKinds, KindSeen{Name: k.Name, FirstSeen: t.UTC(), LongTrace: k.Long()})
	}
	for _, b := range e.buckets(t) {
		b.ErrorRows++
		b.kinds[k.Name] = true
		if first {
			b.NewErrorKinds++
		}
	}
}

// finish turns the tallies into the exported buckets.
func (e *Expected) finish() {
	out := func(m map[time.Time]*bucket) []Bucket {
		var bs []Bucket
		for _, b := range m {
			b.Users, b.Sessions, b.ErrorKinds = len(b.users), len(b.sessions), len(b.kinds)
			bs = append(bs, b.Bucket)
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].From.Before(bs[j].From) })
		return bs
	}
	e.Weeks, e.Days, e.Hours = out(e.weeks), out(e.days), out(e.hours)
	for m, n := range e.minutes {
		if n > e.PeakMinute.Rows || n == e.PeakMinute.Rows && m.Before(e.PeakMinute.Minute) {
			e.PeakMinute.Minute, e.PeakMinute.Rows = m, n
		}
	}
	e.weeks, e.days, e.hours, e.minutes = nil, nil, nil, nil
}
