package customproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Fixtures below are derived from real Amp CLI finder traffic captured on
// 2026-04-16 (see capture/gemini_finder_*.log) with names and content trimmed
// so the tests remain compact and self-contained.

const geminiSingleTurnRequest = `{
  "contents":[
    {"role":"user","parts":[{"text":"Find tailwind configs in my-journal-app"}]}
  ],
  "systemInstruction":{
    "role":"user",
    "parts":[{"text":"You are a fast, parallel code search agent."}]
  },
  "tools":[{"functionDeclarations":[
    {
      "name":"glob",
      "description":"Fast file pattern matching tool",
      "parameters":{
        "type":"OBJECT",
        "required":["filePattern"],
        "properties":{
          "filePattern":{"type":"STRING","description":"Glob pattern"},
          "limit":{"type":"NUMBER","description":"Max results"}
        }
      }
    },
    {
      "name":"Read",
      "description":"Read file or list directory",
      "parameters":{
        "type":"OBJECT",
        "required":["path"],
        "properties":{
          "path":{"type":"STRING","description":"Absolute path"},
          "read_range":{"type":"ARRAY","items":{"type":"NUMBER"}}
        }
      }
    }
  ]}],
  "generationConfig":{
    "temperature":1,
    "maxOutputTokens":65535,
    "seed":614,
    "thinkingConfig":{"includeThoughts":false,"thinkingLevel":"MINIMAL"}
  }
}`

func TestTranslateGeminiRequestToOpenAI_SingleTurn(t *testing.T) {
	out, err := translateGeminiRequestToOpenAI([]byte(geminiSingleTurnRequest), "gpt-5.4-mini(high)")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.4-mini" {
		t.Errorf("model = %q, want %q (thinking suffix must be stripped)", got, "gpt-5.4-mini")
	}
	if got := gjson.GetBytes(out, "stream").Bool(); !got {
		t.Error("stream must be true (augment non-streaming has content-loss)")
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Errorf("reasoning.effort = %q, want %q", got, "high")
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 65535 {
		t.Errorf("max_output_tokens = %d, want 65535", got)
	}

	input := gjson.GetBytes(out, "input").Array()
	if len(input) < 2 {
		t.Fatalf("input must have system + user items, got %d", len(input))
	}

	if got := input[0].Get("role").String(); got != "system" {
		t.Errorf("input[0].role = %q, want %q", got, "system")
	}
	if got := input[0].Get("content").String(); !strings.Contains(got, "parallel code search agent") {
		t.Errorf("system content missing expected text: %q", got)
	}

	if got := input[1].Get("type").String(); got != "message" {
		t.Errorf("input[1].type = %q, want %q", got, "message")
	}
	if got := input[1].Get("role").String(); got != "user" {
		t.Errorf("input[1].role = %q, want %q", got, "user")
	}
	if got := input[1].Get("content.0.type").String(); got != "input_text" {
		t.Errorf("input[1].content[0].type = %q, want %q", got, "input_text")
	}
	if got := input[1].Get("content.0.text").String(); got != "Find tailwind configs in my-journal-app" {
		t.Errorf("input[1].content[0].text = %q", got)
	}

	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2 (flattened from functionDeclarations)", len(tools))
	}
	if got := tools[0].Get("type").String(); got != "function" {
		t.Errorf("tools[0].type = %q, want function", got)
	}
	if got := tools[0].Get("name").String(); got != "glob" {
		t.Errorf("tools[0].name = %q, want glob", got)
	}
	if got := tools[0].Get("parameters.type").String(); got != "object" {
		t.Errorf("tools[0].parameters.type = %q, want lowercased 'object'", got)
	}
	if got := tools[0].Get("parameters.properties.filePattern.type").String(); got != "string" {
		t.Errorf("filePattern.type = %q, want lowercased 'string'", got)
	}
	if got := tools[0].Get("parameters.properties.limit.type").String(); got != "number" {
		t.Errorf("limit.type = %q, want lowercased 'number'", got)
	}
	if got := tools[1].Get("parameters.properties.read_range.type").String(); got != "array" {
		t.Errorf("read_range.type = %q, want lowercased 'array'", got)
	}
	if got := tools[1].Get("parameters.properties.read_range.items.type").String(); got != "number" {
		t.Errorf("read_range.items.type = %q, want lowercased 'number'", got)
	}

	if got := gjson.GetBytes(out, "include.0").String(); got != "reasoning.encrypted_content" {
		t.Errorf("include[0] = %q, want reasoning.encrypted_content", got)
	}
}

func TestTranslateGeminiRequestToOpenAI_NoThinkingSuffix(t *testing.T) {
	out, err := translateGeminiRequestToOpenAI([]byte(geminiSingleTurnRequest), "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.4-mini" {
		t.Errorf("model = %q", got)
	}
	if gjson.GetBytes(out, "reasoning").Exists() {
		t.Error("reasoning must be absent when no thinking suffix")
	}
}

const geminiMultiTurnRequest = `{
  "contents":[
    {"role":"user","parts":[{"text":"Find tailwind configs"}]},
    {"role":"model","parts":[
      {"functionCall":{"name":"glob","args":{"filePattern":"**/tailwind.config.*"}},"thoughtSignature":"CiEBopaque=="},
      {"functionCall":{"name":"glob","args":{"filePattern":"**/postcss.config.*"}}},
      {"functionCall":{"name":"Grep","args":{"pattern":"@tailwind"}}}
    ]},
    {"role":"user","parts":[
      {"functionResponse":{"name":"glob","response":{"output":["tailwind.config.ts"]}}},
      {"functionResponse":{"name":"glob","response":{"output":["postcss.config.cjs"]}}},
      {"functionResponse":{"name":"Grep","response":{"output":["src/main.tsx:3:@tailwind"]}}}
    ]},
    {"role":"model","parts":[
      {"functionCall":{"name":"Read","args":{"path":"/abs/tailwind.config.ts"}}}
    ]},
    {"role":"user","parts":[
      {"functionResponse":{"name":"Read","response":{"output":{"content":"module.exports = {}"}}}}
    ]}
  ],
  "systemInstruction":{"role":"user","parts":[{"text":"search agent"}]}
}`

func TestTranslateGeminiRequestToOpenAI_MultiTurnWithToolResponses(t *testing.T) {
	out, err := translateGeminiRequestToOpenAI([]byte(geminiMultiTurnRequest), "gpt-5.4-mini(high)")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()

	// Expected flattened input items in order:
	//   0: system                     (from systemInstruction)
	//   1: message user               (first user text)
	//   2: function_call glob         (model block 1)
	//   3: function_call glob
	//   4: function_call Grep
	//   5: function_call_output glob  (user functionResponse block 1)
	//   6: function_call_output glob
	//   7: function_call_output Grep
	//   8: function_call Read         (model block 2)
	//   9: function_call_output Read  (user functionResponse block 2)
	if len(input) != 10 {
		t.Fatalf("input count = %d, want 10; first item type=%q", len(input), input[0].Get("type").String())
	}

	if got := input[0].Get("role").String(); got != "system" {
		t.Errorf("input[0].role = %q", got)
	}
	if got := input[1].Get("type").String(); got != "message" {
		t.Errorf("input[1].type = %q", got)
	}

	// Model block 1: three function_calls with synthesised call_ids 0..2
	for i, idx := range []int{2, 3, 4} {
		if got := input[idx].Get("type").String(); got != "function_call" {
			t.Errorf("input[%d].type = %q, want function_call", idx, got)
		}
		wantCallID := "call_gf_" + itoa(i)
		if got := input[idx].Get("call_id").String(); got != wantCallID {
			t.Errorf("input[%d].call_id = %q, want %q", idx, got, wantCallID)
		}
		if got := input[idx].Get("arguments").String(); got == "" {
			t.Errorf("input[%d].arguments is empty; must be a JSON string", idx)
		}
	}

	// User block 1: function_call_outputs must align to call ids 0..2
	for i, idx := range []int{5, 6, 7} {
		if got := input[idx].Get("type").String(); got != "function_call_output" {
			t.Errorf("input[%d].type = %q, want function_call_output", idx, got)
		}
		wantCallID := "call_gf_" + itoa(i)
		if got := input[idx].Get("call_id").String(); got != wantCallID {
			t.Errorf("input[%d].call_id = %q, want %q", idx, got, wantCallID)
		}
		// output must be a JSON string, not a nested object
		outStr := input[idx].Get("output").String()
		if outStr == "" {
			t.Errorf("input[%d].output is empty", idx)
		}
		var parsed any
		if err := json.Unmarshal([]byte(outStr), &parsed); err != nil {
			t.Errorf("input[%d].output not valid JSON string: %v", idx, err)
		}
	}

	// Model block 2: single function_call with call_id 3
	if got := input[8].Get("type").String(); got != "function_call" {
		t.Errorf("input[8].type = %q", got)
	}
	if got := input[8].Get("call_id").String(); got != "call_gf_3" {
		t.Errorf("input[8].call_id = %q, want call_gf_3", got)
	}
	// User block 2: single function_call_output matching call_id 3
	if got := input[9].Get("type").String(); got != "function_call_output" {
		t.Errorf("input[9].type = %q", got)
	}
	if got := input[9].Get("call_id").String(); got != "call_gf_3" {
		t.Errorf("input[9].call_id = %q, want call_gf_3", got)
	}
}

func TestNormalizeSchemaTypeCase(t *testing.T) {
	in := map[string]any{
		"type":     "OBJECT",
		"required": []any{"x"},
		"properties": map[string]any{
			"x": map[string]any{
				"type":        "ARRAY",
				"description": "kept as-is",
				"items": map[string]any{
					"type": "STRING",
					"enum": []any{"FOO", "BAR"}, // enum values must NOT be lowercased
				},
			},
			"y": map[string]any{
				"type": "INTEGER",
			},
		},
	}
	got := normalizeSchemaTypeCase(in).(map[string]any)
	if got["type"] != "object" {
		t.Errorf("top type = %v", got["type"])
	}
	props := got["properties"].(map[string]any)
	if props["x"].(map[string]any)["type"] != "array" {
		t.Errorf("x.type = %v", props["x"])
	}
	if props["x"].(map[string]any)["description"] != "kept as-is" {
		t.Errorf("description was mutated: %v", props["x"])
	}
	items := props["x"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "string" {
		t.Errorf("items.type = %v", items["type"])
	}
	enum := items["enum"].([]any)
	if enum[0] != "FOO" {
		t.Errorf("enum values should be preserved case, got %v", enum)
	}
	if props["y"].(map[string]any)["type"] != "integer" {
		t.Errorf("y.type = %v", props["y"])
	}
}

// sseResponseOnlyText is a trimmed /v1/responses SSE stream with a single
// message item emitting two text deltas and a final response.completed event
// with usage.
const sseResponseOnlyText = `event: response.created
data: {"type":"response.created","sequence_number":0}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"},"output_index":0,"sequence_number":1}

event: response.content_part.added
data: {"type":"response.content_part.added","content_index":0,"item_id":"msg_1","output_index":0,"part":{"type":"output_text","text":""},"sequence_number":2}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello ","item_id":"msg_1","output_index":0,"sequence_number":3}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"world","item_id":"msg_1","output_index":0,"sequence_number":4}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world"}]},"output_index":0,"sequence_number":5}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5.4-mini","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}},"sequence_number":6}

`

func TestCollapseResponsesSSEToGemini_TextOnly(t *testing.T) {
	out, err := collapseResponsesSSEToGemini(strings.NewReader(sseResponseOnlyText), "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "Hello world" {
		t.Errorf("text = %q, want %q", got, "Hello world")
	}
	if got := gjson.GetBytes(out, "usageMetadata.promptTokenCount").Int(); got != 10 {
		t.Errorf("promptTokenCount = %d", got)
	}
	if got := gjson.GetBytes(out, "usageMetadata.candidatesTokenCount").Int(); got != 2 {
		t.Errorf("candidatesTokenCount = %d", got)
	}
	if got := gjson.GetBytes(out, "usageMetadata.totalTokenCount").Int(); got != 12 {
		t.Errorf("totalTokenCount = %d", got)
	}
	if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "STOP" {
		t.Errorf("finishReason = %q", got)
	}
	if got := gjson.GetBytes(out, "modelVersion").String(); got != "gemini-3-flash-preview" {
		t.Errorf("modelVersion = %q", got)
	}
}

const sseResponseFunctionCalls = `event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","summary":[]},"output_index":0,"sequence_number":1}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"xxx"},"output_index":0,"sequence_number":2}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"glob","call_id":"call_abc","arguments":""},"output_index":1,"sequence_number":3}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","delta":"{\"filePattern\":","item_id":"fc_1","output_index":1,"sequence_number":4}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","delta":"\"**/tailwind.config.*\"}","item_id":"fc_1","output_index":1,"sequence_number":5}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"filePattern\":\"**/tailwind.config.*\"}","sequence_number":6}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"glob","call_id":"call_abc","arguments":"{\"filePattern\":\"**/tailwind.config.*\"}"},"output_index":1,"sequence_number":7}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_2","type":"function_call","name":"Grep","call_id":"call_def","arguments":""},"output_index":2,"sequence_number":8}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"fc_2","type":"function_call","name":"Grep","call_id":"call_def","arguments":"{\"pattern\":\"@tailwind\"}"},"output_index":2,"sequence_number":9}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","model":"gpt-5.4-mini","output":[],"usage":{"input_tokens":2037,"output_tokens":125,"total_tokens":2162}},"sequence_number":10}

`

func TestCollapseResponsesSSEToGemini_FunctionCalls(t *testing.T) {
	out, err := collapseResponsesSSEToGemini(strings.NewReader(sseResponseFunctionCalls), "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}

	parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts count = %d, want 2 (reasoning must be dropped)", len(parts))
	}
	if got := parts[0].Get("functionCall.name").String(); got != "glob" {
		t.Errorf("parts[0].functionCall.name = %q", got)
	}
	if got := parts[0].Get("functionCall.args.filePattern").String(); got != "**/tailwind.config.*" {
		t.Errorf("parts[0].functionCall.args = %q", parts[0].Get("functionCall.args").String())
	}
	if got := parts[1].Get("functionCall.name").String(); got != "Grep" {
		t.Errorf("parts[1].functionCall.name = %q", got)
	}
	if got := parts[1].Get("functionCall.args.pattern").String(); got != "@tailwind" {
		t.Errorf("parts[1].functionCall.args = %q", parts[1].Get("functionCall.args").String())
	}

	// Call IDs must NOT leak into the Gemini shape — Gemini has no such field.
	if got := parts[0].Get("functionCall.call_id").Exists(); got {
		t.Errorf("Gemini functionCall should not carry call_id")
	}

	// Ensure no "text" parts were emitted for this tool-only turn.
	for i, p := range parts {
		if p.Get("text").Exists() {
			t.Errorf("parts[%d] unexpectedly has text field", i)
		}
	}

	if got := gjson.GetBytes(out, "usageMetadata.promptTokenCount").Int(); got != 2037 {
		t.Errorf("promptTokenCount = %d", got)
	}
	if got := gjson.GetBytes(out, "usageMetadata.candidatesTokenCount").Int(); got != 125 {
		t.Errorf("candidatesTokenCount = %d", got)
	}
}

func TestCollapseResponsesSSEToGemini_EmptyStreamEmitsPlaceholder(t *testing.T) {
	empty := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n"
	out, err := collapseResponsesSSEToGemini(strings.NewReader(empty), "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1 placeholder", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "" {
		t.Errorf("placeholder text = %q, want empty", got)
	}
}

func TestTranslateOpenAIResponsesJSONToGemini(t *testing.T) {
	openai := []byte(`{
	  "id":"resp_x",
	  "status":"completed",
	  "model":"gpt-5.4-mini",
	  "output":[
	    {"type":"reasoning","id":"rs_1","summary":[]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
	    {"type":"function_call","name":"glob","call_id":"call_1","arguments":"{\"filePattern\":\"*.md\"}"}
	  ],
	  "usage":{"input_tokens":100,"output_tokens":5,"total_tokens":105}
	}`)
	out, err := translateOpenAIResponsesJSONToGemini(openai, "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("translate json: %v", err)
	}
	parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (reasoning dropped, message + function_call)", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "ok" {
		t.Errorf("parts[0].text = %q", got)
	}
	if got := parts[1].Get("functionCall.name").String(); got != "glob" {
		t.Errorf("parts[1].functionCall.name = %q", got)
	}
	if got := parts[1].Get("functionCall.args.filePattern").String(); got != "*.md" {
		t.Errorf("parts[1].functionCall.args.filePattern = %q", got)
	}
}

func TestTranslateGeminiRequestToOpenAI_DropsThoughtSignature(t *testing.T) {
	out, err := translateGeminiRequestToOpenAI([]byte(geminiMultiTurnRequest), "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// thoughtSignature from the captured model part must NOT appear anywhere
	// in the rewritten body — augment has no way to verify it.
	if bytes.Contains(out, []byte("thoughtSignature")) {
		t.Error("translated body must not contain thoughtSignature")
	}
	if bytes.Contains(out, []byte("CiEBopaque==")) {
		t.Error("translated body must not contain the raw opaque signature bytes")
	}
}

