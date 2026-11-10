# ADR 0039 — GitHub App mode: one install for triggers, actions and step access

**Status:** Proposed. Would amend [MVP cut 9](../02-decisions-risks.md#mandatory-mvp-cuts) ("token/PAT first") for the `github` integration only, and take the "GitHub App mode" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs). Extends [ADR 0028](0028-credential-injection.md) (the injection relay) to a second kind of credential. Gated by a new spike, S13.

## Context

The bundled `github` manifest authenticates with a personal token (`github.token`, a PAT or fine-grained token bound to `api.github.com`) and verifies webhooks with `github.webhook_secret`. That works, but:

- **The token belongs to a person.** Comments and statuses appear as that user, the token outlives the reason it was made, and a fine-grained token must be recreated to cover a new repository.
- **Webhooks are per repository.** Each repository needs its own webhook pointing at `/hooks/github`, with the shared secret pasted in.
- **Steps get nothing.** An image step that needs GitHub (push a branch, open a pull request, call the API) can only receive a token through `${{ secrets.* }}` in `env`, inside the container, with whatever scope the token has. Validation refuses integration credentials there, so in practice users create a second PAT for steps. A private repository in a step's `repository:` cannot be fetched at all without a credential the daemon can use.

A GitHub App solves all three. One install covers the repositories an owner picks. Its webhooks for all of them arrive at one URL, signed with one secret. It mints **installation tokens** that last an hour and can be narrowed to a few repositories and permissions per request.

Two blueprint rules stood in the way:

- [ADR 0007](0007-integration-manifests.md): **manifests execute no code**. Minting an installation token means signing a JWT with the App's private key, which the expression language cannot and should not do.
- [Cut 9](../02-decisions-risks.md#mandatory-mvp-cuts): **token/PAT first**, no OAuth flows.

## Decision

### App credentials are a daemon credential provider, not manifest code

The daemon gains one built-in credential provider, `github-app`, written in Go. A manifest refers to it the way it refers to a token, so the manifest itself still runs no code. JWT signing, token minting and caching live in the daemon, next to the injector of [ADR 0028](0028-credential-injection.md).

- **Credentials:**
  - `github.app_id` and `github.app_private_key` (PEM), host-bound like any integration credential to `api.github.com`, or to the GitHub Enterprise Server host from `integrations.github.base_url`;
  - `github.webhook_secret`, as today.

  The private key never leaves the daemon. The provider signs only short-lived JWTs (10 minutes) and sends them only to the bound host.
- **Manifest schema v1** gains:
  - an auth kind `github-app`, with no credential field: it always uses its own integration's `app_*` credentials, so a pack for another integration cannot reach them;
  - an `app` block per action: `repository` (an expression giving `owner/name`, usually `${{ input.repo }}`) and `permissions` (GitHub permission names mapped to `read` or `write`).

  An action's `auth` may name a list, and the executor uses the first entry whose credentials are configured. The bundled manifest declares `auth: [app, token]`, so a user with an App uses it, and a user with only a PAT keeps working unchanged.
- **Minting.** For each action call, the provider resolves the installation in this order:
  1. `trigger.installation_id`, when the run came from a GitHub event (the manifest adds `installation_id` to every payload);
  2. otherwise, `GET /repos/{owner}/{repo}/installation` with the App JWT, cached.

  It then mints a token for exactly that repository and the action's declared permissions. Tokens are cached in memory, keyed by installation, repository and permissions, until five minutes before they expire. They are never stored, and each one is registered with the log redactor when minted.

### Setup with the App Manifest flow

`icaro integration github-app create [--org ORG] [--name NAME]`, and a **Create GitHub App** button on the integration page:

1. The daemon builds an [App manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):
   - name and homepage;
   - `public: false`;
   - the webhook URL `<daemon.public_url>/hooks/github`, active only when `daemon.public_url` is set;
   - a redirect URL on the daemon (the loopback UI, or `daemon.public_url`), carrying a one-time `state`;
   - the default permissions: the union of the bundled actions' `permissions`, plus `metadata: read`, plus what the setup's step options add;
   - the default events: the manifest's events.
2. It opens the browser on GitHub's "new App" page for the user or the organisation, with the manifest. The user reviews it and creates the App on GitHub.
3. GitHub redirects to the daemon with a one-time code. The daemon checks `state`, then exchanges the code (`POST /app-manifests/{code}/conversions`). It stores `app_id`, `app_private_key` and `webhook_secret` with their host binding. The OAuth client secret in the response is not needed, and is discarded.
4. The daemon prints and shows the install link (`https://github.com/apps/<slug>/installations/new`), where the user picks the repositories.

An App made by hand works too: `icaro credential add github app_id` and `icaro credential add github app_private_key`. On GitHub Enterprise Server, the flow uses the host from `integrations.github.base_url`.

This is not an OAuth flow in the sense of cut 9. No user token is ever requested or stored, and everything is done as the App. Cut 9's exclusion of OAuth flows stays for every other integration and for user-to-server tokens.

### Webhooks: one endpoint for every installation

Nothing changes in the pipeline ([ADR 0015](0015-manifest-schema-webhook-pipeline.md)). The App's deliveries for all installations already arrive at `/hooks/github` and are verified with `github.webhook_secret`. The manifest gains `installation.created`, `installation.deleted` and `installation_repositories.added`/`removed` as ordinary events. The daemon also records installations it hears about (id, account, repository selection, suspended), for `icaro integration status github` and the doctor.

App mode does not need a reachable daemon. Without `daemon.public_url`, the App is created with its webhook inactive, and actions and step access still work. That gives laptop users GitHub access with no personal token, while triggers keep falling back to manual and cron as cut 9 says.

### Installation tokens for steps, through the relay

**Repository checkout.** A step's `repository:` on `github.com` (or the GHES host) is fetched by the daemon into the repository cache, as today, with an installation token for that repository and `contents: read` when the App can see it. The token is used host-side and never enters a container.

**Step access.** An image step can ask for GitHub access with a new field:

```yaml
- id: open-pr
  image: ghcr.io/example/tools@sha256:<digest>
  github:
    repositories: ["${{ trigger.repo }}"]
    permissions: { contents: write, pull_requests: write }
  command: ./push-and-open-pr.sh
```

The container never gets an installation token. It gets what an [ADR 0028](0028-credential-injection.md) profile gets:

- `GITHUB_TOKEN` and `GH_TOKEN` set to a placeholder, the step's capability token;
- `GITHUB_API_URL=http://icaro-egress:3129/github/api`;
- git configuration through `GIT_CONFIG_COUNT`/`KEY`/`VALUE`, rewriting `https://github.com/` to `http://icaro-egress:3129/github/git/`.

The run's egress proxy relays those paths to the daemon's injector, which:

- checks the placeholder belongs to a running step of this run;
- narrows the request to the step's repositories (API paths under `/repos/{owner}/{repo}/…`; git smart-HTTP paths under `/{owner}/{repo}.git/…`), and refuses everything else;
- drops client credentials;
- adds an installation token minted for exactly those repositories and permissions;
- calls GitHub over TLS, and filters the token out of the response.

The token is revoked (`DELETE /installation/token`) when the step's attempt ends. Requests are logged per step, with no bodies or header values, as in ADR 0028.

Validation of the `github` field:
- it is allowed on image steps only;
- `repositories` are `owner/name` after rendering, with no `secrets`;
- `permissions` names must be GitHub's, and must not exceed `integrations.github.step_permissions`, an operator ceiling that defaults to `contents: read`, `metadata: read`.

A workflow cannot grant a step more than the operator allowed, whatever the App itself can do.

**Untrusted code.** A step that checks out `trigger.head_sha` of a `pull_request` event may be running code from a fork. Validation warns when such a step also asks for a write permission, naming the risk (as with GitHub's own `pull_request_target`).

### Spike S13 — tools through the relay

Before the step access part is built, S13 checks which common tools work unchanged through the relay's plain-HTTP base URL:
- git clone, fetch and push over smart HTTP with the `insteadOf` rewrite;
- the `gh` CLI;
- Octokit;
- `curl` against `GITHUB_API_URL`.

Pass: git and at least one API client work, with the token never visible in the container. Fallback: for tools that cannot take a base URL, a step may opt into a **short-lived token in its environment**, still minted per step with its permissions and revoked at the end, behind an explicit `github.token_in_env: true` that validation flags.

## Alternatives considered

- **Keep PATs only.** No setup, but the problems in the context stay: a person's identity on every action, per-repository webhooks, and no safe step access.
- **OAuth user-to-server tokens.** They would act as a person, which is the problem with PATs, and would bring a refresh-token flow into the daemon. Cut 9 stays for these.
- **JWT signing in the expression language.** It would make manifests able to sign with a private key, which breaks the reason ADR 0007 gives for complete permission summaries.
- **Installation tokens in the step's environment by default.** Simpler, and what CI systems do. But the token would be readable by everything in the container, including code from an untrusted pull request, and ADR 0007 keeps integration credentials out of containers. The relay keeps them out at the cost of S13. The env fallback exists only for tools that cannot use it.

## Consequences

- One GitHub App replaces the PAT and the per-repository webhooks. Comments and statuses appear as the App, and access follows the installation's repository selection.
- Image steps can push, open pull requests and call the API with a per-step, per-repository, least-privilege token they never see, bounded by an operator ceiling.
- Private GitHub repositories can be used in `repository:` without a personal credential.
- New code in the daemon: the `github-app` provider (JWT, minting, cache, revocation), the setup callback, the installations record, and a second relay target in the injector. This is security-boundary code ([03-security](../03-security.md)), reviewed as such.
- Manifest schema v1 gains `github-app`, the per-action `app` block and `auth` lists. The schema and its drift test change, and third-party packs can declare the kind but only ever reach their own integration's credentials.
- The App's private key is the most powerful credential Ícaro holds: it can act on every repository of every installation. It is stored like other secrets, host-bound, never sent anywhere but the bound host, and `icaro doctor` warns when its binding is wrong or an installation is suspended.
- Implementation is [Phase 18](../phases/18-github-app.md): the provider and manifest changes first, then setup, then step access after S13.
