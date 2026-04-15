package customproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tidwall/gjson"
)

// maxMessagesSSEBytes caps how much SSE we'll accumulate from the upstream
// when collapsing a streamed Anthropic Messages response. Librarian replies
// rarely exceed a few hundred KiB; 4 MiB is a paranoid ceiling that still
// keeps us well clear of OOM territory.
const maxMessagesSSEBytes = 4 * 1024 * 1024

// collapseMessagesSSE reads an Anthropic Messages API server-sent-events
// stream from r and returns a single JSON body shaped like the
// non-streaming /v1/messages response. It handles:
//
//   - message_start       seeds the top-level message envelope
//   - content_block_start initializes a content block (text / tool_use / thinking)
//   - content_block_delta appends text_delta, input_json_delta, or thinking_delta
//   - content_block_stop  finalizes the block and appends it to content
//   - message_delta       updates stop_reason / stop_sequence / usage
//   - message_stop        ends the stream
//
// Unknown event types are skipped. An "error" event aborts the collapse so
// the caller can fall back. The returned bytes are a JSON object that Amp
// CLI's non-streaming client can deserialize unchanged.
func collapseMessagesSSE(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxMessagesSSEBytes)
	scanner := bufio.NewScanner(limited)
	// Individual SSE `data:` lines can carry fairly large tool_use JSON;
	// grow the scanner buffer to tolerate them.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var envelope map[string]any
	content := []map[string]any{}

	var currentBlock map[string]any
	var currentText bytes.Buffer
	var currentPartialJSON bytes.Buffer

	finalizeBlock := func() {
		if currentBlock == nil {
			return
		}
		switch currentBlock["type"] {
		case "text":
			currentBlock["text"] = currentText.String()
		case "tool_use":
			var input any
			if currentPartialJSON.Len() == 0 {
				input = map[string]any{}
			} else if err := json.Unmarshal(currentPartialJSON.Bytes(), &input); err != nil {
				input = map[string]any{}
			}
			currentBlock["input"] = input
		case "thinking":
			currentBlock["thinking"] = currentText.String()
		}
		content = append(content, currentBlock)
		currentBlock = nil
		currentText.Reset()
		currentPartialJSON.Reset()
	}

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
		case "message_start":
			raw := gjson.GetBytes(payload, "message")
			if !raw.Exists() {
				continue
			}
			var env map[string]any
			if err := json.Unmarshal([]byte(raw.Raw), &env); err != nil {
				return nil, fmt.Errorf("message_start parse: %w", err)
			}
			envelope = env

		case "content_block_start":
			finalizeBlock()
			raw := gjson.GetBytes(payload, "content_block")
			if !raw.Exists() {
				continue
			}
			var block map[string]any
			if err := json.Unmarshal([]byte(raw.Raw), &block); err != nil {
				return nil, fmt.Errorf("content_block_start parse: %w", err)
			}
			currentBlock = block
			currentText.Reset()
			currentPartialJSON.Reset()

		case "content_block_delta":
			if currentBlock == nil {
				continue
			}
			switch gjson.GetBytes(payload, "delta.type").String() {
			case "text_delta":
				currentText.WriteString(gjson.GetBytes(payload, "delta.text").String())
			case "input_json_delta":
				currentPartialJSON.WriteString(gjson.GetBytes(payload, "delta.partial_json").String())
			case "thinking_delta":
				currentText.WriteString(gjson.GetBytes(payload, "delta.thinking").String())
			}

		case "content_block_stop":
			finalizeBlock()

		case "message_delta":
			if envelope == nil {
				envelope = map[string]any{}
			}
			if reason := gjson.GetBytes(payload, "delta.stop_reason"); reason.Type == gjson.String && reason.String() != "" {
				envelope["stop_reason"] = reason.String()
			}
			if seq := gjson.GetBytes(payload, "delta.stop_sequence"); seq.Exists() {
				if seq.Type == gjson.Null {
					envelope["stop_sequence"] = nil
				} else if seq.Type == gjson.String {
					envelope["stop_sequence"] = seq.String()
				}
			}
			if usage := gjson.GetBytes(payload, "usage"); usage.IsObject() {
				var incoming map[string]any
				if err := json.Unmarshal([]byte(usage.Raw), &incoming); err == nil {
					base, _ := envelope["usage"].(map[string]any)
					if base == nil {
						base = map[string]any{}
					}
					for k, v := range incoming {
						base[k] = v
					}
					envelope["usage"] = base
				}
			}

		case "message_stop":
			// Graceful end. Keep scanning so trailing events (if any) are
			// still consumed before we exit the loop.

		case "ping", "":
			// SSE keepalives and blank event markers.

		case "error":
			return nil, fmt.Errorf("upstream stream error: %s", string(payload))
		}
	}

	// In case the stream ended without an explicit content_block_stop, flush
	// the in-flight block so no content is silently dropped.
	finalizeBlock()

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan messages stream: %w", err)
	}
	if envelope == nil {
		return nil, errors.New("collapseMessagesSSE: no message_start event seen")
	}

	envelope["content"] = content
	return json.Marshal(envelope)
}
