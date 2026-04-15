package customproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/margbug01/amp-proxy/internal/config"
)

// TestGeminiTranslateEndToEnd exercises the full Gemini translate path:
// a Gemini generateContent request body is fed through
// TranslateGeminiRequestToOpenAI, the resulting body is sent through a live
// ReverseProxy built by the customproxy package, a fake augment server
// answers with a real /v1/responses SSE stream, and ModifyResponse
// translates the reply back into a Gemini generateContent JSON body.
//
// The fake augment verifies every part of the outgoing request shape so
// any regression in the request translator fails this test loudly.
func TestGeminiTranslateEndToEnd(t *testing.T) {
	// Build the fake augment server. It asserts on the incoming OpenAI
	// Responses request and replies with a trimmed SSE stream that mirrors
	// what the real augment endpoint emits.
	augment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("augment: method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("augment: path = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer augment-test-key" {
			t.Errorf("augment: Authorization = %q", got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("augment: read body: %v", err)
		}
		_ = r.Body.Close()

		if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4-mini" {
			t.Errorf("augment: model = %q, want gpt-5.4-mini (suffix stripped)", got)
		}
		if got := gjson.GetBytes(body, "stream").Bool(); !got {
			t.Error("augment: stream must be true")
		}
		if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
			t.Errorf("augment: reasoning.effort = %q, want high", got)
		}
		if got := gjson.GetBytes(body, "tools.0.type").String(); got != "function" {
			t.Errorf("augment: tools[0].type = %q, want function", got)
		}
		if got := gjson.GetBytes(body, "tools.0.parameters.type").String(); got != "object" {
			t.Errorf("augment: tools[0].parameters.type = %q, want lowercased 'object'", got)
		}
		if got := gjson.GetBytes(body, "tools.0.parameters.properties.filePattern.type").String(); got != "string" {
			t.Errorf("augment: filePattern.type = %q, want lowercased 'string'", got)
		}
		if bytes.Contains(body, []byte("thoughtSignature")) {
			t.Error("augment: thoughtSignature must be stripped before forwarding")
		}

		// Emit a trimmed SSE stream: one reasoning item (which must be
		// dropped downstream), one function_call, and a completed event
		// with usage numbers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeSSE := func(event string) {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeSSE("event: response.created\n")
		writeSSE("data: {\"type\":\"response.created\",\"sequence_number\":0}\n\n")
		writeSSE("event: response.output_item.done\n")
		writeSSE("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"hidden\"}]},\"output_index\":0,\"sequence_number\":1}\n\n")
		writeSSE("event: response.output_item.done\n")
		writeSSE("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"glob\",\"call_id\":\"call_xyz\",\"arguments\":\"{\\\"filePattern\\\":\\\"**/tailwind.config.*\\\"}\"},\"output_index\":1,\"sequence_number\":2}\n\n")
		writeSSE("event: response.completed\n")
		writeSSE("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":50,\"output_tokens\":10,\"total_tokens\":60}},\"sequence_number\":3}\n\n")
	}))
	defer augment.Close()

	// Build a customproxy Registry backed by the fake augment. Configure()
	// runs buildProxy which wires up Director + ModifyResponse + retrying
	// transport in the same way production does.
	reg := &Registry{byModel: map[string]*Provider{}}
	if err := reg.Configure([]config.CustomProvider{
		{
			Name:   "fake-augment",
			URL:    augment.URL + "/v1",
			APIKey: "augment-test-key",
			Models: []string{"gpt-5.4-mini"},
		},
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	proxy := reg.ProxyForModel("gpt-5.4-mini(high)")
	if proxy == nil {
		t.Fatal("ProxyForModel returned nil for gpt-5.4-mini(high)")
	}

	// Build a Gemini generateContent request exactly like Amp CLI finder does.
	geminiBody, err := TranslateGeminiRequestToOpenAI([]byte(geminiSingleTurnRequest), "gpt-5.4-mini(high)")
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/provider/google/v1beta1/publishers/google/models/gemini-3-flash-preview:generateContent",
		bytes.NewReader(geminiBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.URL.Path = "/v1/responses"
	req.ContentLength = int64(len(geminiBody))
	req = req.WithContext(WithGeminiTranslate(req.Context(), "gemini-3-flash-preview"))

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("response Content-Type = %q, want application/json", ct)
	}

	respBody := rec.Body.Bytes()
	var parsed map[string]any
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%s", err, respBody)
	}

	parts := gjson.GetBytes(respBody, "candidates.0.content.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (function_call only, reasoning dropped); full body=%s", len(parts), respBody)
	}
	if got := parts[0].Get("functionCall.name").String(); got != "glob" {
		t.Errorf("parts[0].functionCall.name = %q", got)
	}
	if got := parts[0].Get("functionCall.args.filePattern").String(); got != "**/tailwind.config.*" {
		t.Errorf("filePattern = %q", got)
	}
	if got := gjson.GetBytes(respBody, "usageMetadata.promptTokenCount").Int(); got != 50 {
		t.Errorf("promptTokenCount = %d", got)
	}
	if got := gjson.GetBytes(respBody, "usageMetadata.candidatesTokenCount").Int(); got != 10 {
		t.Errorf("candidatesTokenCount = %d", got)
	}
	if got := gjson.GetBytes(respBody, "candidates.0.finishReason").String(); got != "STOP" {
		t.Errorf("finishReason = %q", got)
	}
	if got := gjson.GetBytes(respBody, "modelVersion").String(); got != "gemini-3-flash-preview" {
		t.Errorf("modelVersion = %q", got)
	}
}
