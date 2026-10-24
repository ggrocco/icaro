# Previews

Every worktree that contains a compose file (`compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`) gets a **live preview** at a predictable URL. That covers session worktrees and any directory listed in `previews.roots`. The agent edits code in its own container, and the preview's dev server sees the same files, because both bind-mount the same host worktree.

```sh
icaro preview list
icaro preview up [PATH]          # bring a worktree's preview up now (--force restarts it)
icaro preview down [PATH]        # stop it; the watcher leaves it alone until the next `up`
icaro preview logs ID|PATH
icaro preview rm [PATH] --yes    # tear down containers, volumes and the record
icaro preview lint [PATH]        # check a compose file against the policy (no daemon needed)
```

`icaro preview up` exits non-zero unless the preview ends up running, with `--json` too (it still prints the preview, whose `status` and `error` say why).

## Routing

Mark the service to route with a label, or let Ícaro use port 80 when exactly one service exposes it:

```yaml
services:
  web:
    build: .
    labels:
      icaro.preview.port: "5173"
```

Ícaro reads the same files `docker compose up` would: the compose file and its override file (`compose.override.yaml` and the other names compose accepts), after `.env` interpolation, profiles, `include` and `extends`. A label or port in the override counts, and the override is linted like the compose file. Then:

- A service with the label is routed, on that port.
- Without a label, the one service that publishes or exposes TCP port 80 is routed. `8000:80` counts, because the container port is 80. UDP ports never count.
- Anything else is refused, and the error lists each service with its ports so you can pick one. Ícaro never guesses ([ADR 0026](../blueprint/docs/adr/0026-preview-port-discovery.md)).

Most dev servers need the label: Vite (5173), Next.js or Rails (3000), Django (8000), and any stack with a frontend and an API. A port that only appears in a Dockerfile's `EXPOSE` is not visible either; add the label or an `expose:` entry. Editing a file pulled in through `include` or `extends` doesn't restart the preview on its own; run `icaro preview up --force`.

Ícaro never edits your compose file. It writes an override that joins the routed service to the `icaro-preview` network under a stable alias, and a route file for its Traefik proxy. The proxy uses Traefik's file provider, so it never gets the Docker socket ([ADR 0017](../blueprint/docs/adr/0017-preview-proxy-file-provider.md)). It listens only on `127.0.0.1:<previews.http_port>` (default `8780`).

URLs look like `http://<slug>.localhost:8780`. `*.localhost` resolves to loopback without any DNS setup. The slug comes from the worktree's directory name; when two worktrees share a name, the second becomes `<slug>-<hash>`, so they never collide ([ADR 0018](../blueprint/docs/adr/0018-preview-naming-and-lint-overrides.md)). Every preview also prints a form that always works, `curl -H 'Host: <host>' http://127.0.0.1:8780/`. `previews.domain: test` switches to `.test` names, which need a local resolver you configure yourself; `icaro doctor` warns when it isn't set up. TLS for a custom domain is not built yet.

## Hot reload

Code changes reach the dev server through the bind mount, and reloading is the app's job. Ícaro restarts a preview only when its configuration changes: the compose files, `.env`, `Dockerfile` or a known lockfile. On macOS, Docker Desktop does not deliver file events into containers reliably, so Ícaro sets `CHOKIDAR_USEPOLLING=true` and `WATCHPACK_POLLING=true` on the routed service. Configure polling in other watchers too, for example Vite's `server.watch.usePolling`.

## The compose lint

Preview stacks run your own containers, so instead of the container baseline they go through a lint before they start. It blocks:

- `privileged`;
- `pid`, `ipc`, `network_mode` or `userns_mode` set to `host`;
- devices;
- dangerous `cap_add` values (`SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `ALL`, …);
- `security_opt` entries that set seccomp, AppArmor or SELinux to `unconfined`;
- the Docker socket;
- mounts of `/var/run`, `/run`, `/proc`, `/sys`, `/etc`, `/dev`, `/boot` or `/var/lib/docker`;
- bind mounts outside the worktree and `mounts.allow`.

Each finding names the field and explains the risk. Doing this weakens the preview's isolation from your machine; to proceed anyway, allow that one finding for that one worktree:

```sh
icaro preview allow ./my-worktree --finding web/privileged --yes
```

The override is recorded per finding. A new dangerous field is still blocked, and removing the preview clears its overrides. `icaro doctor` lists blocked compose files and checks the proxy network.

## Requirements

- The Docker Compose plugin (`docker compose`).
- The `previews.traefik_image` image (default `traefik:v3.1`).
- `previews.enabled: true` (the default).
