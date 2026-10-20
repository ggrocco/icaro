# ADR 0017 — The preview proxy uses Traefik's file provider, not the Docker socket

**Status:** Accepted

## Context

Architecture §7 routes previews through an Ícaro-managed Traefik on the dedicated `icaro-preview` network. The common Traefik pattern is its Docker provider: mount `/var/run/docker.sock` into Traefik and read routing from container labels. That would make Traefik the one container Ícaro creates with the Docker socket, which is root on the host, and it would let any compose project route itself just by adding labels. The security model says no Docker socket in Ícaro's containers (03-security), and it narrows even the daemon's own Docker access.

## Decision

- Traefik runs with its **file provider** only. For each preview the daemon writes one route file, `~/.icaro/previews/traefik/<compose-project>.yaml`, mapping `Host(<hostname>)` to `http://<alias>:<port>`. That directory is bind-mounted **read-only** into Traefik, which watches it.
- The routed service joins `icaro-preview` under a **stable network alias** (`icaro-<slug>-<hash>-web`) set by the generated compose override. Traefik reaches it by that alias, so there is no port publishing per preview and no label-based routing. The override also carries informational `icaro.preview.*` labels.
- The proxy container gets a baseline of its own: `--cap-drop ALL`, `no-new-privileges`, a read-only root filesystem, a pids/memory limit, the daemon user's UID, an unprivileged in-container port (`:8080`), and a single host binding on `127.0.0.1:<previews.http_port>`. Its configuration hash is a label, so a change of image, port or directory recreates it.
- On macOS, after a route change the daemon restarts the proxy container, because file events on Docker Desktop bind mounts are unreliable — the same reason polling watch is mandatory for dev servers (architecture §7, spike S1).

## Consequences

- No Ícaro-managed container holds the Docker socket, and a compose project cannot claim a route: only the daemon writes routes, after the compose lint passes.
- The architecture's "Traefik label overrides" became "a network-alias override plus a route file". The user's compose file is still never edited.
- On macOS a route change briefly interrupts other previews (about a second of proxy restart). If spike S1 shows that bind-mount events are reliable, the restart can be dropped.
- The doctor check "Proxy without a dedicated network" recreates `icaro-preview` and drops the proxy so it is recreated on that network.
