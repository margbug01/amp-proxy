// Package claude contains stub Claude-compatible handlers.
//
// These handlers never execute at runtime in amp-proxy: every provider-alias
// route is wrapped by amp.FallbackHandler, which checks util.GetProviderName
// (always empty) and forwards to the Amp upstream before the stub is
// reached. The stubs exist purely so the unmodified amp routes.go compiles
// against a familiar method set.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude/*
// (MIT), reduced to no-op method stubs.
package claude

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/handlers"
)

// ClaudeCodeAPIHandler is a no-op stand-in for the upstream Claude Code
// handler. All methods return 501 Not Implemented; amp.FallbackHandler should
// always short-circuit before these fire.
type ClaudeCodeAPIHandler struct{}

// NewClaudeCodeAPIHandler constructs a stub handler. The base argument is
// ignored.
func NewClaudeCodeAPIHandler(_ *handlers.BaseAPIHandler) *ClaudeCodeAPIHandler {
	return &ClaudeCodeAPIHandler{}
}

// ClaudeMessages is a no-op stub. It should never be reached; a 501 response
// is emitted defensively if it is.
func (h *ClaudeCodeAPIHandler) ClaudeMessages(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: claude messages handler not implemented (should fall through to upstream proxy)",
	})
}

// ClaudeCountTokens is a no-op stub.
func (h *ClaudeCodeAPIHandler) ClaudeCountTokens(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: claude count-tokens handler not implemented (should fall through to upstream proxy)",
	})
}

// ClaudeModels is a no-op stub. It is wired to GET /models routes and
// returns 501 because amp-proxy expects the upstream proxy to handle model
// listings.
func (h *ClaudeCodeAPIHandler) ClaudeModels(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: claude models handler not implemented (should fall through to upstream proxy)",
	})
}
