# NOTICE

`amp-proxy` is a derivative work. Portions of this repository are copied or
adapted from:

- **CLIProxyAPI** — <https://github.com/router-for-me/CLIProxyAPI>
  - License: MIT
  - The `internal/amp/` package and several helpers under `internal/` and
    `sdk/` are derived from CLIProxyAPI's `internal/api/modules/amp/` and
    adjacent subtrees. Each derived file carries a `// Derived from <path>@<commit>`
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

- `internal/amp/fallback_handlers.go` (imports, lines 1–18)
  - **Reason:** New imports `github.com/margbug01/amp-proxy/internal/customproxy`
    and `"strconv"` support the custom-provider routing hook and the
    Content-Length realignment added below.

- `internal/amp/fallback_handlers.go` (`WrapHandler`, lines 228–244)
  - **Reason:** Custom-provider routing hook (amp-proxy extension). After the
    force/default mode branches resolve the model, we short-circuit via
    `customproxy.GetGlobal().ProxyForModel(resolvedModel)` before the
    `len(providers) == 0` ampcode fallback, so requests for configured
    custom-provider models bypass the ampcode.com upstream entirely.

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

- `internal/customproxy/` (entire package, ~450 lines)
  - **Reason:** Non-upstream. New package that routes Amp requests to
    third-party endpoints keyed by model name. Includes an SSE rewriter that
    augments non-compliant `response.completed` frames with an empty
    `output: []` array so downstream Amp clients stay happy. `ModifyResponse`
    also carries two content-loss detectors that `Warn` (without mutating the
    body) when augment returns:
    - a non-streaming `/v1/responses` reply whose `output:[]` is empty
      despite `usage.output_tokens > 0`; or
    - a non-streaming `/v1/messages` reply whose `content:[]` is empty
      despite `usage.output_tokens > 0`. The second case is the root cause
      of Amp CLI's `librarian` subagent silently failing — the main agent
      catches the empty tool output and falls back to its own `web_search`,
      hiding the failure in normal UI.

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

