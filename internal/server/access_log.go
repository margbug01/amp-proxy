package server

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// accessLog returns a Gin middleware that emits one structured log line per
// request. When modelPeek is true, POST/JSON bodies are also read up to a
// bounded size to include the "model" and "stream" fields; the body is then
// restored so downstream handlers still see the full payload.
func accessLog(modelPeek bool) gin.HandlerFunc {
	const maxPeekBytes = 1 << 20 // 1 MiB upper bound on body peek

	return func(c *gin.Context) {
		start := time.Now()
		method := c.Request.Method
		path := c.Request.URL.Path
		remote := c.ClientIP()

		model := ""
		stream := false
		if modelPeek && method == "POST" && strings.Contains(strings.ToLower(c.Request.Header.Get("Content-Type")), "json") {
			if c.Request.Body != nil && c.Request.ContentLength >= 0 && c.Request.ContentLength < maxPeekBytes {
				body, err := io.ReadAll(c.Request.Body)
				if err == nil {
					if v := gjson.GetBytes(body, "model"); v.Exists() {
						model = v.String()
					}
					if v := gjson.GetBytes(body, "stream"); v.Exists() {
						stream = v.Bool()
					}
					c.Request.Body = io.NopCloser(bytes.NewReader(body))
				}
			}
		}

		c.Next()

		status := c.Writer.Status()
		latency := time.Since(start)
		size := c.Writer.Size()
		if size < 0 {
			size = 0
		}

		fields := log.Fields{
			"status":  status,
			"latency": latency.Truncate(time.Millisecond).String(),
			"bytes":   size,
			"method":  method,
			"path":    path,
			"remote":  remote,
		}
		if model != "" {
			fields["model"] = model
		}
		if stream {
			fields["stream"] = true
		}
		if v, exists := c.Get("mapped_model"); exists {
			if s, ok := v.(string); ok && s != "" && s != model {
				fields["mapped_to"] = s
			}
		}

		entry := log.WithFields(fields)
		msg := "http"
		switch {
		case status >= 500:
			entry.Error(msg)
		case status >= 400:
			entry.Warn(msg)
		default:
			entry.Info(msg)
		}
	}
}
