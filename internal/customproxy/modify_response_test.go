package customproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// captureLogrus redirects the standard logger to a buffer for the duration
// of a test. The returned cleanup restores the original sink and level.
func captureLogrus(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	oldOut := log.StandardLogger().Out
	oldLevel := log.StandardLogger().Level
	log.SetOutput(buf)
	log.SetLevel(log.WarnLevel)
	return buf, func() {
		log.SetOutput(oldOut)
		log.SetLevel(oldLevel)
	}
}

// TestIsJSONMessagesPath_RejectsCountTokensAndBatches protects against
// accidentally running the content-loss detector on sibling endpoints that
// legitimately return small or empty payloads.
func TestIsJSONMessagesPath_RejectsCountTokensAndBatches(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/messages", true},
		{"/v1/messages/count_tokens", false},
		{"/v1/messages/batches", false},
		{"/v1/messages/batches/batch_123", false},
		{"/v1/responses", false},
		{"", false},
	}
	for _, tc := range cases {
		u, err := url.Parse("http://example.com" + tc.path)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.path, err)
		}
		req := &http.Request{URL: u}
		if got := isJSONMessagesPath(req); got != tc.want {
			t.Errorf("isJSONMessagesPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestModifyResponse_WarnsOnAnthropicEmptyContent asserts that a non-streaming
// /v1/messages response whose `content` is an empty array but whose
// `usage.output_tokens` is non-zero triggers the augment content-loss warning
// while the body itself passes through unchanged to the client.
func TestModifyResponse_WarnsOnAnthropicEmptyContent(t *testing.T) {
	logBuf, restore := captureLogrus(t)
	defer restore()

	const upstreamBody = `{"type":"message","role":"assistant","model":"gpt-5.4-mini-2026-03-17","content":[],"stop_reason":"end_turn","usage":{"input_tokens":460,"output_tokens":76}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("upstream received unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	outer := httptest.NewServer(proxy)
	defer outer.Close()

	req, err := http.NewRequest(http.MethodPost, outer.URL+"/api/provider/anthropic/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != upstreamBody {
		t.Errorf("response body altered:\n got:  %s\n want: %s", got, upstreamBody)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "empty content array") {
		t.Errorf("expected warning about empty content array; got log:\n%s", logged)
	}
	if !strings.Contains(logged, "output_tokens=76") {
		t.Errorf("expected output_tokens=76 in log fields; got:\n%s", logged)
	}
	if !strings.Contains(logged, "gpt-5.4-mini-2026-03-17") {
		t.Errorf("expected upstream model in log fields; got:\n%s", logged)
	}
}

// TestModifyResponse_QuietOnPopulatedAnthropicContent guards against a false
// positive: a normal Anthropic Messages response with real text blocks must
// not trigger the warning.
func TestModifyResponse_QuietOnPopulatedAnthropicContent(t *testing.T) {
	logBuf, restore := captureLogrus(t)
	defer restore()

	const upstreamBody = `{"type":"message","role":"assistant","model":"gpt-5.4-mini","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	outer := httptest.NewServer(proxy)
	defer outer.Close()

	req, _ := http.NewRequest(http.MethodPost, outer.URL+"/api/provider/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if logged := logBuf.String(); strings.Contains(logged, "empty content array") {
		t.Errorf("warning fired on healthy response:\n%s", logged)
	}
}

