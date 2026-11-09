// Package app is PatientZero's HTTP surface: entry points modelled on
// OutSystems 11 ones, each of which runs business SQL and writes the log rows
// the platform would write for the request, all under one Request_Key with
// the caller's session, user and client IP (ADR 0010).
package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"atrium/tools/patientzero/internal/inject"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/shop"
	"atrium/tools/patientzero/internal/traffic"
)

// The caller's identity. A load generator sets these to give each virtual
// user a stable user and session; X-Forwarded-For gives its client IP.
const (
	HeaderUser    = "X-PatientZero-User"
	HeaderSession = "X-PatientZero-Session"
)

// Entry point paths, shaped like the URLs of the OutSystems entry points they
// stand for.
const (
	PathOrderList  = "/PatientZero_Web/OrderList.aspx"
	PathPlaceOrder = "/PatientZero_App/screenservices/PatientZero_App/MainFlow/Checkout/ServiceAPIPlaceOrder"
	PathAPIOrders  = "/PatientZero_API/rest/Orders/orders"
)

// Entry point names, as Atrium's operation label names them: the screen
// of a screen row, the endpoint of a mobile request, the action of an
// exposed API. Scenario faults target entry points by these names.
const (
	EntryOrderList  = "OrderList"
	EntryPlaceOrder = "PlaceOrder"
	EntryGetOrders  = "GetOrders"
)

// EntryPoints lists the entry point names.
var EntryPoints = []string{EntryOrderList, EntryPlaceOrder, EntryGetOrders}

// The application and its modules (eSpaces), as the log rows name them.
const (
	Application    = "PatientZero"
	ApplicationKey = "0a7e1e57-0000-4000-8000-00000000a001"
	TenantID       = 1
)

type module struct {
	Name string
	ID   int
}

var (
	webModule = module{"PatientZero_Web", 101} // Traditional web screens
	appModule = module{"PatientZero_App", 102} // Reactive app and its server actions
	apiModule = module{"PatientZero_API", 103} // Exposed REST API
)

// Modules maps each module (eSpace) to its entry points: what a deploy
// scenario fault publishes a new version of.
var Modules = map[string][]string{
	webModule.Name: {EntryOrderList},
	appModule.Name: {EntryPlaceOrder},
	apiModule.Name: {EntryGetOrders},
}

// DefaultSlowSQL is the SlowSQL threshold PatientZero runs with: a business
// statement that takes longer gets the platform's slow-query row.
const DefaultSlowSQL = 200 * time.Millisecond

// ordersShown is how many orders the screens and the API return.
const ordersShown = 20

// Server serves the entry points.
type Server struct {
	Store  shop.Store
	Logs   oslog.Writer
	Schema oslog.Schema
	// FrontEnd is the front-end server name written as Executed_By/Server.
	FrontEnd string
	// Ready reports whether both databases are usable; nil means always.
	Ready func(context.Context) error
	Now   func() time.Time
	Log   *slog.Logger
	// Faults are the scenario faults switched on; nil injects nothing.
	Faults *inject.Injector
	// Traffic counts the requests of a scenario run; nil counts nothing.
	Traffic *traffic.Counter
	// Admin serves /admin/ (the scenario runner); nil serves nothing there.
	Admin http.Handler
	// Ledger counts the committed log rows, the known answer of a scenario's
	// delivery checks; nil counts nothing.
	Ledger *oslog.Ledger
	// SlowSQL is how long a business statement may take before the request
	// logs it as the platform does a slow query: a general row with
	// Module_Name SLOWSQL and the Message "<statement> took <n> ms". Zero
	// logs none.
	SlowSQL time.Duration
	// MaxInFlight bounds how many database-backed entry-point requests
	// (OrderList, PlaceOrder, GetOrders) run at once. Beyond it the server
	// sheds load with 503 and a Retry-After header instead of opening more
	// connections and cascading 500s when the database cannot keep up. Zero
	// or less means no limit (the health and admin endpoints are never
	// limited).
	MaxInFlight int
	// shedEvery is how often, at most, the server logs the requests it
	// shed; zero means shedReportEvery. Tests shorten it.
	shedEvery time.Duration
}

// Handler routes the health endpoints and the entry points. The three
// database-backed entry points run behind the in-flight limiter (MaxInFlight).
func (s *Server) Handler() http.Handler {
	limit := s.limiter()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET "+PathOrderList, screen(limit(EntryOrderList, s.orderList)))
	mux.HandleFunc("POST "+PathPlaceOrder, limit(EntryPlaceOrder, s.placeOrder))
	mux.HandleFunc("GET "+PathAPIOrders, limit(EntryGetOrders, s.apiOrders))
	if s.Admin != nil {
		mux.Handle("/admin/", s.Admin)
	}
	return mux
}

// screenHeaders go on every response of the HTML screen, its error and shed
// responses included: no MIME sniffing, and a Content-Security-Policy under
// which the page loads nothing (it has no script, style, image or form of
// its own), cannot be framed, and takes no base URL or form target from
// injected markup. X-Frame-Options covers browsers without frame-ancestors.
var screenHeaders = map[string]string{
	"X-Content-Type-Options":  "nosniff",
	"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	"X-Frame-Options":         "DENY",
}

// screen sets screenHeaders before an HTML screen's handler writes anything.
func screen(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for name, value := range screenHeaders {
			w.Header().Set(name, value)
		}
		next(w, r)
	}
}

// shedReportEvery is how often, at most, the server logs that it is shedding
// load, so a flood of requests does not flood the log too.
const shedReportEvery = 10 * time.Second

// limiter returns a middleware that admits at most MaxInFlight requests at
// once, shedding the rest with 503 and Retry-After. With MaxInFlight <= 0 it
// is a pass-through, so existing callers and tests are unaffected unless they
// set it. The token pool is created once, so every handler it wraps shares the
// one budget. A shed request writes no log rows; the run's traffic counts it
// as shed, under the entry point's name, and the server log reports it.
func (s *Server) limiter() func(name string, next http.HandlerFunc) http.HandlerFunc {
	if s.MaxInFlight <= 0 {
		return func(_ string, next http.HandlerFunc) http.HandlerFunc { return next }
	}
	tokens := make(chan struct{}, s.MaxInFlight)
	every := s.shedEvery
	if every <= 0 {
		every = shedReportEvery
	}
	report := newShedReport(every, func(shed int) {
		s.logger().Warn("in-flight limit reached; shed requests with 503",
			"max_in_flight", s.MaxInFlight, "shed", shed)
	})
	return func(name string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case tokens <- struct{}{}:
				defer func() { <-tokens }()
				next(w, r)
				return
			default:
			}
			s.Traffic.Shed(name)
			report.add()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "overloaded"})
		}
	}
}

// shedReport paces the shedding log lines. The first shed request is logged
// at once; those shed within every of a line are held and logged together
// when that interval ends. So the log has at most one line per interval, and
// every shed request is counted in one, the last of a burst included.
type shedReport struct {
	every time.Duration
	log   func(shed int)
	// now and after are time.Now and time.AfterFunc; tests replace them.
	now   func() time.Time
	after func(time.Duration, func())

	mu        sync.Mutex
	last      time.Time // when the last line was logged
	held      int       // shed since then, not logged yet
	scheduled bool      // a flush of the held ones is due
}

func newShedReport(every time.Duration, log func(shed int)) *shedReport {
	return &shedReport{every: every, log: log, now: time.Now,
		after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// add counts one shed request. It is logged at once when no line was logged
// within every; otherwise it is held for the flush at the interval's end.
func (p *shedReport) add() {
	p.mu.Lock()
	p.held++
	if p.scheduled {
		p.mu.Unlock()
		return
	}
	now := p.now()
	if wait := p.every - now.Sub(p.last); !p.last.IsZero() && wait > 0 {
		p.scheduled = true
		p.mu.Unlock()
		p.after(wait, p.flush)
		return
	}
	n := p.take(now)
	p.mu.Unlock()
	p.log(n)
}

// flush logs the held shed requests, at the end of an interval.
func (p *shedReport) flush() {
	p.mu.Lock()
	p.scheduled = false
	n := p.take(p.now())
	p.mu.Unlock()
	if n > 0 {
		p.log(n)
	}
}

// take returns the held count and starts a new interval at now. p.mu is held.
func (p *shedReport) take(now time.Time) int {
	n := p.held
	p.held, p.last = 0, now
	return n
}

// Ping reports whether db answers, for a Ready func. A pool whose every
// connection is in use has reached SQL Server already and is not pinged: the
// ping would wait for a connection to free up, and when SQL Server is slow
// that wait outlasts the readiness probe, taking the pod out of its Service
// just when the in-flight limiter is shedding load. A request can take the
// last free connection between the check and the ping, but while requests
// queue for a connection the pool stays full and the check holds; without a
// queue, the ping's wait is short.
func Ping(ctx context.Context, db *sql.DB) error {
	if st := db.Stats(); st.MaxOpenConnections > 0 && st.InUse >= st.MaxOpenConnections {
		return nil
	}
	return db.PingContext(ctx)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.Ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// call is one request: who made it, under which Request_Key, since when.
type call struct {
	RequestKey string
	Session    string
	Username   string
	UserID     int
	ClientIP   string
	Start      time.Time
	module     module
	entryPoint string
	action     string
	// name is the entry point's name in EntryPoints.
	name string
	// slowSQL counts the slow-query rows the call wrote.
	slowSQL int
}

func (s *Server) begin(r *http.Request, m module, entryPoint, action, name string) *call {
	c := &call{
		RequestKey: newGUID(),
		Session:    r.Header.Get(HeaderSession),
		Username:   r.Header.Get(HeaderUser),
		ClientIP:   clientIP(r),
		Start:      s.now(),
		module:     m,
		entryPoint: entryPoint,
		action:     action,
		name:       name,
	}
	if c.Session == "" {
		c.Session = newSessionID()
	}
	return c
}

// identify resolves the user against the business database, as the
// platform's session does. Anonymous callers are user 0.
func (s *Server) identify(ctx context.Context, c *call) error {
	if c.Username == "" {
		return nil
	}
	id, err := s.Store.Customer(ctx, c.Username)
	c.UserID = id
	return err
}

// injectFaults applies the scenario faults switched on for the call's
// entry point: a delay, or an injected failure.
func (s *Server) injectFaults(ctx context.Context, c *call) error {
	return s.Faults.Apply(ctx, c.name)
}

// observe counts the call for the scenario run, if one is active.
func (s *Server) observe(c *call, end time.Time, err error) {
	s.Traffic.Observe(traffic.Request{
		EntryPoint: c.name, User: c.Username, Session: c.Session, Duration: end.Sub(c.Start),
		Failed: err != nil, Injected: errors.Is(err, inject.ErrInjected), SlowSQL: c.slowSQL,
	})
}

// query runs one business statement of the call, named as in
// shop.Statements. A sql-latency fault that hits the call makes it run longer
// in SQL Server. When it succeeds after more than SlowSQL, query returns
// the platform's slow-query row for it.
func (s *Server) query(ctx context.Context, c *call, statement string, run func(context.Context) error) ([]oslog.Row, error) {
	ctx = shop.WithDelay(ctx, s.Faults.SQLDelay(c.name))
	if s.SlowSQL <= 0 {
		return nil, run(ctx)
	}
	start := s.now()
	err := run(ctx)
	end := s.now()
	took := end.Sub(start)
	if err != nil || took < s.SlowSQL {
		return nil, err
	}
	c.slowSQL++
	cols := c.common()
	cols["Instant"] = end
	cols["Session_Id"] = c.Session
	cols["Client_IP"] = c.ClientIP
	cols["Message"] = fmt.Sprintf("%s took %d ms", shop.Statements[statement], took.Milliseconds())
	cols["Message_Type"] = "Info"
	cols["Module_Name"] = "SLOWSQL"
	cols["Action_Name"] = c.action
	cols["Entrypoint_Name"] = c.entryPoint
	cols["Error_Id"] = ""
	return []oslog.Row{{Type: "general", Columns: c.user(cols)}}, nil
}

// common are the columns every log row of the call carries.
func (c *call) common() map[string]any {
	return map[string]any{
		"Request_Key":      c.RequestKey,
		"Espace_Id":        c.module.ID,
		"Espace_Name":      c.module.Name,
		"Tenant_Id":        TenantID,
		"Application_Name": Application,
		"Application_Key":  ApplicationKey,
	}
}

func (c *call) user(cols map[string]any) map[string]any {
	if c.Username != "" {
		cols["User_Id"] = c.UserID
		cols["Username"] = c.Username
	} else {
		cols["User_Id"] = 0
	}
	return cols
}

// errorRow is the error log row for a failed call. Its Id links the rows of
// the other log types that have an Error_Id.
func (s *Server) errorRow(c *call, at time.Time, err error) (oslog.Row, string) {
	id := newGUID()
	cols := c.common()
	cols["Id"] = id
	cols["Instant"] = at
	cols["Session_Id"] = c.Session
	cols["Message"] = err.Error()
	cols["Stack"] = stack(c, err)
	cols["Module_Name"] = c.module.Name
	cols["Server"] = s.FrontEnd
	cols["Entrypoint_Name"] = c.entryPoint
	cols["Action_Name"] = c.action
	// eSpaceVer is the module's running version, which a deploy fault
	// changes.
	cols["EnvironmentInformation"] = fmt.Sprintf("eSpaceVer: %d (Id=%d), Module: %s, FrontEnd: %s",
		s.Faults.Version(c.module.Name), c.module.ID, c.module.Name, s.FrontEnd)
	return oslog.Row{Type: "error", Columns: c.user(cols)}, id
}

// dataProviderFrames are the innermost frames of a failed database call, as
// the .NET SQL Server data provider throws it. With them a trace runs past
// the 2000 characters the error cycle table's Stack keeps, so the writer
// stores it in oslog_Error_Detail too, as the platform does with long
// traces.
var dataProviderFrames = []string{
	"at System.Data.SqlClient.SqlConnection.OnError(SqlException exception, Boolean breakConnection, Action`1 wrapCloseInAction)",
	"at System.Data.SqlClient.SqlInternalConnection.OnError(SqlException exception, Boolean breakConnection, Action`1 wrapCloseInAction)",
	"at System.Data.SqlClient.TdsParser.ThrowExceptionAndWarning(TdsParserStateObject stateObj, Boolean callerHasConnectionLock, Boolean asyncClose)",
	"at System.Data.SqlClient.TdsParser.TryRun(RunBehavior runBehavior, SqlCommand cmdHandler, SqlDataReader dataStream, BulkCopySimpleResultSet bulkCopyHandler, TdsParserStateObject stateObj, Boolean& dataReady)",
	"at System.Data.SqlClient.SqlDataReader.TryConsumeMetaData()",
	"at System.Data.SqlClient.SqlDataReader.get_MetaData()",
	"at System.Data.SqlClient.SqlCommand.FinishExecuteReader(SqlDataReader ds, RunBehavior runBehavior, String resetOptionsString, Boolean isInternal, Boolean forDescribeParameterEncryption, Boolean shouldCacheForAlwaysEncrypted)",
	"at System.Data.SqlClient.SqlCommand.RunExecuteReaderTds(CommandBehavior cmdBehavior, RunBehavior runBehavior, Boolean returnStream, Boolean async, Int32 timeout, Task& task, Boolean asyncWrite, Boolean inRetry, SqlDataReader ds, Boolean describeParameterEncryptionRequest)",
	"at System.Data.SqlClient.SqlCommand.RunExecuteReader(CommandBehavior cmdBehavior, RunBehavior runBehavior, Boolean returnStream, String method, TaskCompletionSource`1 completion, Int32 timeout, Task& task, Boolean& usedCache, Boolean asyncWrite, Boolean inRetry)",
	"at System.Data.SqlClient.SqlCommand.RunExecuteReader(CommandBehavior cmdBehavior, RunBehavior runBehavior, Boolean returnStream, String method)",
	"at System.Data.SqlClient.SqlCommand.ExecuteReader(CommandBehavior behavior, String method)",
	"at System.Data.SqlClient.SqlCommand.ExecuteDbDataReader(CommandBehavior behavior)",
	"at System.Data.Common.DbCommand.System.Data.IDbCommand.ExecuteReader(CommandBehavior behavior)",
	"at System.Data.Common.DbDataAdapter.FillInternal(DataSet dataset, DataTable[] datatables, Int32 startRecord, Int32 maxRecords, String srcTable, IDbCommand command, CommandBehavior behavior)",
	"at System.Data.Common.DbDataAdapter.Fill(DataSet dataSet, Int32 startRecord, Int32 maxRecords, String srcTable, IDbCommand command, CommandBehavior behavior)",
	"at System.Data.Common.DbDataAdapter.Fill(DataSet dataSet)",
}

// stack is the error's stack trace: the module's action and entry point,
// under the data provider's frames when the database failed. A refused
// order (shop.ErrInvalid) and a scenario's injected failure are the
// action's own.
func stack(c *call, err error) string {
	var frames []string
	if !errors.Is(err, shop.ErrInvalid) && !errors.Is(err, inject.ErrInjected) {
		frames = append(frames, dataProviderFrames...)
	}
	frames = append(frames,
		fmt.Sprintf("at %s.%s()", c.module.Name, c.action),
		fmt.Sprintf("at %s.%s()", c.module.Name, c.entryPoint))
	return strings.Join(frames, "\n")
}

// finish writes the call's log rows, which end at end. A failed write is
// logged and does not change the response: the platform's log service is
// asynchronous too. A log-flood fault adds general rows; a commit-delay
// fault holds the rows back for a while, after the response, as a log
// service that has fallen behind commits them late.
func (s *Server) finish(ctx context.Context, c *call, end time.Time, rows []oslog.Row) {
	rows = append(rows, s.flood(c, end)...)
	if d := s.Faults.CommitDelay(); d > 0 {
		go func() {
			t := time.NewTimer(d)
			defer t.Stop()
			<-t.C
			s.write(context.Background(), c, rows, d)
		}()
		return
	}
	s.write(context.WithoutCancel(ctx), c, rows, 0)
}

// write commits rows, and counts them in the Ledger and the run's traffic
// once they are committed; held is how long a commit-delay fault held them
// back.
func (s *Server) write(ctx context.Context, c *call, rows []oslog.Row, held time.Duration) {
	if err := s.Logs.Write(ctx, rows); err != nil {
		s.logger().Error("writing log rows failed", "request_key", c.RequestKey, "entry_point", c.entryPoint, "err", err)
		return
	}
	s.Ledger.Add(rows, held)
	committed := map[string]int64{}
	for _, r := range rows {
		committed[r.Type]++
	}
	s.Traffic.Committed(committed)
}

// flood is the general rows a log-flood fault adds to the call: verbose
// LogMessage calls of its module, each Message bytes long.
func (s *Server) flood(c *call, at time.Time) []oslog.Row {
	n, size := s.Faults.Flood(c.name)
	rows := make([]oslog.Row, 0, n)
	for i := 1; i <= n; i++ {
		msg := fmt.Sprintf("%s: step %d of %d ", c.action, i, n)
		if len(msg) < size {
			msg += strings.Repeat(".", size-len(msg))
		}
		cols := c.common()
		cols["Instant"] = at
		cols["Session_Id"] = c.Session
		cols["Client_IP"] = c.ClientIP
		cols["Message"] = msg[:min(len(msg), size)]
		cols["Message_Type"] = "Trace"
		cols["Module_Name"] = c.module.Name
		cols["Action_Name"] = c.action
		cols["Entrypoint_Name"] = c.entryPoint
		cols["Error_Id"] = ""
		rows = append(rows, oslog.Row{Type: "general", Columns: c.user(cols)})
	}
	return rows
}

// Traditional web screen: OrderList, the user's newest orders.
func (s *Server) orderList(w http.ResponseWriter, r *http.Request) {
	c := s.begin(r, webModule, "OrderList", "Preparation", EntryOrderList)
	var orders []shop.Order
	var slow []oslog.Row
	err := s.identify(r.Context(), c)
	if err == nil {
		err = s.injectFaults(r.Context(), c)
	}
	if err == nil {
		slow, err = s.query(r.Context(), c, "Orders", func(ctx context.Context) (err error) {
			orders, err = s.Store.Orders(ctx, c.UserID, ordersShown)
			return err
		})
	}
	var page strings.Builder
	if err == nil {
		err = orderListPage.Execute(&page, struct {
			User   string
			Orders []shop.Order
		}{c.Username, orders})
	}
	end := s.now()

	// The screen log's Instant is when the request started.
	screen := c.common()
	screen["Instant"] = c.Start
	screen["Duration"] = millis(end.Sub(c.Start))
	screen["Screen"] = c.entryPoint
	screen["Screen_Type"] = "Web"
	screen["Access_Mode"] = "Navigate"
	screen["Action_Name"] = c.action
	screen["Session_Id"] = c.Session
	screen["Client_IP"] = c.ClientIP
	screen["Executed_By"] = s.FrontEnd
	screen["Viewstate_Bytes"] = 0
	rows := append([]oslog.Row{{Type: "screen", Columns: c.user(screen)}}, slow...)
	if err != nil {
		row, _ := s.errorRow(c, end, err)
		rows = append(rows, row)
	}
	s.finish(r.Context(), c, end, rows)
	s.observe(c, end, err)

	if err != nil {
		http.Error(w, "An error occurred processing your request.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page.String()))
}

var orderListPage = template.Must(template.New("OrderList").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Orders</title></head><body>
<h1>Orders{{with .User}} of {{.}}{{end}}</h1>
<table>{{range .Orders}}
<tr><td>{{.ID}}</td><td>{{.Product}}</td><td>{{.Quantity}}</td><td>{{printf "%.2f" .Total}}</td></tr>{{else}}
<tr><td>No orders yet.</td></tr>{{end}}
</table></body></html>
`))

// placeOrderInput is the server action's input, as the Reactive client posts
// it.
type placeOrderInput struct {
	ProductID int `json:"productId"`
	Quantity  int `json:"quantity"`
}

// Reactive server action: Checkout's PlaceOrder.
func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	c := s.begin(r, appModule, "Checkout", "PlaceOrder", EntryPlaceOrder)
	var in placeOrderInput
	var order shop.Order
	var slow []oslog.Row
	err := s.identify(r.Context(), c)
	if err == nil {
		if derr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); derr != nil {
			err = fmt.Errorf("%w: %v", shop.ErrInvalid, derr)
		}
	}
	if err == nil {
		err = s.injectFaults(r.Context(), c)
	}
	if err == nil {
		slow, err = s.query(r.Context(), c, "PlaceOrder", func(ctx context.Context) (err error) {
			order, err = s.Store.PlaceOrder(ctx, c.UserID, in.ProductID, in.Quantity)
			return err
		})
	}
	end := s.now()

	rows := slow
	errID := ""
	if err != nil {
		var row oslog.Row
		row, errID = s.errorRow(c, end, err)
		rows = append(rows, row)
	}
	// The mobile request log's Instant is when the request completed; its
	// Duration is in seconds, as the OutSystems reference documents it.
	req := c.common()
	req["Id"] = newGUID()
	req["Instant"] = end
	req["Duration"] = int64(end.Sub(c.Start).Round(time.Second) / time.Second)
	req["Screen"] = c.entryPoint
	req["Endpoint"] = c.action
	req["Source"] = c.ClientIP
	req["Executed_By"] = s.FrontEnd
	req["Login_Id"] = c.Session
	req["Error_Id"] = errID
	rows = append([]oslog.Row{{Type: "mobile_request", Columns: c.user(req)}}, rows...)
	s.finish(r.Context(), c, end, rows)
	s.observe(c, end, err)

	if err != nil {
		writeServerActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"Order": order}})
}

// Exposed REST API: GET orders of the calling user.
func (s *Server) apiOrders(w http.ResponseWriter, r *http.Request) {
	c := s.begin(r, apiModule, "Orders", "GetOrders", EntryGetOrders)
	var orders []shop.Order
	var slow []oslog.Row
	err := s.identify(r.Context(), c)
	if err == nil {
		err = s.injectFaults(r.Context(), c)
	}
	if err == nil {
		slow, err = s.query(r.Context(), c, "Orders", func(ctx context.Context) (err error) {
			orders, err = s.Store.Orders(ctx, c.UserID, ordersShown)
			return err
		})
	}
	end := s.now()

	rows := slow
	errID := ""
	if err != nil {
		var row oslog.Row
		row, errID = s.errorRow(c, end, err)
		rows = append(rows, row)
	}
	// The integration log has no user or session: the API's own LogMessage
	// carries them, as an app that logs its callers does.
	general := c.common()
	general["Instant"] = end
	general["Session_Id"] = c.Session
	general["Client_IP"] = c.ClientIP
	general["Message"] = fmt.Sprintf("GetOrders returned %d orders", len(orders))
	general["Message_Type"] = "Orders"
	general["Module_Name"] = c.module.Name
	general["Action_Name"] = c.action
	general["Entrypoint_Name"] = c.entryPoint
	general["Error_Id"] = errID
	// The integration log's Instant is when the call completed. Source is
	// the caller of an exposed API; Endpoint is empty for exposed ones.
	integration := c.common()
	integration["Id"] = newGUID()
	integration["Instant"] = end
	integration["Duration"] = millis(end.Sub(c.Start))
	integration["Source"] = c.ClientIP
	integration["Endpoint"] = ""
	integration["Action"] = c.action
	integration["Type"] = "REST"
	integration["Executed_By"] = s.FrontEnd
	integration["Is_Expose"] = true
	integration["Error_Id"] = errID
	rows = append([]oslog.Row{
		{Type: "general", Columns: c.user(general)},
		{Type: "integration", Columns: integration},
	}, rows...)
	s.finish(r.Context(), c, end, rows)
	s.observe(c, end, err)

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"Errors": "Internal Server Error"})
		return
	}
	writeJSON(w, http.StatusOK, orders)
}

// writeServerActionError answers a failed server action. A refused order
// (shop.ErrInvalid) is a 400 with a fixed client message; any other error is a
// 500 with a generic message, so internal error text (JSON-decoder messages,
// raw SQL Server/driver errors) is never reflected to the caller. The detail
// still reaches the OutSystems error log row the handler writes.
func writeServerActionError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "Internal Server Error"
	if errors.Is(err, shop.ErrInvalid) {
		status, message = http.StatusBadRequest, "The request could not be processed."
	}
	writeJSON(w, status, map[string]any{"exception": map[string]string{"name": "ServerException", "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func millis(d time.Duration) int64 { return d.Milliseconds() }

// clientIP is the first X-Forwarded-For address, as the platform logs the
// original caller behind a load balancer, or the connection's peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newGUID is a random (version 4) GUID, the form of Request_Key and Id.
func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newSessionID looks like an ASP.NET session id: 24 lower-case characters.
func newSessionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz012345"
	var b [24]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[b[i]%32]
	}
	return string(b[:])
}
