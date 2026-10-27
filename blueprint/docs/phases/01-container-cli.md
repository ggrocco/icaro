# Phase 1 — Container and CLI

**Objective:** isolated profile, the session's first segment, and the `setup`, `sh`, and `claude` commands.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Create a non-root base Dockerfile with asdf (`images/agent-base/Dockerfile`, `make image-agent`).
- [x] Implement image-by-digest, profile, and mount policy ([ADR 0011](../adr/0011-mount-policy.md)).
- [x] Implement an idempotent setup with diff preview and overwrite confirmation.
- [x] Implement `icaro sh`, `icaro claude`, the CLI adapter, and TTY passthrough.
- [x] Install Ícaro inside the image for internal commands.
- [x] Implement the **per-profile persistent home** ([ADR 0005](../adr/0005-persistent-home-egress.md), [ADR 0012](../adr/0012-egress-proxy-and-home-layers.md)): login seeding, golden seed layer + per-session writable overlay, and the mandatory egress allowlist locked to provider endpoints.
- [x] Stand up `internal/session/` and **worktree lifecycle**; an agent runs against a session's worktree ([ADR 0002](../adr/0002-session-spine.md)).
- [x] Persist sessions in the `sessions` table and reconcile them on startup ([ADR 0006](../adr/0006-session-persistence-recovery.md)).
- [x] Migrate the [existing-code](../04-roadmap.md#existing-code) rows assigned to this phase: the master key moved to the OS secret store (password file fallback); agent sessions bind-mount their worktree and live in the `sessions` table; agents get the per-profile home and egress policy. Workflow steps adopt run sessions with the Phase 2 engine (`session.RunSessions` is ready).

## Acceptance criteria

Verified by tests on Linux; the macOS halves need a Docker Desktop host (CI's macOS job runs the unit tests only).

- [x] Running `icaro setup` twice changes nothing without consent (`internal/cli/setup_test.go`).
- [x] A file outside the allowlist is not mounted (`internal/session/manager_test.go`, `TestDisallowedMountFailsStart`).
- [x] `icaro sh` preserves the UID and the expected workspace on Linux (`TestAgentContainerBaseline`, `TestSessionSpineDocker`, `TestCLISessionTTY`). macOS: pending a Docker Desktop run.
- [x] The CLI agent receives a TTY and the correct exit code (`TestCLISessionTTY` drives the binary under a pseudo-terminal).
- [x] The egress allowlist confines every credential present in the agent container: a request to a non-allowlisted host fails (`TestEgressConfinement`). The host's real `~/.claude` is never mounted (mount policy + profile homes).
- [x] A profile with `inject` puts no provider API key in any container ([ADR 0028](../adr/0028-credential-injection.md)). Unit tests cover the session's container spec, every mount and the seed (`TestInjectedSessionHoldsNoKey`), and the injector against a fake TLS provider (`internal/inject`). The integration test of the real proxy path (`TestCredentialInjectionThroughTheProxy`) runs in CI's integration job. Open (S4): whether Claude Code and Codex accept the placeholder with no prompt.
- [x] Containers need no git credentials: `icaro session push` pushes the session's branch from the host, under hardened git options (`TestPushSessionBranch`, `TestHostGitRunsNoPlantedHooks`).
- [x] Two sessions sharing a profile use the seeded login concurrently without corrupting each other's `~/.claude` (`TestOverlayHomesConcurrent`, `TestCopyModeIsolatesSessions`).
- [x] Two sessions on two worktrees run concurrently without interfering (`TestTwoSessionsOnTwoWorktrees`, `TestSessionSpineDocker`).
- [x] After a daemon restart, a running session is re-attached or marked `orphaned`, never left ambiguous (`TestReconcileAfterRestart`, `TestSessionSpineDocker`).
