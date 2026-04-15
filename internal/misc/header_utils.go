// Package misc provides miscellaneous helper functions used by the amp
// reverse proxy.
//
// Derived from github.com/router-for-me/CLIProxyAPI/v6/internal/misc/header_utils.go
// (MIT). Only ScrubProxyAndFingerprintHeaders is retained.
package misc

import "net/http"

// ScrubProxyAndFingerprintHeaders removes all headers that could reveal
// proxy infrastructure, client identity, or browser fingerprints from an
// outgoing request. This ensures requests to upstream services look like
// they originate directly from a native client rather than a third-party
// client behind a reverse proxy.
func ScrubProxyAndFingerprintHeaders(req *http.Request) {
	if req == nil {
		return
	}

	// --- Proxy tracing headers ---
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("X-Forwarded-Port")
	req.Header.Del("X-Real-IP")
	req.Header.Del("Forwarded")
	req.Header.Del("Via")

	// --- Client identity headers ---
	req.Header.Del("X-Title")
	req.Header.Del("X-Stainless-Lang")
	req.Header.Del("X-Stainless-Package-Version")
	req.Header.Del("X-Stainless-Os")
	req.Header.Del("X-Stainless-Arch")
	req.Header.Del("X-Stainless-Runtime")
	req.Header.Del("X-Stainless-Runtime-Version")
	req.Header.Del("Http-Referer")
	req.Header.Del("Referer")

	// --- Browser / Chromium fingerprint headers ---
	req.Header.Del("Sec-Ch-Ua")
	req.Header.Del("Sec-Ch-Ua-Mobile")
	req.Header.Del("Sec-Ch-Ua-Platform")
	req.Header.Del("Sec-Fetch-Mode")
	req.Header.Del("Sec-Fetch-Site")
	req.Header.Del("Sec-Fetch-Dest")
	req.Header.Del("Priority")

	// --- Encoding negotiation ---
	// Some Electron-based clients add "zstd" which is a fingerprint mismatch
	// compared to the native Node.js default; drop the header entirely so
	// the upstream Go client sets its own canonical Accept-Encoding.
	req.Header.Del("Accept-Encoding")
}
