package customproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/margbug01/amp-proxy/internal/thinking"
)

// geminiTranslateKey tags a request whose body we rewrote from a Google
// v1beta1 generateContent shape into an OpenAI Responses API request.
// ModifyResponse uses the tag to decide whether the upstream reply has to be
// translated back into Gemini generateContent JSON before the downstream
// client sees it.
type geminiTranslateKey struct{}

// geminiTranslateCtx carries per-request translator state from the request
// phase (fallback_handlers.go) into the response phase (ModifyResponse).
// Only fields that affect response-side translation are kept here.
type geminiTranslateCtx struct {
	// OriginalModel is the Gemini model name that appeared in the incoming
	// URL path (e.g. "gemini-3-flash-preview"). Used to populate
	// modelVersion in the translated Gemini response envelope.
	OriginalModel string
}

// WithGeminiTranslate returns a derived context that signals the customproxy
// ModifyResponse hook to translate the upstream OpenAI Responses reply back
// into a Gemini generateContent JSON body. originalModel is echoed back into
// the translated response's modelVersion field so downstream Amp CLI logs
// stay coherent.
func WithGeminiTranslate(parent context.Context, originalModel string) context.Context {
	return context.WithValue(parent, geminiTranslateKey{}, &geminiTranslateCtx{
		OriginalModel: originalModel,
	})
}

// geminiTranslateFromContext returns the translator state attached to ctx,
// or nil if the request was not tagged for Gemini translation.
func geminiTranslateFromContext(ctx context.Context) *geminiTranslateCtx {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(geminiTranslateKey{}).(*geminiTranslateCtx)
	return v
}

// TranslateGeminiRequestToOpenAI is the exported entry point used by the amp
// fallback handler to rewrite an incoming Google Gemini v1beta1
// generateContent body into an OpenAI Responses API body. See
// translateGeminiRequestToOpenAI for the detailed transformation rules.
func TranslateGeminiRequestToOpenAI(body []byte, mappedModel string) ([]byte, error) {
	return translateGeminiRequestToOpenAI(body, mappedModel)
}

// maxResponsesSSEBytes caps how much SSE we'll buffer from augment when
// translating a /v1/responses stream into a non-streaming Gemini JSON reply.
// finder turns are typically a few KB of text plus a handful of function
// calls; 8 MiB is a generous ceiling.
const maxResponsesSSEBytes = 8 * 1024 * 1024

// translateGeminiRequestToOpenAI converts a Google Gemini v1beta1
// generateContent request body into an equivalent OpenAI Responses API
// request body that augment's /v1/responses endpoint can service.
//
// mappedModel is the resolved custom-provider target (e.g. "gpt-5.4-mini(high)").
// Any thinking suffix is stripped from the forwarded model name and used to
// populate the OpenAI reasoning.effort field instead.
//
// The translation drops thoughtSignature on incoming assistant parts
// (augment cannot verify Gemini's opaque signatures), synthesizes call_id
// values for functionCall/functionResponse pairing (Gemini has no such
// field), and normalizes tool parameter schema type values from Gemini's
// uppercase convention ("STRING", "OBJECT") to JSON Schema's lowercase
// ("string", "object") form.
func translateGeminiRequestToOpenAI(body []byte, mappedModel string) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("gemini translate: empty request body")
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("gemini translate: parse request: %w", err)
	}

	suffix := thinking.ParseSuffix(mappedModel)
	openAIModel := suffix.ModelName
	reasoningEffort := ""
	if suffix.HasSuffix {
		reasoningEffort = suffix.RawSuffix
	}

	var input []any

	if si, ok := req["systemInstruction"].(map[string]any); ok {
		if sys := collectGeminiText(si["parts"]); sys != "" {
			input = append(input, map[string]any{
				"role":    "system",
				"content": sys,
			})
		}
	}

	contents, _ := req["contents"].([]any)
	var pendingCallIDs []string
	callIDCounter := 0

	for _, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		parts, _ := content["parts"].([]any)

		switch role {
		case "user":
			input, pendingCallIDs = appendUserContent(input, parts, pendingCallIDs)
		case "model":
			var newPending []string
			input, newPending, callIDCounter = appendModelContent(input, parts, callIDCounter)
			pendingCallIDs = newPending
		default:
			// Unknown role (e.g. "function" from a hand-written client) —
			// best-effort: treat text parts as user input, skip others.
			if txt := collectGeminiText(parts); txt != "" {
				input = append(input, map[string]any{
					"type": "message",
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": txt},
					},
				})
			}
		}
	}

	var tools []any
	if rawTools, ok := req["tools"].([]any); ok {
		for _, rt := range rawTools {
			toolGroup, ok := rt.(map[string]any)
			if !ok {
				continue
			}
			decls, _ := toolGroup["functionDeclarations"].([]any)
			for _, d := range decls {
				decl, ok := d.(map[string]any)
				if !ok {
					continue
				}
				tool := map[string]any{
					"type":   "function",
					"name":   decl["name"],
					"strict": false,
				}
				if desc, ok := decl["description"].(string); ok && desc != "" {
					tool["description"] = desc
				}
				if params, ok := decl["parameters"]; ok && params != nil {
					tool["parameters"] = normalizeSchemaTypeCase(params)
				} else {
					tool["parameters"] = map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					}
				}
				tools = append(tools, tool)
			}
		}
	}

	out := map[string]any{
		"model":               openAIModel,
		"input":               input,
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": true,
		"include":             []any{"reasoning.encrypted_content"},
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}

	if gc, ok := req["generationConfig"].(map[string]any); ok {
		if mot, ok := gc["maxOutputTokens"]; ok {
			switch v := mot.(type) {
			case float64:
				if v > 0 {
					out["max_output_tokens"] = int64(v)
				}
			case int64:
				if v > 0 {
					out["max_output_tokens"] = v
				}
			}
		}
	}

	if reasoningEffort != "" {
		out["reasoning"] = map[string]any{
			"effort":  reasoningEffort,
			"summary": "auto",
		}
	}

	return json.Marshal(out)
}

// collectGeminiText concatenates every text value found inside a Gemini
// parts array. Non-text parts are ignored. The returned string is joined
// with "\n\n" between separate parts so multiline system instructions keep
// their visual separation.
func collectGeminiText(raw any) string {
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		txt, _ := part["text"].(string)
		if txt == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(txt)
	}
	return sb.String()
}

// appendUserContent flattens a Gemini user-role parts array into OpenAI
// Responses input items. Plain text parts collapse into a single
// message/input_text item; functionResponse parts emit one
// function_call_output item each, aligned by positional index to the
// pendingCallIDs synthesised by the preceding model turn.
//
// pendingCallIDs is consumed on every user turn. After consumption the
// caller gets back an empty slice so subsequent turns do not reuse stale
// ids.
func appendUserContent(input []any, parts []any, pendingCallIDs []string) ([]any, []string) {
	var textParts []string
	type funcResp struct {
		name string
		resp any
	}
	var funcResponses []funcResp

	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if txt, ok := part["text"].(string); ok && txt != "" {
			textParts = append(textParts, txt)
			continue
		}
		if fr, ok := part["functionResponse"].(map[string]any); ok {
			name, _ := fr["name"].(string)
			resp := fr["response"]
			if resp == nil {
				resp = map[string]any{}
			}
			funcResponses = append(funcResponses, funcResp{name: name, resp: resp})
		}
	}

	if len(textParts) > 0 {
		content := make([]any, 0, len(textParts))
		for _, t := range textParts {
			content = append(content, map[string]any{"type": "input_text", "text": t})
		}
		input = append(input, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		})
	}

	for j, fr := range funcResponses {
		callID := ""
		if j < len(pendingCallIDs) {
			callID = pendingCallIDs[j]
		}
		if callID == "" {
			callID = fmt.Sprintf("call_gf_orphan_%s_%d", fr.name, j)
		}
		outputStr, err := json.Marshal(fr.resp)
		if err != nil {
			outputStr = []byte(`{}`)
		}
		input = append(input, map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  string(outputStr),
		})
	}

	return input, nil
}

// appendModelContent flattens a Gemini model-role parts array into OpenAI
// Responses input items. Assistant text becomes a message/output_text item;
// each functionCall becomes a function_call item with a synthesised call_id.
// The returned slice of call_ids is consumed by the next user turn to align
// functionResponse parts with their originating calls.
func appendModelContent(input []any, parts []any, callIDCounter int) ([]any, []string, int) {
	var newPending []string

	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			name, _ := fc["name"].(string)
			args := fc["args"]
			if args == nil {
				args = map[string]any{}
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				argsJSON = []byte(`{}`)
			}
			callID := fmt.Sprintf("call_gf_%d", callIDCounter)
			callIDCounter++
			newPending = append(newPending, callID)
			input = append(input, map[string]any{
				"type":      "function_call",
				"name":      name,
				"call_id":   callID,
				"arguments": string(argsJSON),
			})
			continue
		}
		if txt, ok := part["text"].(string); ok && txt != "" {
			input = append(input, map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": txt},
				},
			})
		}
		// thoughtSignature, inlineData, fileData, and other Gemini-specific
		// parts are dropped — augment cannot consume them.
	}

	return input, newPending, callIDCounter
}

// normalizeSchemaTypeCase walks a JSON-Schema-shaped value and lowercases
// every string value of a key named "type". Gemini emits uppercase type
// keywords ("OBJECT", "STRING", "NUMBER", "ARRAY", "BOOLEAN") but OpenAI's
// Responses API validates the standard lowercase form, so we translate in
// place. Other keys are copied untouched.
func normalizeSchemaTypeCase(v any) any {
	switch vv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(vv))
		for k, val := range vv {
			if k == "type" {
				if s, ok := val.(string); ok {
					out[k] = strings.ToLower(s)
					continue
				}
			}
			out[k] = normalizeSchemaTypeCase(val)
		}
		return out
	case []any:
		out := make([]any, len(vv))
		for i, item := range vv {
			out[i] = normalizeSchemaTypeCase(item)
		}
		return out
	default:
		return v
	}
}

// collapseResponsesSSEToGemini reads an OpenAI Responses API server-sent-
// events stream from r and returns a single JSON body shaped like a Gemini
// v1beta1 generateContent reply. It mirrors the accumulation strategy in
// sseRewriter: every response.output_item.done event contributes a final
// item to the running list, and response.completed contributes the usage
// numbers. Reasoning items are skipped entirely (no Gemini equivalent).
//
// requestedModel is the Gemini model name from the original URL path and is
// echoed back in the modelVersion field so the downstream Amp CLI logs stay
// coherent.
func collapseResponsesSSEToGemini(r io.Reader, requestedModel string) ([]byte, error) {
	limited := io.LimitReader(r, maxResponsesSSEBytes)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), maxResponsesSSEBytes)

	var items []json.RawMessage
	var usageInputTokens, usageOutputTokens, usageTotalTokens int64
	var finalModel string

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[len("data: "):]
		if len(payload) == 0 {
			continue
		}

		switch gjson.GetBytes(payload, "type").String() {
		case "response.output_item.done":
			item := gjson.GetBytes(payload, "item")
			if item.Exists() {
				raw := make([]byte, len(item.Raw))
				copy(raw, item.Raw)
				items = append(items, raw)
			}
		case "response.completed":
			usageInputTokens = gjson.GetBytes(payload, "response.usage.input_tokens").Int()
			usageOutputTokens = gjson.GetBytes(payload, "response.usage.output_tokens").Int()
			usageTotalTokens = gjson.GetBytes(payload, "response.usage.total_tokens").Int()
			if m := gjson.GetBytes(payload, "response.model").String(); m != "" {
				finalModel = m
			}
			// If the completed event carries a non-empty output array and we
			// haven't seen any output_item.done events, fall back to the
			// embedded items so we don't silently return nothing.
			if len(items) == 0 {
				if arr := gjson.GetBytes(payload, "response.output"); arr.IsArray() {
					for _, it := range arr.Array() {
						raw := make([]byte, len(it.Raw))
						copy(raw, it.Raw)
						items = append(items, raw)
					}
				}
			}
		case "error":
			return nil, fmt.Errorf("upstream /v1/responses stream error: %s", string(payload))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /v1/responses stream: %w", err)
	}

	return buildGeminiResponseFromItems(items, usageInputTokens, usageOutputTokens, usageTotalTokens, finalModel, requestedModel)
}

// translateOpenAIResponsesJSONToGemini converts a non-streaming OpenAI
// Responses reply body into a Gemini generateContent response body. It is a
// safety net for the rare path where augment serves the non-streaming JSON
// branch (which is known to return empty output arrays on content-loss).
// In the happy path we always request stream:true so this function is not
// on the hot path.
func translateOpenAIResponsesJSONToGemini(body []byte, requestedModel string) ([]byte, error) {
	if len(body) == 0 {
		return buildGeminiResponseFromItems(nil, 0, 0, 0, "", requestedModel)
	}
	output := gjson.GetBytes(body, "output")
	var items []json.RawMessage
	if output.IsArray() {
		for _, it := range output.Array() {
			raw := make([]byte, len(it.Raw))
			copy(raw, it.Raw)
			items = append(items, raw)
		}
	}
	usageIn := gjson.GetBytes(body, "usage.input_tokens").Int()
	usageOut := gjson.GetBytes(body, "usage.output_tokens").Int()
	usageTotal := gjson.GetBytes(body, "usage.total_tokens").Int()
	model := gjson.GetBytes(body, "model").String()
	return buildGeminiResponseFromItems(items, usageIn, usageOut, usageTotal, model, requestedModel)
}

// buildGeminiResponseFromItems turns an accumulated list of OpenAI output
// items plus usage numbers into a Gemini generateContent response JSON body.
// Reasoning items are dropped; function_call items become Gemini
// functionCall parts; message/output_text items become text parts.
func buildGeminiResponseFromItems(items []json.RawMessage, usageIn, usageOut, usageTotal int64, upstreamModel, requestedModel string) ([]byte, error) {
	parts := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		itemType := gjson.GetBytes(raw, "type").String()
		switch itemType {
		case "reasoning":
			// Not representable in Gemini shape; skip.
		case "message":
			text := collectMessageOutputText(raw)
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		case "function_call":
			name := gjson.GetBytes(raw, "name").String()
			rawArgs := gjson.GetBytes(raw, "arguments").String()
			var args any
			if rawArgs == "" {
				args = map[string]any{}
			} else if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": name,
					"args": args,
				},
			})
		default:
			// Unknown item types are ignored so a future augment update
			// cannot crash the translator.
		}
	}

	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}

	modelVersion := requestedModel
	if modelVersion == "" {
		modelVersion = upstreamModel
	}
	if usageTotal == 0 {
		usageTotal = usageIn + usageOut
	}

	resp := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": parts,
				},
				"finishReason": "STOP",
				"index":        0,
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     usageIn,
			"candidatesTokenCount": usageOut,
			"totalTokenCount":      usageTotal,
		},
		"modelVersion": modelVersion,
		"createTime":   time.Now().UTC().Format(time.RFC3339Nano),
		"responseId":   fmt.Sprintf("amp-proxy-%d", time.Now().UnixNano()),
	}

	return json.Marshal(resp)
}

// collectMessageOutputText extracts the concatenated text from an OpenAI
// message output item, tolerating both the plain-string and the typed-part
// array content representations that augment has been observed to emit.
func collectMessageOutputText(raw json.RawMessage) string {
	content := gjson.GetBytes(raw, "content")
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var sb strings.Builder
	for _, part := range content.Array() {
		partType := part.Get("type").String()
		if partType != "output_text" && partType != "text" {
			continue
		}
		if t := part.Get("text").String(); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// rewriteGeminiRequestToResponsesPath strips the Google-specific URL
// decoration and returns the canonical OpenAI Responses path that
// customproxy's Director is expected to see. The returned path is
// idempotent under extractLeaf (which strips a leading /v1 before
// re-appending the provider base path).
func rewriteGeminiRequestToResponsesPath() string {
	return "/v1/responses"
}

// translateGeminiResponse is invoked from ModifyResponse when the request
// context carries a gemini translate tag. It drains the upstream OpenAI
// Responses reply (streamed or non-streaming), converts it into a Gemini
// generateContent JSON body, and rewrites the response headers so Amp CLI's
// google-genai-sdk consumer reads a well-formed Gemini reply.
//
// On any translation error the function emits an empty-parts fallback body
// so Amp CLI still receives a structurally valid response, matching the
// no-worse-than-broken-baseline approach used by collapseMessagesSSE in the
// /v1/messages stream-upgrade path.
func translateGeminiResponse(resp *http.Response, gt *geminiTranslateCtx) error {
	originalModel := ""
	if gt != nil {
		originalModel = gt.OriginalModel
	}

	var translated []byte
	var err error

	if isEventStream(resp.Header.Get("Content-Type")) {
		translated, err = collapseResponsesSSEToGemini(resp.Body, originalModel)
		_ = resp.Body.Close()
	} else {
		const maxInspect = 10 * 1024 * 1024
		buf, readErr := io.ReadAll(io.LimitReader(resp.Body, maxInspect))
		_ = resp.Body.Close()
		if readErr != nil {
			err = fmt.Errorf("read non-streaming /v1/responses body: %w", readErr)
		} else {
			log.WithFields(log.Fields{
				"path":         resp.Request.URL.Path,
				"content_type": resp.Header.Get("Content-Type"),
				"status":       resp.StatusCode,
			}).Warn("gemini-translate: upstream returned non-streaming /v1/responses; translating via JSON fallback path")
			translated, err = translateOpenAIResponsesJSONToGemini(buf, originalModel)
		}
	}

	if err != nil {
		log.WithFields(log.Fields{
			"path":  resp.Request.URL.Path,
			"err":   err,
			"model": originalModel,
		}).Error("gemini-translate: response translation failed; emitting empty-parts fallback body")
		translated, _ = buildGeminiResponseFromItems(nil, 0, 0, 0, "", originalModel)
	}

	resp.Body = io.NopCloser(bytes.NewReader(translated))
	resp.ContentLength = int64(len(translated))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(translated)))
	resp.Header.Del("Transfer-Encoding")
	// Gin / net/http transport may have flagged chunked earlier; clearing
	// Transfer-Encoding together with a concrete Content-Length is enough
	// to force a normal Content-Length framed reply.
	return nil
}
