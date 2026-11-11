# Windows

The Ícaro daemon runs on Linux and macOS. On Windows there are two ways to use it ([ADR 0039](../blueprint/docs/adr/0039-windows-client.md)):

- **Everything inside WSL2.** Install the Linux release in a WSL2 distribution with Docker Desktop's WSL integration turned on, and use `icaro` there as on any Linux machine. Start with the [quickstart](quickstart.md).
- **`icaro.exe` as a client.** The Windows build talks to a daemon that runs somewhere else: in WSL2 on the same machine, on a Linux host, or on a Mac. It does not run a daemon.

> **Status:** CI builds `icaro.exe` and runs it on Windows, but it has not yet been used against a daemon from a real Windows machine.

## Install the client

Download `icaro_<version>_windows_amd64.zip` (or `arm64`) from the [releases](https://github.com/ggrocco/icaro/releases), extract `icaro.exe`, and put its folder on your `PATH`.

To check the download, compare its SHA-256 with the line for that file in the release's `checksums.txt`:

```powershell
Get-FileHash .\icaro_<version>_windows_amd64.zip -Algorithm SHA256
```

`checksums.txt` is signed; [upgrading](upgrading.md) describes the signature. There is no `icaro update` on Windows: to update, replace `icaro.exe` with the one from a newer zip.

## Connect it to a daemon

On the machine that runs the daemon (for WSL2, inside the distribution):

```sh
icaro token create --name windows --scope write
icaro serve --listen 127.0.0.1:8787
```

The token is printed once. `--listen` adds a TCP listener next to the Unix socket, and every TCP request needs a token. To keep it across restarts, set `daemon.listen` in the daemon's config instead of passing the flag.

On Windows, point the client at it:

```powershell
$env:ICARO_DAEMON__URL = "http://localhost:8787"
$env:ICARO_DAEMON__TOKEN = "<the token>"
icaro runs list
```

Or put both in `%USERPROFILE%\.icaro\config\icaro.yaml`:

```yaml
daemon:
  url: http://localhost:8787
  token: <the token>
```

A daemon in WSL2 that listens on `127.0.0.1` is reachable from Windows as `localhost`, through WSL's localhost forwarding. For a daemon on another machine, do not expose the plain-HTTP listener on the network: put a TLS reverse proxy in front of it ([security](security.md)).

## What works from Windows

Commands that go through the daemon's API work: `workflow`, `run`, `runs`, `session create|list|get|stop|start|rm|push`, `preview`, `memory`, `secret`, `integration`, `credential`, `deliveries`, `schedules`, `host`, `ui` and `mcp`.

Commands that act on the daemon's own machine do not, and say so: `serve`, `service`, `init`, `setup`, `migrate`, `token`, `doctor`, `usage`, `update`, and the ones that open a terminal in a container (`sh`, `claude`, `codex`, `login`, `session attach`). Run those inside WSL2 or on the daemon's host.

The desktop app is not built for Windows. The web UI is: `icaro ui` on the daemon's machine prints its address.
