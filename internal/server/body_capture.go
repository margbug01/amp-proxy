package server

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// captureWriter tees every Write() into an in-memory buffer (capped) while
// still forwarding to the underlying ResponseWriter. Used for debug body
// capture of upstream proxy responses.
type captureWriter struct {
	gin.ResponseWriter
	body  *bytes.Buffer
	limit int
}

func (cw *captureWriter) Write(p []byte) (int, error) {
	if cw.body.Len() < cw.limit {
		room := cw.limit - cw.body.Len()
		if room > len(p) {
			room = len(p)
		}
		cw.body.Write(p[:room])
	}
	return cw.ResponseWriter.Write(p)
}

// Allow gin to recognise the wrapper as a CloseNotifier / Flusher / Hijacker
// indirectly via the embedded ResponseWriter. No extra work needed.

var captureSeq uint64

func shouldRedactHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "_", "-")
	switch name {
	case "authorization", "cookie", "set-cookie", "x-api-key", "x-goog-api-key", "api-key":
		return true
	default:
		return strings.HasSuffix(name, "-api-key")
	}
}

// bodyCapture returns a middleware that saves the request body, a truncated
// copy of the streamed response, and the status code to one file per
// request under dir. Only requests whose URL path contains pathSubstring
// are captured, to avoid ballooning disk usage in normal traffic.
//
// This is a development/debug tool. It holds up to 2 MiB of response body
// in memory per request and writes it out synchronously when the handler
// returns. Disable in production by removing the Use() call.
func bodyCapture(dir, pathSubstring string) gin.HandlerFunc {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Errorf("bodyCapture: mkdir %q: %v", dir, err)
	}
	const limit = 2 * 1024 * 1024
	return func(c *gin.Context) {
		if pathSubstring != "" && !strings.Contains(c.Request.URL.Path, pathSubstring) {
			c.Next()
			return
		}

		// Capture request body (bounded).
		var reqBody []byte
		if c.Request.Body != nil && c.Request.ContentLength >= 0 && c.Request.ContentLength < limit {
			b, err := io.ReadAll(c.Request.Body)
			if err == nil {
				reqBody = b
				c.Request.Body = io.NopCloser(bytes.NewReader(b))
			}
		}

		cw := &captureWriter{ResponseWriter: c.Writer, body: bytes.NewBuffer(nil), limit: limit}
		c.Writer = cw

		c.Next()

		seq := atomic.AddUint64(&captureSeq, 1)
		ts := time.Now().Format("150405.000")
		safePath := strings.Trim(c.Request.URL.Path, "/")
		safePath = strings.ReplaceAll(safePath, "/", "_")
		// Windows treats ':' as an NTFS alternate-data-stream separator, so a
		// filename like "foo:generateContent.log" silently becomes an empty
		// "foo" file with the bytes tucked into the ":generateContent.log"
		// stream. Google v1beta paths end in ":generateContent" so replace
		// unconditionally to keep capture files portable.
		safePath = strings.ReplaceAll(safePath, ":", "_")
		if safePath == "" {
			safePath = "root"
		}
		name := fmt.Sprintf("%s-%04d-%s.log", ts, seq, safePath)
		p := filepath.Join(dir, name)

		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			log.Errorf("bodyCapture: create %q: %v", p, err)
			return
		}
		defer f.Close()

		fmt.Fprintf(f, "=== %s %s ===\n", c.Request.Method, c.Request.URL.Path)
		fmt.Fprintf(f, "Client-Headers:\n")
		for k, v := range c.Request.Header {
			if shouldRedactHeader(k) {
				fmt.Fprintf(f, "  %s: <redacted>\n", k)
				continue
			}
			fmt.Fprintf(f, "  %s: %s\n", k, strings.Join(v, ", "))
		}
		fmt.Fprintf(f, "\n=== REQUEST BODY (%d bytes) ===\n", len(reqBody))
		f.Write(reqBody)
		fmt.Fprintf(f, "\n\n=== RESPONSE status=%d captured=%d of ??? ===\n", cw.Status(), cw.body.Len())
		f.Write(cw.body.Bytes())

		log.WithFields(log.Fields{
			"file":       p,
			"req_bytes":  len(reqBody),
			"resp_bytes": cw.body.Len(),
			"status":     cw.Status(),
			"path":       c.Request.URL.Path,
		}).Info("bodyCapture: saved")
	}
}
