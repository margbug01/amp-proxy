// Package util provides the minimal utility surface required by the amp
// module.
//
// GetProviderName reports the set of local providers capable of serving a
// given model name. In amp-proxy there are two sources:
//
//  1. customproxy: third-party upstream endpoints configured via
//     ampcode.custom-providers in the YAML. A match here surfaces as
//     "custom:<name>" so that downstream logic (amp.fallback_handlers) can
//     distinguish it from legacy registry-based providers.
//  2. registry: the in-memory model registry, populated only by tests in
//     amp-proxy. Production code never registers anything here, so for a
//     real workload the registry branch is effectively empty.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/util/provider.go
// (MIT), reduced and rewired for amp-proxy's extraction scope.
package util

import (
	"strings"

	"github.com/margbug01/amp-proxy/internal/customproxy"
	"github.com/margbug01/amp-proxy/internal/registry"
)

// CustomProviderPrefix is the prefix stamped on provider names surfaced by
// GetProviderName when the model is served by a CustomProvider. Callers that
// need to route through the customproxy package can detect this prefix and
// fetch the proxy handler from customproxy.GetGlobal().ProxyForModel.
const CustomProviderPrefix = "custom:"

// GetProviderName reports the set of local providers that can serve the
// given model name. It checks the customproxy registry first, then falls
// back to the in-memory test registry.
func GetProviderName(modelName string) []string {
	if modelName == "" {
		return nil
	}
	if p := customproxy.GetGlobal().ProviderForModel(modelName); p != nil {
		return []string{CustomProviderPrefix + p.Name}
	}
	return registry.GetGlobalRegistry().GetModelProviders(modelName)
}

// IsCustomProvider reports whether a provider name returned by
// GetProviderName originates from the customproxy registry.
func IsCustomProvider(provider string) bool {
	return strings.HasPrefix(provider, CustomProviderPrefix)
}
