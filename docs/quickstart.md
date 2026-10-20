# Quickstart

From nothing to an agent working in an isolated session, a workflow run, and the web UI. You need macOS or Linux and a running Docker engine (Docker Desktop on macOS).

> The shell blocks marked `quickstart` below are executed in CI by `go test -tags e2e ./hack/quickstart`, so they stay correct.

## 1. Install

```sh
curl -fsSL https://github.com/ggrocco/icaro/releases/latest/download/install.sh | sh
```

`install.sh` verifies the release signature and checksum and installs nothing if either fails. On macOS you can use Homebrew instead (`brew install ggrocco/tap/icaro`). See [Installing, upgrading and rolling back](upgrading.md) for the options and for building from source (`make build` puts the binary in `bin/icaro`).

## 2. Initialize

```sh quickstart
icaro init
```

`icaro init` creates `~/.icaro` (override with `ICARO_HOME` or `--home`) with private permissions, stores the master key in your OS keychain (macOS Keychain or the Linux Secret Service; without one it uses an encrypted file and asks for a password, or reads `ICARO_PASSWORD`), and creates the database.

`icaro setup` then writes `~/.icaro/config/icaro.yaml` with the built-in profiles, creates each profile's home, and pulls the profile images, pinning them by digest. It shows a diff and asks before every change, so running it twice changes nothing:

```sh
icaro setup
```

## 3. Start the daemon

Every command talks to one daemon per user over a Unix socket and starts it on demand. Starting it explicitly is equivalent:

```sh quickstart
icaro serve --background
```

A second `icaro serve` refuses to start (exit code 3). `icaro doctor` checks the installation, and `icaro doctor --fix` cleans up after a crash.

## 4. Run an agent in a session

Log the agent in once per profile; the login is reused by every session of the profile and never touches your own `~/.claude`:

```sh
icaro login claude
```

Then, inside any git repository:

```sh
cd ~/src/my-app
icaro claude
```

That creates a git worktree on a new branch `icaro/<name>`, starts the agent in a hardened container with the worktree at `/workspace`, and attaches your terminal. The container can only reach the hosts in the profile's egress allowlist (for `claude`: Anthropic's API). `icaro sh` opens a shell in the same session instead, and `icaro session list` shows every session. See [Sessions](sessions.md).

If the repository has a `compose.yaml`, the session's worktree also gets a live preview; see [Previews](previews.md).

## 5. Run a workflow

Workflows are YAML DAGs of container steps and integration actions. Validate, store and run the example (from a checkout of this repository, or copy [`examples/hello.yaml`](../examples/hello.yaml)):

```sh quickstart
icaro workflow validate examples/hello.yaml
icaro workflow apply examples/hello.yaml
icaro run hello --set who=icaro --wait
```

`--wait` streams the logs and prints the result:

```text
run 01M5DJH7NJD4CCXWNG9QPBGR42 queued; streaming logs
[greet] hello icaro
[greet] input.json: {"who":"icaro"}
[shout] HELLO ICARO
run 01M5DJH7NJD4CCXWNG9QPBGR42  workflow=hello v1  status=succeeded  trigger=manual
  STEP   NEEDS  STATUS     ATTEMPT  EXIT  OUTPUTS                     ERROR
  greet  -      succeeded  1        0     {"greeting":"hello icaro"}  
  shout  greet  succeeded  1        0     {}                          
```

Inspect it afterwards:

```sh quickstart
icaro runs list --workflow hello
icaro runs logs "$(icaro runs list --workflow hello | awk 'NR==2 {print $1}')"
```

See [Workflows](workflows.md) for the format and [Integrations](integrations.md) for webhook triggers and `uses:` actions.

## 6. Open the web UI

```sh
icaro ui
```

This opens a loopback-only listener on `127.0.0.1:8787` and your browser with a single-use sign-in link valid for two minutes (`--print` prints it instead). See [Web UI](ui.md).

## 7. Let agents author workflows

```sh
icaro skill install
```

installs the `icaro-workflows` skill and the `icaro` MCP server entry into Claude Code (`--target codex` for Codex). See [Agents and MCP](agents.md).

## 8. Check everything

```sh quickstart
icaro doctor
```
