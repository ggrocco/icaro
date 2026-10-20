# ADR 0016 — Pack loading and action-execution rules

**Status:** Accepted

## Context

[ADR 0007](0007-integration-manifests.md) and the [integration policies](../03-security.md#mandatory-policies) state the goals: last good version on a broken manifest, shadowing builtins, collisions as errors, host-bound credentials, an SSRF guard. Implementing them forced several smaller choices that change behavior users can see.

## Decision

**Loading.**

- The registry's owner goroutine publishes immutable snapshots; readers never lock.
- A pack is any directory under `~/.icaro/integrations/` other than `builtin`; manifests live at its root or under `manifests/`, and a manifest's `name` must match its file name.
- "Last good version" is kept **in memory** per pack/file. A manifest that is already broken when the daemon starts has no last good version and is not loaded; its error surfaces in `icaro integration status`, the API and `icaro doctor`.
- When two user packs define the same integration, **neither** is loaded, and the builtin of that name serves if one exists.
- `icaro integration add` clones into a hidden staging directory, prints the complete permission summary and activates only on confirmation. `update` is `git pull --ff-only` and prints the new summary.
- UI/API edits go to the `local` pack only, are validated before any write, and commit with a structured message (`Edited-By`, hosts, events, actions).

**Credentials.**

- `icaro credential add <integration> <name>` defaults the binding to the manifest's `hosts` plus the host of a user-configured `base_url`.
- Webhook verification secrets get **no** binding, because they are never sent anywhere. Action credentials require at least one host.

**Execution.**

- The action executor ignores proxy environment variables, so the SSRF guard sees, and dials, the real destination.
- It resolves DNS itself and checks every address at dial time: loopback, RFC 1918, link-local/metadata, CGNAT, ULA, multicast, reserved and NAT64 ranges are refused unless the user sets `integrations.<name>.allow_private`. Plain `http` is allowed only under that same opt-in.
- Redirects are refused, so a response cannot steer a credential to another host. Responses are capped at 1 MiB.
- Secret values and well-known token patterns are redacted from outputs and errors.

**Scheduling.**

- Each cron trigger's next fire time is persisted.
- A pass that finds a past fire time fires once, keyed `cron:<workflow>:<idx>:<scheduled unix time>`, then schedules from *now*. Any number of ticks missed while the daemon was down collapse into that one run.
- A changed expression reschedules without firing.

## Consequences

- A broken manifest pushed to a pack never takes down a working integration while the daemon runs. After a restart, the fix has to be pushed before the integration works again.
- Self-hosted GitHub Enterprise, GitLab or Sentry needs two explicit user settings: `base_url`, plus `allow_private` when the host resolves to a private address. A pack can set neither.
- The executor cannot be used behind a mandatory corporate HTTP proxy; supporting one would need a proxy-aware guard and a new ADR.
