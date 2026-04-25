package amp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/config"
	"github.com/margbug01/amp-proxy/internal/registry"
)

func TestFallbackHandler_MissingModelProxiesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	proxy, err := createReverseProxy(upstream.URL, NewStaticSecretSource(""))
	if err != nil {
		t.Fatal(err)
	}
	fallback := NewFallbackHandler(func() *httputil.ReverseProxy { return proxy })

	handlerCalled := false
	r := gin.New()
	r.POST("/chat/completions", fallback.WrapHandler(func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusNotImplemented)
	}))
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/chat/completions", "application/json", bytes.NewReader([]byte(`{"messages":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if handlerCalled {
		t.Fatal("handler should not be called when missing model can be proxied")
	}
	if !upstreamCalled {
		t.Fatal("expected missing-model request to proxy upstream")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", resp.StatusCode)
	}
}

func TestFallbackHandler_MissingModelReturnsBadRequestWithoutUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fallback := NewFallbackHandler(func() *httputil.ReverseProxy { return nil })
	handlerCalled := false
	r := gin.New()
	r.POST("/chat/completions", fallback.WrapHandler(func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusNotImplemented)
	}))

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader([]byte(`{"messages":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if handlerCalled {
		t.Fatal("handler should not be called when missing model has no upstream")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
}

type repeatedByteReader struct {
	remaining int
	b         byte
}

func (r *repeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = r.b
	}
	r.remaining -= len(p)
	return len(p), nil
}

func TestFallbackHandler_RejectsOverLimitBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fallback := NewFallbackHandler(func() *httputil.ReverseProxy { return nil })
	handlerCalled := false

	r := gin.New()
	r.POST("/chat/completions", fallback.WrapHandler(func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", io.NopCloser(&repeatedByteReader{remaining: maxFallbackRequestBody + 1, b: 'x'}))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if handlerCalled {
		t.Fatal("wrapped handler should not be called for over-limit body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"request_body_too_large"`)) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestFallbackHandler_ModelMapping_PreservesThinkingSuffixAndRewritesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-client-amp-fallback", "codex", []*registry.ModelInfo{
		{ID: "test/gpt-5.2", OwnedBy: "openai", Type: "codex"},
	})
	defer reg.UnregisterClient("test-client-amp-fallback")

	mapper := NewModelMapper([]config.AmpModelMapping{
		{From: "gpt-5.2", To: "test/gpt-5.2"},
	})

	fallback := NewFallbackHandlerWithMapper(func() *httputil.ReverseProxy { return nil }, mapper, nil)

	handler := func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"model":      req.Model,
			"seen_model": req.Model,
		})
	}

	r := gin.New()
	r.POST("/chat/completions", fallback.WrapHandler(handler))

	reqBody := []byte(`{"model":"gpt-5.2(xhigh)"}`)
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var resp struct {
		Model     string `json:"model"`
		SeenModel string `json:"seen_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response JSON: %v", err)
	}

	if resp.Model != "gpt-5.2(xhigh)" {
		t.Errorf("Expected response model gpt-5.2(xhigh), got %s", resp.Model)
	}
	if resp.SeenModel != "test/gpt-5.2(xhigh)" {
		t.Errorf("Expected handler to see test/gpt-5.2(xhigh), got %s", resp.SeenModel)
	}
}
