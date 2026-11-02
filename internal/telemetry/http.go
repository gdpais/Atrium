package telemetry

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// unmatchedRoute is the route label of a request no route matched: a 404,
// or a 405 for a known path with another method.
const unmatchedRoute = "unmatched"

// otherMethod is the method label of a request whose method is not one of
// knownMethods.
const otherMethod = "other"

// knownMethods are the request methods kept as method labels. Any other
// method is labelled otherMethod, so a client sending made-up methods adds no
// series.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// HTTPMetrics records the count and duration of the HTTP requests a service
// serves. A nil *HTTPMetrics records nothing.
type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewHTTPMetrics registers a service's HTTP request metrics with registerer,
// under the names it gives them: requests counts by method, route and code,
// duration is a histogram by method and route.
func NewHTTPMetrics(registerer prometheus.Registerer, requests, duration string) (*HTTPMetrics, error) {
	m := &HTTPMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: requests,
			Help: "HTTP requests served, by method, route and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    duration,
			Help:    "Time to serve an HTTP request, by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	for _, c := range []prometheus.Collector{m.requests, m.duration} {
		if err := registerer.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Instrument records every request next serves. next must be the
// [http.ServeMux] that routes the request, since the route label is the
// pattern it matched, without the method: "/readyz", later
// "/api/v1/applications/{id}". The labels come only from registered routes
// and knownMethods, so unknown paths and methods add no series.
func (m *HTTPMetrics) Instrument(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		method, route := requestLabels(r)
		m.requests.WithLabelValues(method, route, strconv.Itoa(recorder.status)).Inc()
		m.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	})
}

// requestLabels returns the method and route labels of r after a ServeMux
// served it. ServeMux sets r.Pattern to the pattern it matched, and leaves
// it empty when none did.
func requestLabels(r *http.Request) (method, route string) {
	method = r.Method
	if !knownMethods[method] {
		method = otherMethod
	}
	route = r.Pattern
	if _, path, ok := strings.Cut(route, " "); ok {
		route = path
	}
	if route == "" {
		route = unmatchedRoute
	}
	return method, route
}

// statusRecorder remembers the status code a handler writes; 200 when it
// writes a body without one.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	// A 1xx status is informational; the final one follows.
	if !r.wroteHeader && status >= http.StatusOK {
		r.status, r.wroteHeader = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets [http.ResponseController] reach the underlying writer, to
// flush or set deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
