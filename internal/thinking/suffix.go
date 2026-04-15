// Package thinking provides unified thinking configuration processing.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/thinking/suffix.go
// (MIT). Only ParseSuffix and the SuffixResult return type are retained; the
// broader upstream suffix / level / mode interpretation helpers are not used
// by the amp module and are omitted.
package thinking

import "strings"

// ParseSuffix extracts a thinking suffix from a model name.
//
// The suffix format is: model-name(value)
// Examples:
//   - "claude-sonnet-4-5(16384)" -> ModelName="claude-sonnet-4-5", RawSuffix="16384"
//   - "gpt-5.2(high)"            -> ModelName="gpt-5.2",           RawSuffix="high"
//   - "gemini-2.5-pro"            -> ModelName="gemini-2.5-pro",    HasSuffix=false
//
// The function only extracts the suffix; it does not validate or interpret
// the suffix content.
func ParseSuffix(model string) SuffixResult {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 {
		return SuffixResult{ModelName: model, HasSuffix: false}
	}

	if !strings.HasSuffix(model, ")") {
		return SuffixResult{ModelName: model, HasSuffix: false}
	}

	modelName := model[:lastOpen]
	rawSuffix := model[lastOpen+1 : len(model)-1]

	return SuffixResult{
		ModelName: modelName,
		HasSuffix: true,
		RawSuffix: rawSuffix,
	}
}
