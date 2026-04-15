package customproxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestCollapseMessagesSSE_SimpleText mirrors the raw augment response we
// captured while probing /v1/messages streaming directly. A single text
// content block across one text_delta event.
func TestCollapseMessagesSSE_SimpleText(t *testing.T) {
	fixture := `event: message_start
data: {"type":"message_start","message":{"id":"resp_abc","type":"message","role":"assistant","model":"gpt-5.4-mini-2026-03-17","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":32}}

event: message_stop
data: {"type":"message_stop"}
`
	out, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("collapseMessagesSSE: %v", err)
	}

	if id := gjson.GetBytes(out, "id").String(); id != "resp_abc" {
		t.Errorf("id: got %q, want resp_abc", id)
	}
	if role := gjson.GetBytes(out, "role").String(); role != "assistant" {
		t.Errorf("role: got %q, want assistant", role)
	}
	if blocks := gjson.GetBytes(out, "content").Array(); len(blocks) != 1 {
		t.Fatalf("content length: got %d, want 1\nraw: %s", len(blocks), out)
	}
	if kind := gjson.GetBytes(out, "content.0.type").String(); kind != "text" {
		t.Errorf("content[0].type: got %q, want text", kind)
	}
	if text := gjson.GetBytes(out, "content.0.text").String(); text != "Hi" {
		t.Errorf("content[0].text: got %q, want Hi", text)
	}
	if reason := gjson.GetBytes(out, "stop_reason").String(); reason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn", reason)
	}
	if in := gjson.GetBytes(out, "usage.input_tokens").Int(); in != 12 {
		t.Errorf("usage.input_tokens: got %d, want 12 (message_delta override)", in)
	}
	if outTok := gjson.GetBytes(out, "usage.output_tokens").Int(); outTok != 32 {
		t.Errorf("usage.output_tokens: got %d, want 32", outTok)
	}
}

// TestCollapseMessagesSSE_MultipleTextDeltas verifies that several
// text_delta events in the same block are concatenated in order.
func TestCollapseMessagesSSE_MultipleTextDeltas(t *testing.T) {
	fixture := `data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", "}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

data: {"type":"message_stop"}
`
	out, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("collapseMessagesSSE: %v", err)
	}
	if text := gjson.GetBytes(out, "content.0.text").String(); text != "Hello, world" {
		t.Errorf("concatenated text: got %q, want %q", text, "Hello, world")
	}
}

// TestCollapseMessagesSSE_ToolUse checks that a tool_use block built from
// input_json_delta events is reassembled with the final `input` object.
func TestCollapseMessagesSSE_ToolUse(t *testing.T) {
	fixture := `data: {"type":"message_start","message":{"id":"m2","type":"message","role":"assistant","content":[]}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"web_search","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"objective\":"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"find AI projects\"}"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}

data: {"type":"message_stop"}
`
	out, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("collapseMessagesSSE: %v", err)
	}
	if kind := gjson.GetBytes(out, "content.0.type").String(); kind != "tool_use" {
		t.Errorf("block type: got %q, want tool_use", kind)
	}
	if name := gjson.GetBytes(out, "content.0.name").String(); name != "web_search" {
		t.Errorf("block name: got %q, want web_search", name)
	}
	if id := gjson.GetBytes(out, "content.0.id").String(); id != "toolu_1" {
		t.Errorf("block id: got %q, want toolu_1", id)
	}
	if obj := gjson.GetBytes(out, "content.0.input.objective").String(); obj != "find AI projects" {
		t.Errorf("input.objective: got %q, want %q", obj, "find AI projects")
	}
	if reason := gjson.GetBytes(out, "stop_reason").String(); reason != "tool_use" {
		t.Errorf("stop_reason: got %q, want tool_use", reason)
	}
}

// TestCollapseMessagesSSE_MixedTextAndToolUse reproduces the typical
// librarian tool-use turn: a leading text block followed by a tool_use
// block in the same message.
func TestCollapseMessagesSSE_MixedTextAndToolUse(t *testing.T) {
	fixture := `data: {"type":"message_start","message":{"id":"m3","type":"message","role":"assistant","content":[]}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me search."}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"web_search","input":{}}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"objective\":\"x\"}"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

data: {"type":"message_stop"}
`
	out, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("collapseMessagesSSE: %v", err)
	}
	blocks := gjson.GetBytes(out, "content").Array()
	if len(blocks) != 2 {
		t.Fatalf("content length: got %d, want 2\nraw: %s", len(blocks), out)
	}
	if kind := gjson.GetBytes(out, "content.0.type").String(); kind != "text" {
		t.Errorf("content[0].type: got %q, want text", kind)
	}
	if text := gjson.GetBytes(out, "content.0.text").String(); text != "Let me search." {
		t.Errorf("content[0].text: got %q, want %q", text, "Let me search.")
	}
	if kind := gjson.GetBytes(out, "content.1.type").String(); kind != "tool_use" {
		t.Errorf("content[1].type: got %q, want tool_use", kind)
	}
	if obj := gjson.GetBytes(out, "content.1.input.objective").String(); obj != "x" {
		t.Errorf("content[1].input.objective: got %q, want x", obj)
	}
}

// TestCollapseMessagesSSE_ErrorEvent asserts an upstream "error" SSE event
// causes collapseMessagesSSE to return an error so the caller can fall back.
func TestCollapseMessagesSSE_ErrorEvent(t *testing.T) {
	fixture := `data: {"type":"message_start","message":{"id":"m4","type":"message","role":"assistant","content":[]}}

data: {"type":"error","error":{"type":"overloaded","message":"try again"}}
`
	_, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("collapseMessagesSSE: expected error on error event, got nil")
	}
	if !strings.Contains(err.Error(), "upstream stream error") {
		t.Errorf("error message: got %q, want to contain \"upstream stream error\"", err.Error())
	}
}

// TestCollapseMessagesSSE_NoMessageStart rejects streams that never emit
// message_start — without it there is no envelope to build.
func TestCollapseMessagesSSE_NoMessageStart(t *testing.T) {
	fixture := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_stop","index":0}
`
	_, err := collapseMessagesSSE(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("collapseMessagesSSE: expected error when no message_start, got nil")
	}
}
