package audittest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// Collector is a fake OTel Collector's OTLP/HTTP logs receiver: it keeps
// the requests it accepts, and answers each with the status Status returns.
type Collector struct {
	// URL is its base URL, the endpoint a drainer is given.
	URL string

	mu       sync.Mutex
	status   func(n int) int
	attempts int
	accepted []*logspb.LogsData
}

// NewCollector starts a Collector that accepts everything, and stops it
// when the test ends.
func NewCollector(t testing.TB) *Collector {
	c := &Collector{status: func(int) int { return http.StatusOK }}
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	c.URL = server.URL
	return c
}

// Answer makes the Collector answer the n-th request (from 1) with status(n);
// a 2xx accepts it.
func (c *Collector) Answer(status func(n int) int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

// ServeHTTP implements [http.Handler].
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/logs" || r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "want POST /v1/logs of application/x-protobuf", http.StatusBadRequest)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := &logspb.LogsData{}
	if err := proto.Unmarshal(raw, data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.attempts++
	status := c.status(c.attempts)
	if status >= 200 && status < 300 {
		c.accepted = append(c.accepted, data)
	}
	c.mu.Unlock()
	w.WriteHeader(status)
}

// Attempts returns how many requests it received.
func (c *Collector) Attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// Accepted returns the requests it accepted, in order.
func (c *Collector) Accepted() []*logspb.LogsData {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*logspb.LogsData(nil), c.accepted...)
}

// Records returns the log records of every accepted request, in order.
func (c *Collector) Records() []*logspb.LogRecord {
	var records []*logspb.LogRecord
	for _, data := range c.Accepted() {
		for _, rl := range data.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				records = append(records, sl.LogRecords...)
			}
		}
	}
	return records
}

// Attribute returns the string value of attribute key of record, or "".
func Attribute(record *logspb.LogRecord, key string) string {
	for _, kv := range record.Attributes {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}
