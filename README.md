# amp-proxy

A focused reverse proxy for the [Sourcegraph Amp CLI](https://ampcode.com),
derived from [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

amp-proxy sits between Amp CLI and its upstream, lets you route specific
models to third-party OpenAI-compatible endpoints, and fixes a handful of
issues in the Amp request path that would otherwise break subagents on
non-standard providers.

---

## Why it exists

Running Amp CLI against a large self-hosted OpenAI-compatible endpoint
instead of `ampcode.com` is a useful pattern — it keeps control over model
choice and billing, and works offline. Upstream CLIProxyAPI can do this in
principle, but its Amp integration has a few rough edges on third-party
providers:

- Non-streaming `/v1/messages` requests (used by Amp CLI's `librarian`
  subagent) silently return empty content from some upstreams, causing
  `librarian` to fall back to its own web search.
- Google v1beta1 `generateContent` requests (used by the `finder`
  subagent) 404 on any upstream that only speaks OpenAI Responses /
  Anthropic Messages.
- Small windows-hosting, logging, and stream-handling papercuts.

`amp-proxy` is the minimum subset of CLIProxyAPI needed to host the Amp
reverse proxy, plus a `customproxy` package that routes models to
third-party endpoints and translates between the protocols Amp CLI speaks
and the protocols real-world gateways speak.

---

## Features

- **Model-keyed routing** — send specific Amp CLI models to any OpenAI-
  compatible endpoint with a one-line config entry.
- **Anthropic stream upgrade** — non-streaming `/v1/messages` requests are
  silently upgraded to streaming on the wire and collapsed back to a
  single JSON body downstream, working around upstream content loss.
- **Gemini ↔ OpenAI Responses translator** — optionally rewrites Amp CLI
  `finder`'s Google `generateContent` requests into OpenAI Responses API
  calls so they can be serviced by the same custom provider as everything
  else (no more falling back to `ampcode.com` for Gemini traffic).
- **Model mappings** — rewrite `model` fields before routing so you can,
  for example, redirect `claude-opus-4-6` to `gpt-5.4(high)` on a local
  endpoint.
- **Hot-reloadable config** — providers, model mappings, and route modes
  update without restart when `config.yaml` changes.
- **Ampcode.com fallback** — anything not claimed by a custom provider
  still works; it just transparently proxies to the Amp control plane.

---

## Quick start

### Prerequisites

- Go 1.23 or newer
- A local API key you'll present to Amp CLI
- Either a real `ampcode.com` token, or an OpenAI-compatible endpoint you
  want to route to (or both)

### Build

```bash
git clone https://github.com/margbug01/amp-proxy.git
cd amp-proxy
go build -o amp-proxy .
```

### Configure

Copy `config.example.yaml` to `config.yaml` and edit it. The file is short
on purpose — see the **Configuration** section below for a complete
walkthrough.

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
```

### Run

```bash
./amp-proxy --config config.yaml
```

Then point Amp CLI at it:

```bash
export AMP_URL=http://127.0.0.1:8317
export AMP_API_KEY=<the api-key from your config.yaml>
amp
```

On Windows with PowerShell, `scripts/restart.ps1` kills any stale
`amp-proxy.exe` and relaunches it with stdout+stderr redirected to
`run.log` for easy debugging.

---

## Configuration

Everything lives in a single YAML file. Top-level structure:

```yaml
host: "127.0.0.1"
port: 8317

# Local API keys Amp CLI must present (match AMP_API_KEY in your shell).
api-keys:
  - "change-me"

ampcode:
  # Fallback upstream when no custom provider claims the model.
  upstream-url: "https://ampcode.com"
  upstream-api-key: ""  # your Amp session token, or empty

  # Optional: rewrite model names before routing.
  model-mappings:
    - from: "claude-opus-4-6"
      to: "gpt-5.4(high)"

  force-model-mappings: true

  # Route specific models to a third-party OpenAI-compatible endpoint.
  custom-providers:
    - name: "my-gateway"
      url: "http://host:port/v1"
      api-key: "your-bearer-token"
      models:
        - "gpt-5.4"
        - "gpt-5.4-mini"

  # Gemini route mode — "ampcode" (default) or "translate".
  gemini-route-mode: "translate"
```

### Routing decisions, in order

1. Amp CLI sends a request with a `model` field (or a Gemini URL that
   encodes the model in the path).
2. If `force-model-mappings` is on and the model matches a `from:` entry,
   the `model` is rewritten to the `to:` value.
3. If the (possibly rewritten) model is listed under any
   `custom-providers[*].models`, the request is forwarded to that
   provider's `url` with `Authorization: Bearer <api-key>` — **Amp
   credits are not consumed**.
4. Otherwise the request falls through to `upstream-url`
   (`ampcode.com`), consuming Amp credits.

### `gemini-route-mode`

Amp CLI's `finder` subagent uses Google `v1beta1 generateContent` calls,
which most OpenAI-compatible gateways don't speak. Two options:

| Value | Behaviour |
|-------|-----------|
| `ampcode` (default) | Fall through to `ampcode.com`, consuming Amp credits. Guaranteed protocol fidelity. |
| `translate` | amp-proxy rewrites the Gemini request body into an OpenAI Responses API call, forwards it to the matched custom provider, then translates the reply back into Gemini JSON before `finder` reads it. Saves credits; synthesised `call_id`s and a dropped `thoughtSignature` are the only semantic losses. |

### Authentication model

**amp-proxy only supports URL + Bearer token for custom providers.** It
does not have an OAuth login flow for ChatGPT / Claude Code / Gemini CLI
— those were deliberately excluded during the extraction from upstream
CLIProxyAPI. If you need OAuth, run a separate local gateway (e.g.
CLIProxyAPI itself, or any OpenAI-compat bridge) that terminates the
OAuth flow and exposes a plain bearer endpoint, then point a
`custom-provider` entry at it.

---

## Development

### Tests

```bash
go test ./internal/customproxy/...
go test ./internal/...
```

The `customproxy` package ships integration tests that run a fake
upstream via `httptest` and exercise the full request/response
translation chain end-to-end. A live end-to-end smoke test for the
Gemini translate path is available as a standalone Node.js script:

```bash
node scripts/test_gemini_translate.js
```

Set `AMP_PROXY_URL` / `AMP_PROXY_KEY` to target a non-default instance.

### Debug capture

Set `debug.capture-path-substring` in your config to have amp-proxy
write raw request/response bodies for matching URLs into
`./capture/*.log`. Intended for local development — bodies contain
prompts and tool calls, do not leave it on in production.

### Divergence tracking

`NOTICE.md` lists every file that diverges from the upstream CLIProxyAPI
baseline, with a short rationale for each change. Keep it updated when
you cherry-pick or fork-forward.

---

## Acknowledgments

amp-proxy is a derivative of
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) by the
`router-for-me` team, used and extended under the MIT license. The
original codebase does most of the heavy lifting — amp-proxy only carves
out the Amp subsystem and adds the `customproxy` routing layer plus a
handful of fixes. See `NOTICE.md` for the full attribution.

## License

[MIT](LICENSE), inherited from upstream.
