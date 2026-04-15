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

		proxy, err := buildProxy(rawURL, c.APIKey)
		if err != nil {
			log.Errorf("customproxy: failed to build proxy for %q: %v", name, err)
			continue
		}

		p := &Provider{
			Name:   name,
			URL:    rawURL,
			APIKey: c.APIKey,
			Models: append([]string(nil), c.Models...),
			proxy:  proxy,
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
// request path and Authorization header.
func buildProxy(rawURL, apiKey string) (*httputil.ReverseProxy, error) {
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

			log.WithFields(log.Fields{
				"method": req.Method,
				"from":   originalPath,
				"to":     target.Scheme + "://" + target.Host + req.URL.Path,
			}).Info("customproxy: forwarding")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Errorf("customproxy: upstream %s error: %v", target.Host, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"customproxy: upstream request failed","detail":"` + err.Error() + `"}`))
		},
		ModifyResponse: func(resp *http.Response) error {
			if isEventStream(resp.Header.Get("Content-Type")) {
				// Strip Content-Length because we may grow the response when
				// we patch response.completed. Transport will fall back to
				// chunked encoding.
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
