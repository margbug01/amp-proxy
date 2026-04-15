// Package logging provides Gin middleware helpers for the amp-proxy server.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/logging/gin_logger.go
// (MIT). Only SkipGinRequestLogging is retained; the upstream GinLogrusLogger
// and GinLogrusRecovery middlewares are intentionally omitted for the M1
// minimal extraction.
package logging

import "github.com/gin-gonic/gin"

const skipGinLogKey = "__gin_skip_request_logging__"

// SkipGinRequestLogging marks the provided Gin context so that any request
// logger middleware respecting this convention will skip emitting a log line
// for the associated request.
func SkipGinRequestLogging(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(skipGinLogKey, true)
}

// ShouldSkipGinRequestLogging reports whether SkipGinRequestLogging was
// previously called on the given context. Exposed for server-side loggers.
func ShouldSkipGinRequestLogging(c *gin.Context) bool {
	if c == nil {
		return false
	}
	val, exists := c.Get(skipGinLogKey)
	if !exists {
		return false
	}
	flag, ok := val.(bool)
	return ok && flag
}
