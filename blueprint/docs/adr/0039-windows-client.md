# ADR 0039 — Windows gets a client, not a daemon

**Status:** Accepted

## Context

Releases shipped binaries for Linux and macOS only, and [CONTRIBUTING](../../../CONTRIBUTING.md#releases) said why: "Windows is not a target: the daemon depends on Unix sockets and `flock`". Windows users had nothing to download.

The daemon's Unix dependencies go further than the listener and the lock:

- The memory MCP server and the credential injector reach an agent through a Unix socket bind-mounted into its container ([ADR 0019](0019-memory-mcp-transport.md), [ADR 0028](0028-credential-injection.md)). Docker Desktop on Windows cannot mount a Windows socket into a Linux container.
- Containers, the preview proxy and worktree files are given the user's numeric UID and GID. Windows has neither.
- The login service is launchd or systemd, and the secret store's key lives in the macOS Keychain or the Linux Secret Service.

A port would redesign each of those and needs a Windows machine to test on. It is a project of its own, and the [roadmap](../04-roadmap.md) has no phase for it.

The CLI, by contrast, is already a thin client. With `daemon.url` and `daemon.token` set it talks to a daemon over TCP and starts nothing locally ([architecture §3](../01-architecture.md#3-transport-between-ui-cli-and-daemon)). The Linux release also runs unchanged inside WSL2.

## Decision

**The release ships `icaro.exe` for Windows (amd64 and arm64) as a client of a daemon that runs elsewhere:** in WSL2 on the same machine, on a Linux host, or on a Mac.

- **One binary, gated at run time.** The Windows build is the same `cmd/icaro` with the same commands. `platform.DaemonSupported()` is false on Windows, and the CLI then does two things:
  - Commands that act on the daemon's own machine fail with a message that says so. These are the ones that run the daemon (`serve`, `service`), open its state directly (`init`, `setup`, `migrate`, `token`, `doctor`, `usage`, `update`), or exec into its containers through the local Docker engine (`sh`, `claude`, `codex`, `login`, `session attach`).
  - Every other command goes through the API. Without `daemon.url` it fails and names the two settings, instead of trying to start a daemon.
- **No daemon code runs on Windows.** `internal/platform` has a Windows file whose lock can never be taken, so the daemon package compiles and `icaro serve` cannot start even if the gate were bypassed.
- **Packaging.** GoReleaser builds `icaro_<version>_windows_<arch>.zip`, listed in the signed `checksums.txt` like every archive. There is no installer, no `install.sh` path and no `icaro update` on Windows: a newer zip replaces the old one. The `.deb`, `.rpm` and Homebrew cask stay Linux and macOS only.
- **Tests.** The unit suite still runs on Linux and macOS only; some of its tests use Unix calls and do not compile for Windows. CI cross-builds for Windows and, on a Windows runner, runs the real binary: it starts, refuses the host-only commands, asks for `daemon.url`, and with one set gets as far as the network. The gate itself is unit-tested on every platform through an injectable `daemonSupported`.

**Not decided here:** a native Windows daemon, and the desktop app on Windows. Either needs its own ADR.

## Consequences

- Windows users get a supported way to drive Ícaro, and the docs can say exactly what works: workflows, runs, sessions (create, list, stop, remove), previews, memory, secrets, integrations, hosts and the MCP bridge.
- Running an agent interactively (`icaro claude`, `icaro sh`) is not available from Windows. It is done inside WSL2 or on the daemon's host, because those commands exec through the Docker engine next to the daemon.
- A daemon reached from Windows must listen on TCP (`icaro serve --listen`), which needs an API token on every request ([ADR 0023](0023-browser-access.md)). In WSL2 the listener stays on loopback, which Windows reaches through WSL's localhost forwarding.
- The host-only list is one more thing to keep in step with the command tree: a new command that touches local state must be added to it.
- The client has not been run against a daemon from a Windows machine. CI covers the binary's own behavior, not a WSL2 round trip.
