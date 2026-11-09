package scale

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"atrium/tools/patientzero/internal/logschema"
)

// Week is how long one cycle table is current.
const Week = 7 * 24 * time.Hour

// LastRotation is the platform's last weekly rotation at or before t: Friday
// 23:45 in t's location (the log time zone).
func LastRotation(t time.Time) time.Time {
	r := time.Date(t.Year(), t.Month(), t.Day(), 23, 45, 0, 0, t.Location())
	for r.Weekday() != time.Friday || r.After(t) {
		r = r.AddDate(0, 0, -1)
	}
	return r
}

// Sink stores the generated rows.
type Sink interface {
	// Row stores one row of the log type in the cycle's table. values are
	// in the order of [Columns] for the log type.
	Row(logType string, cycle int, values []any) error
	// Flush is called after every generated day (UTC) and at the end.
	Flush() error
}

// columns are the cycle-table columns each log type's rows set, named as
// the fixtures name them; the others keep their defaults or NULL.
var columns = map[string][]string{
	"screen": {"Instant", "Duration", "Screen", "Session_Id", "User_Id", "Espace_Id", "Tenant_Id", "Screen_Type",
		"Executed_By", "Viewstate_Bytes", "Session_Bytes", "Session_Requests", "Access_Mode", "Request_Key", "Action_Name",
		"Client_IP", "Espace_Name", "Application_Name", "Application_Key", "Username"},
	"mobile_request": {"Id", "Instant", "Espace_Id", "Tenant_Id", "Screen", "Endpoint", "Source", "Duration", "Executed_by",
		"Error_Id", "Request_Key", "Login_Id", "User_Id", "Espace_Name", "Application_Name", "Application_Key", "Username"},
	"integration": {"Id", "Instant", "Duration", "Source", "Endpoint", "Action", "Type", "Espace_Id", "Tenant_Id", "Error_Id",
		"Executed_by", "Is_Expose", "REQUEST_KEY", "Espace_Name", "Application_Name", "Application_Key"},
	"extension": {"Instant", "Duration", "Action_Name", "Session_Id", "User_Id", "Espace_Id", "Tenant_Id", "Extension_Id",
		"Executed_By", "Error_Id", "Request_Key", "Espace_Name", "Extension_Name", "Application_Name", "Application_Key", "Username"},
	"general": {"Instant", "Session_Id", "User_Id", "Espace_Id", "Tenant_Id", "Error_Id", "Message", "Message_Type",
		"Module_Name", "Request_Key", "Entrypoint_Name", "Action_Name", "Client_IP", "Espace_Name", "Application_Name",
		"Application_Key", "Username"},
	"error": {"Id", "Instant", "Session_Id", "User_Id", "Espace_Id", "Tenant_Id", "Message", "Stack", "Module_Name", "Server",
		"EnvironmentInformation", "Request_Key", "Entrypoint_Name", "Action_Name", "Espace_Name", "Application_Name",
		"Application_Key", "Username"},
	"request_event": {"INSTANT", "REQUESTKEY", "REQUESTEVENTNAME", "MODULEKEY", "MODULENAME", "APPLICATIONKEY",
		"APPLICATIONNAME", "EVENTDETAILS"},
	"cyclic_job": {"Instant", "Duration", "Cyclic_Job_Key", "Espace_Id", "Tenant_Id", "Executed_By", "Error_Id",
		"Should_Have_Run_At", "Next_Run", "Request_Key", "Espace_Name", "Application_Name", "Application_Key", "Cyclic_Job_Name"},
}

// Columns are the columns the log type's rows set, in the order of the
// values [Sink.Row] gets.
func Columns(logType string) []string { return columns[logType] }

// LogTypes are the log types the generator writes.
func LogTypes() []string {
	out := make([]string, 0, len(columns))
	for t := range columns {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Options says what to generate.
type Options struct {
	Profile Profile
	Seed    uint64
	// End is the last instant generated; rows start one week before the
	// last rotation at or before it, so the previous week's view is full and
	// the current week's runs up to End.
	End time.Time
}

type session struct {
	id     string
	user   *user
	end    time.Time
	steps  int
	module module
}

type user struct {
	id   int
	name string
	ip   string
}

type generator struct {
	p       Profile
	r       *rand.Rand
	sink    Sink
	mods    []module
	users   []*user
	kinds   []*errorKind // active kinds; their weights in cum
	cum     []float64
	pending []*errorKind // new kinds, not seen yet, at planned times
	planned []time.Time
	exp     *Expected

	rotation time.Time
	active   []*session
	last     *session
	// Sessions created in the current cycle week, for the user assignment.
	weekSessions int
	weekUsers    []*user
	week         int
}

// Generate writes the rows of Options into sink and returns the known
// answers about them.
func Generate(o Options, sink Sink, progress func(time.Time)) (*Expected, error) {
	p := o.Profile
	if err := p.Validate(); err != nil {
		return nil, err
	}
	end := o.End.Truncate(time.Minute)
	rotation := LastRotation(end)
	start := rotation.Add(-Week)
	g := &generator{
		p: p, r: rand.New(rand.NewPCG(o.Seed, o.Seed^0x5ca1e)), sink: sink, mods: modules(), rotation: rotation, week: -1,
		exp: newExpected(p, o.Seed, start, rotation, end),
	}
	for i := range p.Users {
		g.users = append(g.users, &user{
			id: 1000 + i, name: fmt.Sprintf("user%05d", i+1),
			ip: fmt.Sprintf("10.%d.%d.%d", 20+i/62500, i/250%250, i%250+1),
		})
	}
	all := errorKinds(g.r, g.mods, p.RecurringErrorKinds+p.NewErrorKindsPerDay*15, p.LongTraceShare)
	for _, k := range all[:min(len(all), p.RecurringErrorKinds)] {
		g.activate(k)
	}
	newKinds := all[min(len(all), p.RecurringErrorKinds):]
	for day := start.Truncate(24 * time.Hour).Add(24 * time.Hour); day.Before(end) && len(newKinds) > 0; day = day.Add(24 * time.Hour) {
		for range p.NewErrorKindsPerDay {
			if len(newKinds) == 0 {
				break
			}
			at := day.Add(8*time.Hour + time.Duration(g.r.Int64N(int64(10*time.Hour))))
			if at.After(start) && at.Before(end) {
				g.pending = append(g.pending, newKinds[0])
				g.planned = append(g.planned, at)
			}
			newKinds = newKinds[1:]
		}
	}
	sort.Sort(byPlanned{g})

	var curve float64
	busiest := 0
	for h, v := range p.HourlyRows {
		curve += v
		if v > p.HourlyRows[busiest] {
			busiest = h
		}
	}
	// The spike minute's share of the busiest hour's requests, so the
	// week's busiest minute is PeakMinuteFactor times its average minute;
	// spike is the chance a request goes there rather than to a random
	// minute, which may be the spike minute too.
	share := min(0.9, p.PeakMinuteFactor*curve/(1440*p.HourlyRows[busiest]))
	spike := max(0, (share-1.0/60)/(1-1.0/60))
	weekly := func(n int, hourOfDay int) float64 { return float64(n) / 7 * p.HourlyRows[hourOfDay] / curve }

	var requestCarry, sessionCarry float64
	for hour := start.Truncate(time.Hour); hour.Before(end); hour = hour.Add(time.Hour) {
		hod := hour.Hour()
		n := weekly(p.requests(), hod) + requestCarry
		requests := int(n)
		requestCarry = n - float64(requests)
		n = weekly(p.Sessions, hod) + sessionCarry
		sessions := int(n)
		sessionCarry = n - float64(sessions)
		if err := g.hour(hour, start, end, requests, sessions, hod == busiest, spike); err != nil {
			return nil, err
		}
		if err := g.timers(hour, start, end); err != nil {
			return nil, err
		}
		if hour.Add(time.Hour).Hour() == 0 {
			if err := sink.Flush(); err != nil {
				return nil, err
			}
			if progress != nil {
				progress(hour.Truncate(24 * time.Hour))
			}
		}
	}
	if err := sink.Flush(); err != nil {
		return nil, err
	}
	g.exp.finish()
	return g.exp, nil
}

type byPlanned struct{ g *generator }

func (b byPlanned) Len() int           { return len(b.g.pending) }
func (b byPlanned) Less(i, j int) bool { return b.g.planned[i].Before(b.g.planned[j]) }
func (b byPlanned) Swap(i, j int) {
	b.g.pending[i], b.g.pending[j] = b.g.pending[j], b.g.pending[i]
	b.g.planned[i], b.g.planned[j] = b.g.planned[j], b.g.planned[i]
}

func (g *generator) activate(k *errorKind) {
	g.kinds = append(g.kinds, k)
	total := k.weight
	if len(g.cum) > 0 {
		total += g.cum[len(g.cum)-1]
	}
	g.cum = append(g.cum, total)
}

// cycle is the cycle table an instant's row goes into.
func (g *generator) cycle(t time.Time) int {
	if t.Before(g.rotation) {
		return logschema.InitialPrevious
	}
	return logschema.InitialCurrent
}

// hour generates the requests and sessions that start in [hour, hour+1h),
// within [start, end).
func (g *generator) hour(hour, start, end time.Time, requests, sessions int, busiest bool, spike float64) error {
	spikeMinute := -1
	if busiest && spike > 0 {
		spikeMinute = g.r.IntN(60)
	}
	at := func() time.Time {
		minute := g.r.IntN(60)
		if spikeMinute >= 0 && g.r.Float64() < spike {
			minute = spikeMinute
		}
		return hour.Add(time.Duration(minute)*time.Minute + time.Duration(g.r.IntN(60000))*time.Millisecond)
	}
	starts := make([]time.Time, sessions)
	for i := range starts {
		starts[i] = hour.Add(time.Duration(g.r.Int64N(int64(time.Hour))))
	}
	slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
	times := make([]time.Time, requests)
	for i := range times {
		times[i] = at()
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	next := 0
	for _, t := range times {
		for next < len(starts) && !starts[next].After(t) {
			g.startSession(starts[next])
			next++
		}
		if !t.Before(end) {
			break
		}
		if t.Before(start) {
			continue
		}
		if err := g.request(t); err != nil {
			return err
		}
	}
	for ; next < len(starts); next++ {
		if starts[next].Before(end) {
			g.startSession(starts[next])
		}
	}
	return nil
}

// startSession opens a session. Each cycle week, its first sessions go to
// every user once, in a shuffled order, so the week has all Users; later
// ones go to users with a long tail of frequent ones.
func (g *generator) startSession(t time.Time) {
	if w := g.cycle(t); w != g.week {
		g.week, g.weekSessions = w, 0
		g.weekUsers = slices.Clone(g.users)
		g.r.Shuffle(len(g.weekUsers), func(i, j int) { g.weekUsers[i], g.weekUsers[j] = g.weekUsers[j], g.weekUsers[i] })
	}
	var u *user
	if g.weekSessions < len(g.weekUsers) {
		u = g.weekUsers[g.weekSessions]
	} else {
		f := g.r.Float64()
		u = g.weekUsers[int(f*f*float64(len(g.weekUsers)))]
	}
	g.weekSessions++
	hours := g.p.SessionHours[0] + g.r.Float64()*(g.p.SessionHours[1]-g.p.SessionHours[0])
	s := &session{id: g.sessionID(), user: u, end: t.Add(time.Duration(hours * float64(time.Hour))), module: g.mods[g.r.IntN(len(g.mods))]}
	g.active = append(g.active, s)
	g.last = s
}

// pick is a session open at t, or the last one opened when none is.
func (g *generator) pick(t time.Time) *session {
	for len(g.active) > 0 {
		i := g.r.IntN(len(g.active))
		if s := g.active[i]; !s.end.Before(t) {
			return s
		}
		g.active[i] = g.active[len(g.active)-1]
		g.active = g.active[:len(g.active)-1]
	}
	return g.last
}

// errorKind is the kind of an error at t: a new kind due by then, else an
// active one by weight.
func (g *generator) errorKind(t time.Time) *errorKind {
	if len(g.pending) > 0 && !g.planned[0].After(t) {
		k := g.pending[0]
		g.pending, g.planned = g.pending[1:], g.planned[1:]
		g.activate(k)
		return k
	}
	x := g.r.Float64() * g.cum[len(g.cum)-1]
	i, _ := slices.BinarySearch(g.cum, x)
	return g.kinds[min(i, len(g.kinds)-1)]
}

// request writes the rows of one request that starts at t.
func (g *generator) request(t time.Time) error {
	s := g.pick(t)
	if s == nil {
		return nil
	}
	s.steps++
	p := g.p
	u, m := s.user, s.module
	var kind *errorKind
	if g.r.Float64() < p.ErrorShare {
		kind = g.errorKind(t)
		m = kind.module
	}
	screen := m.Screens[g.r.IntN(len(m.Screens))]
	action := m.Actions[g.r.IntN(len(m.Actions))]
	if kind != nil {
		action = kind.action
	}
	dur := g.duration(250, 120_000)
	done := t.Add(time.Duration(dur) * time.Millisecond)
	key := g.guid()
	server := frontEnds[g.r.IntN(len(frontEnds))]
	g.exp.request(t, u.id, s.id)

	if g.r.Float64()*float64(p.requests()) < float64(p.MobileRequest) {
		if err := g.row("mobile_request", done, g.guid(), done, m.ID, 1, screen, action, u.ip, int(math.Round(float64(dur)/1000)), server,
			"", key, s.id, u.id, m.Name, m.Application, m.AppKey, u.name); err != nil {
			return err
		}
	} else {
		if err := g.row("screen", t, t, dur, screen, s.id, u.id, m.ID, 1, "Web", server, g.r.IntN(20000), g.r.IntN(50000),
			s.steps, "Navigate", key, action, u.ip, m.Name, m.Application, m.AppKey, u.name); err != nil {
			return err
		}
	}
	errID := ""
	if kind != nil {
		errID = g.guid()
	}
	// The error links the request's first other row, as a failed call does.
	linked := false
	link := func() string {
		if linked || errID == "" {
			return ""
		}
		linked = true
		return errID
	}
	within := func() time.Time { return t.Add(time.Duration(g.r.Int64N(int64(dur)+1)) * time.Millisecond) }
	requests := float64(p.requests())
	for range g.poisson(float64(p.Integration) / requests) {
		callDur := g.duration(180, 60_000)
		partner := partners[g.r.IntN(len(partners))]
		entity := entities[g.r.IntN(len(entities))]
		typ := "REST"
		if g.r.IntN(10) == 0 {
			typ = "SOAP"
		}
		ended := within()
		if err := g.row("integration", ended, g.guid(), ended, callDur, "", fmt.Sprintf("https://%s.example.test/api/%ss", partner, entity),
			"Get"+entity, typ, m.ID, 1, link(), server, false, key, m.Name, m.Application, m.AppKey); err != nil {
			return err
		}
	}
	for range g.poisson(float64(p.Extension) / requests) {
		x := g.r.IntN(len(extensionNames))
		started := within()
		if err := g.row("extension", started, started, g.duration(40, 30_000), extensionActs[g.r.IntN(len(extensionActs))], s.id, u.id,
			m.ID, 1, x+1, server, link(), key, m.Name, extensionNames[x], m.Application, m.AppKey, u.name); err != nil {
			return err
		}
	}
	for range g.poisson(float64(p.General) / requests) {
		logged := within()
		message, typ, moduleName := fmt.Sprintf("Processed %d items for %s", g.r.IntN(500), screen), "Info", m.Name
		if g.r.Float64() < p.SlowSQLShare {
			entity := entities[g.r.IntN(len(entities))]
			message = fmt.Sprintf("SELECT %s.Id, %s.Status FROM OSUSR_abc_%s %s WHERE %s.CustomerId = @CustomerId took %d ms",
				entity, entity, entity, entity, entity, 200+int(g.duration(600, 60_000)))
			moduleName = "SLOWSQL"
		}
		if err := g.row("general", logged, logged, s.id, u.id, m.ID, 1, link(), message, typ, moduleName, key, screen, action,
			u.ip, m.Name, m.Application, m.AppKey, u.name); err != nil {
			return err
		}
	}
	for range g.poisson(float64(p.RequestEvent) / requests) {
		if err := g.row("request_event", t, t, key, "ScreenRequest", fmt.Sprintf("0a7e1e57-0000-4000-9000-%012x", m.ID), m.Name,
			m.AppKey, m.Application, fmt.Sprintf(`{"screen":"%s","action":"%s"}`, screen, action)); err != nil {
			return err
		}
	}
	if kind != nil {
		message, stack := kind.occurrence(g.r)
		if err := g.row("error", done, errID, done, s.id, u.id, m.ID, 1, message, stack, m.Name, server,
			fmt.Sprintf("eSpaceVer: %d (Id=%d), RequestUrl: https://apps.example.test/%s/%s.aspx", 40+g.r.IntN(20), m.ID, m.Name, screen),
			key, screen, action, m.Name, m.Application, m.AppKey, u.name); err != nil {
			return err
		}
		g.exp.error(done, kind)
	}
	return nil
}

// timers writes the cyclic job rows of the runs scheduled in [hour,
// hour+1h), within [start, end). Each timer runs every TimerEvery from its own offset,
// a few seconds late; Duration is in seconds, as the platform logs it.
func (g *generator) timers(hour, start, end time.Time) error {
	p := g.p
	if p.Timers == 0 {
		return nil
	}
	every := p.TimerEvery
	for i := range p.Timers {
		m := g.mods[i%len(g.mods)]
		offset := time.Duration(i) * every / time.Duration(p.Timers)
		first := hour.Truncate(every).Add(offset)
		for at := first; at.Before(hour.Add(time.Hour)); at = at.Add(every) {
			if at.Before(hour) || at.Before(start) {
				continue
			}
			started := at.Add(time.Duration(g.r.IntN(30_000)) * time.Millisecond)
			if !started.Before(end) {
				continue
			}
			if err := g.row("cyclic_job", started, started, 1+g.r.IntN(120), fmt.Sprintf("0a7e1e57-0000-4000-a000-%012x", i+1), m.ID, 1,
				frontEnds[i%len(frontEnds)], "", at, at.Add(every), g.guid(), m.Name, m.Application, m.AppKey,
				timerNames[i%len(timerNames)]); err != nil {
				return err
			}
		}
	}
	return nil
}

// row sends one row to the sink, into the cycle table of at, and counts it.
func (g *generator) row(logType string, at time.Time, values ...any) error {
	if n := len(columns[logType]); len(values) != n {
		return fmt.Errorf("%s row: %d values for %d columns", logType, len(values), n)
	}
	g.exp.row(logType, at, g.cycle(at))
	return g.sink.Row(logType, g.cycle(at), values)
}

// duration is a log-normal duration in milliseconds around median, capped.
func (g *generator) duration(median, cap float64) int {
	return int(min(cap, math.Max(1, median*math.Exp(g.r.NormFloat64()))))
}

// poisson is a Poisson count with mean lambda (Knuth's method; the means
// here are small).
func (g *generator) poisson(lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= g.r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// guid is a random (version 4) GUID, the form of Request_Key and Id.
func (g *generator) guid() string {
	a, b := g.r.Uint64(), g.r.Uint64()
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", a>>32, a>>16&0xffff, a&0xfff, b>>48&0x3fff|0x8000, b&0xffffffffffff)
}

// sessionID looks like an ASP.NET session id: 24 lower-case characters.
func (g *generator) sessionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz012345"
	var b [24]byte
	for i := range b {
		b[i] = alphabet[g.r.IntN(len(alphabet))]
	}
	return string(b[:])
}
