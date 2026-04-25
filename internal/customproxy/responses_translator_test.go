package customproxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/margbug01/amp-proxy/internal/bodylimit"
	"github.com/tidwall/gjson"
)

// Helper: marshal the translate output, parse with gjson, return pretty
// access to assert against specific fields.
func runTranslate(t *testing.T, req map[string]any) (out string, ctx *responsesTranslateCtx) {
	t.Helper()
	body, _ := json.Marshal(req)
	newBody, tctx, err := translateResponsesRequestToChat(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return string(newBody), tctx
}

func TestTranslateResponsesRequestBody_ErrorsOnOverLimitBody(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", io.NopCloser(&repeatedByteReader{remaining: 16*1024*1024 + 1, b: 'x'}))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	newBody, ctx, err := translateResponsesRequestBody(req)
	if err == nil {
		t.Fatal("translateResponsesRequestBody: expected over-limit error")
	}
	if !errors.Is(err, bodylimit.ErrTooLarge) {
		t.Fatalf("error = %v, want bodylimit.ErrTooLarge", err)
	}
	if newBody != nil {
		t.Fatalf("newBody len = %d, want nil", len(newBody))
	}
	if ctx != nil {
		t.Fatalf("ctx = %#v, want nil", ctx)
	}
}

func TestTranslateResponsesRequest_SimpleUserMessage(t *testing.T) {
	out, _ := runTranslate(t, map[string]any{
		"model":  "gpt-5.4",
		"stream": true,
		"input": []any{
			map[string]any{"role": "system", "content": "You are amp."},
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hi "},
					map[string]any{"type": "input_text", "text": "there"},
				},
			},
		},
		"reasoning":         map[string]any{"effort": "high", "summary": "auto"},
		"max_output_tokens": 1024,
	})

	if gjson.Get(out, "model").String() != "gpt-5.4" {
		t.Errorf("model: got %q", gjson.Get(out, "model").String())
	}
	if !gjson.Get(out, "stream").Bool() {
		t.Error("stream should be true")
	}
	if gjson.Get(out, "max_tokens").Int() != 1024 {
		t.Errorf("max_tokens: got %v", gjson.Get(out, "max_tokens").Int())
	}
	if gjson.Get(out, "reasoning_effort").String() != "high" {
		t.Errorf("reasoning_effort: got %q", gjson.Get(out, "reasoning_effort").String())
	}
	if gjson.Get(out, "thinking.type").String() != "enabled" {
		t.Errorf("thinking.type: got %q", gjson.Get(out, "thinking.type").String())
	}
	if gjson.Get(out, "reasoning.summary").Exists() {
		t.Error("Responses-only reasoning.summary should be dropped")
	}

	msgs := gjson.Get(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Get("role").String() != "system" || msgs[0].Get("content").String() != "You are amp." {
		t.Errorf("system message: %v", msgs[0])
	}
	if msgs[1].Get("role").String() != "user" {
		t.Errorf("user role: %q", msgs[1].Get("role").String())
	}
	// input_text parts concatenated.
	if msgs[1].Get("content").String() != "hi there" {
		t.Errorf("user content: %q", msgs[1].Get("content").String())
	}
}

func TestTranslateResponsesRequest_PreservesReasoningAndDropsResponsesOnly(t *testing.T) {
	out, _ := runTranslate(t, map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "reasoning", "id": "rs_x", "encrypted_content": "blob", "summary": []any{}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "hello"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "again"}}},
		},
		"include":          []any{"reasoning.encrypted_content"},
		"store":            false,
		"stream_options":   map[string]any{"include_obfuscation": false},
		"prompt_cache_key": "T-abc",
	})

	// Responses-only fields dropped.
	for _, k := range []string{"include", "store", "stream_options", "prompt_cache_key"} {
		if gjson.Get(out, k).Exists() {
			t.Errorf("%s should be dropped", k)
		}
	}

	msgs := gjson.Get(out, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(msgs), out)
	}
	if msgs[2].Get("role").String() != "assistant" || msgs[2].Get("content").String() != "hello" {
		t.Errorf("assistant msg: %v", msgs[2])
	}
	if msgs[2].Get("reasoning_content").String() != "blob" {
		t.Errorf("reasoning_content should be preserved on assistant msg: %v", msgs[2])
	}
}

func TestTranslateResponsesRequest_ToolsUnwrapped(t *testing.T) {
	out, _ := runTranslate(t, map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
		},
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "shell_command",
				"description": "Run a shell command.",
				"parameters":  map[string]any{"type": "object"},
				"strict":      false,
			},
		},
	})

	ts := gjson.Get(out, "tools").Array()
	if len(ts) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(ts))
	}
	if ts[0].Get("type").String() != "function" {
		t.Errorf("wrapped type: %q", ts[0].Get("type").String())
	}
	if ts[0].Get("function.name").String() != "shell_command" {
		t.Errorf("nested function.name: %q", ts[0].Get("function.name").String())
	}
	if ts[0].Get("function.description").String() != "Run a shell command." {
		t.Errorf("description: %q", ts[0].Get("function.description").String())
	}
	if !ts[0].Get("function.parameters.type").Exists() {
		t.Errorf("parameters should be nested under function: %s", ts[0].Raw)
	}
	// Top-level `name` should be gone (moved inside function object).
	if ts[0].Get("name").Exists() {
		t.Errorf("tool.name should not remain at top level")
	}
}

func TestTranslateResponsesRequest_FunctionCallsMergeIntoAssistant(t *testing.T) {
	// Reproduce the multi-turn shape Amp sends when the model previously
	// fired parallel_tool_calls=true and we're now passing back the
	// function_call items + their outputs.
	out, _ := runTranslate(t, map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "do stuff"}}},
			map[string]any{"type": "reasoning", "id": "rs_x", "encrypted_content": "blob"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "ok working"}}},
			map[string]any{"type": "function_call", "name": "shell_command",
				"call_id": "call_1", "arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call", "name": "read_file",
				"call_id": "call_2", "arguments": `{"path":"README"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1",
				"output": "a\nb"},
			map[string]any{"type": "function_call_output", "call_id": "call_2",
				"output": "readme contents"},
		},
	})

	msgs := gjson.Get(out, "messages").Array()
	// expected: system, user, assistant(with text + 2 tool_calls), tool(call_1), tool(call_2)
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d. out=%s", len(msgs), out)
	}
	asst := msgs[2]
	if asst.Get("role").String() != "assistant" {
		t.Fatalf("msgs[2].role=%q", asst.Get("role").String())
	}
	if asst.Get("content").String() != "ok working" {
		t.Errorf("assistant content: %q", asst.Get("content").String())
	}
	if asst.Get("reasoning_content").String() != "blob" {
		t.Errorf("assistant reasoning_content: %q", asst.Get("reasoning_content").String())
	}
	tcs := asst.Get("tool_calls").Array()
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool_calls merged, got %d: %s", len(tcs), asst.Raw)
	}
	if tcs[0].Get("id").String() != "call_1" || tcs[0].Get("function.name").String() != "shell_command" {
		t.Errorf("tool_calls[0]: %v", tcs[0])
	}
	if tcs[1].Get("id").String() != "call_2" || tcs[1].Get("function.name").String() != "read_file" {
		t.Errorf("tool_calls[1]: %v", tcs[1])
	}
	// Two tool messages follow.
	if msgs[3].Get("role").String() != "tool" || msgs[3].Get("tool_call_id").String() != "call_1" {
		t.Errorf("tool msg #1: %v", msgs[3])
	}
	if msgs[4].Get("role").String() != "tool" || msgs[4].Get("tool_call_id").String() != "call_2" {
		t.Errorf("tool msg #2: %v", msgs[4])
	}
}

func TestTranslateResponsesRequest_FunctionCallsWithoutPriorAssistantText(t *testing.T) {
	// Edge case: model went straight to tool_calls without a text reply.
	// The translator must still produce a valid assistant message with
	// content:null and tool_calls populated.
	out, _ := runTranslate(t, map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "run ls"}}},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "need tool"}}},
			map[string]any{"type": "function_call", "name": "shell_command",
				"call_id": "call_z", "arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_z",
				"output": "files"},
		},
	})
	msgs := gjson.Get(out, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(msgs), out)
	}
	// msgs[2] is the assistant wrapper (no text, only tool_calls)
	if msgs[2].Get("role").String() != "assistant" {
		t.Fatalf("msgs[2].role=%q", msgs[2].Get("role").String())
	}
	if msgs[2].Get("content").Type != gjson.Null {
		t.Errorf("assistant content should be null, got %v", msgs[2].Get("content"))
	}
	if msgs[2].Get("reasoning_content").String() != "need tool" {
		t.Errorf("assistant reasoning_content: %q", msgs[2].Get("reasoning_content").String())
	}
	if len(msgs[2].Get("tool_calls").Array()) != 1 {
		t.Errorf("expected 1 tool_call: %s", msgs[2].Raw)
	}
}

func TestTranslateResponsesRequest_ContextCarriesStreamFlag(t *testing.T) {
	_, ctx := runTranslate(t, map[string]any{
		"model":  "gpt-5.4",
		"stream": false,
		"input":  []any{map[string]any{"role": "system", "content": "sys"}},
	})
	if ctx == nil {
		t.Fatal("ctx is nil")
	}
	if ctx.stream {
		t.Fatal("ctx.stream should preserve stream:false")
	}
}

func TestTranslateChatCompletionToResponses_NonStreaming(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":123,"model":"deepseek-v4-pro","choices":[{"message":{"role":"assistant","reasoning_content":"think","content":"Hello world","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell_command","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	out, ok, err := translateChatCompletionToResponses(body, &responsesTranslateCtx{origModel: "gpt-5.4", promptCacheKey: "T-abc"})
	if err != nil {
		t.Fatalf("translate chat response: %v", err)
	}
	if !ok {
		t.Fatal("expected chat completion to translate")
	}
	if gjson.GetBytes(out, "model").String() != "gpt-5.4" {
		t.Errorf("model: %q", gjson.GetBytes(out, "model").String())
	}
	if gjson.GetBytes(out, "prompt_cache_key").String() != "T-abc" {
		t.Errorf("prompt_cache_key missing: %s", out)
	}
	items := gjson.GetBytes(out, "output").Array()
	if len(items) != 3 {
		t.Fatalf("expected reasoning+message+tool output, got %d: %s", len(items), out)
	}
	if items[0].Get("type").String() != "reasoning" || items[0].Get("summary.0.text").String() != "think" {
		t.Errorf("reasoning item: %s", items[0].Raw)
	}
	if items[1].Get("content.0.text").String() != "Hello world" {
		t.Errorf("message item: %s", items[1].Raw)
	}
	if items[2].Get("type").String() != "function_call" || items[2].Get("call_id").String() != "call_1" {
		t.Errorf("tool item: %s", items[2].Raw)
	}
	if gjson.GetBytes(out, "usage.input_tokens").Int() != 10 || gjson.GetBytes(out, "usage.output_tokens").Int() != 5 {
		t.Errorf("usage not mapped: %s", out)
	}
}

func TestTranslateChatCompletionToResponses_PassesThroughErrorShape(t *testing.T) {
	_, ok, err := translateChatCompletionToResponses([]byte(`{"error":{"message":"bad"}}`), &responsesTranslateCtx{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("error payload should not translate")
	}
}

// ---------- SSE translator tests ----------

// writeUpstreamSSE produces a fake chat/completions SSE stream suitable
// for feeding into responsesSSETranslator. Each entry in events is the
// JSON body of a `data:` line (we add the prefix and blank-line
// delimiter here).
func writeUpstreamSSE(events []string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// readAllTranslated reads every byte out of a responsesSSETranslator.
func readAllTranslated(t *testing.T, tr *responsesSSETranslator) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := tr.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

// parseSSEEvents returns an ordered list of {eventName, dataJSON} from an
// SSE stream.
type sseEvent struct {
	Name string
	Data string
}

func parseSSEEvents(s string) []sseEvent {
	var out []sseEvent
	lines := strings.Split(s, "\n")
	var cur sseEvent
	for _, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			cur.Name = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			cur.Data = strings.TrimPrefix(line, "data: ")
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func TestResponsesSSETranslator_SimpleChat(t *testing.T) {
	// Simulate DeepSeek: a bit of reasoning_content, then content, then stop.
	stream := writeUpstreamSSE([]string{
		`{"choices":[{"delta":{"role":"assistant"}}],"model":"deepseek-v4-pro"}`,
		`{"choices":[{"delta":{"reasoning_content":"let me "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"think."}}]}`,
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":" world"}}]}`,
		`{"choices":[{"finish_reason":"stop","delta":{}}]}`,
	})
	tr := newResponsesSSETranslator(nopCloser(strings.NewReader(stream)), &responsesTranslateCtx{origModel: "gpt-5.4"})
	got := readAllTranslated(t, tr)

	events := parseSSEEvents(got)
	if len(events) == 0 {
		t.Fatalf("no events produced. stream=%s", got)
	}

	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Name
	}

	// Sanity: the event order must include these, in this relative order.
	expectedOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.output_item.done",  // reasoning
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message
		"response.completed",
	}
	if !containsInOrder(names, expectedOrder) {
		t.Fatalf("events out of order.\ngot:      %v\nexpected: %v", names, expectedOrder)
	}

	// Find the final response.completed and verify model + output items.
	var completed sseEvent
	for _, e := range events {
		if e.Name == "response.completed" {
			completed = e
		}
	}
	if completed.Data == "" {
		t.Fatal("no response.completed event")
	}
	// origModel must win over upstream model.
	if gjson.Get(completed.Data, "response.model").String() != "gpt-5.4" {
		t.Errorf("final model should echo origModel gpt-5.4, got %q",
			gjson.Get(completed.Data, "response.model").String())
	}
	out := gjson.Get(completed.Data, "response.output").Array()
	if len(out) != 2 {
		t.Fatalf("expected 2 output items (reasoning+message), got %d: %s",
			len(out), completed.Data)
	}
	if out[0].Get("type").String() != "reasoning" {
		t.Errorf("output[0].type=%q", out[0].Get("type").String())
	}
	// reasoning.summary carries plaintext.
	sumText := out[0].Get("summary.0.text").String()
	if !strings.Contains(sumText, "let me think.") {
		t.Errorf("reasoning summary text: %q", sumText)
	}
	if out[1].Get("type").String() != "message" {
		t.Errorf("output[1].type=%q", out[1].Get("type").String())
	}
	if out[1].Get("content.0.text").String() != "Hello world" {
		t.Errorf("message text: %q", out[1].Get("content.0.text").String())
	}
}

func TestResponsesSSETranslator_ToolCalls(t *testing.T) {
	// Single tool_call with split arguments delta, followed by finish_reason=tool_calls.
	stream := writeUpstreamSSE([]string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"must call tool"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_42","type":"function","function":{"name":"shell_command","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"choices":[{"finish_reason":"tool_calls","delta":{}}]}`,
	})
	tr := newResponsesSSETranslator(nopCloser(strings.NewReader(stream)), &responsesTranslateCtx{origModel: "gpt-5.4"})
	got := readAllTranslated(t, tr)
	events := parseSSEEvents(got)

	// Expect at least one function_call_arguments.delta and one done, plus
	// an output_item.added and output_item.done for the function_call item.
	var argsDelta, argsDone int
	for _, e := range events {
		switch e.Name {
		case "response.function_call_arguments.delta":
			argsDelta++
		case "response.function_call_arguments.done":
			argsDone++
		}
	}
	if argsDelta == 0 {
		t.Errorf("expected function_call_arguments.delta events, got 0. stream=%s", got)
	}
	if argsDone != 1 {
		t.Errorf("expected 1 function_call_arguments.done, got %d", argsDone)
	}

	// Verify the final completed envelope has a function_call output item.
	var completedData string
	for _, e := range events {
		if e.Name == "response.completed" {
			completedData = e.Data
		}
	}
	if completedData == "" {
		t.Fatal("no response.completed")
	}
	out := gjson.Get(completedData, "response.output").Array()
	foundFC := false
	for _, it := range out {
		if it.Get("type").String() == "function_call" {
			foundFC = true
			if it.Get("call_id").String() != "call_42" {
				t.Errorf("function_call call_id=%q", it.Get("call_id").String())
			}
			if it.Get("name").String() != "shell_command" {
				t.Errorf("function_call name=%q", it.Get("name").String())
			}
			if it.Get("arguments").String() != `{"cmd":"ls"}` {
				t.Errorf("function_call args=%q", it.Get("arguments").String())
			}
		}
	}
	if !foundFC {
		t.Errorf("no function_call item in final output: %s", completedData)
	}
}

func TestResponsesSSETranslator_SequenceNumbersMonotonic(t *testing.T) {
	stream := writeUpstreamSSE([]string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"finish_reason":"stop","delta":{}}]}`,
	})
	tr := newResponsesSSETranslator(nopCloser(strings.NewReader(stream)), &responsesTranslateCtx{})
	got := readAllTranslated(t, tr)
	events := parseSSEEvents(got)

	prev := int64(-1)
	for _, e := range events {
		seq := gjson.Get(e.Data, "sequence_number").Int()
		if !gjson.Get(e.Data, "sequence_number").Exists() {
			continue
		}
		if seq <= prev {
			t.Errorf("sequence_number not strictly increasing: prev=%d curr=%d event=%s",
				prev, seq, e.Name)
		}
		prev = seq
	}
}

func TestResponsesSSETranslator_EmitsCompletedExactlyOnce(t *testing.T) {
	// Regression: finish_reason=stop followed by [DONE] previously emitted
	// two response.completed events (once from finishAll, once from
	// finishIfPending on stream end).
	stream := writeUpstreamSSE([]string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"finish_reason":"stop","delta":{}}]}`,
	})
	tr := newResponsesSSETranslator(nopCloser(strings.NewReader(stream)), &responsesTranslateCtx{})
	got := readAllTranslated(t, tr)
	events := parseSSEEvents(got)
	completed := 0
	for _, e := range events {
		if e.Name == "response.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Errorf("expected exactly 1 response.completed, got %d", completed)
	}
}

func TestResponsesSSETranslator_MapsUsageFromFinalChunk(t *testing.T) {
	stream := writeUpstreamSSE([]string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
	})
	tr := newResponsesSSETranslator(nopCloser(strings.NewReader(stream)), &responsesTranslateCtx{})
	got := readAllTranslated(t, tr)
	events := parseSSEEvents(got)
	var completedData string
	for _, e := range events {
		if e.Name == "response.completed" {
			completedData = e.Data
		}
	}
	if completedData == "" {
		t.Fatal("no response.completed")
	}
	if gjson.Get(completedData, "response.usage.input_tokens").Int() != 7 {
		t.Errorf("usage not mapped: %s", completedData)
	}
	if gjson.Get(completedData, "response.usage.output_tokens").Int() != 2 {
		t.Errorf("usage not mapped: %s", completedData)
	}
}

// ---- small helpers ----

type nopReadCloser struct {
	r *strings.Reader
}

func (n *nopReadCloser) Read(p []byte) (int, error) { return n.r.Read(p) }
func (n *nopReadCloser) Close() error               { return nil }

func nopCloser(r *strings.Reader) *nopReadCloser {
	return &nopReadCloser{r: r}
}

func containsInOrder(haystack, needles []string) bool {
	i := 0
	for _, h := range haystack {
		if i < len(needles) && h == needles[i] {
			i++
		}
	}
	return i == len(needles)
}
