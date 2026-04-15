package customproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetryingTransport_RetriesOnTransientError verifies that a transient
// connection failure on the first attempt is followed by a successful
// second attempt, and that the handler is invoked exactly twice.
func TestRetryingTransport_RetriesOnTransientError(t *testing.T) {
	var calls int32

	// Custom handler: the first call hijacks the connection and closes it
	// without writing a response, which produces an io.EOF / connection
	// reset at the client's RoundTrip. The second call returns 200 OK.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Hijack and drop without writing a response: the client
			// should observe an EOF-class transport error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("test server does not support Hijacker")
				http.Error(w, "no hijacker", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rt := &retryingTransport{
		base:  http.DefaultTransport,
		delay: 250 * time.Millisecond,
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RoundTrip after retry: got err %v, want nil", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("handler calls: got %d, want 2", got)
	}
	// Sanity check: retry delay should have elapsed at least roughly.
	// Allow a generous lower bound to avoid flaking on slow CI.
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed %v, expected at least ~250ms backoff", elapsed)
	}
}

// TestRetryingTransport_DoesNotRetryOn4xx verifies that HTTP status codes
// (which are application-level errors, not transport errors) are passed
// through without a retry. A 400 response must only invoke the handler once.
func TestRetryingTransport_DoesNotRetryOn4xx(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	rt := &retryingTransport{
		base:  http.DefaultTransport,
		delay: 250 * time.Millisecond,
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: unexpected err %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("handler calls: got %d, want 1 (4xx must not trigger retry)", got)
	}
}
