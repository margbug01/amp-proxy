package customproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sseRewriter wraps an upstream SSE body and patches OpenAI Responses API
// events to work around non-compliant upstreams.
//
// The specific workaround implemented here: augment's /v1/responses endpoint
// emits a final `response.completed` event whose `response.output` array is
// empty, even though the stream already delivered full `response.output_item.done`
// events containing the reasoning and message items. Amp CLI's Stainless SDK
// reads `response.output` from the completed event as the authoritative final
// state, sees an empty array, and discards the streamed message — producing
// the "content flashes then disappears" symptom.
//
// Fix: accumulate every `response.output_item.done` item, and when the
// `response.completed` event arrives, inject the accumulated list into
// `response.output` before forwarding to the client.
//
// The rewriter streams line-by-line via bufio.Scanner so clients still see
// incremental deltas as they arrive. Only the single `response.completed`
// line is materially modified.
type sseRewriter struct {
	upstream io.ReadCloser
	buf      bytes.Buffer
	scanner  *bufio.Scanner
	items    []json.RawMessage
	closed   bool
	doneIn   bool
}

// newSSERewriter wraps the upstream body for SSE rewriting. Caller should
// set resp.Body = newSSERewriter(resp.Body) inside a ReverseProxy's
// ModifyResponse hook.
func newSSERewriter(upstream io.ReadCloser) *sseRewriter {
	r := &sseRewriter{upstream: upstream}
	r.scanner = bufio.NewScanner(upstream)
	// SSE events on reasoning upstreams can be very large (augment echoes
	// the full tools list inside response.completed). 16 MiB is a generous
	// upper bound; anything larger is a bug somewhere else anyway.
	r.scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return r
}

// Read fills p by pulling tokens from the scanner and emitting them as SSE
// lines with a trailing "\n" restored.
func (r *sseRewriter) Read(p []byte) (int, error) {
	for r.buf.Len() < len(p) && !r.doneIn {
		if !r.scanner.Scan() {
			r.doneIn = true
			if err := r.scanner.Err(); err != nil {
				log.Errorf("customproxy: sse scanner error: %v", err)
			}
			break
		}
		line := r.scanner.Bytes()

		if bytes.HasPrefix(line, []byte("data: ")) {
			payload := line[len("data: "):]
			patched := r.transformEvent(payload)
			r.buf.WriteString("data: ")
			r.buf.Write(patched)
		} else {
			r.buf.Write(line)
		}
		r.buf.WriteByte('\n')
	}

	n, _ := r.buf.Read(p)
	if r.doneIn && r.buf.Len() == 0 {
		return n, io.EOF
	}
	return n, nil
}

// Close closes the upstream body.
func (r *sseRewriter) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.upstream.Close()
}

// transformEvent inspects a single `data:` JSON payload. For
// `response.output_item.done` it records the item. For `response.completed`
// it injects the accumulated items into `response.output` when that array
// is empty. Unmodified payloads are returned unchanged.
func (r *sseRewriter) transformEvent(data []byte) []byte {
	eventType := gjson.GetBytes(data, "type").String()

	switch eventType {
	case "response.output_item.done":
		if item := gjson.GetBytes(data, "item"); item.Exists() {
			raw := make([]byte, len(item.Raw))
			copy(raw, item.Raw)
			r.items = append(r.items, raw)
		}
	case "response.completed":
		existing := gjson.GetBytes(data, "response.output")
		if existing.Exists() && existing.IsArray() && len(existing.Array()) > 0 {
			return data
		}
		if len(r.items) == 0 {
			return data
		}
		merged, err := mergeOutputArray(r.items)
		if err != nil {
			log.Errorf("customproxy: marshal output items: %v", err)
			return data
		}
		patched, err := sjson.SetRawBytes(data, "response.output", merged)
		if err != nil {
			log.Errorf("customproxy: set response.output: %v", err)
			return data
		}
		log.WithField("items", len(r.items)).Info("customproxy: patched response.completed.output")
		return patched
	}

	return data
}

// mergeOutputArray returns a JSON array built from the raw item messages.
func mergeOutputArray(items []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
