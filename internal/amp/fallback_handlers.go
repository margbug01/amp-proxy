package amp

import (
	"bytes"
	"io"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/margbug01/amp-proxy/internal/customproxy"
	"github.com/margbug01/amp-proxy/internal/thinking"
	"github.com/margbug01/amp-proxy/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AmpRouteType represents the type of routing decision made for an Amp request
type AmpRouteType string

const (
	// RouteTypeLocalProvider indicates the request is handled by a local OAuth provider (free)
	RouteTypeLocalProvider AmpRouteType = "LOCAL_PROVIDER"
	// RouteTypeModelMapping indicates the request was remapped to another available model (free)
	RouteTypeModelMapping AmpRouteType = "MODEL_MAPPING"
	// RouteTypeAmpCredits indicates the request is forwarded to ampcode.com (uses Amp credits)
	RouteTypeAmpCredits AmpRouteType = "AMP_CREDITS"
	// RouteTypeNoProvider indicates no provider or fallback available
	RouteTypeNoProvider AmpRouteType = "NO_PROVIDER"
)

// MappedModelContextKey is the Gin context key for passing mapped model names.
const MappedModelContextKey = "mapped_model"

// logAmpRouting logs the routing decision for an Amp request with structured fields
func logAmpRouting(routeType AmpRouteType, requestedModel, resolvedModel, provider, path string) {
	fields := log.Fields{
		"component":       "amp-routing",
		"route_type":      string(routeType),
		"requested_model": requestedModel,
		"path":            path,
		"timestamp":       time.Now().Format(time.RFC3339),
	}

	if resolvedModel != "" && resolvedModel != requestedModel {
		fields["resolved_model"] = resolvedModel
	}
	if provider != "" {
		fields["provider"] = provider
	}

	switch routeType {
	case RouteTypeLocalProvider:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		log.WithFields(fields).Debugf("amp using local provider for model: %s", requestedModel)

	case RouteTypeModelMapping:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		fields["mapping"] = requestedModel + " -> " + resolvedModel
		// model mapping already logged in mapper; avoid duplicate here

	case RouteTypeAmpCredits:
		fields["cost"] = "amp_credits"
		fields["source"] = "ampcode.com"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		// Error-level: for an amp-proxy operator this is a billable event and
		// typically means the model leaked past the routing table.
		log.WithFields(fields).Errorf("amp-proxy: unmapped model %q — forwarding to ampcode.com (billable). Add this model to ampcode.custom-providers.models or ampcode.model-mappings to route it elsewhere.", requestedModel)

	case RouteTypeNoProvider:
		fields["cost"] = "none"
		fields["source"] = "error"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("no provider available for model_id: %s", requestedModel)
	}
}

// FallbackHandler wraps a standard handler with fallback logic to ampcode.com
// when the model's provider is not available in CLIProxyAPI
type FallbackHandler struct {
	getProxy           func() *httputil.ReverseProxy
	modelMapper        ModelMapper
	forceModelMappings func() bool
	// geminiRouteMode returns the current ampcode.gemini-route-mode config
	// value. Empty string or "ampcode" preserves the upstream-compatible
	// behavior (fall through to ampcode.com for Google native paths).
	// "translate" asks WrapHandler to rewrite Gemini requests into OpenAI
	// Responses shape before forwarding to the matched custom provider.
	geminiRouteMode func() string
}

// NewFallbackHandler creates a new fallback handler wrapper
// The getProxy function allows lazy evaluation of the proxy (useful when proxy is created after routes)
func NewFallbackHandler(getProxy func() *httputil.ReverseProxy) *FallbackHandler {
	return &FallbackHandler{
		getProxy:           getProxy,
		forceModelMappings: func() bool { return false },
		geminiRouteMode:    func() string { return "" },
	}
}

// NewFallbackHandlerWithMapper creates a new fallback handler with model mapping support
func NewFallbackHandlerWithMapper(getProxy func() *httputil.ReverseProxy, mapper ModelMapper, forceModelMappings func() bool) *FallbackHandler {
	if forceModelMappings == nil {
		forceModelMappings = func() bool { return false }
	}
	return &FallbackHandler{
		getProxy:           getProxy,
		modelMapper:        mapper,
		forceModelMappings: forceModelMappings,
		geminiRouteMode:    func() string { return "" },
	}
}

// SetGeminiRouteMode installs a hot-reloadable getter that decides whether
// Google native paths translate into OpenAI Responses requests or fall
// through to the ampcode.com proxy. A nil getter restores the default
// behaviour (empty string, equivalent to "ampcode").
func (fh *FallbackHandler) SetGeminiRouteMode(getter func() string) {
	if getter == nil {
		fh.geminiRouteMode = func() string { return "" }
		return
	}
	fh.geminiRouteMode = getter
}

// SetModelMapper sets the model mapper for this handler (allows late binding)
func (fh *FallbackHandler) SetModelMapper(mapper ModelMapper) {
	fh.modelMapper = mapper
}

// WrapHandler wraps a gin.HandlerFunc with fallback logic
// If the model's provider is not configured in CLIProxyAPI, it forwards to ampcode.com
func (fh *FallbackHandler) WrapHandler(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestPath := c.Request.URL.Path

		// Read the request body to extract the model name
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			log.Errorf("amp fallback: failed to read request body: %v", err)
			handler(c)
			return
		}

		// Sanitize request body: remove thinking blocks with invalid signatures
		// to prevent upstream API 400 errors
		bodyBytes = SanitizeAmpRequestBody(bodyBytes)

		// Restore the body for the handler to read
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Try to extract model from request body or URL path (for Gemini)
		modelName := extractModelFromRequest(bodyBytes, c)
		if modelName == "" {
			// Can't determine model, proceed with normal handler
			handler(c)
			return
		}

		// Normalize model (handles dynamic thinking suffixes)
		suffixResult := thinking.ParseSuffix(modelName)
		normalizedModel := suffixResult.ModelName
		thinkingSuffix := ""
		if suffixResult.HasSuffix {
			thinkingSuffix = "(" + suffixResult.RawSuffix + ")"
		}

		resolveMappedModel := func() (string, []string) {
			if fh.modelMapper == nil {
				return "", nil
			}

			mappedModel := fh.modelMapper.MapModel(modelName)
			if mappedModel == "" {
				mappedModel = fh.modelMapper.MapModel(normalizedModel)
			}
			mappedModel = strings.TrimSpace(mappedModel)
			if mappedModel == "" {
				return "", nil
			}

			// Preserve dynamic thinking suffix (e.g. "(xhigh)") when mapping applies, unless the target
			// already specifies its own thinking suffix.
			if thinkingSuffix != "" {
				mappedSuffixResult := thinking.ParseSuffix(mappedModel)
				if !mappedSuffixResult.HasSuffix {
					mappedModel += thinkingSuffix
				}
			}

			mappedBaseModel := thinking.ParseSuffix(mappedModel).ModelName
			mappedProviders := util.GetProviderName(mappedBaseModel)
			if len(mappedProviders) == 0 {
				return "", nil
			}

			return mappedModel, mappedProviders
		}

		// Track resolved model for logging (may change if mapping is applied)
		resolvedModel := normalizedModel
		usedMapping := false
		var providers []string

		// Check if model mappings should be forced ahead of local API keys
		forceMappings := fh.forceModelMappings != nil && fh.forceModelMappings()

		if forceMappings {
			// FORCE MODE: Check model mappings FIRST (takes precedence over local API keys)
			// This allows users to route Amp requests to their preferred OAuth providers
			if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
				// Mapping found and provider available - rewrite the model in request body
				bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				// Store mapped model in context for handlers that check it (like gemini bridge)
				c.Set(MappedModelContextKey, mappedModel)
				resolvedModel = mappedModel
				usedMapping = true
				providers = mappedProviders
			}

			// If no mapping applied, check for local providers
			if !usedMapping {
				providers = util.GetProviderName(normalizedModel)
			}
		} else {
			// DEFAULT MODE: Check local providers first, then mappings as fallback
			providers = util.GetProviderName(normalizedModel)

			if len(providers) == 0 {
				// No providers configured - check if we have a model mapping
				if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
					// Mapping found and provider available - rewrite the model in request body
					bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
					c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					// Store mapped model in context for handlers that check it (like gemini bridge)
					c.Set(MappedModelContextKey, mappedModel)
					resolvedModel = mappedModel
					usedMapping = true
					providers = mappedProviders
				}
			}
		}

		// Custom-provider routing (amp-proxy extension): if a customproxy
		// entry claims the resolved model, forward the request directly to
		// that upstream and bypass both the local handler chain and the
		// ampcode.com fallback. The Director in customproxy rewrites the
		// path and swaps the Authorization header to the provider's key.
		//
		// Google v1beta / v1beta1 native paths (:generateContent,
		// :streamGenerateContent) have two possible behaviours, selected
		// by ampcode.gemini-route-mode in config:
		//
		//   "" / "ampcode" (default): fall through to the ampcode.com
		//       proxy so the real Google backend services the request.
		//       This preserves strict protocol fidelity at the cost of
		//       consuming Amp credits.
		//
		//   "translate": rewrite the Gemini generateContent body into an
		//       OpenAI Responses API body, retarget the request at
		//       /v1/responses, tag the context so ModifyResponse can flip
		//       the reply back into Gemini shape, then let the customproxy
		//       ReverseProxy forward to the matched custom provider. No
		//       Amp credits are consumed, at the cost of minor parity loss
		//       (no thoughtSignature, synthesised call ids).
		//
		// Gemini requests carry the model name in the URL path rather than
		// the body, so rewriteModelInRequest was a no-op and bodyBytes
		// still contains the original request unchanged.
		customProxy := customproxy.GetGlobal().ProxyForModel(resolvedModel)
		if customProxy != nil && isGoogleNativePath(c.Request.URL.Path) {
			mode := ""
			if fh.geminiRouteMode != nil {
				mode = strings.ToLower(strings.TrimSpace(fh.geminiRouteMode()))
			}

			if mode == "translate" {
				if handled := fh.serveGeminiTranslate(c, customProxy, bodyBytes, modelName, resolvedModel, requestPath); handled {
					return
				}
				// Translator refused (only :streamGenerateContent or
				// translator error). Fall through to the ampcode.com
				// fallback below so Amp CLI still sees a usable reply.
			}

			log.WithFields(log.Fields{
				"model": resolvedModel,
				"path":  c.Request.URL.Path,
				"mode":  mode,
			}).Warn("amp fallback: customproxy skipped for Google v1beta path (format incompatible or translate mode off); forwarding directly to ampcode.com")
			if proxy := fh.getProxy(); proxy != nil {
				logAmpRouting(RouteTypeAmpCredits, modelName, "", "", requestPath)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
			// No ampcode proxy available: let the normal fall-through run.
			// That path will eventually hand the request to the Gemini
			// stub handler and return 501, which is strictly better than
			// a silent 404 from augment.
			customProxy = nil
		}
		if customProxy != nil {
			logAmpRouting(RouteTypeLocalProvider, modelName, resolvedModel, "custom", requestPath)
			// rewriteModelInRequest above changed bodyBytes' length, but
			// c.Request.ContentLength still reflects the original bytes.
			// net/http panics (ContentLength != Body length) unless we
			// realign both the field and the Content-Length header.
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			c.Request.ContentLength = int64(len(bodyBytes))
			c.Request.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
			customProxy.ServeHTTP(c.Writer, c.Request)
			return
		}

		// If no providers available, fallback to ampcode.com
		if len(providers) == 0 {
			proxy := fh.getProxy()
			if proxy != nil {
				// Log: Forwarding to ampcode.com (uses Amp credits)
				logAmpRouting(RouteTypeAmpCredits, modelName, "", "", requestPath)

				// Restore body again for the proxy
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				// Forward to ampcode.com
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}

			// No proxy available, let the normal handler return the error
			logAmpRouting(RouteTypeNoProvider, modelName, "", "", requestPath)
		}

		// Log the routing decision
		providerName := ""
		if len(providers) > 0 {
			providerName = providers[0]
		}

		if usedMapping {
			// Log: Model was mapped to another model
			log.Debugf("amp model mapping: request %s -> %s", normalizedModel, resolvedModel)
			logAmpRouting(RouteTypeModelMapping, modelName, resolvedModel, providerName, requestPath)
			rewriter := NewResponseRewriter(c.Writer, modelName)
			rewriter.suppressThinking = true
			c.Writer = rewriter
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			rewriter.Flush()
			log.Debugf("amp model mapping: response %s -> %s", resolvedModel, modelName)
		} else if len(providers) > 0 {
			// Log: Using local provider (free)
			logAmpRouting(RouteTypeLocalProvider, modelName, resolvedModel, providerName, requestPath)
			// Wrap with ResponseRewriter for local providers too, because upstream
			// proxies (e.g. NewAPI) may return a different model name and lack
			// Amp-required fields like thinking.signature.
			rewriter := NewResponseRewriter(c.Writer, modelName)
			rewriter.suppressThinking = providerName != "claude"
			c.Writer = rewriter
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			rewriter.Flush()
		} else {
			// No provider, no mapping, no proxy: fall back to the wrapped handler so it can return an error response
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
		}
	}
}

// serveGeminiTranslate rewrites a Google v1beta / v1beta1 generateContent
// request into an OpenAI Responses API request and forwards it through the
// supplied customproxy ReverseProxy. It returns true when the request was
// fully handled (response already written to c.Writer) and false when the
// caller should fall through to a different code path — either because the
// request uses :streamGenerateContent (not supported in the initial
// translate implementation) or because translation failed.
//
// The customproxy ModifyResponse hook observes the context tag installed
// here and translates augment's OpenAI Responses SSE reply back into a
// Gemini generateContent JSON body before the downstream client reads it.
func (fh *FallbackHandler) serveGeminiTranslate(c *gin.Context, customProxy *httputil.ReverseProxy, bodyBytes []byte, requestedModel, resolvedModel, requestPath string) bool {
	// Streaming Gemini replies need a matching event stream on the wire;
	// the initial translator only emits non-streaming JSON, so bail out
	// and let the caller fall through to the ampcode.com fallback.
	if strings.HasSuffix(c.Request.URL.Path, ":streamGenerateContent") {
		log.WithFields(log.Fields{
			"model": resolvedModel,
			"path":  c.Request.URL.Path,
		}).Warn("gemini-translate: streamGenerateContent not yet supported; falling through to ampcode.com")
		return false
	}
	if !strings.HasSuffix(c.Request.URL.Path, ":generateContent") {
		return false
	}

	newBody, err := customproxy.TranslateGeminiRequestToOpenAI(bodyBytes, resolvedModel)
	if err != nil {
		log.WithFields(log.Fields{
			"model": resolvedModel,
			"path":  c.Request.URL.Path,
			"err":   err,
		}).Warn("gemini-translate: request translation failed; falling through to ampcode.com")
		return false
	}

	// Rewrite the request so customproxy's Director forwards it to the
	// OpenAI Responses endpoint. The Director's extractLeaf helper strips a
	// leading /v1 prefix and reappends it from the custom provider's base
	// path, so setting URL.Path to "/v1/responses" is idempotent.
	c.Request.URL.Path = "/v1/responses"
	c.Request.URL.RawPath = ""
	c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
	c.Request.ContentLength = int64(len(newBody))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Accept", "text/event-stream")
	// The Google API client header only makes sense at the Google backend
	// and may confuse augment; drop it so the forwarded request looks like
	// a plain OpenAI Responses call.
	c.Request.Header.Del("X-Goog-Api-Client")
	c.Request.Header.Del("X-Goog-Api-Key")

	ctx := customproxy.WithGeminiTranslate(c.Request.Context(), requestedModel)
	c.Request = c.Request.WithContext(ctx)

	logAmpRouting(RouteTypeLocalProvider, requestedModel, resolvedModel, "custom-gemini-translate", requestPath)
	log.WithFields(log.Fields{
		"gemini_model":   requestedModel,
		"resolved_model": resolvedModel,
		"new_path":       c.Request.URL.Path,
		"new_body_bytes": len(newBody),
	}).Info("gemini-translate: forwarding rewritten request to customproxy")

	customProxy.ServeHTTP(c.Writer, c.Request)
	return true
}

// filterAntropicBetaHeader filters Anthropic-Beta header to remove features requiring special subscription
// This is needed when using local providers (bypassing the Amp proxy)
func filterAntropicBetaHeader(c *gin.Context) {
	if betaHeader := c.Request.Header.Get("Anthropic-Beta"); betaHeader != "" {
		if filtered := filterBetaFeatures(betaHeader, "context-1m-2025-08-07"); filtered != "" {
			c.Request.Header.Set("Anthropic-Beta", filtered)
		} else {
			c.Request.Header.Del("Anthropic-Beta")
		}
	}
}

// rewriteModelInRequest replaces the model name in a JSON request body
func rewriteModelInRequest(body []byte, newModel string) []byte {
	if !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	result, err := sjson.SetBytes(body, "model", newModel)
	if err != nil {
		log.Warnf("amp model mapping: failed to rewrite model in request body: %v", err)
		return body
	}
	return result
}

// extractModelFromRequest attempts to extract the model name from various request formats
func extractModelFromRequest(body []byte, c *gin.Context) string {
	// First try to parse from JSON body (OpenAI, Claude, etc.)
	// Check common model field names
	if result := gjson.GetBytes(body, "model"); result.Exists() && result.Type == gjson.String {
		return result.String()
	}

	// For Gemini requests, model is in the URL path
	// Standard format: /models/{model}:generateContent -> :action parameter
	if action := c.Param("action"); action != "" {
		// Split by colon to get model name (e.g., "gemini-pro:generateContent" -> "gemini-pro")
		parts := strings.Split(action, ":")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}

	// AMP CLI format: /publishers/google/models/{model}:method -> *path parameter
	// Example: /publishers/google/models/gemini-3-pro-preview:streamGenerateContent
	if path := c.Param("path"); path != "" {
		// Look for /models/{model}:method pattern
		if idx := strings.Index(path, "/models/"); idx >= 0 {
			modelPart := path[idx+8:] // Skip "/models/"
			// Split by colon to get model name
			if colonIdx := strings.Index(modelPart, ":"); colonIdx > 0 {
				return modelPart[:colonIdx]
			}
		}
	}

	return ""
}

// isGoogleNativePath reports whether the request path is Google's v1beta /
// v1beta1 generateContent shape. Custom providers in this repo (augment)
// only understand OpenAI Responses and Anthropic Messages, so the
// customproxy routing hook must skip these paths and let them fall
// through to the ampcode.com proxy instead.
func isGoogleNativePath(p string) bool {
	if strings.Contains(p, "/v1beta1/") || strings.Contains(p, "/v1beta/") {
		return true
	}
	if strings.HasSuffix(p, ":generateContent") || strings.HasSuffix(p, ":streamGenerateContent") {
		return true
	}
	return false
}
