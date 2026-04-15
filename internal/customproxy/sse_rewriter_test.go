package customproxy

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// readAllFromRewriter wraps a string SSE fixture with an sseRewriter and
// drains it to completion, returning the full transformed output.
func readAllFromRewriter(t *testing.T, fixture string) []byte {
	t.Helper()
	src := io.NopCloser(strings.NewReader(fixture))
	rw := newSSERewriter(src)
	out, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("readAllFromRewriter: io.ReadAll returned err: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("readAllFromRewriter: Close returned err: %v", err)
	}
	return out
}

// extractCompletedPayload returns the `data:` bytes of the
// `response.completed` SSE line, or nil if none is found.
func extractCompletedPayload(t *testing.T, out []byte) []byte {
	t.Helper()
	// Walk each line, find the one whose payload has "type":"response.completed".
	lines := bytes.Split(out, []byte{'\n'})
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[len("data: "):]
		if gjson.GetBytes(payload, "type").String() == "response.completed" {
			return payload
		}
	}
	return nil
}

func TestSSERewriter_PatchesEmptyOutputArray(t *testing.T) {
	fixture := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"..."}]},"output_index":0,"sequence_number":1}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},"output_index":1,"sequence_number":2}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}
`
	out := readAllFromRewriter(t, fixture)

	payload := extractCompletedPayload(t, out)
	if payload == nil {
		t.Fatalf("could not find response.completed line in output:\n%s", out)
	}

	gotLen := gjson.GetBytes(payload, "response.output.#").Int()
	if gotLen != 2 {
		t.Fatalf("response.output length: got %d, want 2\npayload: %s", gotLen, payload)
	}

	if id := gjson.GetBytes(payload, "response.output.0.id").String(); id != "rs_1" {
		t.Errorf("response.output[0].id: got %q, want %q", id, "rs_1")
	}
	if id := gjson.GetBytes(payload, "response.output.1.id").String(); id != "msg_1" {
		t.Errorf("response.output[1].id: got %q, want %q", id, "msg_1")
	}
}

func TestSSERewriter_IdempotentOnNonEmptyOutput(t *testing.T) {
	// Upstream already populated response.output with one item; rewriter
	// must not overwrite or duplicate it even though it also saw an
	// item.done event.
	fixture := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_ignored","type":"message","role":"assistant","content":[{"type":"output_text","text":"ignored"}]},"output_index":0,"sequence_number":1}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_real","type":"message","role":"assistant","content":[{"type":"output_text","text":"real"}]}]}}
`
	out := readAllFromRewriter(t, fixture)

	payload := extractCompletedPayload(t, out)
	if payload == nil {
		t.Fatalf("could not find response.completed line in output:\n%s", out)
	}

	gotLen := gjson.GetBytes(payload, "response.output.#").Int()
	if gotLen != 1 {
		t.Fatalf("response.output length: got %d, want 1 (should be idempotent)\npayload: %s", gotLen, payload)
	}
	if id := gjson.GetBytes(payload, "response.output.0.id").String(); id != "msg_real" {
		t.Errorf("response.output[0].id: got %q, want %q", id, "msg_real")
	}
}

func TestSSERewriter_NonDataLinesPassThrough(t *testing.T) {
	// Ensure event: lines, blank lines, and non-data: lines survive
	// unchanged. We include no completed/done events so the transform
	// pipeline has nothing to patch.
	fixture := "event: ping\n" +
		"\n" +
		": heartbeat\n" +
		"event: custom\n" +
		"data: {\"type\":\"noop\"}\n"

	out := readAllFromRewriter(t, fixture)

	// Every line in the fixture should appear verbatim in the output, in order.
	// bufio.Scanner strips the trailing "\n" per token and the rewriter adds
	// one back, so the output should be byte-for-byte identical to fixture.
	if !bytes.Equal(out, []byte(fixture)) {
		t.Errorf("non-data passthrough mismatch:\n  got: %q\n want: %q", out, fixture)
	}
}

func TestSSERewriter_MultipleItemsInOrder(t *testing.T) {
	fixture := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning"},"output_index":0,"sequence_number":1}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]},"output_index":1,"sequence_number":2}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]},"output_index":2,"sequence_number":3}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}
`
	out := readAllFromRewriter(t, fixture)

	payload := extractCompletedPayload(t, out)
	if payload == nil {
		t.Fatalf("could not find response.completed line in output:\n%s", out)
	}

	gotLen := gjson.GetBytes(payload, "response.output.#").Int()
	if gotLen != 3 {
		t.Fatalf("response.output length: got %d, want 3\npayload: %s", gotLen, payload)
	}

	wantIDs := []string{"rs_1", "msg_1", "msg_2"}
	for i, want := range wantIDs {
		got := gjson.GetBytes(payload, "response.output."+itoa(i)+".id").String()
		if got != want {
			t.Errorf("response.output[%d].id: got %q, want %q", i, got, want)
		}
	}
}

// itoa is a tiny helper so we don't pull in strconv just for one format.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func TestSSERewriter_SplitAcrossSmallReadBuffers(t *testing.T) {
	// Same fixture drained with a 16-byte read loop vs. one big io.ReadAll.
	// Both paths must produce identical bytes; this guards against state
	// corruption when bufio.Scanner straddles a Read boundary.
	fixture := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking out loud"}]},"output_index":0,"sequence_number":1}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer here"}]},"output_index":1,"sequence_number":2}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}
`

	// Baseline: one big read.
	baseline := readAllFromRewriter(t, fixture)

	// Small-buffer drain.
	src := io.NopCloser(strings.NewReader(fixture))
	rw := newSSERewriter(src)
	defer rw.Close()

	var small bytes.Buffer
	tmp := make([]byte, 16)
	for {
		n, err := rw.Read(tmp)
		if n > 0 {
			small.Write(tmp[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("small-buffer Read: unexpected err: %v", err)
		}
	}

	if !bytes.Equal(baseline, small.Bytes()) {
		t.Errorf("streaming mismatch between one-shot and small-buffer drain\n  baseline (%d bytes): %q\n  small    (%d bytes): %q",
			len(baseline), baseline, small.Len(), small.Bytes())
	}

	// Extra paranoia: the small-buffer payload must still carry the patch.
	payload := extractCompletedPayload(t, small.Bytes())
	if payload == nil {
		t.Fatalf("small-buffer output missing response.completed line")
	}
	if gjson.GetBytes(payload, "response.output.#").Int() != 2 {
		t.Errorf("small-buffer response.output length: got %d, want 2",
			gjson.GetBytes(payload, "response.output.#").Int())
	}
}
