// Package otlp sends logs to an OpenTelemetry Collector over OTLP/HTTP with
// protobuf encoding.
//
// [Client.Export] posts to <endpoint>/v1/logs, gzip-compressed, splitting the
// records into requests of at most [Client.MaxRequestBytes] of protobuf.  It
// retries a request that failed with a network error, 429 or a 5xx status,
// with exponential backoff and Retry-After, until it is accepted or the
// context ends ([Client.MaxAttempts] can bound the attempts); any other
// status fails at once.  Export returns nil only when every request was
// accepted, so a caller that moves a watermark after it delivers each record
// at least once.
//
// A busy OTel Collector is waited for, not failed: the chart's Collector
// holds one request in its logs export queue and keeps the next one waiting
// in the receiver until the first is in Loki (block_on_overflow).  Each
// attempt is bounded by [DefaultRequestTimeout], below the Collector
// receiver's 30 s write timeout, so the client always gives up first: a
// request the Collector queued after the client had gone would be answered
// into a closed connection, and sent again.  An attempt that timed out is
// retried at once, since it already waited.
//
// The request body is a LogsData message, which is wire-compatible with
// ExportLogsServiceRequest: both are `repeated ResourceLogs resource_logs = 1`.
// Using it keeps the gRPC service packages, and gRPC, out of the build.
package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// LogsPath is appended to the endpoint, as OTLP/HTTP exporters do with a base
// endpoint such as http://otel-collector:4318.
const LogsPath = "/v1/logs"

const (
	// DefaultMaxRequestBytes bounds the protobuf size of one request, well
	// below the OTel Collector's default 20 MiB request body limit.
	DefaultMaxRequestBytes = 4 << 20
	// DefaultInitialBackoff is the wait before the first retry; it doubles up
	// to DefaultMaxBackoff.
	DefaultInitialBackoff = 250 * time.Millisecond
	// DefaultMaxBackoff caps the wait between attempts, Retry-After included.
	DefaultMaxBackoff = 5 * time.Second
	// DefaultRequestTimeout bounds one HTTP attempt.  It must stay below the
	// OTel Collector receiver's write timeout (30 s by default; see the
	// package documentation).
	DefaultRequestTimeout = 10 * time.Second
	// recordOverhead covers the tag and length prefix of a nested message.
	recordOverhead = 8
	// maxErrorBody is how much of a rejected response's body an error quotes.
	maxErrorBody = 512
)

// Client sends OTLP logs.  Its zero value is not usable; use [NewClient].  A
// Client is safe for concurrent use.
type Client struct {
	// HTTP sends the requests.
	HTTP *http.Client
	// MaxRequestBytes bounds the protobuf size of one request.  A single
	// record larger than it is sent alone.
	MaxRequestBytes int
	// MaxAttempts is how many times one request is tried; below 1, it is
	// tried until it is accepted, rejected for good, or the context ends.
	MaxAttempts int
	// InitialBackoff and MaxBackoff shape the wait between attempts.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Logger receives a warning when the receiver rejects some records of an
	// accepted request (OTLP partial success).  Nil means [slog.Default].
	Logger *slog.Logger
	// sleep waits for d or until ctx ends, reporting whether d elapsed.
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewClient returns a Client with the default limits.
func NewClient() *Client {
	return &Client{
		HTTP:            &http.Client{Timeout: DefaultRequestTimeout},
		MaxRequestBytes: DefaultMaxRequestBytes,
		InitialBackoff:  DefaultInitialBackoff,
		MaxBackoff:      DefaultMaxBackoff,
		sleep:           sleepContext,
	}
}

// Export sends resourceLogs to endpoint (an OTLP/HTTP base URL) and returns
// nil once every record was accepted.  Requests are sent in order and the
// first one that fails stops the export, so on error some records may have
// been delivered; delivering them again is safe for an at-least-once caller.
func (c *Client) Export(ctx context.Context, endpoint string, resourceLogs []*logspb.ResourceLogs) error {
	url := strings.TrimRight(endpoint, "/") + LogsPath
	for _, request := range Split(resourceLogs, c.MaxRequestBytes) {
		body, err := encode(request)
		if err != nil {
			return fmt.Errorf("encode OTLP logs: %w", err)
		}
		if err := c.send(ctx, url, body); err != nil {
			return err
		}
	}
	return nil
}

// send posts one encoded request, retrying as the package documentation
// describes.
func (c *Client) send(ctx context.Context, url string, body []byte) error {
	backoff := c.InitialBackoff
	for attempt := 1; ; attempt++ {
		retryAfter, err := c.post(ctx, url, body)
		if err == nil {
			return nil
		}
		var permanent *permanentError
		if errors.As(err, &permanent) || (c.MaxAttempts > 0 && attempt >= c.MaxAttempts) || ctx.Err() != nil {
			return fmt.Errorf("export logs to %s (%s): %w", url, c.attemptText(attempt), err)
		}
		if observe, ok := ctx.Value(retryObserverKey{}).(RetryObserver); ok && observe != nil {
			observe(attempt, err)
		}
		wait := min(max(backoff, retryAfter), c.MaxBackoff)
		if isTimeout(err) && retryAfter == 0 {
			wait = 0
		}
		if !c.wait(ctx, wait) {
			return fmt.Errorf("export logs to %s (%s): %w (%w)", url, c.attemptText(attempt), err, ctx.Err())
		}
		backoff = min(2*backoff, c.MaxBackoff)
	}
}

// RetryObserver is told about every failed attempt that will be retried.
type RetryObserver func(attempt int, err error)

type retryObserverKey struct{}

// WithRetryObserver returns a context under which [Client.Export] calls
// observe for every failed attempt it retries, so a caller that waits for an
// export can tell why it waits.
func WithRetryObserver(ctx context.Context, observe RetryObserver) context.Context {
	return context.WithValue(ctx, retryObserverKey{}, observe)
}

// attemptText describes attempt for an error message.
func (c *Client) attemptText(attempt int) string {
	if c.MaxAttempts > 0 {
		return fmt.Sprintf("attempt %d of %d", attempt, c.MaxAttempts)
	}
	return fmt.Sprintf("attempt %d", attempt)
}

// isTimeout reports whether err is, or wraps, a network timeout, such as the
// HTTP client's per-attempt timeout.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// permanentError is a response that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// post sends one attempt.  It returns the Retry-After delay of a retryable
// response, and a *permanentError for a status that is not retryable.
func (c *Client) post(ctx context.Context, url string, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, &permanentError{err}
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if rejected, message := partialSuccess(payload); rejected > 0 {
			c.logger().Warn("OTLP receiver rejected some log records", "endpoint", url, "rejected", rejected, "message", message)
		}
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("receiver answered %s", statusText(resp, payload))
	default:
		return 0, &permanentError{fmt.Errorf("receiver rejected the request: %s", statusText(resp, payload))}
	}
}

// wait sleeps for d unless ctx ends first.
func (c *Client) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func (c *Client) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// Split groups the records of resourceLogs into requests of at most limit
// bytes of protobuf, keeping each record with its resource and scope and the
// records in order.  A record that alone exceeds limit gets a request of its
// own.  A limit below 1 puts everything in one request.
func Split(resourceLogs []*logspb.ResourceLogs, limit int) [][]*logspb.ResourceLogs {
	if limit < 1 {
		return [][]*logspb.ResourceLogs{resourceLogs}
	}
	var requests [][]*logspb.ResourceLogs
	var current []*logspb.ResourceLogs
	size := 0
	for _, rl := range resourceLogs {
		resourceSize := proto.Size(&logspb.ResourceLogs{Resource: rl.Resource, SchemaUrl: rl.SchemaUrl}) + recordOverhead
		var currentResource *logspb.ResourceLogs
		for _, sl := range rl.ScopeLogs {
			scopeSize := proto.Size(&logspb.ScopeLogs{Scope: sl.Scope, SchemaUrl: sl.SchemaUrl}) + recordOverhead
			var currentScope *logspb.ScopeLogs
			for _, record := range sl.LogRecords {
				need := proto.Size(record) + recordOverhead
				if currentScope == nil {
					need += scopeSize
				}
				if currentResource == nil {
					need += resourceSize
				}
				if size > 0 && size+need > limit {
					requests = append(requests, current)
					current, size = nil, 0
					if currentScope != nil {
						need += scopeSize
					}
					if currentResource != nil {
						need += resourceSize
					}
					currentResource, currentScope = nil, nil
				}
				if currentResource == nil {
					currentResource = &logspb.ResourceLogs{Resource: rl.Resource, SchemaUrl: rl.SchemaUrl}
					current = append(current, currentResource)
				}
				if currentScope == nil {
					currentScope = &logspb.ScopeLogs{Scope: sl.Scope, SchemaUrl: sl.SchemaUrl}
					currentResource.ScopeLogs = append(currentResource.ScopeLogs, currentScope)
				}
				currentScope.LogRecords = append(currentScope.LogRecords, record)
				size += need
			}
		}
	}
	if len(current) > 0 {
		requests = append(requests, current)
	}
	return requests
}

// encode marshals one request and gzips it.
func encode(resourceLogs []*logspb.ResourceLogs) ([]byte, error) {
	raw, err := proto.Marshal(&logspb.LogsData{ResourceLogs: resourceLogs})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// partialSuccess reads ExportLogsServiceResponse.partial_success (field 1):
// rejected_log_records (field 1) and error_message (field 2).  An empty or
// unreadable body reports nothing rejected.
func partialSuccess(body []byte) (rejected int64, message string) {
	partial := field(body, 1, protowire.BytesType)
	if partial == nil {
		return 0, ""
	}
	if raw := field(partial, 1, protowire.VarintType); raw != nil {
		value, _ := protowire.ConsumeVarint(raw)
		rejected = int64(value)
	}
	return rejected, string(field(partial, 2, protowire.BytesType))
}

// field returns the last value of field number num of type typ in a protobuf
// message: the payload of a bytes field, or the encoded varint.  It returns
// nil when the field is absent or the message does not parse.
func field(message []byte, num protowire.Number, typ protowire.Type) []byte {
	var out []byte
	for len(message) > 0 {
		n, t, length := protowire.ConsumeTag(message)
		if length < 0 {
			return nil
		}
		message = message[length:]
		valueLength := protowire.ConsumeFieldValue(n, t, message)
		if valueLength < 0 {
			return nil
		}
		if n == num && t == typ {
			out = message[:valueLength]
			if typ == protowire.BytesType {
				out, _ = protowire.ConsumeBytes(out)
			}
		}
		message = message[valueLength:]
	}
	return out
}

// retryAfter parses a Retry-After header in seconds; dates and invalid
// values count as none.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// statusText describes a response by its status and the start of its body
// when that is text.
func statusText(resp *http.Response, body []byte) string {
	text := resp.Status
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/") || strings.Contains(resp.Header.Get("Content-Type"), "json") {
		snippet := strings.TrimSpace(string(body[:min(len(body), maxErrorBody)]))
		if snippet != "" {
			text += ": " + snippet
		}
	}
	return text
}

// sleepContext waits for d or until ctx ends, reporting whether d elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
