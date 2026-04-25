package customproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

var outboundCaptureSeq uint64

func captureOutboundChatRequest(body []byte) {
	if strings.TrimSpace(os.Getenv("AMP_PROXY_CAPTURE_OUTBOUND")) == "" {
		return
	}
	dir := strings.TrimSpace(os.Getenv("AMP_PROXY_CAPTURE_OUTBOUND_DIR"))
	if dir == "" {
		dir = "capture_outbound"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warnf("customproxy: outbound capture mkdir %q: %v", dir, err)
		return
	}

	seq := atomic.AddUint64(&outboundCaptureSeq, 1)
	name := fmt.Sprintf("%s-%04d-chat_completions.json", time.Now().Format("150405.000"), seq)
	path := filepath.Join(dir, name)

	content := body
	var pretty map[string]any
	if err := json.Unmarshal(body, &pretty); err == nil {
		if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			content = b
		}
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		log.Warnf("customproxy: outbound capture write %q: %v", path, err)
		return
	}
	log.WithField("file", path).Info("customproxy: outbound chat request captured")
}
