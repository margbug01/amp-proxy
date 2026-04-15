// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/thinking/types.go
// (MIT). Only SuffixResult is retained; upstream's ThinkingConfig, ProviderApplier
// interface, and associated types pull in internal/registry and are unused by
// the amp module.
package thinking

// SuffixResult represents the result of parsing a model name for a thinking
// suffix. A thinking suffix is specified in the format model-name(value),
// where value can be a numeric budget (e.g., "16384") or a level name
// (e.g., "high").
type SuffixResult struct {
	// ModelName is the model name with the suffix removed. If no suffix was
	// found, this equals the original input.
	ModelName string

	// HasSuffix indicates whether a valid suffix was found.
	HasSuffix bool

	// RawSuffix is the content inside the parentheses, without the parentheses.
	// Empty string if HasSuffix is false.
	RawSuffix string
}
