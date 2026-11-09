package scale

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/logschema"
)

const fixtureDir = "../../../../services/db-collector/internal/collectors/outsystems/testdata/oslog"

// memory keeps what a sink gets.
type memory struct {
	rows    map[string][2]int // log type -> rows per cycle 0 and 1
	first   time.Time
	last    time.Time
	stacks  []string
	minutes map[time.Time]int
	flushes int
	t       *testing.T
}

func newMemory(t *testing.T) *memory {
	return &memory{rows: map[string][2]int{}, minutes: map[time.Time]int{}, t: t}
}

func (m *memory) Row(logType string, cycle int, values []any) error {
	cols := Columns(logType)
	if len(values) != len(cols) {
		m.t.Fatalf("%s: %d values for %d columns", logType, len(values), len(cols))
	}
	c := m.rows[logType]
	c[cycle]++
	m.rows[logType] = c
	at := values[slices.IndexFunc(cols, func(c string) bool { return strings.EqualFold(c, "Instant") })].(time.Time)
	if m.first.IsZero() || at.Before(m.first) {
		m.first = at
	}
	if at.After(m.last) {
		m.last = at
	}
	m.minutes[at.Truncate(time.Minute)]++
	if logType == "error" {
		m.stacks = append(m.stacks, values[slices.Index(cols, "Stack")].(string))
	}
	return nil
}

func (m *memory) Flush() error { m.flushes++; return nil }

func TestLastRotation(t *testing.T) {
	for _, c := range []struct{ at, want string }{
		{"2026-10-20T12:00:00Z", "2026-10-16T23:45:00Z"}, // Tuesday
		{"2026-10-16T23:45:00Z", "2026-10-16T23:45:00Z"}, // the rotation itself
		{"2026-10-16T23:44:59Z", "2026-10-09T23:45:00Z"}, // a second before
		{"2026-10-17T00:10:00Z", "2026-10-16T23:45:00Z"},
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		if got := LastRotation(at).Format(time.RFC3339); got != c.want {
			t.Errorf("LastRotation(%s) = %s, want %s", c.at, got, c.want)
		}
	}
}

// generate runs the surveyed profile at a hundredth of its volume.
func generate(t *testing.T, seed uint64) (*Expected, *memory) {
	t.Helper()
	mem := newMemory(t)
	end, _ := time.Parse(time.RFC3339, "2026-10-20T12:00:00Z")
	exp, err := Generate(Options{Profile: Surveyed().Scaled(0.01), Seed: seed, End: end}, mem, nil)
	if err != nil {
		t.Fatal(err)
	}
	return exp, mem
}

func TestGenerateFollowsTheProfile(t *testing.T) {
	exp, mem := generate(t, 1)
	p := Surveyed().Scaled(0.01)
	start, rotation, end := exp.Start, exp.Rotation, exp.End
	if !start.Equal(rotation.Add(-Week)) || rotation.Format(time.RFC3339) != "2026-10-16T23:45:00Z" {
		t.Fatalf("start %s, rotation %s", start, rotation)
	}
	if mem.first.Before(start) || !mem.last.Before(end.Add(3*time.Minute)) {
		t.Fatalf("rows from %s to %s, want within [%s, %s]", mem.first, mem.last, start, end)
	}
	// The previous week is whole: its volumes are the profile's.
	previous := exp.Weeks[0]
	for logType, want := range map[string]int{"screen": p.Screen, "integration": p.Integration, "extension": p.Extension, "general": p.General} {
		got := previous.Rows[logType]
		// Rows per request are Poisson: allow 3%, or four standard
		// deviations for the small log types.
		if math.Abs(float64(got-want)) > math.Max(0.03*float64(want), 4*math.Sqrt(float64(want))) {
			t.Errorf("previous week: %d %s rows, want about %d", got, logType, want)
		}
		if mem.rows[logType][logschema.InitialPrevious] != got {
			t.Errorf("%s: %d rows in the previous cycle table, expected.json says %d", logType, mem.rows[logType][logschema.InitialPrevious], got)
		}
	}
	if previous.Users != p.Users || previous.Sessions > p.Sessions || previous.Sessions < p.Sessions*9/10 {
		t.Errorf("previous week: %d users and %d sessions, want %d and about %d", previous.Users, previous.Sessions, p.Users, p.Sessions)
	}
	if want := 7 * 24 * 60 / int(p.TimerEvery/time.Minute) * p.Timers; previous.Rows["cyclic_job"] != want {
		t.Errorf("previous week: %d timer runs, want %d", previous.Rows["cyclic_job"], want)
	}
	if mem.rows["mobile_request"] != [2]int{} || mem.rows["request_event"] != [2]int{} {
		t.Errorf("mobile request or request event rows from a profile without them: %v", mem.rows)
	}
	errorShare := float64(previous.ErrorRows) / float64(previous.Requests)
	if math.Abs(errorShare-p.ErrorShare) > p.ErrorShare/4 {
		t.Errorf("error share %.4f, want about %.4f", errorShare, p.ErrorShare)
	}
	// The busiest minute is about PeakMinuteFactor times the average one.
	var total int
	for _, b := range exp.Weeks {
		for _, n := range b.Rows {
			total += n
		}
	}
	average := float64(total) / end.Sub(start).Minutes()
	if f := float64(exp.PeakMinute.Rows) / average; f < p.PeakMinuteFactor*0.75 || f > p.PeakMinuteFactor*1.5 {
		t.Errorf("busiest minute %d rows, %.1f times the average %.1f; want about %.1f", exp.PeakMinute.Rows, f, average, p.PeakMinuteFactor)
	}
	// The hourly shape: 04:00 is the quietest hour, 11:00 the busiest.
	byHour := [24]int{}
	for _, h := range exp.Hours {
		if !h.From.Before(rotation) {
			continue
		}
		byHour[h.From.Hour()] += h.Requests
	}
	if slices.Index(byHour[:], slices.Max(byHour[:])) != 11 || slices.Index(byHour[:], slices.Min(byHour[:])) != 4 {
		t.Errorf("requests by hour %v, want the busiest at 11 and the quietest at 04", byHour)
	}
	if days := int(end.Sub(start).Hours()/24) + 1; mem.flushes < days {
		t.Errorf("%d flushes, want one per day", mem.flushes)
	}
}

func TestGenerateAddsNewErrorKindsEachDay(t *testing.T) {
	exp, mem := generate(t, 1)
	p := Surveyed()
	seen := map[string]bool{}
	for _, k := range exp.ErrorKinds {
		if seen[k.Name] {
			t.Fatalf("error kind %s seen first twice", k.Name)
		}
		seen[k.Name] = true
	}
	// Every whole day after the first brings NewErrorKindsPerDay kinds.
	for _, d := range exp.Days[1 : len(exp.Days)-1] {
		if d.NewErrorKinds < p.NewErrorKindsPerDay {
			t.Errorf("%s: %d new error kinds, want at least %d", d.From.Format(time.DateOnly), d.NewErrorKinds, p.NewErrorKindsPerDay)
		}
	}
	long := 0
	for _, s := range mem.stacks {
		if len(s) > 2000 {
			long++
		}
	}
	if long == 0 || long == len(mem.stacks) {
		t.Errorf("%d of %d error rows have a trace longer than 2000 characters, want some", long, len(mem.stacks))
	}
}

// Error kinds differ in their message or their stack's first frame, which
// the collector's fingerprint keeps after replacing numbers and quoted
// values, so each kind is one fingerprint.
func TestErrorKindsAreDistinctFingerprints(t *testing.T) {
	exp, _ := generate(t, 3)
	kinds := errorKinds(newRand(1), modules(), 200, 0.25)
	keys := map[string]string{}
	for _, k := range kinds {
		key := normalized(k.message) + "\n" + normalized(k.frames[0])
		if other, ok := keys[key]; ok {
			t.Fatalf("%s and %s share a fingerprint", k.Name, other)
		}
		keys[key] = k.Name
	}
	if len(exp.ErrorKinds) < Surveyed().NewErrorKindsPerDay*10 {
		t.Fatalf("only %d error kinds seen", len(exp.ErrorKinds))
	}
}

func TestGenerateIsReproducible(t *testing.T) {
	a, _ := generate(t, 7)
	b, _ := generate(t, 7)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("the same seed generated different logs")
	}
	c, _ := generate(t, 8)
	if jc, _ := json.Marshal(c); string(jc) == string(ja) {
		t.Fatal("another seed generated the same log")
	}
}

// Every column the generator sets is in its log type's cycle table.
func TestColumnsAreInTheFixtures(t *testing.T) {
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]logschema.LogType{}
	for _, lt := range types {
		byName[lt.Name] = lt
	}
	for _, logType := range LogTypes() {
		lt, ok := byName[logType]
		if !ok {
			t.Errorf("log type %s has no fixture", logType)
			continue
		}
		for _, c := range Columns(logType) {
			if lt.ColumnType(c) == "" {
				t.Errorf("%s: column %s is not in %s", logType, c, lt.Table(0))
			}
		}
	}
	if len(LogTypes()) != len(types) {
		t.Errorf("generator writes %v, fixtures have %d log types", LogTypes(), len(types))
	}
}

func TestProfileValidation(t *testing.T) {
	if err := Surveyed().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Surveyed()
	bad.Sessions = bad.Users - 1
	if bad.Validate() == nil {
		t.Error("fewer sessions than users validated")
	}
	if s := Surveyed().Scaled(0.5); s.Screen != Surveyed().Screen/2 || s.MobileRequest != 0 || s.Users != 2415 {
		t.Errorf("Scaled(0.5): %+v", s)
	}
}

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed)) }

// normalized is the collector's fingerprint normalization
// (services/db-collector/internal/collectors/outsystems/fingerprint.go),
// which this module cannot import.
func normalized(s string) string {
	s = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`).ReplaceAllString(s, "<guid>")
	s = regexp.MustCompile(`'[^']*'|"[^"]*"`).ReplaceAllString(s, "<str>")
	s = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b|\b[0-9a-f]{8,}\b`).ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") {
			return "<hex>"
		}
		return m
	})
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "<num>")
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "))
}
