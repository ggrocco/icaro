# ADR 0008 — The blueprint supersedes the workflow-engine design

**Status:** Accepted

## Context

The repository carried two designs. `docs/blueprint-analysis.md` and `docs/implementation-plan.md` described a server-side workflow engine: headless AI CLI runs as container steps triggered by events, in the lineage of ai-launcher. The first Go code implements Phase 1 of that plan. `blueprint/` describes a local-first platform built around the session spine ([ADR 0002](0002-session-spine.md)), with workflows as one facet. The two disagree on the step model, interpolation syntax, credentials, transport, memory, and the desktop surface, so a contributor could not tell which one to build.

## Decision

`blueprint/` is the single source of truth. Every idea in the `docs/` design received one disposition:

- **Adopted** when it fills a gap without contradicting a blueprint decision, MVP cut, ADR, or schema field. It now lives in its canonical blueprint home: the image-step I/O contract, the agent authoring interface (MCP tools + skill), sandbox hardening, delivery records with redelivery, portable SQL with a Postgres alternative, and token-based auth for TCP and the web UI.
- **Deferred** when an MVP cut blocks it: the shared library, `call_workflow` and in-step MCP, an `agent` step type, approval steps, GitHub App auth, and multi-runner scale-out. Each is listed with its blocker under [Deferred designs](../02-decisions-risks.md#deferred-designs).
- **Superseded** when it contradicts the blueprint:
  - the headless server-side engine framing gives way to the session spine;
  - `run`/`http` steps and an `agent` step type give way to `image`/`uses` ([ADR 0007](0007-integration-manifests.md));
  - integrations packaged as Docker images with an `integration.yml`, and compiled-in SCM trigger adapters, give way to declarative manifests in git packs ([ADR 0007](0007-integration-manifests.md));
  - `text/template` `{{ }}` with `if:` gives way to `${{ }}` with `when`;
  - `on:` triggers, a per-workflow webhook URL, and raw-payload passthrough give way to `triggers:`, `/hooks/<integration>`, and the normalized `trigger.*` context;
  - linear steps give way to a DAG with `needs`;
  - named connections injected as `ICARO_CONN_*` give way to the daemon secret store, `${{ secrets.* }}`, host-bound integration credentials, and the profile home ([ADR 0005](0005-persistent-home-egress.md));
  - a generated master-key file gives way to the OS keychain with a password-protected fallback;
  - ai-memory "memory spaces" as the memory engine give way to in-daemon memory over MCP ([ADR 0004](0004-memory-mcp.md)), with ai-memory reachable through an importer;
  - a systray tray gives way to the Wails app;
  - `server`/`runner` roles on a default TCP API give way to one daemon on a Unix socket ([ADR 0001](0001-single-daemon.md));
  - per-step `sandbox`/`network` fields give way to per-profile policy;
  - "steps never see host paths" gives way to the session worktree bind mount.

The `docs/` files are reduced to stubs pointing here. Their full text remains in git at commit `4fa63e8`.

## Consequences

- The existing code diverges from the blueprint. The [existing-code table](../04-roadmap.md#existing-code) maps each difference to the phase that resolves it.
- The published schema generated from the Go types must converge on [the blueprint contract](../schemas/workflow.schema.json).
- A deferred design returns only through a new ADR that lifts or amends the cut blocking it.
