// Package customproxy routes amp-proxy requests to third-party upstream
// endpoints based on the requested model name.
//
// In amp-only minimal extraction, the default behavior is for amp.FallbackHandler
// to forward every provider-alias request to the ampcode.com reverse proxy
// because util.GetProviderName always returns empty. customproxy introduces a
// second decision point: before the ampcode.com fallback fires, the handler
// checks whether any configured CustomProvider claims the request's model. If
// so, the request is forwarded to that provider instead.
//
// The registry is a process-wide singleton so that hot-reloads in amp.go
// OnConfigUpdated can swap the active provider set atomically.
package customproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/margbug01/amp-proxy/internal/config"
)

// upgradedMessagesKey tags a request whose /v1/messages body we rewrote
// with "stream":true. ModifyResponse uses the tag to decide whether the
// incoming SSE needs to be collapsed back into a single JSON body.
type upgradedMessagesKey struct{}

// applyMessagesMutations rewrites an Anthropic /messages request body in
// place to do two things:
//  1. Upgrade non-streaming requests to streaming (workaround for augment's
//     content-loss bug; harmless for endpoints without that bug).
//  2. Merge provider-configured request-overrides into the body so upstreams
//     that require a fixed extra field (e.g. DeepSeek's reasoning_effort)
//     get it even though Amp CLI never emits one.
//
// Returns (upgraded, newBody, err). upgraded reports whether we set
// stream:true — it drives whether ModifyResponse needs to collapse the SSE
// back into a JSON body for the downstream client. newBody always carries
// forward-replayable bytes even on the no-op path so the caller can swap
// it in unconditionally.
func applyMessagesMutations(req *http.Request, overrides map[string]any) (bool, io.ReadCloser, error) {
	const maxBody = 16 * 1024 * 1024
	if req.Body == nil {
		return false, http.NoBody, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxBody))
	_ = req.Body.Close()
	if err != nil {
		return false, io.NopCloser(bytes.NewReader(raw)), err
	}
	alreadyStreaming := gjson.GetBytes(raw, "stream").Bool()
	// Fast path: nothing to do. Keeps augment's existing behaviour byte-for-
	// byte when the operator hasn't configured overrides and the client
	// already asked for streaming.
	if alreadyStreaming && len(overrides) == 0 {
		return false, io.NopCloser(bytes.NewReader(raw)), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, io.NopCloser(bytes.NewReader(raw)), fmt.Errorf("parse request body: %w", err)
	}
	upgraded := false
	if !alreadyStreaming {
		obj["stream"] = true
		upgraded = true
	}
	// Overrides deliberately win over client-supplied values: they exist
	// specifically to force a field (e.g. reasoning_effort:"max") that the
	// client would otherwise not send. If the client ever needs to shadow
	// one, the operator can drop the key from config.
	for k, v := range overrides {
		obj[k] = v
	}
	newBody, err := json.Marshal(obj)
	if err != nil {
		return false, io.NopCloser(bytes.NewReader(raw)), fmt.Errorf("marshal mutated body: %w", err)
	}
	req.ContentLength = int64(len(newBody))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(newBody)))
	// Advertise SSE so the upstream picks its streaming representation
	// even if it decides format from the Accept header.
	req.Header.Set("Accept", "text/event-stream")
	return upgraded, io.NopCloser(bytes.NewReader(newBody)), nil
}

// isEventStream reports whether the given Content-Type is an SSE stream
// we should feed through sseRewriter.
func isEventStream(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// isJSONResponsesPath reports whether the request is an OpenAI Responses
// API call that would normally return either SSE or a single JSON body.
// Used by ModifyResponse to gate the non-streaming inspection branch.
func isJSONResponsesPath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	return strings.Contains(req.URL.Path, "/v1/responses") || strings.HasSuffix(req.URL.Path, "/responses")
}

// isJSONMessagesPath reports whether the request is an Anthropic Messages
// completion call whose path ends exactly in /messages. Sibling endpoints
// like /v1/messages/count_tokens and /v1/messages/batches/... end in a
// different segment and are naturally excluded by HasSuffix.
func isJSONMessagesPath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	return strings.HasSuffix(req.URL.Path, "/messages")
}

// Provider represents a single configured upstream endpoint plus its
// pre-built ReverseProxy instance.
type Provider struct {
	Name   string
	URL    string
	APIKey string
	Models []string

	// requestOverrides is a shallow-merged patch applied to every
	// POST /messages JSON body forwarded to this provider. See
	// config.CustomProvider.RequestOverrides for semantics.
	requestOverrides map[string]any

	proxy *httputil.ReverseProxy
}

// Registry holds the active model → Provider mapping.
type Registry struct {
	mu      sync.RWMutex
	byModel map[string]*Provider
}

var globalRegistry = &Registry{byModel: make(map[string]*Provider)}

// GetGlobal returns the process-wide custom provider registry.
func GetGlobal() *Registry {
	return globalRegistry
}

// Configure replaces the active set of providers. It is safe to call
// concurrently with ProxyForModel lookups; the old map stays readable until
// the swap completes. Invalid provider entries are logged and skipped;
// other valid entries still register.
func (r *Registry) Configure(cfgs []config.CustomProvider) error {
	newMap := make(map[string]*Provider, len(cfgs)*2)

	for i := range cfgs {
		c := cfgs[i]
		name := strings.TrimSpace(c.Name)
		rawURL := strings.TrimSpace(c.URL)

		if name == "" || rawURL == "" || len(c.Models) == 0 {
			log.Warnf("customproxy: skipping invalid provider entry (name=%q url=%q models=%d)", c.Name, c.URL, len(c.Models))
			continue
		}

		// Deep-enough copy of overrides so later Configure calls can't
		// mutate an already-registered provider's map out from under an
		// in-flight Director invocation.
		var overrides map[string]any
		if len(c.RequestOverrides) > 0 {
			overrides = make(map[string]any, len(c.RequestOverrides))
			for k, v := range c.RequestOverrides {
				overrides[k] = v
			}
		}

		proxy, err := buildProxy(rawURL, c.APIKey, overrides)
		if err != nil {
			log.Errorf("customproxy: failed to build proxy for %q: %v", name, err)
			continue
		}

		p := &Provider{
			Name:             name,
			URL:              rawURL,
			APIKey:           c.APIKey,
			Models:           append([]string(nil), c.Models...),
			requestOverrides: overrides,
			proxy:            proxy,
		}

		for _, model := range c.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if existing, ok := newMap[model]; ok {
				log.Warnf("customproxy: model %q served by both %q and %q; keeping %q", model, existing.Name, name, existing.Name)
				continue
			}
			newMap[model] = p
		}
	}

	r.mu.Lock()
	r.byModel = newMap
	r.mu.Unlock()

	if len(newMap) > 0 {
		models := make([]string, 0, len(newMap))
		for k := range newMap {
			models = append(models, k)
		}
		log.Infof("customproxy: active for models: %v", models)
	} else {
		log.Debug("customproxy: no custom providers configured")
	}
	return nil
}

// ProxyForModel returns the reverse proxy that serves the given model, or
// nil if no custom provider claims it. If the exact name is not registered,
// the lookup falls back to the name with any trailing thinking suffix
// ("(high)", "(xhigh)", "(16384)", ...) stripped. This matches
// fallback_handlers.go's resolvedModel, which may carry a suffix inherited
// from the incoming request.
func (r *Registry) ProxyForModel(model string) *httputil.ReverseProxy {
	if model == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byModel[model]; ok {
		return p.proxy
	}
	if base := stripThinkingSuffix(model); base != model {
		if p, ok := r.byModel[base]; ok {
			return p.proxy
		}
	}
	return nil
}

// ProviderForModel returns the full Provider metadata for logging purposes.
// Suffix-stripped fallback matches ProxyForModel.
func (r *Registry) ProviderForModel(model string) *Provider {
	if model == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byModel[model]; ok {
		return p
	}
	if base := stripThinkingSuffix(model); base != model {
		if p, ok := r.byModel[base]; ok {
			return p
		}
	}
	return nil
}

// stripThinkingSuffix removes a trailing thinking suffix of the form
// "(content)" from a model name. Mirrors the smaller half of
// internal/thinking.ParseSuffix without introducing a dependency on it.
// Returns the input unchanged if no suffix is present.
func stripThinkingSuffix(model string) string {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen <= 0 {
		return model
	}
	if !strings.HasSuffix(model, ")") {
		return model
	}
	return model[:lastOpen]
}

// buildProxy constructs a ReverseProxy with a Director that rewrites the
// request path and Authorization header. overrides, if non-empty, is
// shallow-merged into the body of every POST /messages request before
// forwarding.
func buildProxy(rawURL, apiKey string, overrides map[string]any) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse target %q: %w", rawURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("target %q must include scheme and host", rawURL)
	}

	basePath := strings.TrimRight(target.Path, "/")
	bearer := "Bearer " + strings.TrimSpace(apiKey)
	trimmedKey := strings.TrimSpace(apiKey)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			originalPath := req.URL.Path
			leaf := extractLeaf(originalPath)

			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = basePath + leaf
			// Clear RawPath so net/http re-encodes from Path.
			req.URL.RawPath = ""
			req.Host = target.Host

			// Replace auth headers with the custom provider's API key.
			if trimmedKey != "" {
				req.Header.Set("Authorization", bearer)
			} else {
				req.Header.Del("Authorization")
			}
			req.Header.Del("x-api-key")
			req.Header.Del("X-Api-Key")

			// Drop amp-specific beta features that only apply to the
			// Anthropic control plane; they confuse plain OpenAI endpoints.
			req.Header.Del("Anthropic-Beta")
			req.Header.Del("anthropic-beta")

			// Non-streaming /v1/messages content-loss workaround plus
			// provider request-overrides injection. augment silently
			// drops assistant content in the non-streaming Anthropic
			// Messages path but serves the streaming path correctly, so
			// we flip stream:true here and collapse the SSE reply in
			// ModifyResponse. overrides are merged in the same pass so a
			// provider like DeepSeek can force reasoning_effort without
			// Amp CLI knowing about the field.
			upgraded := false
			if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
				ok, newBody, err := applyMessagesMutations(req, overrides)
				if err != nil {
					log.Warnf("customproxy: mutate /messages body failed: %v", err)
				}
				// applyMessagesMutations always returns a fresh body
				// reader, even on no-op paths, so we can swap it in
				// unconditionally.
				req.Body = newBody
				if ok {
					upgraded = true
					ctx := context.WithValue(req.Context(), upgradedMessagesKey{}, true)
					*req = *req.WithContext(ctx)
				}
			}

			log.WithFields(log.Fields{
				"method":   req.Method,
				"from":     originalPath,
				"to":       target.Scheme + "://" + target.Host + req.URL.Path,
				"upgraded": upgraded,
			}).Info("customproxy: forwarding")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Errorf("customproxy: upstream %s error: %v", target.Host, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"customproxy: upstream request failed","detail":"` + err.Error() + `"}`))
		},
		ModifyResponse: func(resp *http.Response) error {
			// Gemini translate: if the request context was tagged in
			// fallback_handlers.go, collapse the upstream OpenAI Responses
			// reply back into a Gemini generateContent JSON body before the
			// downstream Amp CLI reads it. This branch MUST run before the
			// /v1/messages and /v1/responses branches below because it
			// rewrites the body shape entirely and must not be double-
			// processed by sseRewriter or the empty-output warning paths.
			if gt := geminiTranslateFromContext(resp.Request.Context()); gt != nil {
				return translateGeminiResponse(resp, gt)
			}

			if isEventStream(resp.Header.Get("Content-Type")) {
				// Upgraded /v1/messages: collapse the SSE stream we just
				// asked augment for back into a single JSON body so the
				// downstream client (which originally sent a non-streaming
				// request) still receives the shape it expects.
				if upgraded, _ := resp.Request.Context().Value(upgradedMessagesKey{}).(bool); upgraded {
					collapsed, err := collapseMessagesSSE(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						log.Errorf("customproxy: collapseMessagesSSE failed: %v", err)
						// Fall back to an empty assistant envelope so the
						// client still sees a well-formed reply. This is
						// strictly no worse than today's broken baseline.
						collapsed = []byte(`{"type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`)
					}
					resp.Body = io.NopCloser(bytes.NewReader(collapsed))
					resp.ContentLength = int64(len(collapsed))
					resp.Header.Set("Content-Type", "application/json")
					resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(collapsed)))
					resp.Header.Del("Transfer-Encoding")
					return nil
				}
				// /v1/messages SSE that we did not upgrade (e.g. client
				// explicitly asked for streaming): pass through untouched.
				// sseRewriter targets the OpenAI Responses schema and
				// would corrupt Anthropic Messages events.
				if isJSONMessagesPath(resp.Request) {
					return nil
				}
				// /v1/responses SSE: strip Content-Length because we may
				// grow the response when we patch response.completed;
				// transport will fall back to chunked encoding.
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
				resp.Body = newSSERewriter(resp.Body)
			} else if isJSONResponsesPath(resp.Request) && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
				// Non-streaming /v1/responses branch: read up to maxInspect
				// bytes, check for the "empty output array despite completion
				// tokens" anomaly, and restore the body untouched. We do not
				// rewrite here because there is no item.done stream to
				// accumulate for a single JSON reply.
				const maxInspect = 10 * 1024 * 1024
				buf, err := io.ReadAll(io.LimitReader(resp.Body, maxInspect))
				if err != nil {
					return err
				}
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(buf))
				resp.ContentLength = int64(len(buf))

				outputLen := 0
				if arr := gjson.GetBytes(buf, "output"); arr.IsArray() {
					outputLen = len(arr.Array())
				}
				outputTokens := gjson.GetBytes(buf, "usage.output_tokens").Int()
				if outputLen == 0 && outputTokens > 0 {
					log.WithFields(log.Fields{
						"path":          resp.Request.URL.Path,
						"output_tokens": outputTokens,
					}).Warn("customproxy: non-streaming /v1/responses returned empty output array despite completion tokens; client may not render the message correctly. Consider stream: true")
				}
			} else if isJSONMessagesPath(resp.Request) && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
				// Non-streaming /v1/messages branch. Amp CLI's librarian
				// subagent hardcodes non-streaming Anthropic Messages
				// requests; augment's upstream silently drops the text
				// blocks so content:[] comes back despite a non-zero
				// usage.output_tokens. The symptom is that librarian
				// tool_use calls return an empty string to the main agent
				// and Amp CLI either bubbles a Connection error or falls
				// back to its own web_search. We only warn here so the
				// main agent's fallback path keeps working; a future fix
				// can stream-upgrade the upstream request to recover the
				// lost content.
				const maxInspect = 10 * 1024 * 1024
				buf, err := io.ReadAll(io.LimitReader(resp.Body, maxInspect))
				if err != nil {
					return err
				}
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(buf))
				resp.ContentLength = int64(len(buf))

				contentLen := 0
				if arr := gjson.GetBytes(buf, "content"); arr.IsArray() {
					contentLen = len(arr.Array())
				}
				outputTokens := gjson.GetBytes(buf, "usage.output_tokens").Int()
				if contentLen == 0 && outputTokens > 0 {
					log.WithFields(log.Fields{
						"path":          resp.Request.URL.Path,
						"output_tokens": outputTokens,
						"model":         gjson.GetBytes(buf, "model").String(),
					}).Warn("customproxy: non-streaming /v1/messages returned empty content array despite output_tokens; augment content-loss bug — librarian subagent will silently fail")
				}
			}
			return nil
		},
	}
	proxy.Transport = &retryingTransport{
		base:  http.DefaultTransport,
		delay: 250 * time.Millisecond,
	}
	return proxy, nil
}

// extractLeaf strips /api/provider/<name>/ and an optional /v1 or /v1beta
// version prefix from the incoming request path, returning the suffix that
// should be appended to the target base URL.
//
// Examples:
//
//	/api/provider/openai/v1/chat/completions   → /chat/completions
//	/api/provider/anthropic/v1/messages        → /messages
//	/v1/chat/completions                        → /chat/completions
//	/api/provider/google/v1beta/models/x:y     → /models/x:y
//	/chat/completions                           → /chat/completions
func extractLeaf(p string) string {
	stripped := p
	if strings.HasPrefix(stripped, "/api/provider/") {
		rest := stripped[len("/api/provider/"):]
		if idx := strings.Index(rest, "/"); idx >= 0 {
			stripped = rest[idx:]
		} else {
			stripped = "/"
		}
	}
	for _, prefix := range []string{"/v1beta1", "/v1beta", "/v1"} {
		if strings.HasPrefix(stripped, prefix+"/") {
			return stripped[len(prefix):]
		}
		if stripped == prefix {
			return "/"
		}
	}
	return stripped
}
