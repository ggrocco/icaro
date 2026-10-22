# Web UI

The daemon serves a web UI at `/ui/`. It uses the same HTTP API as the CLI, so everything it shows comes from the same service layer.

## Opening it

```sh
icaro ui            # opens your browser
icaro ui --print    # prints the link instead
```

By default the daemon listens only on its Unix socket, which a browser cannot reach. `icaro ui` asks the daemon, over that socket, to open a **loopback-only** listener on `daemon.ui_listen` (default `127.0.0.1:8787`; the config rejects anything that is not a loopback address). It then prints a **sign-in link** that works once and expires after two minutes ([ADR 0023](../blueprint/docs/adr/0023-browser-access.md)):

```text
http://127.0.0.1:8787/ui/?token=icl_…
(single use, valid for 120s)
```

You can also paste an API token (`icaro token create --name me --scope write`) into the sign-in page.

## Sessions and CSRF

Signing in exchanges the link or token for an `HttpOnly`, `SameSite=Strict` session cookie that lasts 12 hours. Every request that changes something, sign-in included, must also carry the `X-Icaro-CSRF: 1` header. A cross-site page cannot send that header without a CORS preflight, and the daemon answers none. The UI is served with a strict Content-Security-Policy (`frame-ancestors 'none'`, same-origin scripts and connections). **Sign out** deletes the session. The token's scope applies: a `read` token can look but not change anything.

## What's in it

- **Workflows:** the list, a graph of each workflow's DAG, the version history with diffs between versions, enable/disable, and **Run**. Run prompts for the `trigger.*` fields a manual run needs.
- **Editor:**
  - a form editor and a YAML editor over the same document, so fields the form doesn't know about, and comments, survive a round trip;
  - key completion from the JSON Schema;
  - the daemon's validation issues shown inline at their JSON Pointer;
  - a canonical JSON view;
  - saving only once the document validates.
  
  `uses:` steps get a typed form built from the action's inputs, and integration triggers pick from the installed events.
- **Runs:** a run is drawn from the workflow version it ran with, so pending steps are visible. Status, attempts, outputs and live logs stream over SSE, with no polling. **Cancel** kills the step containers; **Re-run** starts the workflow again with the same trigger payload.
- **Deliveries:** every inbound webhook with its verification result, the normalized payload, headers (secrets masked), the body, what each subscribed workflow did with it, and **Redeliver**.
- **Integrations:** the installed integrations, and a manifest editor that validates an edit and commits it to your `local` pack. An invalid edit is rejected before anything is written, and **Push** stays a manual button.

## Building it

Release binaries embed the UI. From source, run `make web` (needs Node 24) before `make build`; without it, `/ui/` explains how to build it. The end-to-end tests in `web/e2e/` drive the UI against a real daemon and Docker engine (`cd web && npx playwright test` after `make web build`).
