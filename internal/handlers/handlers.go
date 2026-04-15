// Package handlers holds minimal stand-ins for the upstream SDK-specific
// request handlers. In amp-proxy these types exist purely to let the
// unmodified amp routing module compile. In practice, the amp fallback
// handler short-circuits every call to these stubs by forwarding the request
// to the upstream Amp reverse proxy before the underlying handler methods are
// ever invoked.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/handlers.go
// (MIT), reduced to the bare struct surface referenced by amp.
package handlers

// BaseAPIHandler is an opaque carrier that the amp module accepts when
// registering routes. No methods are called on it in amp-proxy.
type BaseAPIHandler struct{}
