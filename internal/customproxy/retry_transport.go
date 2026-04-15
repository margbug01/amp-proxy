package customproxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// retryingTransport wraps an http.RoundTripper with a single automatic retry
// on transient transport-level errors. The goal is to paper over short-lived
// upstream glitches (endpoint restarts, TLS handshake races, TCP resets)
// without surfacing them to the client.
//
// Retry is only attempted when RoundTrip returns a Go error; a response with
// any HTTP status (including 4xx/5xx) is always returned to the caller as-is
// because retrying an application-level error could double-bill, replay
// side effects, or corrupt partial streams.
type retryingTransport struct {
	base  http.RoundTripper
	delay time.Duration
}

// RoundTrip executes a single request and retries once if the first attempt
// fails with a transient error. The request body is buffered up-front so it
// can be replayed on the second attempt.
func (t *retryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body up-front: req.Body is single-read so we must keep a
	// copy for the potential retry. Also populate req.GetBody so net/http
	// can replay on 3xx redirects in the base transport.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if req.GetBody == nil {
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyBytes)), nil
			}
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isTransient(err) {
		return nil, err
	}

	log.WithFields(log.Fields{
		"host":  req.URL.Host,
		"err":   err.Error(),
		"delay": t.delay.String(),
	}).Info("customproxy: retrying after transient error")

	time.Sleep(t.delay)

	// Replay body for the second attempt. Leave GetBody in place.
	if bodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	return t.base.RoundTrip(req)
}

// isTransient reports whether err is a transport-level failure that we
// believe can be safely retried. Anything we don't recognize is returned
// to the caller to avoid masking bugs.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// dial / read failures during connection establishment or
		// header read are safe to replay because no bytes have been
		// committed to the client yet.
		if opErr.Op == "dial" || opErr.Op == "read" {
			return true
		}
	}
	return false
}
