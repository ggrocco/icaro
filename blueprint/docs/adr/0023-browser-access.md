# ADR 0023 — The browser reaches the UI over an opt-in loopback listener

**Status:** Accepted — resolves [open item O5](../02-decisions-risks.md#open-items)

## Context

MVP cut 7 makes the daemon-served web UI the MVP surface, but the daemon listens only on a Unix socket unless started with `--listen` and a token (architecture §3), and a browser cannot reach a Unix socket.

## Decision

- `icaro ui` makes the daemon open a **loopback-only** TCP listener (default `127.0.0.1:8787`, configurable with `daemon.ui_listen`) if it is not already listening, mints a **one-time, short-lived login token** (valid for 2 minutes, single use, `write` scope), and opens `http://127.0.0.1:8787/ui/?token=…` in the browser. `--print` prints the link instead.
- The login exchanges the token for an `HttpOnly`, `SameSite=Strict` session cookie scoped to `/`. Pasting any API token on the login page works the same way.
- Cookie-authenticated mutations must also carry `X-Icaro-CSRF: 1`. Browsers cannot add that header cross-origin without a CORS preflight, and the daemon answers no CORS preflight, so a forged cross-site form or fetch is refused.
- Bearer tokens keep working on every TCP request; the Unix socket stays trusted as the local user.
- Remote access beyond loopback still requires `--listen` explicitly (MVP cut 2).

## Consequences

- No TCP port is open by default; opening the UI is an explicit act, and the link carries a token that is useless after first use or two minutes.
- The UI and the CLI share one HTTP contract and one service layer.
