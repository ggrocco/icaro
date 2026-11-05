# ADR 0033 — Remote hosts, and an opt-in sign-in-free loopback UI

**Status:** Accepted. Amends [ADR 0023](0023-browser-access.md) (the loopback UI listener) and the auth row of the [decision index](../02-decisions-risks.md#decision-index).

## Context

Two needs came up together.

- **Local use should just work.** On your own machine, the web UI asks for a sign-in, its session lasts 12 hours, and it shows a **Sign out** button. The CLI talks to the same daemon over the Unix socket with no sign-in at all. For one user on one machine the browser sign-in is friction.
- **Several daemons.** A user can run Ícaro on more than one machine (a laptop, a lab box, a shared server behind `--listen`). Each daemon has its own workflows, and there was no way to see them together, or to stay signed in to more than one.

The loopback listener is TCP. Unlike the `0600` Unix socket, any process and any OS user on the machine can connect to it, and a web page can make the browser send requests to it (cross-site forms, and DNS rebinding, where a hostile hostname resolves to `127.0.0.1`). That is why ADR 0023 required a sign-in.

## Decision

### Trusted loopback UI (opt-in)

- A new setting, `daemon.ui_trust_loopback` (default `false`). When it is on, the UI listener (`daemon.ui_listen`, which must already be a loopback address) serves the browser without a sign-in. A request on that listener that has no bearer token acts as the local user with `admin` scope, the same as the Unix socket.
- The trust belongs to the listener, not to routes. The `--listen` TCP listener never gets it, and neither does a connection whose peer address is not a loopback address.
- Three checks stay in force, and they stop a web page from using that trust:
  - **Host.** The `Host` header must be `localhost` or a loopback IP (any port). A DNS-rebinding page sends its own hostname, so its requests are refused with `403`.
  - **Origin.** When an `Origin` header is present, it must be the same origin as the request.
  - **CSRF.** Every mutation must carry `X-Icaro-CSRF: 1`. A cross-site page cannot add that header without a CORS preflight, and the daemon answers none.
- A bearer token still decides who the caller is when one is sent. `GET /api/v1/ui/me` reports `local: true`, and the UI then shows no sign-in page and no **Sign out**. `icaro ui` prints a plain link with no token.
- The daemon logs a warning at start when the setting is on.

**What this gives up.** With the setting on, every local process and every other OS user on the machine can drive the daemon as you. This includes running containers and using your integration credentials through `uses:` steps. Turn it on only on a single-user machine you control. It is off by default, so the default security model of ADR 0023 is unchanged.

### Remote hosts

- The daemon can be signed in to any number of remote Ícaro daemons at once. Each is stored in a new `remote_hosts` table (migration `00016`, both dialects):
  - an **alias**: lowercase, at most 32 characters (`^[a-z][a-z0-9-]{0,31}$`), and never `local`;
  - a **label**: the display name, which defaults to the alias;
  - the base **URL**: `http` or `https`, with no credentials, query or fragment;
  - the remote's **API token**, sealed with the master key;
  - the principal and scope that the remote reported.
- The local daemon is always the host `local` and needs no sign-in. Its label is the machine's hostname.
- **Signing in** (`POST /api/v1/hosts`, `icaro host login ALIAS URL`, the UI's **Hosts** page) checks the token against the remote's `GET /api/v1/ui/me` before storing it. A refused token is a `400` with the remote's message, and an unreachable host is a `502`. Signing in again with the same alias replaces that sign-in. **Signing out** (`DELETE /api/v1/hosts/{alias}`, `icaro host logout`) forgets the token.
- Sign-in and sign-out need `admin` scope. They store another daemon's credential, and they make this daemon call a URL that the caller chooses. Listing hosts and their workflows needs `read`.
- **The token stays bound to its URL.** The client follows no redirects, and the token is never returned by any API, written to a log, or sent anywhere else. It is a credential for the remote daemon only. Unlike integration credentials, it is not in the secret store, so workflows cannot reach it, even by name.
- **References carry the host.** A cross-host listing (`GET /api/v1/hosts/workflows`, `icaro workflow list --all-hosts`, the UI's **Workflows** page) returns each workflow with:
  - `host` (the alias) and `host_label`;
  - `ref`, in the form `alias:name`. Aliases and workflow names cannot contain `:`, so a ref is never ambiguous;
  - for a remote workflow, `ui_url`, its page in that host's own UI.

  `?host=ALIAS` (`--host`) lists a single host.
- **Every host is asked in parallel.** The registry starts one goroutine per host. Each goroutine sends exactly one answer on a channel that is buffered to the number of hosts, and the caller collects them. The registry holds no state of its own: the store is the source of truth, so no lock is needed. Each host has its own timeout (10 s). This keeps one slow host to the cost of one timeout, not one per host. A host that fails is reported in `hosts[].error` and hides no other host's workflows. The CLI prints the failures and exits non-zero, so a script can tell when a listing is partial.

## Consequences

- On a single-user machine, the local UI needs no sign-in if you opt in. Nothing changes by default.
- One UI and one CLI show the workflows of every daemon you are signed in to, with the host's alias and label on every reference.
- Opening a remote workflow uses that host's own UI, which has its own sign-in. Running, editing or approving on another host through this daemon is not part of this decision. MVP cut 2 (no remote execution) still holds: this daemon only lists what other daemons report.
- A remote host's token lets this daemon do whatever its scope allows on that host. For listing, a `read` token is enough, so prefer one.
- Plain `http://` remote URLs are accepted, for example a LAN box or an SSH tunnel to `127.0.0.1`, but the token crosses that network in clear. Use `https://` across any network you do not control.
