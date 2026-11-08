// Package scale fills a test logging database with a production-sized
// OutSystems 11 log: the volume, daily shape, users, sessions and errors of
// the surveyed installation (ADR 0010), written straight into the cycle
// tables, and measures the collector's queries against it. A production
// logging database cannot be load-tested before go-live; a test one scaled to
// the same traffic can.
//
// Rows are synthetic and generated from a seed, so a run is reproducible and
// its known answers (distinct users and sessions, error kinds first seen) are
// written next to the data (see [Expected]).
package scale

import (
	"fmt"
	"math"
	"time"
)

// Profile is one week of an installation's logs.
type Profile struct {
	// Rows per log type in one week. Screen and mobile request rows are the
	// requests; integration, extension, general and request event rows hang
	// off them, about Rows/requests each.
	Screen, MobileRequest, Integration, Extension, General, RequestEvent int
	// Timers run every TimerEvery, one cyclic job row per run.
	Timers     int
	TimerEvery time.Duration
	// Users and Sessions are the distinct users and sessions of one week.
	Users, Sessions int
	// SessionHours bounds a session's length, inclusive.
	SessionHours [2]float64
	// ErrorShare is the share of requests that log an error row.
	ErrorShare float64
	// RecurringErrorKinds are the distinct errors (one fingerprint each) the
	// first day already has; NewErrorKindsPerDay more appear each day after.
	RecurringErrorKinds, NewErrorKindsPerDay int
	// LongTraceShare is the share of error kinds whose stack trace is longer
	// than the error cycle table's Stack, so it goes to oslog_Error_Detail.
	LongTraceShare float64
	// SlowSQLShare is the share of general rows that are the platform's slow
	// query rows (Module_Name SLOWSQL).
	SlowSQLShare float64
	// HourlyRows is the shape of a day: log rows per hour of the day, in any
	// unit; requests and session starts follow it.
	HourlyRows [24]float64
	// PeakMinuteFactor is the week's busiest minute over its average minute,
	// in rows.
	PeakMinuteFactor float64
}

// Surveyed is the surveyed installation's week (ADR 0010; the aggregates in
// load/traffic.js and docs/architecture/application-observability.md): 4.51
// million screen requests, 1.43 million integration calls, 159,852 extension
// rows and about 72,000 slow-SQL rows (30,702 in its first three days), 3.8
// times the average minute at the busiest one, 4,829 users and 13,871
// sessions. The survey's extract held no other general rows, and no error,
// mobile request, cyclic job or request event rows: those volumes are
// assumptions.
func Surveyed() Profile {
	return Profile{
		Screen:      4_510_000,
		Integration: 1_430_000,
		Extension:   160_000,
		// The slow-SQL rows are SlowSQLShare of these.
		General: 90_000,
		// A Traditional web installation: no Reactive requests.
		MobileRequest:       0,
		RequestEvent:        0,
		Timers:              20,
		TimerEvery:          10 * time.Minute,
		Users:               4_829,
		Sessions:            13_871,
		SessionHours:        [2]float64{1, 8},
		ErrorShare:          0.005,
		RecurringErrorKinds: 40,
		NewErrorKindsPerDay: 3,
		LongTraceShare:      0.25,
		SlowSQLShare:        0.8,
		HourlyRows: [24]float64{218, 119, 69, 49, 34, 40, 62, 139, 272, 495, 655, 804,
			755, 614, 672, 728, 717, 659, 621, 581, 514, 488, 506, 382},
		PeakMinuteFactor: 3.8,
	}
}

// Scaled multiplies the profile's volumes, users and sessions by f; the
// shape, shares and error kinds stay.
func (p Profile) Scaled(f float64) Profile {
	n := func(v int) int {
		if v == 0 {
			return 0
		}
		return max(1, int(math.Round(float64(v)*f)))
	}
	p.Screen, p.MobileRequest = n(p.Screen), n(p.MobileRequest)
	p.Integration, p.Extension, p.General, p.RequestEvent = n(p.Integration), n(p.Extension), n(p.General), n(p.RequestEvent)
	p.Users, p.Sessions = n(p.Users), n(p.Sessions)
	return p
}

// Validate reports a profile the generator cannot honour.
func (p Profile) Validate() error {
	switch {
	case p.Screen+p.MobileRequest <= 0:
		return fmt.Errorf("profile: no requests (Screen and MobileRequest are 0)")
	case p.Users <= 0 || p.Sessions < p.Users:
		return fmt.Errorf("profile: want at least one user and as many sessions as users, got %d users and %d sessions", p.Users, p.Sessions)
	case p.SessionHours[0] <= 0 || p.SessionHours[1] < p.SessionHours[0]:
		return fmt.Errorf("profile: SessionHours %v", p.SessionHours)
	case p.ErrorShare < 0 || p.ErrorShare > 1 || p.LongTraceShare < 0 || p.LongTraceShare > 1 || p.SlowSQLShare < 0 || p.SlowSQLShare > 1:
		return fmt.Errorf("profile: shares must be between 0 and 1")
	case p.Timers > 0 && p.TimerEvery < time.Minute:
		return fmt.Errorf("profile: TimerEvery %s is under a minute", p.TimerEvery)
	case p.RecurringErrorKinds < 1:
		return fmt.Errorf("profile: want at least one recurring error kind")
	case p.PeakMinuteFactor < 1:
		return fmt.Errorf("profile: PeakMinuteFactor %v is under 1", p.PeakMinuteFactor)
	}
	var sum float64
	for _, v := range p.HourlyRows {
		if v < 0 {
			return fmt.Errorf("profile: negative HourlyRows")
		}
		sum += v
	}
	if sum == 0 {
		return fmt.Errorf("profile: HourlyRows are all 0")
	}
	return nil
}

// requests is the week's requests: screen and mobile request rows.
func (p Profile) requests() int { return p.Screen + p.MobileRequest }
