package amp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/customproxy"
	"github.com/margbug01/amp-proxy/internal/handlers"
	"github.com/margbug01/amp-proxy/internal/handlers/claude"
	"github.com/margbug01/amp-proxy/internal/handlers/gemini"
	"github.com/margbug01/amp-proxy/internal/handlers/openai"
	"github.com/margbug01/amp-proxy/internal/logging"
	log "github.com/sirupsen/logrus"
)

// clientAPIKeyContextKey is the context key used to pass the client API key
// from gin.Context to the request context for SecretSource lookup.
type clientAPIKeyContextKey struct{}

// clientAPIKeyMiddleware injects the authenticated client API key from gin.Context["apiKey"]
// into the request context so that SecretSource can look it up for per-client upstream routing.
func clientAPIKeyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract the client API key from gin context (set by AuthMiddleware)
		if apiKey, exists := c.Get("apiKey"); exists {
			if keyStr, ok := apiKey.(string); ok && keyStr != "" {
				// Inject into request context for SecretSource.Get(ctx) to read
				ctx := context.WithValue(c.Request.Context(), clientAPIKeyContextKey{}, keyStr)
				c.Request = c.Request.WithContext(ctx)
			}
		}
		c.Next()
	}
}

// getClientAPIKeyFromContext retrieves the client API key from request context.
// Returns empty string if not present.
func getClientAPIKeyFromContext(ctx context.Context) string {
	if val := ctx.Value(clientAPIKeyContextKey{}); val != nil {
		if keyStr, ok := val.(string); ok {
			return keyStr
		}
	}
	return ""
}

// localhostOnlyMiddleware returns a middleware that dynamically checks the module's
// localhost restriction setting. This allows hot-reload of the restriction without restarting.
func (m *AmpModule) localhostOnlyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check current setting (hot-reloadable)
		if !m.IsRestrictedToLocalhost() {
			c.Next()
			return
		}

		// Use actual TCP connection address (RemoteAddr) to prevent header spoofing.
		// This cannot be forged by X-Forwarded-For or other client-controlled headers.
		if !isLoopbackRemoteAddr(c.Request.RemoteAddr) {
			log.Warnf("amp management: non-localhost connection from %s attempted access, denying", c.Request.RemoteAddr)
			c.AbortWithStatusJSON(403, gin.H{
				"error": "Access denied: management routes restricted to localhost",
			})
			return
		}

		c.Next()
	}
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// noCORSMiddleware disables CORS for management routes to prevent browser-based attacks.
// This overwrites any global CORS headers set by the server.
func noCORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Remove CORS headers to prevent cross-origin access from browsers
		c.Header("Access-Control-Allow-Origin", "")
		c.Header("Access-Control-Allow-Methods", "")
		c.Header("Access-Control-Allow-Headers", "")
		c.Header("Access-Control-Allow-Credentials", "")

		// For OPTIONS preflight, deny with 403
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(403)
			return
		}

		c.Next()
	}
}

// managementAvailabilityMiddleware short-circuits management routes when the upstream
// proxy is disabled, preventing noisy localhost warnings and accidental exposure.
func (m *AmpModule) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.getProxy() == nil {
			logging.SkipGinRequestLogging(c)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "amp upstream proxy not available",
			})
			return
		}
		c.Next()
	}
}

// wrapManagementAuth skips auth for selected management paths while keeping authentication elsewhere.
func wrapManagementAuth(auth gin.HandlerFunc, prefixes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		for _, prefix := range prefixes {
			if strings.HasPrefix(path, prefix) && (len(path) == len(prefix) || path[len(prefix)] == '/') {
				c.Next()
				return
			}
		}
		auth(c)
	}
}

func proxyToUpstream(c *gin.Context, getProxy func() *httputil.ReverseProxy) bool {
	proxy := getProxy()
	if proxy == nil {
		return false
	}
	proxy.ServeHTTP(c.Writer, c.Request)
	return true
}

func synthesizeCustomProviderModels(c *gin.Context) {
	models := customproxy.GetGlobal().ModelIDs()
	if len(models) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "amp upstream proxy not available"})
		return
	}

	data := make([]gin.H, 0, len(models))
	for _, model := range models {
		data = append(data, gin.H{
			"id":       model,
			"object":   "model",
			"owned_by": "custom",
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}

// registerManagementRoutes registers Amp management proxy routes
// These routes proxy through to the Amp control plane for OAuth, user management, etc.
// Uses dynamic middleware and proxy getter for hot-reload support.
// The auth middleware validates Authorization header against configured API keys.
func (m *AmpModule) registerManagementRoutes(engine *gin.Engine, baseHandler *handlers.BaseAPIHandler, auth gin.HandlerFunc) {
	ampAPI := engine.Group("/api")

	// Always disable CORS for management routes to prevent browser-based attacks
	ampAPI.Use(m.managementAvailabilityMiddleware(), noCORSMiddleware())

	// Apply dynamic localhost-only restriction (hot-reloadable via m.IsRestrictedToLocalhost())
	ampAPI.Use(m.localhostOnlyMiddleware())

	// Apply authentication middleware - requires valid API key in Authorization header
	var authWithBypass gin.HandlerFunc
	if auth != nil {
		ampAPI.Use(auth)
		authWithBypass = wrapManagementAuth(auth, "/threads", "/auth", "/docs", "/settings")
	}

	// Inject client API key into request context for per-client upstream routing
	ampAPI.Use(clientAPIKeyMiddleware())

	// Dynamic proxy handler that uses m.getProxy() for hot-reload support
	proxyHandler := func(c *gin.Context) {
		// Swallow ErrAbortHandler panics from ReverseProxy copyResponse to avoid noisy stack traces
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// Upstream already wrote the status (often 404) before the client/stream ended.
					return
				}
				panic(rec)
			}
		}()

		if !proxyToUpstream(c, m.getProxy) {
			c.JSON(503, gin.H{"error": "amp upstream proxy not available"})
		}
	}

	// Management routes - these are proxied directly to Amp upstream
	ampAPI.Any("/internal", proxyHandler)
	ampAPI.Any("/internal/*path", proxyHandler)
	ampAPI.Any("/user", proxyHandler)
	ampAPI.Any("/user/*path", proxyHandler)
	ampAPI.Any("/auth", proxyHandler)
	ampAPI.Any("/auth/*path", proxyHandler)
	ampAPI.Any("/meta", proxyHandler)
	ampAPI.Any("/meta/*path", proxyHandler)
	ampAPI.Any("/ads", proxyHandler)
	ampAPI.Any("/telemetry", proxyHandler)
	ampAPI.Any("/telemetry/*path", proxyHandler)
	ampAPI.Any("/threads", proxyHandler)
	ampAPI.Any("/threads/*path", proxyHandler)
	ampAPI.Any("/otel", proxyHandler)
	ampAPI.Any("/otel/*path", proxyHandler)
	ampAPI.Any("/tab", proxyHandler)
	ampAPI.Any("/tab/*path", proxyHandler)

	// Root-level routes that AMP CLI expects without /api prefix
	// These need the same security middleware as the /api/* routes (dynamic for hot-reload)
	rootMiddleware := []gin.HandlerFunc{m.managementAvailabilityMiddleware(), noCORSMiddleware(), m.localhostOnlyMiddleware()}
	if authWithBypass != nil {
		rootMiddleware = append(rootMiddleware, func(c *gin.Context) {
			if m.IsRestrictedToLocalhost() && isLoopbackRemoteAddr(c.Request.RemoteAddr) {
				authWithBypass(c)
				return
			}
			auth(c)
		})
	}
	// Add clientAPIKeyMiddleware after auth for per-client upstream routing
	rootMiddleware = append(rootMiddleware, clientAPIKeyMiddleware())
	engine.GET("/threads", append(rootMiddleware, proxyHandler)...)
	engine.GET("/threads/*path", append(rootMiddleware, proxyHandler)...)
	engine.GET("/docs", append(rootMiddleware, proxyHandler)...)
	engine.GET("/docs/*path", append(rootMiddleware, proxyHandler)...)
	engine.GET("/settings", append(rootMiddleware, proxyHandler)...)
	engine.GET("/settings/*path", append(rootMiddleware, proxyHandler)...)

	engine.GET("/threads.rss", append(rootMiddleware, proxyHandler)...)
	engine.GET("/news.rss", append(rootMiddleware, proxyHandler)...)

	// Root-level auth routes for CLI login flow
	// Amp uses multiple auth routes: /auth/cli-login, /auth/callback, /auth/sign-in, /auth/logout
	// We proxy all /auth/* to support the complete OAuth flow
	engine.Any("/auth", append(rootMiddleware, proxyHandler)...)
	engine.Any("/auth/*path", append(rootMiddleware, proxyHandler)...)

	// Google v1beta1 passthrough with OAuth fallback
	// AMP CLI uses non-standard paths like /publishers/google/models/...
	// We bridge these to our standard Gemini handler to enable local OAuth.
	// If no local OAuth is available, falls back to ampcode.com proxy.
	geminiHandlers := gemini.NewGeminiAPIHandler(baseHandler)
	geminiBridge := createGeminiBridgeHandler(geminiHandlers.GeminiHandler)
	geminiV1Beta1Fallback := NewFallbackHandlerWithMapper(func() *httputil.ReverseProxy {
		return m.getProxy()
	}, m.modelMapper, m.forceModelMappings)
	geminiV1Beta1Fallback.SetGeminiRouteMode(m.geminiRouteMode)
	geminiV1Beta1Handler := geminiV1Beta1Fallback.WrapHandler(geminiBridge)

	// Route POST model calls through Gemini bridge with FallbackHandler.
	// FallbackHandler checks provider -> mapping -> proxy fallback automatically.
	// All other methods (e.g., GET model listing) always proxy to upstream to preserve Amp CLI behavior.
	ampAPI.Any("/provider/google/v1beta1/*path", func(c *gin.Context) {
		if c.Request.Method == "POST" {
			if path := c.Param("path"); strings.Contains(path, "/models/") {
				// POST with /models/ path -> use Gemini bridge with fallback handler
				// FallbackHandler will check provider/mapping and proxy if needed
				geminiV1Beta1Handler(c)
				return
			}
		}
		// Non-POST or no local provider available -> proxy upstream
		proxyHandler(c)
	})
}

// registerProviderAliases registers /api/provider/{provider}/... routes
// These allow Amp CLI to route requests like:
//
//	/api/provider/openai/v1/chat/completions
//	/api/provider/anthropic/v1/messages
//	/api/provider/google/v1beta/models
func (m *AmpModule) registerProviderAliases(engine *gin.Engine, baseHandler *handlers.BaseAPIHandler, auth gin.HandlerFunc) {
	// Create handler instances for different providers
	openaiHandlers := openai.NewOpenAIAPIHandler(baseHandler)
	geminiHandlers := gemini.NewGeminiAPIHandler(baseHandler)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(baseHandler)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(baseHandler)

	// Create fallback handler wrapper that forwards to ampcode.com when provider not found
	// Uses m.getProxy() for hot-reload support (proxy can be updated at runtime)
	// Also includes model mapping support for routing unavailable models to alternatives
	fallbackHandler := NewFallbackHandlerWithMapper(func() *httputil.ReverseProxy {
		return m.getProxy()
	}, m.modelMapper, m.forceModelMappings)
	fallbackHandler.SetGeminiRouteMode(m.geminiRouteMode)

	// Provider-specific routes under /api/provider/:provider
	ampProviders := engine.Group("/api/provider")
	if auth != nil {
		ampProviders.Use(auth)
	}
	// Inject client API key into request context for per-client upstream routing
	ampProviders.Use(clientAPIKeyMiddleware())

	provider := ampProviders.Group("/:provider")

	ampModelsHandler := func(c *gin.Context) {
		if proxyToUpstream(c, m.getProxy) {
			return
		}
		synthesizeCustomProviderModels(c)
	}

	// Root-level routes (for providers that omit /v1, like groq/cerebras)
	// Wrap handlers with fallback logic to forward to ampcode.com when provider not found
	provider.GET("/models", ampModelsHandler)
	provider.POST("/chat/completions", fallbackHandler.WrapHandler(openaiHandlers.ChatCompletions))
	provider.POST("/completions", fallbackHandler.WrapHandler(openaiHandlers.Completions))
	provider.POST("/responses", fallbackHandler.WrapHandler(openaiResponsesHandlers.Responses))

	// /v1 routes (OpenAI/Claude-compatible endpoints)
	v1Amp := provider.Group("/v1")
	{
		v1Amp.GET("/models", ampModelsHandler)

		// OpenAI-compatible endpoints with fallback
		v1Amp.POST("/chat/completions", fallbackHandler.WrapHandler(openaiHandlers.ChatCompletions))
		v1Amp.POST("/completions", fallbackHandler.WrapHandler(openaiHandlers.Completions))
		v1Amp.POST("/responses", fallbackHandler.WrapHandler(openaiResponsesHandlers.Responses))

		// Claude/Anthropic-compatible endpoints with fallback
		v1Amp.POST("/messages", fallbackHandler.WrapHandler(claudeCodeHandlers.ClaudeMessages))
		v1Amp.POST("/messages/count_tokens", fallbackHandler.WrapHandler(claudeCodeHandlers.ClaudeCountTokens))
	}

	// /v1beta routes (Gemini native API)
	// Note: Gemini handler extracts model from URL path, so fallback logic needs special handling
	v1betaAmp := provider.Group("/v1beta")
	{
		v1betaAmp.GET("/models", ampModelsHandler)
		v1betaAmp.POST("/models/*action", fallbackHandler.WrapHandler(geminiHandlers.GeminiHandler))
		v1betaAmp.GET("/models/*action", func(c *gin.Context) {
			if !proxyToUpstream(c, m.getProxy) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "amp upstream proxy not available"})
			}
		})
	}
}
