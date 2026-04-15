# NOTICE

`amp-proxy` is a derivative work. Portions of this repository are copied or
adapted from:

- **CLIProxyAPI** — <https://github.com/router-for-me/CLIProxyAPI>
  - License: MIT
  - The `internal/amp/` package and several helpers under `internal/` are
    derived from CLIProxyAPI's `internal/api/modules/amp/` and adjacent
    subtrees. Each derived file carries a `// Derived from <path>@<commit>`
    comment at the top for provenance tracking.

The upstream `LICENSE` file is preserved at the repository root under `LICENSE`.

## Scope of this derivative

This project extracts **only** the Sourcegraph Amp reverse-proxy functionality
from CLIProxyAPI. All other providers (Claude Code, Gemini CLI, ChatGPT Codex,
Qwen Code, iFlow, Kimi, Antigravity), the TUI, and the broader plumbing
(go-git, pgx, minio, bubbletea, utls, etc.) are intentionally excluded.

The goal of extraction is to enable focused bug-fix maintenance of the Amp
proxy subsystem independent of the upstream project's release cadence.

## Local Divergence from Upstream

Upstream pin: `router-for-me/CLIProxyAPI` @ `8fac29631db5cbcd69f396592f4718e165464724`.

The following files diverge from the upstream baseline. Each entry lists the
file (and approximate line range where relevant) together with a short reason.

- `internal/amp/amp.go` (lines 130–133)
  - **Reason:** Upstream uses the Go 1.26 `new(value)` shortcut
    (`m.lastConfig = new(settings)`). Rewritten as
    `settingsCopy := settings; m.lastConfig = &settingsCopy` so the package
    still builds under Go 1.23+.

- `internal/amp/amp.go` (`AmpModule.geminiRouteMode`)
  - **Reason:** New method returning `AmpCode.GeminiRouteMode` under the
    module's config mutex. Passed into `FallbackHandler.SetGeminiRouteMode`
    by `routes.go` so the Gemini translate branch reads a hot-reloadable
    view of the config. Analogous to the pre-existing `forceModelMappings`
    accessor.

- `internal/amp/fallback_handlers.go` (imports, lines 1–18)
  - **Reason:** New imports `github.com/margbug01/amp-proxy/internal/customproxy`
    and `"strconv"` support the custom-provider routing hook and the
    Content-Length realignment added below.

- `internal/amp/fallback_handlers.go` (`isGoogleNativePath`, end of file)
  - **Reason:** New unexported helper used by the custom-provider routing
    hook to identify Google v1beta / v1beta1 request paths and fall
    through to ampcode.com rather than a format-incompatible custom
    provider.

- `internal/amp/fallback_handlers.go` (`WrapHandler`, lines 228–260)
  - **Reason:** Custom-provider routing hook (amp-proxy extension). After the
    force/default mode branches resolve the model, we short-circuit via
    `customproxy.GetGlobal().ProxyForModel(resolvedModel)` before the
    `len(providers) == 0` ampcode fallback, so requests for configured
    custom-provider models bypass the ampcode.com upstream entirely.
    The hook skips the short-circuit when `isGoogleNativePath` matches the
    request URL (Google `v1beta` / `v1beta1` `generateContent` shapes),
    because custom providers like augment only speak OpenAI Responses and
    Anthropic Messages. Without this guard, a Gemini model mapped onto a
    custom provider would be forwarded to an endpoint that 404s on the
    Google path, breaking Amp CLI's `finder` subagent.

- `internal/amp/fallback_handlers.go` (Gemini translate branch + `serveGeminiTranslate`)
  - **Reason:** When `ampcode.gemini-route-mode` is set to `"translate"`,
    the Google native path branch no longer unconditionally falls through
    to ampcode.com. Instead `serveGeminiTranslate` rewrites the incoming
    Gemini v1beta1 `generateContent` body into an OpenAI Responses API
    request via `customproxy.TranslateGeminiRequestToOpenAI`, retargets
    the request URL to `/v1/responses`, tags the request context with
    `customproxy.WithGeminiTranslate`, and lets the custom provider
    ReverseProxy deliver the reply. The ModifyResponse hook on the
    customproxy side reads the tag and translates the upstream OpenAI
    Responses SSE stream back into a Gemini `generateContent` JSON body
    before the downstream Amp CLI consumer reads it. `:streamGenerateContent`
    and translator-error paths still fall through to the ampcode.com
    fallback, preserving the prior behaviour from commit `061e0f7`.

- `internal/amp/fallback_handlers.go` (`SetGeminiRouteMode`, `FallbackHandler.geminiRouteMode` field)
  - **Reason:** New hot-reloadable getter injected by `routes.go` so
    `WrapHandler` can read the active `gemini-route-mode` string from
    `AmpCode` config without taking a dependency on the `config` package.
    Mirrors the existing `forceModelMappings` closure pattern.

- `internal/amp/fallback_handlers.go` (`WrapHandler`, lines 239–241)
  - **Reason:** Realigns `c.Request.ContentLength` and the `Content-Length`
    header with the rewritten body length before `customProxy.ServeHTTP`.
    Without this, `rewriteModelInRequest` leaves the net/http client
    inconsistent and the upstream `net/http: ContentLength=X with Body length Y`
    panic fires on the next hop.

- `internal/amp/fallback_handlers.go` (`logAmpRouting`, lines 66–72)
  - **Reason:** `RouteTypeAmpCredits` was upgraded from `Warnf` to `Errorf`
    with a clearer message. For an amp-proxy operator, an unmapped model is
    a billable event that indicates a routing-table miss and deserves an
    error-level signal in the run log.

- `internal/amp/routes.go` (`SetGeminiRouteMode` calls)
  - **Reason:** Two new call sites — one inside `registerProviderAliases`
    and one inside the Google v1beta1 route registration — wire the
    module's `geminiRouteMode` accessor into both `FallbackHandler`
    instances so the translate branch can read the active config.

- `internal/config/config.go` (`AmpCode.GeminiRouteMode`)
  - **Reason:** New optional field with YAML key `gemini-route-mode`.
    Empty string or `"ampcode"` preserves the existing divert-to-ampcode
    behaviour; `"translate"` activates the Gemini↔OpenAI Responses
    translator in `internal/customproxy/gemini_translator.go`. Field is
    read via `AmpModule.geminiRouteMode()` so it is hot-reloadable with
    the rest of the Amp config surface.

- `internal/access/` (moved from `sdk/access/`)
  - **Reason:** Upstream keeps its access-manager types under `sdk/access/`
    because the SDK is meant to be consumable by external integrators.
    amp-proxy has no external consumers — all code lives behind
    `internal/` — so the `sdk/` root directory was just an extra level
    of nesting. Files are imported as `sdkaccess
    "github.com/margbug01/amp-proxy/internal/access"` in three places
    (`internal/amp/amp.go`, `internal/amp/amp_test.go`,
    `internal/server/server.go`). No API changes, import alias kept as
    `sdkaccess` so future cherry-picks from upstream stay diff-friendly.

- `internal/customproxy/` (entire package, ~1200 lines)
  - **Reason:** Non-upstream. New package that routes Amp requests to
    third-party endpoints keyed by model name. Includes:
    - An SSE rewriter (`sse_rewriter.go`) that augments non-compliant
      OpenAI `response.completed` frames with a populated `output: []`
      array so downstream Amp clients stay happy.
    - A non-streaming `/v1/messages` stream-upgrade path
      (`sse_messages_collapser.go`): augment's Anthropic Messages
      endpoint drops assistant content in the non-streaming code path but
      serves streaming correctly. `Director` rewrites such requests with
      `"stream":true` and tags the request context; `ModifyResponse`
      collapses the SSE reply back into a single JSON body with
      `collapseMessagesSSE` so the downstream client sees the shape it
      expected. Without this, Amp CLI's `librarian` subagent receives
      empty tool output and the main agent silently falls back to its
      own `web_search`.
    - Two `ModifyResponse` detectors that `Warn` (without mutating the
      body) when augment returns a non-streaming `/v1/responses` reply
      whose `output:[]` is empty or a non-streaming `/v1/messages` reply
      whose `content:[]` is empty despite non-zero `usage.output_tokens`.
      Kept as a safety net in case the stream-upgrade path is ever
      bypassed.
    - A Gemini ↔ OpenAI Responses protocol translator
      (`gemini_translator.go`): exports
      `TranslateGeminiRequestToOpenAI` for the amp fallback handler and
      a `WithGeminiTranslate` context helper. The private
      `collapseResponsesSSEToGemini` and
      `translateOpenAIResponsesJSONToGemini` helpers convert augment's
      `/v1/responses` reply back into a Gemini `generateContent` JSON
      body in `ModifyResponse`. Reasoning items are dropped, call ids
      are synthesised so OpenAI's `function_call` / `function_call_output`
      pairing has a key to work with, JSON schema type values are
      lowercased from Gemini's uppercase form, and `thoughtSignature`
      bytes are stripped because augment cannot verify them. The
      ModifyResponse branch is gated on the context tag and runs before
      the existing `/v1/messages` and `/v1/responses` branches so those
      code paths stay untouched by the translator.

- `internal/handlers/{claude,gemini,openai}/` (plus `handlers.go`)
  - **Reason:** Hand-written no-op stubs that replace upstream
    `sdk/api/handlers/{claude,gemini,openai}`. They let
    `internal/amp/routes.go` compile unchanged; at runtime they are never
    reached because customproxy or the ampcode fallback short-circuits first.

- `internal/util/provider.go` (~49 lines)
  - **Reason:** Rewritten. First consults the customproxy registry and
    returns `"custom:<name>"` for a match, then falls back to
    `internal/registry`. Upstream carries a full-featured model→provider
    lookup backed by ~2000 lines of model registry data we do not ship.

- `internal/registry/registry.go` (~80 lines)
  - **Reason:** Hand-written minimal in-memory registry. Exists only so that
    upstream `internal/amp/` tests (`model_mapping_test.go`,
    `fallback_handlers_test.go`) compile. Upstream has a ~1300-line
    counterpart we intentionally omit.

- `internal/server/body_capture.go` and `internal/server/access_log.go`
  - **Reason:** Non-upstream. Local development middlewares for access
    logging and opt-in request/response body capture. `bodyCapture` is
    gated behind the new `debug.capture-path-substring` config key so it
    stays off by default.

