package customproxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// responsesSSETranslator wraps an upstream chat/completions SSE body and
// re-emits equivalent OpenAI Responses API SSE events to the downstream Amp
// CLI client.
//
// The translator runs as a state machine over the upstream stream. Each
// chat/completions "data: {...}" line carries a choice delta that may
// contain one or more of: role, reasoning_content, content, tool_calls.
// We emit Responses events as soon as the state machine can commit to a
// boundary (e.g. the first content delta flushes the in-progress reasoning
// item, the first tool_call opens a new output item, finish_reason closes
// everything and emits response.completed).
//
// Output events, in order of first emission per response:
//
//	response.created / response.in_progress     once, at the top
//	response.output_item.added(reasoning)       on first reasoning_content (if any)
//	response.output_item.done(reasoning)        on transition out of reasoning
//	response.output_item.added(message)         on first content delta (if any)
//	response.content_part.added(output_text)    paired with message
//	response.output_text.delta × N              per content chunk
//	response.output_text.done                   on message close
//	response.content_part.done                  paired
//	response.output_item.done(message)          on message close
//	response.output_item.added(function_call)   per tool_call id (first delta)
//	response.function_call_arguments.delta × N  per tool_call arguments chunk
//	response.function_call_arguments.done       on tool_call close
//	response.output_item.done(function_call)    on tool_call close
//	response.completed                          once, at the end

type responsesSSETranslator struct {
	upstream io.ReadCloser
	scanner  *bufio.Scanner
	out      bytes.Buffer
	closed   bool
	done     bool
	err      error

	tctx *responsesTranslateCtx

	// finalUsage mirrors chat/completions' usage object when the upstream
	// includes it on the final chunk. It is copied into response.completed.
	finalUsage map[string]any

	// Response metadata. respID is stable for the whole response; createdAt
	// is a Unix timestamp we synthesise once at the top.
	respID    string
	createdAt int64

	// Global per-response sequence counter for sequence_number, and
	// monotonically-increasing output_index for output_item.added events.
	sequence    int
	outputIndex int

	// Reasoning accumulator. We buffer DeepSeek's reasoning_content into
	// reasoningBuf and emit a single summary-bearing reasoning item on
	// close; encrypted_content stays empty because we have no cipher to
	// produce (Amp CLI treats it as an opaque blob and never decodes it
	// locally).
	reasoningBuf    strings.Builder
	reasoningOpen   bool
	reasoningItemID string
	reasoningIndex  int

	// Message (assistant text) accumulator. messageBuf is the running
	// output_text for response.output_text.done at the end.
	messageBuf    strings.Builder
	messageOpen   bool
	messageItemID string
	messageIndex  int

	// Per tool_call state. The index used in chat/completions delta
	// (delta.tool_calls[k].index) is the map key; output_index assigned at
	// open-time is stored in toolCall.outputIndex.
	toolCalls map[int]*toolCallState

	// Final response.completed output list — we build it as we close
	// items, then emit a single response.completed event carrying the
	// canonical `output` array.
	outputItems []map[string]any

	// emittedCreated tracks whether we've already pushed response.created
	// and response.in_progress at the top. Emission is lazy — triggered
	// by the first upstream data line that actually has a choice.
	emittedCreated   bool
	emittedCompleted bool

	// finalModel is the model string returned inside response.completed.
	// DeepSeek's chat/completions echoes the upstream model name in every
	// chunk; we snapshot it on the first delta.
	finalModel string
}

type toolCallState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	argsBuf     strings.Builder
	opened      bool
	closed      bool
}

// newResponsesSSETranslator wraps upstream for Responses-SSE emission.
func newResponsesSSETranslator(upstream io.ReadCloser, tctx *responsesTranslateCtx) *responsesSSETranslator {
	r := &responsesSSETranslator{
		upstream:  upstream,
		tctx:      tctx,
		respID:    synthResponseID(),
		createdAt: nowUnix(),
		toolCalls: make(map[int]*toolCallState),
	}
	r.scanner = bufio.NewScanner(upstream)
	r.scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return r
}

// Read pulls as many bytes as requested from the synthesised SSE stream,
// driving the underlying scanner forward as needed.
func (r *responsesSSETranslator) Read(p []byte) (int, error) {
	for r.out.Len() < len(p) && !r.done {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				r.err = err
				log.Errorf("customproxy: responses translator scanner: %v", err)
			}
			// Upstream stream ended. If we never saw a finish_reason (truncated
			// upstream), synthesise a best-effort response.completed so the
			// client isn't left waiting on an open stream.
			r.finishIfPending()
			r.done = true
			break
		}
		line := r.scanner.Text()
		if line == "" {
			continue
		}
		if err := r.processUpstreamLine(line); err != nil {
			log.Warnf("customproxy: responses translator line error: %v (line=%.120s)", err, line)
		}
	}
	n, _ := r.out.Read(p)
	if r.done && r.out.Len() == 0 {
		return n, io.EOF
	}
	return n, nil
}

// Close closes the underlying upstream body.
func (r *responsesSSETranslator) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.upstream.Close()
}

// processUpstreamLine consumes one line from the chat/completions SSE
// stream. Non-data lines (event:, :ping, blank) are silently ignored —
// chat/completions doesn't use `event:` labels.
func (r *responsesSSETranslator) processUpstreamLine(line string) error {
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		r.finishIfPending()
		r.done = true
		return nil
	}

	// The full delta event body. We only need a handful of fields.
	if !gjson.Valid(payload) {
		return fmt.Errorf("invalid json")
	}

	if !r.emittedCreated {
		if m := gjson.Get(payload, "model").String(); m != "" {
			r.finalModel = m
		}
		r.emitCreated()
	}

	finishReason := gjson.Get(payload, "choices.0.finish_reason").String()
	if usage := gjson.Get(payload, "usage"); usage.Exists() && usage.Raw != "null" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(usage.Raw), &parsed); err == nil {
			r.finalUsage = parsed
		}
	}

	// Reasoning content delta.
	if rc := gjson.Get(payload, "choices.0.delta.reasoning_content"); rc.Exists() && rc.String() != "" {
		r.handleReasoningDelta(rc.String())
	}

	// Text content delta.
	if c := gjson.Get(payload, "choices.0.delta.content"); c.Exists() && c.String() != "" {
		r.handleContentDelta(c.String())
	}

	// Tool call deltas.
	if tcs := gjson.Get(payload, "choices.0.delta.tool_calls"); tcs.IsArray() {
		for _, tc := range tcs.Array() {
			r.handleToolCallDelta(tc)
		}
	}

	if finishReason != "" {
		r.finishAll(finishReason)
	}

	return nil
}

// emitCreated pushes the response.created + response.in_progress pair
// exactly once, at the top of the stream.
func (r *responsesSSETranslator) emitCreated() {
	if r.emittedCreated {
		return
	}
	r.emittedCreated = true
	resp := r.buildResponseEnvelope("in_progress", nil)
	r.writeSSE("response.created", map[string]any{
		"type":            "response.created",
		"response":        resp,
		"sequence_number": r.nextSeq(),
	})
	r.writeSSE("response.in_progress", map[string]any{
		"type":            "response.in_progress",
		"response":        resp,
		"sequence_number": r.nextSeq(),
	})
}

// handleReasoningDelta opens the reasoning item on first delta and
// appends text to the local buffer. We emit a single output_item.done
// on flush (no per-chunk summary events) because Amp CLI's `summary:"auto"`
// mode is happy with the summary block delivered at item close.
func (r *responsesSSETranslator) handleReasoningDelta(text string) {
	if !r.reasoningOpen {
		r.reasoningOpen = true
		r.reasoningItemID = synthItemID("rs")
		r.reasoningIndex = r.outputIndex
		r.outputIndex++
		r.writeSSE("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"output_index":    r.reasoningIndex,
			"item":            r.reasoningItemAt("in_progress"),
			"sequence_number": r.nextSeq(),
		})
	}
	r.reasoningBuf.WriteString(text)
}

// flushReasoning closes any open reasoning item, emitting its final
// output_item.done and appending the item to the final output list.
func (r *responsesSSETranslator) flushReasoning() {
	if !r.reasoningOpen {
		return
	}
	r.reasoningOpen = false
	item := r.reasoningItemAt("completed")
	r.writeSSE("response.output_item.done", map[string]any{
		"type":            "response.output_item.done",
		"output_index":    r.reasoningIndex,
		"item":            item,
		"sequence_number": r.nextSeq(),
	})
	r.outputItems = append(r.outputItems, item)
}

// reasoningItemAt builds the reasoning item payload at the given status.
// DeepSeek requires reasoning_content to be passed back after tool calls;
// Amp carries reasoning turns as encrypted_content, so we stash the plaintext
// there as well as in summary[0].summary_text for later request translation.
func (r *responsesSSETranslator) reasoningItemAt(status string) map[string]any {
	reasoningText := r.reasoningBuf.String()
	return map[string]any{
		"id":                r.reasoningItemID,
		"type":              "reasoning",
		"status":            status,
		"encrypted_content": reasoningText,
		"summary": []any{map[string]any{
			"type": "summary_text",
			"text": reasoningText,
		}},
	}
}

// handleContentDelta opens the assistant message on first delta and
// streams output_text.delta events for each subsequent chunk.
func (r *responsesSSETranslator) handleContentDelta(text string) {
	// Any reasoning item must close first — content always comes after
	// reasoning in OpenAI-compliant streams.
	if r.reasoningOpen {
		r.flushReasoning()
	}
	if !r.messageOpen {
		r.messageOpen = true
		r.messageItemID = synthItemID("msg")
		r.messageIndex = r.outputIndex
		r.outputIndex++
		r.writeSSE("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"output_index":    r.messageIndex,
			"item":            r.messageItemBase("in_progress", nil),
			"sequence_number": r.nextSeq(),
		})
		r.writeSSE("response.content_part.added", map[string]any{
			"type":            "response.content_part.added",
			"content_index":   0,
			"item_id":         r.messageItemID,
			"output_index":    r.messageIndex,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
			"sequence_number": r.nextSeq(),
		})
	}
	r.messageBuf.WriteString(text)
	r.writeSSE("response.output_text.delta", map[string]any{
		"type":            "response.output_text.delta",
		"content_index":   0,
		"delta":           text,
		"item_id":         r.messageItemID,
		"logprobs":        []any{},
		"output_index":    r.messageIndex,
		"sequence_number": r.nextSeq(),
	})
}

// flushMessage closes any open assistant message.
func (r *responsesSSETranslator) flushMessage() {
	if !r.messageOpen {
		return
	}
	r.messageOpen = false
	fullText := r.messageBuf.String()
	r.writeSSE("response.output_text.done", map[string]any{
		"type":            "response.output_text.done",
		"content_index":   0,
		"item_id":         r.messageItemID,
		"logprobs":        []any{},
		"output_index":    r.messageIndex,
		"text":            fullText,
		"sequence_number": r.nextSeq(),
	})
	r.writeSSE("response.content_part.done", map[string]any{
		"type":            "response.content_part.done",
		"content_index":   0,
		"item_id":         r.messageItemID,
		"output_index":    r.messageIndex,
		"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		"sequence_number": r.nextSeq(),
	})
	contentPart := map[string]any{
		"type":        "output_text",
		"annotations": []any{},
		"logprobs":    []any{},
		"text":        fullText,
	}
	item := r.messageItemBase("completed", []any{contentPart})
	r.writeSSE("response.output_item.done", map[string]any{
		"type":            "response.output_item.done",
		"output_index":    r.messageIndex,
		"item":            item,
		"sequence_number": r.nextSeq(),
	})
	r.outputItems = append(r.outputItems, item)
}

// messageItemBase produces the shape of a message output item at a given
// status. When status == "completed", content should be populated.
func (r *responsesSSETranslator) messageItemBase(status string, content []any) map[string]any {
	m := map[string]any{
		"id":     r.messageItemID,
		"type":   "message",
		"status": status,
		"role":   "assistant",
	}
	if content != nil {
		m["content"] = content
	} else {
		m["content"] = []any{}
	}
	return m
}

// handleToolCallDelta routes an individual chat delta.tool_calls[k] entry
// to the per-index state, opening a new function_call output item the
// first time we see it, and emitting arguments.delta events on every
// subsequent chunk.
func (r *responsesSSETranslator) handleToolCallDelta(tc gjson.Result) {
	idx := int(tc.Get("index").Int())
	st, ok := r.toolCalls[idx]
	if !ok {
		st = &toolCallState{}
		r.toolCalls[idx] = st
	}

	if !st.opened {
		// Collect id/name from this first delta (may or may not have args).
		if id := tc.Get("id").String(); id != "" {
			st.callID = id
		}
		if n := tc.Get("function.name").String(); n != "" {
			st.name = n
		}
		// Defer opening until we have at least id+name (in practice DeepSeek
		// supplies both in the first chunk).
		if st.callID == "" || st.name == "" {
			// Still buffer any arguments fragment that came along.
			if a := tc.Get("function.arguments").String(); a != "" {
				st.argsBuf.WriteString(a)
			}
			return
		}

		// Close any open reasoning or message first — tool_calls arrive
		// after the assistant text in OpenAI streams.
		r.flushReasoning()
		r.flushMessage()

		st.opened = true
		st.itemID = synthItemID("fc")
		st.outputIndex = r.outputIndex
		r.outputIndex++
		r.writeSSE("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"output_index":    st.outputIndex,
			"item":            toolCallItem(st, "in_progress"),
			"sequence_number": r.nextSeq(),
		})
	}

	if a := tc.Get("function.arguments").String(); a != "" {
		st.argsBuf.WriteString(a)
		r.writeSSE("response.function_call_arguments.delta", map[string]any{
			"type":            "response.function_call_arguments.delta",
			"delta":           a,
			"item_id":         st.itemID,
			"output_index":    st.outputIndex,
			"sequence_number": r.nextSeq(),
		})
	}
}

// flushToolCalls closes every open tool_call entry, emitting the final
// arguments.done + output_item.done events in chat-delta order.
func (r *responsesSSETranslator) flushToolCalls() {
	if len(r.toolCalls) == 0 {
		return
	}
	// Preserve upstream order (tool_calls delta index).
	for idx := 0; idx < len(r.toolCalls); idx++ {
		st, ok := r.toolCalls[idx]
		if !ok || st.closed {
			continue
		}
		r.closeToolCall(st)
	}
	// Catch any out-of-band indices we didn't anticipate.
	for _, st := range r.toolCalls {
		if !st.closed {
			r.closeToolCall(st)
		}
	}
}

func (r *responsesSSETranslator) closeToolCall(st *toolCallState) {
	if !st.opened {
		// Never saw id/name — skip. Very defensive; shouldn't happen.
		st.closed = true
		return
	}
	args := st.argsBuf.String()
	r.writeSSE("response.function_call_arguments.done", map[string]any{
		"type":            "response.function_call_arguments.done",
		"arguments":       args,
		"item_id":         st.itemID,
		"output_index":    st.outputIndex,
		"sequence_number": r.nextSeq(),
	})
	item := toolCallItem(st, "completed")
	r.writeSSE("response.output_item.done", map[string]any{
		"type":            "response.output_item.done",
		"output_index":    st.outputIndex,
		"item":            item,
		"sequence_number": r.nextSeq(),
	})
	r.outputItems = append(r.outputItems, item)
	st.closed = true
}

func toolCallItem(st *toolCallState, status string) map[string]any {
	return map[string]any{
		"id":        st.itemID,
		"type":      "function_call",
		"status":    status,
		"arguments": st.argsBuf.String(),
		"call_id":   st.callID,
		"name":      st.name,
	}
}

// finishAll closes every open item and emits response.completed. Called
// when an upstream delta carries finish_reason (either "stop" or
// "tool_calls").
func (r *responsesSSETranslator) finishAll(finishReason string) {
	if !r.emittedCreated {
		r.emitCreated()
	}
	r.flushReasoning()
	if finishReason == "tool_calls" {
		// Message (if any) closes before tool_calls in OpenAI order.
		r.flushMessage()
		r.flushToolCalls()
	} else {
		r.flushMessage()
	}
	r.emitCompleted()
}

// finishIfPending is called when the upstream stream ends without an
// explicit finish_reason or [DONE]. We close whatever is open and emit
// response.completed so the client isn't left hanging.
func (r *responsesSSETranslator) finishIfPending() {
	if !r.emittedCreated {
		// Zero-delta upstream — emit a minimal created + completed pair so
		// the client sees a well-formed envelope.
		r.emitCreated()
	}
	r.flushReasoning()
	r.flushMessage()
	r.flushToolCalls()
	r.emitCompleted()
}

func (r *responsesSSETranslator) emitCompleted() {
	if r.emittedCompleted {
		return
	}
	r.emittedCompleted = true
	resp := r.buildResponseEnvelope("completed", r.outputItems)
	r.writeSSE("response.completed", map[string]any{
		"type":            "response.completed",
		"response":        resp,
		"sequence_number": r.nextSeq(),
	})
}

// buildResponseEnvelope returns a Responses "response" object at the
// given lifecycle status with the given output items.
func (r *responsesSSETranslator) buildResponseEnvelope(status string, output []map[string]any) map[string]any {
	outList := make([]any, len(output))
	for i, it := range output {
		outList[i] = it
	}
	model := r.finalModel
	if r.tctx != nil && r.tctx.origModel != "" {
		// Echo the client's original model label rather than the upstream's
		// (DeepSeek will surface its own id, but Amp logs want the label it
		// asked for). This is the same courtesy augment does.
		model = r.tctx.origModel
	}
	env := map[string]any{
		"id":                   r.respID,
		"object":               "response",
		"created_at":           r.createdAt,
		"status":               status,
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"max_tool_calls":       nil,
		"model":                model,
		"output":               outList,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": "auto", "summary": "auto"},
		"store":                false,
		"temperature":          1.0,
		"top_p":                1.0,
		"usage":                translateChatUsage(r.finalUsage),
	}
	if status == "completed" {
		env["completed_at"] = nowUnix()
	} else {
		env["completed_at"] = nil
	}
	if r.tctx != nil && r.tctx.promptCacheKey != "" {
		env["prompt_cache_key"] = r.tctx.promptCacheKey
	}
	return env
}

// writeSSE emits one Responses SSE event to r.out. Format follows the
// OpenAI Responses wire protocol: an `event:` line naming the type, a
// `data:` line with the JSON payload, and a terminating blank line.
func (r *responsesSSETranslator) writeSSE(eventName string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Errorf("customproxy: responses translator marshal %s: %v", eventName, err)
		return
	}
	fmt.Fprintf(&r.out, "event: %s\ndata: %s\n\n", eventName, b)
}

func (r *responsesSSETranslator) nextSeq() int {
	r.sequence++
	return r.sequence - 1
}

// synthResponseID returns a best-effort unique id shaped like OpenAI's
// "resp_<hex>". The only consumer (Amp CLI) uses it as an opaque
// correlator, so randomness is enough.
func synthResponseID() string {
	return "resp_" + randHex(24)
}

func synthItemID(prefix string) string {
	return prefix + "_" + randHex(24)
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		// rand.Read should never fail; fall back to a fixed tag rather
		// than panicking inside a streaming path.
		return "ffffffffffffffffffffffffffffffffffffffffffffffff"[:nBytes*2]
	}
	return hex.EncodeToString(b)
}

// nowUnix exists as a variable so tests can patch it deterministically.
var nowUnix = func() int64 {
	return time.Now().Unix()
}
