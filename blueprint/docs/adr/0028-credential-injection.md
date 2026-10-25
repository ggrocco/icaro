# ADR 0028 — Credential injection: provider API keys stay on the host

**Status:** Accepted. Amends [ADR 0005](0005-persistent-home-egress.md) for profiles that opt in with `profiles.<name>.inject`; every other profile keeps ADR 0005 unchanged. Whether Claude Code and Codex accept the placeholder with no further prompt is an open check for spike [S4](../02-decisions-risks.md#feasibility-gate).

## Context

ADR 0005 puts the agent's credential inside its container and makes the egress allowlist the main mitigation: a prompt-injected agent can use the credential against allowlisted hosts, and it can also **copy** it out through any allowlisted channel, for example by putting it in a request body. ADR 0005 names the end state that removes the copy: "a credential-injecting proxy (`ANTHROPIC_BASE_URL` → daemon, real credential added host-side, container holds nothing usable)". ADR 0003's broker failed because the agent CLIs have no "ask a broker" interface. They do, however, read a provider **base URL** and an **API key** from the environment (`ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY`, `OPENAI_BASE_URL`/`OPENAI_API_KEY`), so a proxy needs no change to the agent.

Two constraints shape the design:

- Agent containers sit on an internal network with no route out ([ADR 0012](0012-egress-proxy-and-home-layers.md)); the only thing they reach is the profile's egress proxy container.
- The daemon already serves per-session endpoints to containers over bind-mounted Unix sockets ([ADR 0019](0019-memory-mcp-transport.md)).

## Decision

**Configuration.** A profile opts in per provider, naming a secret in the existing secret store:

```yaml
profiles:
  claude:
    inject: { anthropic: anthropic-key }   # openai: openai-key for codex
```

`config.Validate` accepts the providers `anthropic` and `openai` and valid secret names. The secret must be **host-bound** to the provider's API host (`icaro secret set anthropic-key --host api.anthropic.com`), like integration credentials. The injector resolves it with the secret store's `ResolveFor`, so it can never be sent to another host. The key is read on every request, so `icaro secret set` takes effect without a restart. `icaro doctor` warns when a referenced secret is missing or is not bound to the provider host.

**Path.** No container ever holds the key, not even the proxy:

1. An agent container of an injecting profile gets `ANTHROPIC_BASE_URL=http://icaro-egress:3129/anthropic` (`OPENAI_BASE_URL=http://icaro-egress:3129/openai/v1`). Its API key is a placeholder: the session's capability token. `icaro-egress` is added to `NO_PROXY`. Plain HTTP is enough, because the hop never leaves the profile's internal network.
2. The profile's egress proxy container also listens on port 3129 (`icaro egress-proxy --inject-socket`). It forwards `/anthropic/...` and `/openai/...` unchanged and streaming to `<run>/inject/<profile>/inject.sock`. That socket's directory is bind-mounted read-only into **that proxy container only**. The proxy runs as the session user so it can open the `0600` socket; otherwise it keeps the helper baseline. A profile that injects but has an empty allowlist still gets a proxy, but only on the internal network.
3. The daemon (`internal/inject`) serves one socket per injecting profile:
   - it checks the token;
   - it narrows the request;
   - it removes every client-sent credential;
   - it adds the real header: `x-api-key` for Anthropic, `Authorization: Bearer` for OpenAI;
   - it calls the provider over TLS with the system roots and normal certificate verification.

A simpler design that also kept the key out of every container would be to let the agent reach the daemon socket directly. It was rejected: it would put a daemon endpoint in the agent container itself, and the agent CLIs need an HTTP URL, not a socket. Relaying through the proxy keeps the agent's view to one host, `icaro-egress`, and keeps the daemon socket out of the container that runs model output.

**Narrowing.**
- Only the endpoints a coding session needs are allowed:
  - Anthropic: `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models`, `GET /v1/models/{id}`;
  - OpenAI: `POST /v1/responses`, `POST /v1/chat/completions`, `GET /v1/models`, `GET /v1/models/{id}`.
- Paths with encoded, empty or dot segments are refused. The query string is passed through, because the Anthropic SDK sends `?beta=true`.
- These client headers are dropped: `Authorization`, `X-Api-Key`, `Proxy-Authorization`, `Cookie`, forwarding headers, and OpenAI organisation and project selectors.
- Request bodies are capped at 32 MiB:
  - a declared larger body is refused before anything is sent;
  - a chunked body is cut off at the cap.
- Responses stream with a flush per read, so server-sent events are not delayed.
- The key is filtered out of response headers and the body by a streaming filter. It holds back only a tail that could start the key, which in practice is nothing.
- `Set-Cookie` is dropped.
- Errors use the providers' `error.message` shape.

**Attribution.**
- Each start of an agent session issues a fresh token, `icaro-injected.<session id>.<32 random bytes>`. Only its SHA-256 is stored (`session_inject_tokens`, migration 00014), so running sessions keep working across a daemon restart.
- A token is accepted only as the current token of a **running agent session of the same profile**.
- Stopping, orphaning, removing or restarting the session revokes it.
- The token is never a provider credential, and it works only through the proxy on the profile's network.
- Every authorised request appends one JSON line to `~/.icaro/logs/inject/<session>.log`, with no bodies or header values and run through the log redactor:
  - time, session, profile and provider;
  - method and path;
  - status;
  - request and response bytes;
  - duration.
- Refused tokens are logged to the daemon log without the token.

## Consequences

- **The key cannot be exfiltrated.** It is not in the agent container's env, files, home, mounts or labels, and not in the proxy container. Unit tests check the session's container create request, every bind-mounted path, the profile seed, injector responses, audit lines and logs. An integration test checks the real container path.
- **The key can still be used, within the path allowlist.** A compromised agent can make allowed provider calls through the proxy, for example to burn quota, and attribution tells you which session did. The egress allowlist still bounds every other channel, and an injecting profile can drop the provider's API host from its allowlist, because the agent no longer calls it directly.
- **Subscription logins are not covered.** With Claude Pro/Max or ChatGPT sign-in, the CLI holds and refreshes its own OAuth token in its home, which is the ADR 0005 situation. Injecting those tokens waits on spike S4 on real accounts. Profiles without `inject` keep ADR 0005's behaviour.
- **Open check (S4).** Whether current Claude Code and Codex accept a placeholder key plus a base URL with no further prompt (Claude Code asks once before using a custom `ANTHROPIC_API_KEY`), and whether the path allowlist is complete for them, must be verified on real installs. This ADR does not claim it. Widening the allowlist is a code change, reviewed like this one.
- The token travels as plain HTTP on the profile's internal network. Another container on that network, meaning another session of the same profile, could use a token it learned only by attacking that session. It already holds its own token.
- `icaro session push` ([sessions](../../../docs/sessions.md#pushing-a-sessions-branch)) is the matching change for git: the daemon pushes a session's branch with the host user's credentials, so containers need no git credentials either.
