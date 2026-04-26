package customproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/margbug01/amp-proxy/internal/bodylimit"
)

// responsesTranslateKey tags a request whose body we rewrote from an OpenAI
// Responses API shape into a chat/completions shape. ModifyResponse reads the
// tag to decide whether to translate the upstream reply back into Responses
// SSE. Distinct from geminiTranslateKey and upgradedMessagesKey — each has
// its own response-phase branch.
type responsesTranslateKey struct{}

// responsesTranslateCtx holds per-request state carried from the request
// phase to the response phase. origModel is what Amp CLI asked for; the
// translator echoes it back in the final response.completed.response.model
// so downstream Amp UI logs stay coherent.
type responsesTranslateCtx struct {
	origModel string
	stream    bool
	// promptCacheKey is Amp CLI's thread-scoped idempotency hint. We don't
	// forward it upstream (DeepSeek doesn't read it), but we echo it back
	// in response.created / response.in_progress events so the client sees
	// consistent shape.
	promptCacheKey string
}

// responsesTranslateFromContext returns the tag if present, else nil.
func responsesTranslateFromContext(ctx context.Context) *responsesTranslateCtx {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(responsesTranslateKey{}).(*responsesTranslateCtx)
	return v
}

// translateResponsesRequestBody consumes req.Body, translates it from
// Responses shape to chat/completions shape, updates Content-Length/
// ContentLength on req, and returns forwardable body bytes plus the
// per-request translate context. Used by the customproxy Director.
func translateResponsesRequestBody(req *http.Request) ([]byte, *responsesTranslateCtx, error) {
	const maxBody = 16 * 1024 * 1024
	if req.Body == nil {
		return nil, nil, fmt.Errorf("nil request body")
	}
	raw, err := bodylimit.ReadAll(req.Body, maxBody)
	_ = req.Body.Close()
	if err != nil {
		return nil, nil, bodylimit.Wrap("responses request body", maxBody, err)
	}
	newBody, tctx, err := translateResponsesRequestToChat(raw)
	if err != nil {
		setRequestBodyLength(req, len(raw))
		return raw, nil, err
	}
	setRequestBodyLength(req, len(newBody))
	return newBody, tctx, nil
}

func setRequestBodyLength(req *http.Request, n int) {
	req.ContentLength = int64(n)
	req.Header.Set("Content-Length", strconv.Itoa(n))
}

// translateResponsesRequestToChat rewrites an OpenAI Responses API request
// body into an OpenAI chat/completions request body. The output targets
// providers that implement chat/completions only (e.g. DeepSeek). See the
// field map in the package README / design notes for rationale.
//
// The input body shape (captured from Amp CLI):
//
//	{
//	  "model": "gpt-5.4",
//	  "input": [
//	    {"role":"system", "content":"<string>"},
//	    {"type":"message", "role":"user"|"assistant", "content":[{"type":"input_text"|"output_text","text":"..."}]},
//	    {"type":"reasoning", "id":"rs_...", "encrypted_content":"...", "summary":[]},
//	    {"type":"function_call", "name":"...", "call_id":"call_...", "arguments":"<json string>"},
//	    {"type":"function_call_output", "call_id":"call_...", "output":"<string>"}
//	  ],
//	  "tools":[{"type":"function","name":"...","description":"...","parameters":{...},"strict":bool}],
//	  "reasoning":{"effort":"high","summary":"auto"},
//	  "max_output_tokens": 128000,
//	  "stream": true,
//	  "parallel_tool_calls": true,
//	  // + Responses-only fields that are dropped: include, store, stream_options, prompt_cache_key
//	}
func translateResponsesRequestToChat(body []byte) ([]byte, *responsesTranslateCtx, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("parse responses request: %w", err)
	}

	out := make(map[string]any, len(req))

	if v, ok := req["model"].(string); ok {
		out["model"] = v
	}

	stream, ok := req["stream"].(bool)
	if !ok {
		// Amp CLI normally sets stream=true in deep mode; keep chat upstreams
		// streaming by default so the reply can be translated back to Responses SSE.
		stream = true
	}
	out["stream"] = stream

	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}

	if v, ok := req["parallel_tool_calls"].(bool); ok {
		out["parallel_tool_calls"] = v
	}

	// reasoning.effort → reasoning_effort (DeepSeek extension).
	// reasoning.summary is Responses-only; drop.
	if r, ok := req["reasoning"].(map[string]any); ok {
		if eff, ok := r["effort"].(string); ok && eff != "" {
			out["reasoning_effort"] = eff
			out["thinking"] = map[string]any{"type": "enabled"}
		}
	}

	// tools: unwrap the flat Responses-style tool into chat/completions'
	// {type:"function", function:{name,description,parameters}} shape.
	if rawTools, ok := req["tools"].([]any); ok && len(rawTools) > 0 {
		chatTools := make([]any, 0, len(rawTools))
		for _, rt := range rawTools {
			t, _ := rt.(map[string]any)
			if t == nil {
				continue
			}
			ttype, _ := t["type"].(string)
			if ttype != "function" {
				// Non-function tools (built-in web_search, file_search, …)
				// are not in our scope.
				continue
			}
			fn := map[string]any{}
			if name, ok := t["name"].(string); ok && name != "" {
				fn["name"] = name
			}
			if desc, ok := t["description"].(string); ok && desc != "" {
				fn["description"] = desc
			}
			if params, ok := t["parameters"]; ok {
				fn["parameters"] = params
			}
			chatTool := map[string]any{"type": "function", "function": fn}
			if strict, ok := t["strict"].(bool); ok {
				chatTool["strict"] = strict
			}
			chatTools = append(chatTools, chatTool)
		}
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}

	if tc, ok := req["tool_choice"]; ok {
		out["tool_choice"] = tc
	}

	// Detect whether thinking/reasoning mode is active. DeepSeek requires
	// every assistant message to carry reasoning_content when thinking is on.
	thinkingEnabled := false
	if r, ok := req["reasoning"].(map[string]any); ok {
		if eff, _ := r["effort"].(string); eff != "" {
			thinkingEnabled = true
		}
	}

	// input → messages. This is the bulk of the translation.
	inputArr, _ := req["input"].([]any)
	messages, err := translateInputToMessages(inputArr, thinkingEnabled)
	if err != nil {
		return nil, nil, fmt.Errorf("translate input: %w", err)
	}
	out["messages"] = messages

	// Context echoes for the response phase.
	origModel, _ := req["model"].(string)
	promptCacheKey, _ := req["prompt_cache_key"].(string)
	ctx := &responsesTranslateCtx{
		origModel:      origModel,
		stream:         stream,
		promptCacheKey: promptCacheKey,
	}

	newBody, err := json.Marshal(out)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return newBody, ctx, nil
}

// translateInputToMessages walks the Responses `input` array and produces
// an OpenAI chat/completions `messages` array. Merging rules:
//
//   - `{role:system, content:str}` (simplified, no type) → {"role":"system","content":str}
//   - `{type:message, role:user, content:[input_text...]}` → user message with concatenated text
//   - `{type:message, role:assistant, content:[output_text...]}` → base for pending assistant msg
//   - adjacent `{type:function_call, ...}` merge into the pending assistant
//     msg's tool_calls array (OpenAI mandates assistant+tool_calls on one message)
//   - `{type:function_call_output, ...}` flushes any pending assistant then
//     emits a {"role":"tool", tool_call_id, content}
//   - `{type:reasoning, ...}` carries DeepSeek's previous reasoning_content;
//     it is attached to the following assistant/tool_call wrapper message.
func translateInputToMessages(input []any, thinkingEnabled bool) ([]any, error) {
	out := make([]any, 0, len(input))

	// pendingAssistant buffers a not-yet-emitted assistant message so that
	// subsequent function_call items can attach to it. Flushed when we hit
	// a non-assistant-related item or the end of the input.
	var pendingAssistant map[string]any
	pendingReasoning := ""
	flush := func() {
		if pendingAssistant != nil {
			out = append(out, pendingAssistant)
			pendingAssistant = nil
		}
	}

	openAssistant := func() map[string]any {
		if pendingAssistant == nil {
			pendingAssistant = map[string]any{"role": "assistant"}
			if pendingReasoning != "" {
				pendingAssistant["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			} else if thinkingEnabled {
				// DeepSeek requires reasoning_content on every assistant
				// message when thinking mode is active, even if empty.
				pendingAssistant["reasoning_content"] = ""
			}
		}
		return pendingAssistant
	}

	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		itemType, _ := item["type"].(string)
		role, _ := item["role"].(string)

		switch {
		case itemType == "" && role == "system":
			// Simplified system: {role, content:str}.
			flush()
			content, _ := item["content"].(string)
			out = append(out, map[string]any{"role": "system", "content": content})

		case itemType == "message":
			text := extractMessageText(item)
			switch role {
			case "user":
				flush()
				out = append(out, map[string]any{"role": "user", "content": text})
			case "assistant":
				flush()
				pendingAssistant = nil
				asst := openAssistant()
				if text != "" {
					asst["content"] = text
				}
			case "system":
				flush()
				out = append(out, map[string]any{"role": "system", "content": text})
			default:
				// Unknown role; best-effort skip.
			}

		case itemType == "function_call":
			// Attach to pending assistant, opening one if necessary.
			asst := openAssistant()
			if _, ok := asst["content"]; !ok {
				// OpenAI: if a message has tool_calls and no text content,
				// content must be null (not empty string).
				asst["content"] = nil
			}
			var tcs []any
			if existing, ok := asst["tool_calls"].([]any); ok {
				tcs = existing
			}
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			tc := map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}
			tcs = append(tcs, tc)
			asst["tool_calls"] = tcs

		case itemType == "function_call_output":
			flush()
			callID, _ := item["call_id"].(string)
			output, _ := item["output"].(string)
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      output,
			})

		case itemType == "reasoning":
			if reasoning := extractReasoningText(item); reasoning != "" {
				pendingReasoning = reasoning
			}
			continue

		default:
			// Unknown type — drop defensively rather than poisoning the
			// chat request.
			continue
		}
	}
	flush()
	return out, nil
}

// extractMessageText collapses a Responses message `content` array into a
// single string. content parts may be {"type":"input_text","text":"..."}
// or {"type":"output_text","text":"..."} — both are flattened by
// concatenation. A string-valued content (legacy) is passed through.
func extractMessageText(item map[string]any) string {
	if s, ok := item["content"].(string); ok {
		return s
	}
	parts, _ := item["content"].([]any)
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, rp := range parts {
		p, _ := rp.(map[string]any)
		if p == nil {
			continue
		}
		t, _ := p["type"].(string)
		switch t {
		case "input_text", "output_text", "text":
			if s, ok := p["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

func extractReasoningText(item map[string]any) string {
	// Try encrypted_content first, but skip it if it looks like an
	// opaque encrypted blob (e.g. GPT-5.x "gAAAAAB…" tokens) because
	// downstream providers like DeepSeek need plaintext reasoning_content.
	if s, ok := item["encrypted_content"].(string); ok && s != "" {
		if !strings.HasPrefix(s, "gAAAAAB") {
			return s
		}
		// Fall through to summary which contains the plaintext.
	}
	if s, ok := item["reasoning_content"].(string); ok && s != "" {
		return s
	}
	summary, _ := item["summary"].([]any)
	var b strings.Builder
	for _, raw := range summary {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		if s, ok := part["text"].(string); ok && s != "" {
			b.WriteString(s)
			continue
		}
		if s, ok := part["summary_text"].(string); ok && s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

// translateChatCompletionToResponses rewrites a non-streaming chat/completions
// JSON response into a non-streaming OpenAI Responses JSON response. It returns
// ok=false for bodies that do not look like successful chat completion replies
// so callers can pass upstream error payloads through unchanged.
func translateChatCompletionToResponses(body []byte, tctx *responsesTranslateCtx) ([]byte, bool, error) {
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, false, fmt.Errorf("parse chat response: %w", err)
	}
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil, false, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, false, nil
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, false, nil
	}

	output := make([]any, 0, 3)
	if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
		output = append(output, map[string]any{
			"id":                synthItemID("rs"),
			"type":              "reasoning",
			"status":            "completed",
			"encrypted_content": reasoning,
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": reasoning,
			}},
		})
	}
	if content := chatMessageContent(message["content"]); content != "" {
		output = append(output, map[string]any{
			"id":     synthItemID("msg"),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"annotations": []any{},
				"logprobs":    []any{},
				"text":        content,
			}},
		})
	}
	if toolCalls, _ := message["tool_calls"].([]any); len(toolCalls) > 0 {
		for _, raw := range toolCalls {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			output = append(output, map[string]any{
				"id":        synthItemID("fc"),
				"type":      "function_call",
				"status":    "completed",
				"arguments": stringValue(fn["arguments"]),
				"call_id":   stringValue(tc["id"]),
				"name":      stringValue(fn["name"]),
			})
		}
	}

	model := stringValue(chat["model"])
	if tctx != nil && tctx.origModel != "" {
		model = tctx.origModel
	}
	createdAt := int64Value(chat["created"])
	if createdAt == 0 {
		createdAt = nowUnix()
	}
	resp := map[string]any{
		"id":                   synthResponseID(),
		"object":               "response",
		"created_at":           createdAt,
		"status":               "completed",
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"max_tool_calls":       nil,
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": "auto", "summary": "auto"},
		"store":                false,
		"temperature":          1.0,
		"top_p":                1.0,
		"usage":                translateChatUsage(chat["usage"]),
		"completed_at":         nowUnix(),
	}
	if tctx != nil && tctx.promptCacheKey != "" {
		resp["prompt_cache_key"] = tctx.promptCacheKey
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, false, fmt.Errorf("marshal responses response: %w", err)
	}
	return out, true, nil
}

func chatMessageContent(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			p, _ := part.(map[string]any)
			if p == nil {
				continue
			}
			if s := stringValue(p["text"]); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func translateChatUsage(raw any) any {
	usage, _ := raw.(map[string]any)
	if usage == nil {
		return nil
	}
	out := map[string]any{}
	copyUsageField(out, usage, "input_tokens", "prompt_tokens")
	copyUsageField(out, usage, "output_tokens", "completion_tokens")
	copyUsageField(out, usage, "total_tokens", "total_tokens")
	copyUsageField(out, usage, "input_tokens_details", "prompt_tokens_details")
	copyUsageField(out, usage, "output_tokens_details", "completion_tokens_details")
	if len(out) == 0 {
		return usage
	}
	return out
}

func copyUsageField(dst, src map[string]any, dstKey, srcKey string) {
	if v, ok := src[srcKey]; ok {
		dst[dstKey] = v
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func int64Value(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
