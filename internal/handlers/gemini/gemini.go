// Package gemini contains stub Gemini-compatible handlers.
//
// These handlers never execute at runtime in amp-proxy: every provider-alias
// route is wrapped by amp.FallbackHandler, which checks util.GetProviderName
// (always empty) and forwards to the Amp upstream before the stub is
// reached. The stubs exist purely so the unmodified amp routes.go compiles
// against a familiar method set.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/gemini/*
// (MIT), reduced to no-op method stubs.
package gemini

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/handlers"
)

// GeminiAPIHandler is a no-op stand-in for the upstream Gemini handler.
type GeminiAPIHandler struct{}

// NewGeminiAPIHandler constructs a stub handler. The base argument is ignored.
func NewGeminiAPIHandler(_ *handlers.BaseAPIHandler) *GeminiAPIHandler {
	return &GeminiAPIHandler{}
}

// GeminiHandler is a no-op stub for POST /models/:action requests.
func (h *GeminiAPIHandler) GeminiHandler(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: gemini handler not implemented (should fall through to upstream proxy)",
	})
}

// GeminiGetHandler is a no-op stub for GET /models/:action requests.
func (h *GeminiAPIHandler) GeminiGetHandler(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: gemini get handler not implemented (should fall through to upstream proxy)",
	})
}

// GeminiModels is a no-op stub for /models listings.
func (h *GeminiAPIHandler) GeminiModels(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: gemini models handler not implemented (should fall through to upstream proxy)",
	})
}
