# amp-proxy

A focused reverse proxy for [Sourcegraph Amp](https://ampcode.com), derived
from [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

## Status

**Pre-M1 — bootstrap phase.** Not yet buildable. See
`C:\Users\marg0\.claude\plans\synthetic-roaming-hollerith.md` for the
milestone plan.

## What this is

- A standalone HTTP proxy that forwards requests to the Sourcegraph Amp
  control plane (`ampcode.com`)
- Provider-alias routes (`/api/provider/{openai,anthropic,google}/...`) that
  let existing OpenAI/Claude/Gemini SDKs target Amp
- Management route passthrough for OAuth login, user info, threads, etc.
- Hot-reloadable config (model mappings, upstream URL, API keys)

## What this is NOT

- Not a fork of CLIProxyAPI. All non-Amp providers are deliberately excluded.
- Not a drop-in replacement for CLIProxyAPI CLI tooling (TUI, credential
  manager, update subsystem, etc.)

## License

MIT, inherited from upstream. See `LICENSE` and `NOTICE.md` for attribution.
