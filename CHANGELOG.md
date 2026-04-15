# Changelog

All notable changes to amp-proxy are documented in this file. The format is
loosely based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-04-16

First tagged release. Binaries for macOS (Intel + Apple Silicon), Linux
(x86_64 + arm64) and Windows (x86_64 + arm64) are attached to the GitHub
release; grab the archive matching your platform and see the
[README](README.md) for setup.

### Added

- **`customproxy` package** — model-keyed routing to any OpenAI-compatible
  endpoint. A request whose (possibly remapped) `model` field is claimed
  by a `custom-providers` entry in `config.yaml` is forwarded to that
  upstream with the provider's Bearer token.
- **Anthropic `/v1/messages` stream upgrade** — non-streaming Amp CLI
  requests (used by the `librarian` subagent) are silently upgraded to
  SSE on the wire and collapsed back into a single JSON body downstream.
  Works around an upstream content-loss bug where the non-streaming
  response comes back with an empty `content` array despite non-zero
  `usage.output_tokens`.
- **Gemini ↔ OpenAI Responses translator** — new
  `gemini-route-mode: "translate"` config switch rewrites Amp CLI's
  `finder` subagent's Google `v1beta1 generateContent` requests into
  OpenAI Responses API calls, forwards them through the custom provider,
  and translates the reply back into Gemini JSON before the client reads
  it. Covers both single-turn and multi-turn conversations, drops
  upstream-opaque `thoughtSignature` bytes, synthesises `call_id`s for
  `functionCall` ↔ `functionResponse` alignment, and lowercases Gemini's
  uppercase JSON-schema type values. Ships with unit and integration
  tests.
- **Model mappings** — rewrite the `model` field pre-routing so that,
  for example, `claude-opus-4-6` can be redirected to `gpt-5.4(high)` on
  a local endpoint. `force-model-mappings: true` makes mappings win over
  local OAuth providers when both match.
- **Ready-to-use Amp CLI routing defaults** — `config.example.yaml`
  ships pre-filled with the full 9-entry mapping table covering the
  claude / gpt / gemini model families Amp CLI actually requests.
  Operators only need to point `custom-providers` at their gateway URL.
- **`scripts/restart.ps1`** — Windows helper that kills any stale
  `amp-proxy.exe`, relaunches the server, and redirects stdout/stderr
  to `run.log`.
- **`scripts/test_gemini_translate.js`** — Node.js end-to-end smoke test
  that drives the Gemini translate path against a running amp-proxy
  instance and verifies the response shape.
- **Debug body capture middleware** — opt-in via
  `debug.capture-path-substring` in config. Writes raw request/response
  bytes for matching URL paths to `./capture/*.log`. Intended for local
  development only.
- **Bilingual README** — Chinese is the primary `README.md`; English
  lives alongside at `README_en.md`, with language-toggle links at the
  top of both files.
- **GitHub Actions CI** — runs `go build`, `go vet`, and
  `go test ./internal/customproxy/...` on every push and PR to `main`.
- **GoReleaser pipeline** — tagged commits trigger a release workflow
  that produces cross-platform archives, checksums, and a GitHub
  release with auto-grouped changelog notes.

### Fixed

- **Empty output array warning on `/v1/responses`** — `sseRewriter`
  now injects the accumulated `response.output_item.done` items into
  `response.completed` when the upstream sends an empty array, keeping
  Amp CLI's Stainless SDK from silently discarding the reply.
- **Windows NTFS filename collision in bodyCapture** — URL paths
  containing `:` (Google v1beta `generateContent` calls) previously
  wrote zero-byte files because `:` is an alternate-data-stream
  separator on NTFS. Replaced with `_` so capture files stay portable.
- **`restart.ps1` stderr loss** — stdout and stderr are now both
  redirected into `run.log`; the `run.log.err` sibling file is kept
  for legacy callers but no longer receives content by default.
- **Amp CLI `ContentLength` mismatch after `rewriteModelInRequest`**
  — custom provider forwarding now realigns both the
  `c.Request.ContentLength` field and the `Content-Length` header with
  the rewritten body length, preventing the
  `net/http: ContentLength=X with Body length Y` panic on the next hop.
- **`pre-existing logAmpRouting` noise** — the `RouteTypeAmpCredits`
  path is now logged at `Errorf` with an explicit message telling the
  operator the model leaked past the routing table and is billable.

### Changed

- **`sdk/access/` moved to `internal/access/`** — amp-proxy has no
  external SDK consumers, so the `sdk/` root directory was flattened
  into `internal/`. Import alias `sdkaccess` is preserved in the three
  call sites so future cherry-picks from upstream stay diff-friendly.
- **`internal/amp/fallback_handlers.go`** — the custom-provider
  routing hook now skips Google v1beta native paths unless
  `gemini-route-mode: translate` is on; otherwise it falls through
  to the ampcode.com proxy to preserve protocol fidelity.

### Notes

- amp-proxy is a derivative of
  [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Every
  file that diverges from the upstream baseline is listed in
  [NOTICE.md](NOTICE.md) with a short rationale.
- The extraction deliberately excludes upstream's OAuth login flows
  for ChatGPT / Claude Code / Gemini CLI. For OAuth-backed providers,
  run a separate bridge that terminates the OAuth flow and exposes a
  plain Bearer endpoint, then point a `custom-providers` entry at it.

[Unreleased]: https://github.com/margbug01/amp-proxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/margbug01/amp-proxy/releases/tag/v0.1.0
