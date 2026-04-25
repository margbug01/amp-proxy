package customproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key", nil, false)
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

// TestModifyResponse_UpgradesAndCollapsesAnthropicMessages exercises the
// full non-streaming-to-streaming upgrade path end-to-end:
//  1. client sends a non-streaming POST /api/provider/anthropic/v1/messages
//  2. customproxy's Director rewrites the body with "stream":true and sets
//     Accept: text/event-stream
//  3. the fake upstream sees the upgraded body, returns an Anthropic
//     Messages SSE stream
//  4. customproxy's ModifyResponse collapses the SSE into a single JSON
//     assistant message that the client receives as if the upstream had
//     replied non-streaming correctly
func TestModifyResponse_UpgradesAndCollapsesAnthropicMessages(t *testing.T) {
	// Canonical augment-style SSE fixture: single text block "Hello world".
	const sseFixture = `event: message_start
data: {"type":"message_start","message":{"id":"resp_e2e","type":"message","role":"assistant","model":"gpt-5.4-mini","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

	var sawStreamTrue bool
	var sawAcceptSSE bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("upstream path: got %s, want .../v1/messages", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if strings.Contains(string(body), `"stream":true`) {
			sawStreamTrue = true
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			sawAcceptSSE = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseFixture))
	}))
	defer upstream.Close()

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key", nil, false)
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	outer := httptest.NewServer(proxy)
	defer outer.Close()

	// Client sends a non-streaming request (no stream field).
	clientBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"say hi"}]}`
	req, err := http.NewRequest(http.MethodPost, outer.URL+"/api/provider/anthropic/v1/messages", strings.NewReader(clientBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if !sawStreamTrue {
		t.Errorf("upstream did not receive stream:true in body")
	}
	if !sawAcceptSSE {
		t.Errorf("upstream did not receive Accept: text/event-stream")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("client Content-Type: got %q, want application/json", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	// Validate reconstructed message.
	if id := gjson.GetBytes(body, "id").String(); id != "resp_e2e" {
		t.Errorf("id: got %q, want resp_e2e", id)
	}
	if text := gjson.GetBytes(body, "content.0.text").String(); text != "Hello world" {
		t.Errorf("content[0].text: got %q, want %q\nraw: %s", text, "Hello world", body)
	}
	if reason := gjson.GetBytes(body, "stop_reason").String(); reason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn", reason)
	}
}

// TestModifyResponse_PassesThroughAlreadyStreamingMessages verifies we do
// not touch a request that the client already marked stream:true, and that
// the upstream SSE body is not rewritten by sseRewriter on its way back.
func TestModifyResponse_PassesThroughAlreadyStreamingMessages(t *testing.T) {
	const sseFixture = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n"

	var sawStreamTrue bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			sawStreamTrue = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseFixture))
	}))
	defer upstream.Close()

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key", nil, false)
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	outer := httptest.NewServer(proxy)
	defer outer.Close()

	clientBody := `{"model":"claude-sonnet-4-6","stream":true,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, outer.URL+"/api/provider/anthropic/v1/messages", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if !sawStreamTrue {
		t.Errorf("upstream should have received stream:true verbatim")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("client Content-Type: got %q, want text/event-stream", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "message_start") {
		t.Errorf("client did not receive raw SSE body; got: %s", body)
	}
}

// TestModifyResponse_QuietOnPopulatedAnthropicContent guards against a false
// positive: a normal Anthropic Messages response with real text blocks must
// not trigger the warning.
func TestModifyResponse_ErrorHandlerReturnsSanitizedJSON(t *testing.T) {
	secretErr := `dial tcp 10.0.0.1:443: api_key=secret"bad`
	proxy, err := buildProxy("http://127.0.0.1:1/v1", "test-key", nil, false)
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()
	proxy.ErrorHandler(w, req, errors.New(secretErr))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusBadGateway)
	}
	if !gjson.Valid(w.Body.String()) {
		t.Fatalf("body is not valid JSON: %q", w.Body.String())
	}
	if gjson.Get(w.Body.String(), "error").String() != "customproxy_upstream_error" {
		t.Fatalf("unexpected error payload: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretErr) || strings.Contains(w.Body.String(), "detail") {
		t.Fatalf("error response leaked detail: %s", w.Body.String())
	}
}

func TestModifyResponse_PreservesOverLimitInspectionBody(t *testing.T) {
	const maxInspect = 10 * 1024 * 1024
	prefix := `{"output":[],"usage":{"output_tokens":1},"pad":"`
	suffix := `"}`
	totalLen := maxInspect + 1024
	if len(prefix)+len(suffix) >= totalLen {
		t.Fatal("test fixture prefix/suffix too large")
	}
	upstreamBody := prefix + strings.Repeat("x", totalLen-len(prefix)-len(suffix)) + suffix

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key", nil, false)
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	outer := httptest.NewServer(proxy)
	defer outer.Close()

	resp, err := http.Post(outer.URL+"/api/provider/openai/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(body) != upstreamBody {
		t.Fatalf("over-limit inspection body was not preserved: got %d bytes want %d", len(body), len(upstreamBody))
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length header: got %q, want empty for skipped inspection", got)
	}
}

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

	proxy, err := buildProxy(upstream.URL+"/v1", "test-key", nil, false)
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
