// Package config provides the minimal configuration surface needed by the
// Amp routing module in amp-proxy.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/config/config.go
// (MIT). The upstream Config type carries settings for many providers that are
// not part of amp-proxy's scope; only the AmpCode subtree and a thin server
// struct are retained here.
package config

// Config represents the application's configuration loaded from a YAML file.
// Only the fields consumed by the amp module and by the amp-proxy server
// bootstrap are declared here.
type Config struct {
	// Host is the network host/interface on which the API server binds.
	// Empty ("") binds all interfaces.
	Host string `yaml:"host" json:"host"`

	// Port is the network port the API server listens on.
	Port int `yaml:"port" json:"port"`

	// APIKeys is the list of client API keys accepted by the local proxy.
	// These are validated by the auth middleware and are unrelated to the
	// upstream Amp API key configured under AmpCode.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// AmpCode contains Amp upstream configuration, management restrictions,
	// and model mappings.
	AmpCode AmpCode `yaml:"ampcode" json:"ampcode"`

	// Debug groups optional development-only middlewares. All fields default
	// to the zero value (everything off).
	Debug DebugConfig `yaml:"debug,omitempty" json:"debug,omitempty"`
}

// DebugConfig enables optional development-only middlewares.
// All fields default to the zero value (everything off).
type DebugConfig struct {
	// CapturePathSubstring, when non-empty, enables bodyCapture middleware
	// on every request whose URL path contains the substring. The captured
	// request and response bodies (up to 2 MiB each) are written under
	// CaptureDir, one file per request. Leave empty in production.
	CapturePathSubstring string `yaml:"capture-path-substring,omitempty" json:"capture-path-substring,omitempty"`

	// CaptureDir is the directory for bodyCapture output. Defaults to
	// "./capture" relative to the server working directory when unset and
	// CapturePathSubstring is non-empty.
	CaptureDir string `yaml:"capture-dir,omitempty" json:"capture-dir,omitempty"`
}

// AmpModelMapping defines a model name mapping for Amp CLI requests.
// When Amp requests a model that isn't available locally, this mapping
// allows routing to an alternative model that IS available.
type AmpModelMapping struct {
	// From is the model name that Amp CLI requests (e.g., "claude-opus-4.5").
	From string `yaml:"from" json:"from"`

	// To is the target model name to route to (e.g., "claude-sonnet-4").
	To string `yaml:"to" json:"to"`

	// Regex indicates whether the 'from' field should be interpreted as a
	// regular expression for matching model names. When true, this mapping
	// is evaluated after exact matches and in the order provided.
	Regex bool `yaml:"regex,omitempty" json:"regex,omitempty"`
}

// AmpCode groups Amp CLI integration settings including upstream routing,
// optional overrides, management route restrictions, and model fallback mappings.
type AmpCode struct {
	// UpstreamURL defines the upstream Amp control plane used for non-provider
	// calls (e.g. https://ampcode.com).
	UpstreamURL string `yaml:"upstream-url" json:"upstream-url"`

	// UpstreamAPIKey optionally overrides the Authorization header when
	// proxying Amp upstream calls.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// UpstreamAPIKeys maps client API keys (from top-level api-keys) to
	// per-client upstream API keys. When a request is authenticated with one
	// of the listed client keys, the associated upstream key is used.
	UpstreamAPIKeys []AmpUpstreamAPIKeyEntry `yaml:"upstream-api-keys,omitempty" json:"upstream-api-keys,omitempty"`

	// RestrictManagementToLocalhost restricts Amp management routes
	// (/api/user, /api/threads, /api/auth, /docs, /settings, etc.) to only
	// accept connections from 127.0.0.1 / ::1. Prevents drive-by browser
	// attacks and remote access to management endpoints. Default: false.
	RestrictManagementToLocalhost bool `yaml:"restrict-management-to-localhost" json:"restrict-management-to-localhost"`

	// ModelMappings defines model name mappings for Amp CLI requests.
	ModelMappings []AmpModelMapping `yaml:"model-mappings" json:"model-mappings"`

	// ForceModelMappings when true causes model mappings to take precedence
	// over local API keys. When false (default), local API keys are used first.
	ForceModelMappings bool `yaml:"force-model-mappings" json:"force-model-mappings"`

	// CustomProviders defines additional upstream endpoints keyed by model
	// name. When an incoming Amp CLI request carries a model that matches one
	// of the configured providers, the request is forwarded to that
	// provider's URL instead of the ampcode.com upstream. This is how
	// amp-proxy routes requests to third-party OpenAI-compatible endpoints.
	CustomProviders []CustomProvider `yaml:"custom-providers,omitempty" json:"custom-providers,omitempty"`

	// GeminiRouteMode controls how Google v1beta / v1beta1 generateContent
	// requests are handled when their (mapped) model would otherwise land on
	// a custom provider that only speaks OpenAI Responses / Anthropic Messages.
	//
	// Valid values:
	//
	//   ""          Default. Same behaviour as "ampcode".
	//   "ampcode"   Fall through to the ampcode.com proxy so Amp CLI's
	//               finder subagent hits the real Google Gemini backend
	//               (consumes Amp credits but guarantees fidelity).
	//   "translate" amp-proxy rewrites the Gemini request body into an
	//               OpenAI Responses API request and forwards it to the
	//               custom provider claiming the mapped model. The reply is
	//               translated back into a Gemini generateContent JSON body
	//               so Amp CLI never sees the shape change. Saves credits
	//               at the cost of a small loss of parity (no thoughtSignature).
	GeminiRouteMode string `yaml:"gemini-route-mode,omitempty" json:"gemini-route-mode,omitempty"`
}

// CustomProvider describes a single third-party upstream endpoint that
// amp-proxy can route to based on the requested model name. When the amp
// fallback handler extracts a model from an incoming request and finds it
// listed in a CustomProvider's Models slice, the request is forwarded to
// that provider's URL with Authorization rewritten to the provider's APIKey.
//
// Currently only OpenAI-compatible endpoints are tested. Anthropic or
// Gemini-shaped endpoints may work as long as the incoming Amp CLI request
// and the target endpoint share the same API shape (no format translation
// is performed).
type CustomProvider struct {
	// Name is a human-readable identifier used in logs.
	Name string `yaml:"name" json:"name"`

	// URL is the base upstream endpoint, e.g. "http://host:port/v1".
	// The amp-proxy path rewriter strips the incoming "/api/provider/<name>"
	// and "/v1" (or "/v1beta") prefix and appends the remaining path suffix
	// to this base URL.
	URL string `yaml:"url" json:"url"`

	// APIKey is substituted into the upstream Authorization header as
	// "Bearer <apiKey>". Client-side keys (x-api-key, the incoming
	// Authorization header) are dropped before forwarding.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Models is the list of model names this provider serves. Requests whose
	// body contains a matching model field are routed here.
	Models []string `yaml:"models" json:"models"`
}

// AmpUpstreamAPIKeyEntry maps a set of client API keys to a specific upstream
// API key. When a request is authenticated with one of the APIKeys, the
// corresponding UpstreamAPIKey is used for the upstream Amp request.
type AmpUpstreamAPIKeyEntry struct {
	// UpstreamAPIKey is the API key forwarded to the Amp upstream.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// APIKeys are the client API keys that map to this upstream key.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}
