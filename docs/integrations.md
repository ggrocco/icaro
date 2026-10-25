# Integrations

An integration is a declarative YAML **manifest**, not code ([ADR 0007](../blueprint/docs/adr/0007-integration-manifests.md)). A manifest declares:

- **triggers:** how to verify an inbound webhook, which events it carries, and how to normalize each event's payload into `${{ trigger.* }}`;
- **actions:** typed inputs, an HTTP request template, an auth reference, and the outputs to extract from the response.

Workflows use them as `triggers: [{integration, event, filter}]` and `uses: <integration>.<action>` steps. Manifests execute no code.

## Built in

| Integration | Events | Actions |
|---|---|---|
| `github` | `pull_request.opened`, `pull_request.synchronize`, `pull_request.closed`, `push`, `issue_comment.created` | `create-issue-comment`, `create-issue`, `create-commit-status` |
| `gitlab` | `merge_request.opened/updated/merged/closed`, `push`, `note.created`, `note.merge_request` | `create-mr-note` |
| `slack` | `app_mention`, `message` | `post-message`, `post-reply`, `update-message` |
| `jira` | `issue.created`, `issue.updated` | `create-issue`, `add-comment`, `transition` |
| `linear` | `issue.created`, `issue.updated`, `comment.created` | `create-issue`, `create-comment` |
| `sentry` | `issue.created`, `issue.resolved`, `alert.triggered` | `resolve-issue` |
| `datadog` | `monitor.triggered`, `monitor.recovered`, `monitor.any` | `post-event` |
| `webhook` | `received` (generic, URL-token authenticated) | — |

`icaro integration show NAME` prints an integration's hosts, auth, verification, every event's `trigger.*` fields, and every action's typed inputs and outputs:

```text
$ icaro integration show github
github (pack builtin, github.yaml)
GitHub repositories, pull requests, issues and commit statuses.
hosts:  api.github.com
auth:   token: bearer (credential github.token)
verify: hmac-sha256
event  pull_request.opened: trigger.{action, author, base_ref, draft, head_ref, head_sha, merged, pr_number, repo, sender, title, url}
...
action github.create-issue-comment(body:string!, number:number!, repo:string!) -> id, url
```

The built-in manifests were checked against example payloads written from each provider's documentation. Checking them against the live APIs (spike S8) is still open.

## A webhook-triggered workflow

```yaml workflow
version: 1
name: review-pr
triggers:
  - integration: github
    event: pull_request.opened
    filter: trigger.base_ref == 'main' && !trigger.draft
steps:
  - id: check
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    env:
      TITLE: ${{ trigger.title }}
    command: |
      echo "checking: $TITLE"
  - id: comment
    needs: [check]
    uses: github.create-issue-comment
    input:
      repo: ${{ trigger.repo }}
      number: ${{ trigger.pr_number }}
      body: "Checks passed for ${{ trigger.head_sha }}"
```

Run it by hand with the same fields, for example `icaro run review-pr --set repo=acme/app --set pr_number=42 ...`. A manual run prompts for each `trigger.*` field the workflow reads, so an integration-triggered workflow can always be tested without a webhook.

## Credentials

A credential that an action sends is **host-bound**: it is stored with the hosts it may be sent to, and the executor refuses to attach it to a request for any other host. A malicious pack update gets a refusal, not your token.

```sh
printf %s "$GITHUB_TOKEN" | icaro credential add github token   # bound to api.github.com
printf %s "$WEBHOOK_SECRET" | icaro credential add github webhook_secret
icaro credential list                                           # names and hosts, never values
```

The binding defaults to the manifest's `hosts` plus the host of a `base_url` you configured. Use `--host` to choose it yourself (a hostname, or a wildcard such as `*.example.com`; a top-level wildcard like `*.com` is refused, in a binding and in a manifest's `hosts`), and `--yes` to skip the confirmation. `<integration>.webhook_secret` verifies inbound webhooks and is never sent anywhere, so it has no binding. Credentials never enter a container, and a workflow cannot read one through `${{ secrets.NAME }}`: validation and the engine refuse any host-bound secret and any secret named `<integration>.<name>` for an installed integration. Use the integration's `uses:` actions instead.

## Webhooks

Each integration has one endpoint: `POST /hooks/<integration>`. The generic `webhook` integration uses `POST /hooks/webhook/<token>`, with one random token per subscribed workflow (`icaro integration webhook-tokens`, `--rotate WORKFLOW`). The daemon must be reachable by the provider, which means a TCP listener (`icaro serve --listen 0.0.0.0:8787` behind TLS, or a reverse proxy). Set `daemon.public_url` so the generated URLs are correct. On a laptop, use manual or `cron` triggers instead; a `cron` workflow whose first step checks the source and gates the rest with `when` stands in for polling.

Every request goes through the same pipeline ([ADR 0015](../blueprint/docs/adr/0015-manifest-schema-webhook-pipeline.md)):

1. Rate limit (`webhooks.rate_per_min`) and body cap (`webhooks.max_body_bytes`).
2. Verify the signature as the manifest says (HMAC-SHA256/SHA1, shared-secret header, or URL token), and check the timestamp header when the manifest declares one (`webhooks.max_skew`). A timestamp only stops replays when the signature covers it, as Slack's does.
3. Record the delivery, rejected ones included. A rejected request is unauthenticated, so only its first 4 KiB is kept, with its full size and SHA-256.
4. Deduplicate: a provider retry of a processed delivery starts nothing. A delivery is a duplicate when its delivery id matches, or when its HMAC-signed material does: delivery ids travel in headers the signature does not cover, so a replay with a fresh id is still caught.
5. Normalize the payload into `trigger.*`, evaluate each subscribed workflow's `filter`, and enqueue.

```sh
icaro deliveries list                 # status: processed, duplicate, rejected, no_match, error, challenge
icaro deliveries get ID               # headers (secrets masked), body, matched workflows, runs
icaro deliveries redeliver ID         # re-run the pipeline from the stored record
```

Delivery records are kept for `webhooks.retention` (default `720h`, 30 days), which is also how long a replay is recognized as a duplicate; only the newest `webhooks.max_rejected` rejected deliveries are kept (default `1000`). Verified deliveries are only aged out, so a flood of rejected requests cannot push out the records that catch replays. The daemon prunes hourly.

```yaml
webhooks:
  retention: 720h
  max_rejected: 1000
```

A delivery record answers "why didn't my workflow fire?". It shows the verification error, the duplicate, the absence of a matching event, the filter that excluded the run, the concurrency policy that skipped it, or the run id. The web UI's **Deliveries** page shows the same data.

## Actions

`uses` steps run in the daemon. The HTTP executor:

- sends requests only to the manifest's hosts;
- attaches credentials only to hosts inside their binding;
- refuses redirects;
- caps responses at 1 MiB;
- redacts secret values and well-known token formats from outputs and errors.

The executor blocks private and loopback addresses by default, checking the resolved IP at dial time, so a DNS trick cannot reach your LAN ([ADR 0016](../blueprint/docs/adr/0016-pack-loading-action-execution.md)). Self-hosted services need opt-ins that only you can set in the config, never a pack:

```yaml
integrations:
  gitlab:
    base_url: https://gitlab.internal.example
    allow_private: true
```

## Packs

Manifests ship in **packs**, which are git repositories under `~/.icaro/integrations/`:

- `builtin` is embedded in the binary and read-only.
- `icaro integration add <git-url>` clones a third-party pack, prints its complete permission summary (hosts, auth kinds, events, actions), and activates it only after you confirm. `icaro integration update PACK` pulls and shows the new summary, `icaro integration remove PACK --yes` deletes it, and `icaro integration push PACK` pushes its commits.
- `local` is created on first edit. `icaro integration edit NAME -f file.yaml` and the web UI's manifest editor validate the manifest **before** writing it, then commit it to `local` with a structured message. Pushing stays manual.
- A user pack may shadow a builtin of the same name. If two user packs define the same integration, neither loads (the builtin serves, if there is one), and `icaro integration status` reports the collision.
- The daemon watches the packs. A manifest that becomes invalid keeps its last good version serving, and the error shows in `icaro integration status`, the API and `icaro doctor`.

`icaro integration validate FILE` checks a manifest offline, and `icaro integration schema` prints the [manifest JSON Schema](../blueprint/docs/schemas/integration-manifest.schema.json).
