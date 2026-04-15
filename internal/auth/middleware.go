// Package auth provides the local-proxy API key middleware.
//
// The amp module expects a gin.HandlerFunc that validates incoming requests
// against a set of allowed API keys and exposes the authenticated key via
// c.Set("apiKey", key) so downstream amp logic can route per client.
package auth

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Validator holds a mutable allowlist of accepted client API keys.
// The allowlist can be replaced at runtime via SetKeys to support hot reload.
type Validator struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

// NewValidator constructs a Validator seeded with the given keys. Empty keys
// are ignored. A Validator with zero keys rejects every request.
func NewValidator(keys []string) *Validator {
	v := &Validator{}
	v.SetKeys(keys)
	return v
}

// SetKeys replaces the active allowlist.
func (v *Validator) SetKeys(keys []string) {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		set[trimmed] = struct{}{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys = set
}

// Middleware returns a gin.HandlerFunc that authenticates requests by
// comparing an extracted API key against the Validator's allowlist. On
// success it sets c.Set("apiKey", key) so the amp module can route per-client.
func (v *Validator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := extractKey(c.Request)
		if key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing API key (provide x-api-key header or Authorization: Bearer <key>)",
			})
			return
		}

		v.mu.RLock()
		_, ok := v.keys[key]
		v.mu.RUnlock()
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid API key",
			})
			return
		}

		c.Set("apiKey", key)
		c.Next()
	}
}

// extractKey pulls the client API key from the most common header slots:
// x-api-key first, then Authorization: Bearer <key>.
func extractKey(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		return k
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[len("bearer "):])
		}
		return auth
	}
	return ""
}
