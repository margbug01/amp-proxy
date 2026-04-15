// Package openai contains stub OpenAI-compatible handlers.
//
// These handlers never execute at runtime in amp-proxy: every provider-alias
// route is wrapped by amp.FallbackHandler, which checks util.GetProviderName
// (always empty) and forwards to the Amp upstream before the stub is
// reached. The stubs exist purely so the unmodified amp routes.go compiles
// against a familiar method set.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai/*
// (MIT), reduced to no-op method stubs.
package openai

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/handlers"
)

// OpenAIAPIHandler is a no-op stand-in for the upstream OpenAI handler.
type OpenAIAPIHandler struct{}

// NewOpenAIAPIHandler constructs a stub handler. The base argument is ignored.
func NewOpenAIAPIHandler(_ *handlers.BaseAPIHandler) *OpenAIAPIHandler {
	return &OpenAIAPIHandler{}
}

// ChatCompletions is a no-op stub.
func (h *OpenAIAPIHandler) ChatCompletions(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: openai chat completions handler not implemented (should fall through to upstream proxy)",
	})
}

// Completions is a no-op stub.
func (h *OpenAIAPIHandler) Completions(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: openai completions handler not implemented (should fall through to upstream proxy)",
	})
}

// OpenAIModels is a no-op stub.
func (h *OpenAIAPIHandler) OpenAIModels(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: openai models handler not implemented (should fall through to upstream proxy)",
	})
}

// OpenAIResponsesAPIHandler is a no-op stand-in for the upstream OpenAI
// Responses API handler.
type OpenAIResponsesAPIHandler struct{}

// NewOpenAIResponsesAPIHandler constructs a stub handler. The base argument
// is ignored.
func NewOpenAIResponsesAPIHandler(_ *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{}
}

// Responses is a no-op stub.
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
		"error": "amp-proxy: openai responses handler not implemented (should fall through to upstream proxy)",
	})
}
