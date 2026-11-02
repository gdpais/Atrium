package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusRecorder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    int
	}{
		{"body without a status", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }, http.StatusOK},
		{"nothing written", func(http.ResponseWriter, *http.Request) {}, http.StatusOK},
		{"explicit status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, http.StatusServiceUnavailable},
		{"second status ignored", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusCreated},
		{"informational status first", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusAccepted)
		}, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
			tc.handler(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.status != tc.want {
				t.Fatalf("status = %d, want %d", recorder.status, tc.want)
			}
		})
	}
}

func TestStatusRecorderKeepsTheResponseController(t *testing.T) {
	// http.ResponseController reaches the real writer through Unwrap, so a
	// handler behind the metrics can still flush a streamed response.
	inner := httptest.NewRecorder()
	recorder := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	if err := http.NewResponseController(recorder).Flush(); err != nil {
		t.Fatalf("Flush through the recorder: %v", err)
	}
	if !inner.Flushed {
		t.Fatal("the underlying writer was not flushed")
	}
}
