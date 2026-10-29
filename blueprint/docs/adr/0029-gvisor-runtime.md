# ADR 0029 — gVisor as the stronger boundary for untrusted profiles

**Status:** Accepted. Closes spike [S5](../02-decisions-risks.md#feasibility-gate) for Linux. Apple Containerization on macOS was not evaluated.

## Context

Docker non-root is a weak boundary for semi-trusted LLM output: a container escape is a kernel bug away. Spike S5 asked whether per-container stronger isolation is viable for untrusted profiles, and named Apple Containerization (macOS), gVisor and Firecracker (Linux).

What the code had before this decision:

- `profiles.<name>.runtime` set the OCI runtime of a profile's agent containers.
- `docker.strict_runtime` and a `strict` sandbox profile were meant for workflow steps. Nothing selected the `strict` profile, so the setting reached no container.

## Decision

**gVisor, through the profile's OCI runtime.** `profiles.<name>.runtime: runsc` runs that profile's containers under gVisor's user-space kernel. The container baseline (non-root, no capabilities, seccomp, read-only root, limits, no Docker socket) stays as it is; gVisor is added underneath it. An escape then needs a gVisor bug as well as a host kernel bug.

- **One setting.** The runtime of `workflows.profile` also applies to a workflow's image steps. `docker.strict_runtime` and the `strict` sandbox profile are removed, because they never took effect.
- **Only untrusted code moves.** Ícaro's helpers (the egress proxy, the preview proxy) and preview stacks keep Docker's default runtime. They run Ícaro's own code or the user's own compose project.
- **Host sockets.** A session reaches its memory server over `/icaro/mcp.sock`, a host Unix socket bind-mounted into the container ([ADR 0019](0019-memory-mcp-transport.md)). By default runsc refuses connections to host sockets, so it must be registered with `--host-uds=open` (`sudo runsc install -- --host-uds=open`). Ícaro mounts no other socket into an agent container, and the [mount policy](0011-mount-policy.md) refuses engine sockets.
- **Doctor.** `icaro doctor` reports a profile whose runtime Docker has not registered, because its sessions and steps would fail to start.
- **CI.** The `gvisor` job installs a pinned gVisor release (checksum verified) and runs the container integration tests and the session spine end to end with their agent containers on runsc. The tests check the runtime from inside the container (gVisor's kernel), so a silent fallback to runc fails them. Covered: hardening, the egress proxy, credential injection, `.git` protection, the memory socket bridge, previews of a session's edits.

**Not chosen:**

- **Firecracker and Kata.** Both need KVM. GitHub's hosted runners and most laptops' Docker VMs lack nested virtualization, and both need a separate engine integration.
- **Apple Containerization.** It needs macOS 26 on Apple Silicon and an engine adapter beside the Docker one. It stays open with the other macOS checks.

## Consequences

- Untrusted profiles on Linux get a real boundary without changing how Ícaro builds containers: no new engine, no new adapter.
- Docker Desktop on macOS has no gVisor runtime, so this protects Linux hosts only.
- System calls cost more under gVisor, so builds and tests in a session run slower. That is why it is per profile, not the default.
- `--host-uds=open` is a runtime-wide flag. Every container on that runtime may connect to a host socket that is mounted into it. Ícaro mounts one socket, the session's own memory endpoint.
