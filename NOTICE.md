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
