# Phase 18 — GitHub App mode

**Objective:** the `github` integration authenticates as a GitHub App instead of a personal token: actions use per-call installation tokens, setup uses the App Manifest flow, and steps get scoped GitHub access through the injection relay without holding a token. As proposed in [ADR 0039](../adr/0039-github-app-mode.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0039 **accepted**, with its open questions settled:

- how PAT or App is chosen (an ordered `auth` list, or one setting per integration);
- the default `integrations.github.step_permissions` ceiling;
- whether write access on pull-request code warns or is refused;
- whether the token-in-environment fallback is allowed at all.

A change to any of them is made in the ADR first, then here.

The phase is built in three parts, each with its own exit:
- **Part A**, the provider and the manifest changes, can ship alone: actions work with an App configured by hand.
- **Part B** adds setup.
- **Part C**, step access, starts only after spike S13 has passed.

## Spike S13 — GitHub tools through the relay

Run before Part C, with a test App installed on a scratch repository and a step container behind a run's egress proxy.

- [ ] git: `clone`, `fetch` and `push` over smart HTTP with the `insteadOf` rewrite from `GIT_CONFIG_COUNT`/`KEY`/`VALUE`, including a large pack and an authenticated push.
- [ ] `gh` (with `GH_TOKEN`, `GH_HOST` and an API URL override), Octokit (with `baseUrl`) and `curl` against `GITHUB_API_URL`: one read and one write each.
- [ ] Check that the installation token never appears in the container's environment, files, process list or the responses it receives.
- [ ] Record what works, with versions, in ADR 0039. Pass: git and at least one API client. Otherwise the fallback the ADR settles applies.

## Deliverables

### Part A — provider and manifests

- [ ] **Credentials.** `github.app_id` and `github.app_private_key` in the secret store, host-bound to `api.github.com`, or to the GHES host from `integrations.github.base_url`. `icaro credential add github app_private_key` reads a PEM file and checks it parses as an RSA key.
- [ ] **Provider.** A new `internal/githubapp` package, wired in `internal/app`:
  - **JWT:** RS256, 10 minutes, clock skew allowed by backdating `iat` 60 s.
  - **Installation lookup:** `trigger.installation_id`, else `GET /repos/{owner}/{repo}/installation`, cached with a TTL and dropped on `404`.
  - **Minting:** one repository and the declared permissions per request.
  - **Token cache:** an owner goroutine holds the in-memory cache, keyed by installation, repository and permissions, and refreshes a token five minutes before it expires (no mutexes).
  - **Revocation:** a token can be revoked.

  Every minted token is registered with the log redactor. The private key is read from the secret store for each JWT and never logged.
- [ ] **Manifest schema v1.**
  - The `github-app` auth kind, with no credential field.
  - The per-action `app` block: `repository` (an expression) and `permissions` (GitHub names, `read` or `write`).
  - `auth` as a string or an ordered list.

  Semantic validation: `github-app` only reaches its own integration's `app_*` credentials; permission names are GitHub's; `repository` parses at load time. The embedded schema, the published [manifest schema](../schemas/integration-manifest.schema.json) and their drift test change together. The manifest `FuzzParse` target covers the new fields.
- [ ] **Executor.** For a `github-app` auth, the executor:
  1. renders `repository`;
  2. asks the provider for a token;
  3. attaches it only to a request whose host is in the credential's binding.

  The SSRF guard and redirect refusal of [ADR 0016](../adr/0016-pack-loading-action-execution.md) are unchanged. A `401` from GitHub drops the cached token and retries the request once.
- [ ] **Bundled manifest.** `github.yaml` gains:
  - `auth: [app, token]` on every action;
  - each action's `app.repository` and least `permissions`;
  - `installation_id` in every event payload;
  - the `installation.*` and `installation_repositories.*` events.

  Its payload fixtures are updated, including App deliveries.
- [ ] **Installations record.** A migration in both dialects adds `github_installations` (id, account, target type, repository selection, suspended, updated). Installation events keep it current. Tests pass on SQLite and Postgres.

### Part B — setup

- [ ] **App Manifest flow.**
  - `icaro integration github-app create [--org ORG] [--name NAME]` builds the App manifest the ADR describes, from the bundled actions' permissions, the manifest's events and `daemon.public_url` (webhook inactive without it).
  - It opens the browser on GitHub's "new App" page. The daemon serves the redirect at an authenticated route with a one-time `state` that expires after 10 minutes.
  - It exchanges the code, stores the three credentials with their binding, discards the client secret, and prints the install link.
  - On GHES the flow uses the configured host.
  - `go run ./hack/gendocs` regenerates `docs/cli.md`.
- [ ] **Web UI.** On the GitHub integration page:
  - **Create GitHub App** and **Use an existing App**;
  - the App's slug and the auth in use (App or token);
  - the installations, with their accounts and suspended state;
  - the install link.
- [ ] **Status and doctor.**
  - `icaro integration status github` shows the mode, the App id, the installations and the last token error.
  - Doctor checks: the App's credentials are present and bound; the key parses; a JWT is accepted (`GET /app`); no installation is suspended; the webhook is active when `daemon.public_url` is set.

  The checks are mapped in the doctor coverage test, with a row in the [doctor checks](../03-security.md#icaro-doctor).

### Part C — step access (after S13)

- [ ] **Repository checkout.** The repository cache fetches `github.com` (or GHES) repositories with a host-side installation token for that repository and `contents: read`, when the App can see it, and falls back to unauthenticated fetch otherwise. The token is never written into the cache's git config or remotes.
- [ ] **Contract.** The `github` step field (`repositories`, `permissions`) on image steps, plus the fallback field if the ADR keeps it. Regenerate `schema/workflow.schema.json` and update the [workflow contract](../schemas/workflow.schema.json).
- [ ] **Validation.**
  - `github` is on image steps only.
  - `repositories` render to `owner/name`, with no `secrets`.
  - Permission names are GitHub's and within `integrations.github.step_permissions`.
  - The pull-request guard: a step that checks out `trigger.head_sha` of a `pull_request` event and asks for a write permission gets the warning or refusal the ADR settles.
- [ ] **Config.** `integrations.github.step_permissions`, validated at load, defaulting as the ADR settles.
- [ ] **Relay.** `internal/inject` gains a `github` target:
  - the run's egress proxy gets the inject listener when a step of the run has `github`;
  - `/github/api/…` and `/github/git/…` are relayed;
  - the step's capability token is checked and bound to the running step attempt;
  - requests are narrowed to the step's repositories (API `/repos/{owner}/{repo}/…`, and the endpoints git smart HTTP uses under `/{owner}/{repo}.git/…`);
  - client credentials are dropped, a token minted for exactly the step's repositories and permissions is added, and the token is filtered from responses;
  - each request is logged per step, as in [ADR 0028](../adr/0028-credential-injection.md).
- [ ] **Engine.** A step with `github` gets:
  - `GITHUB_TOKEN`/`GH_TOKEN` (the placeholder);
  - `GITHUB_API_URL`;
  - the `GIT_CONFIG_*` rewrite;
  - `icaro-egress` in `NO_PROXY`.

  Its token is revoked when the attempt ends, including after a crash (on the next start, through the step run's record).
- [ ] **Docs.**
  - `docs/integrations.md`: "GitHub App mode" (setup, permissions, installations, fallback to a PAT).
  - `docs/workflows.md`: the `github` step field, with an example in a ```` ```yaml workflow ```` block, and the pull-request guidance.
  - [Security](../03-security.md): the App key as a credential, the relay's second target, and the step ceiling.
  - The blueprint: ADR 0039 marked Accepted, a decision-index row, cut 9's wording, the deferred-designs row removed, and S13's result.

## Acceptance criteria

Part A:
- [ ] With an App configured by hand and no PAT, `uses: github.create-issue-comment` comments as the App on an installed repository. With only a PAT, the same workflow comments as the user, unchanged.
- [ ] Each action's token is minted for one repository with exactly the action's declared permissions; a test against a fake GitHub asserts the minting request. Tokens are reused from the cache until five minutes before they expire, and a `401` triggers one re-mint.
- [ ] A run triggered by an App delivery uses `trigger.installation_id` without a lookup; a manual run looks the installation up once and caches it.
- [ ] A third-party pack that declares `github-app` cannot reach `github.app_*`. A `github` manifest whose action URL leaves the bound host is refused before any token is minted.
- [ ] No minted token, JWT or private key appears in a log, SQL row, API response or delivery record.

Part B:
- [ ] `icaro integration github-app create` against a fake GitHub completes the flow:
  - it rejects a wrong or reused `state`;
  - it stores the three credentials, bound;
  - it discards the client secret;
  - it prints the install link.

  Without `daemon.public_url`, the manifest's webhook is inactive.
- [ ] An `installation.deleted` delivery marks the installation removed, and `icaro doctor` reports a suspended installation.

Part C:
- [ ] A step with `github: {repositories: [the repo], permissions: {contents: write}}` clones, commits and pushes through the relay, with no installation token visible in its environment, files, process list or received data.
- [ ] The same step's request for another repository, or for an API path outside `/repos/{owner}/{repo}`, is refused by the relay.
- [ ] A permission above `integrations.github.step_permissions` fails validation, with the setting named.
- [ ] The step's token is revoked when the attempt ends, and also after a daemon crash mid-step.
- [ ] A private repository in `repository:` is fetched with the App's read token, and the token is absent from the cache's git config.
- [ ] Every new decision has unit tests with a fake GitHub (JWT, minting, cache, relay narrowing). The relay and repository-cache paths have tests behind the `integration` build tag. Coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.

## Where it lives

- Provider: `internal/githubapp`; executor and manifest changes: `internal/integration` (and `builtin/github.yaml`).
- Setup route and CLI: `internal/api`, `internal/cli`; UI: `web/src/pages` (integrations).
- Relay target: `internal/inject` and the egress proxy in `internal/container`; step wiring: `internal/workflow` and `internal/workflow/engine`.
- Repository cache: `internal/session`; installations record: `internal/store`.
