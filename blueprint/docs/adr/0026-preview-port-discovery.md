# ADR 0026 — Preview port discovery on real compose projects (spike S6)

**Status:** Accepted

## Context

[Architecture §7](../01-architecture.md#7-previews) routes a preview to the service labelled `icaro.preview.port`, or else to the only service on port `80`, and forbids guessing. Spike S6 asked how real projects behave under that rule. The pass criterion was that the label plus the unambiguous-`80` fallback routes the sample set without guessing. The fallback was to require the label and document the layouts that are not supported.

## Spike

The sample set is 22 layouts in `internal/preview/testdata/s6`, shaped like common projects:
- static nginx, WordPress, and a reverse proxy in front of an app;
- Vite, Django, Rails with Postgres and Redis, and a frontend plus an API;
- two sites on port 80, and a service built from a Dockerfile with no ports in compose;
- override files, `.env` interpolation, profiles, `include`, `extends`, port ranges, expose-only services, the long port syntax, list-form labels, and a UDP port 80.

Each layout is normalized by `docker compose config`, as the manager does, and put through port discovery (`TestComposeSamples`). Three of them also run end to end through Traefik (`TestIntegrationPreviewSampleRouting`).

Results:
- **Routed correctly (15):** every layout with a label, or with exactly one service on TCP port 80. That includes port 80 behind a published host port (`8000:80`), expose-only services, host-IP bindings, ranges, interpolated ports and labels, inactive profiles, `include` and `extends`.
- **Refused (7):** a single dev server off port 80 (Vite on 5173, Django on 8000), Rails with its databases, a frontend plus an API, two sites on 80, a build with only a Dockerfile `EXPOSE`, and an app next to a UDP service on 80. None of them was routed by a guess.
- **Three defects found and fixed:**
  - Ícaro passed only the compose file to compose (`-f compose.yaml`), so compose skipped the project's `compose.override.yaml`. Ports and labels there were invisible to discovery. Worse, `up` ran a different stack from the developer's own `docker compose up`, and compose lint never saw what the override added.
  - A UDP port 80 counted as HTTP and could win discovery.
  - A refusal said only "add the label", without naming the services and ports to choose from.

## Decision

- **The rule stands.** The label wins. Otherwise exactly one service publishing or exposing TCP port `80` is routed. Otherwise the preview is refused. The spike's pass criterion holds: nothing in the sample set is routed by a guess.
- **The stack is what compose itself would load:** the compose file plus its override (`compose.override.yml`, `compose.override.yaml`, `docker-compose.override.yml`, `docker-compose.override.yaml`, in compose's precedence). Discovery, compose lint and `up` all use the same set, so lint covers everything the preview runs. Editing any override restarts the preview. Without the docker CLI, the raw files are merged as a superset (lists joined, flags ORed) for lint.
- **Only TCP ports are candidates.** UDP and SCTP mappings never take part in discovery.
- **A refusal names the candidates:** every service with a TCP port, with its ports, plus the label to add. It still picks none.
- **Rejected heuristics:**
  - "The only service with any port", because databases and caches publish ports too: Rails would route to Postgres.
  - "The only service with a build context", because frontend plus API stacks have two.
  - Reading a Dockerfile's `EXPOSE`, because it needs a built image, ties routing to build order, and still guesses when several ports are exposed.
  - Well-known dev ports (3000, 5173, 8000), because they collide with APIs and admin tools in the same stack.

## Consequences

- Common dev-server stacks need the label. That matches the MVP scope ("compose projects with a declared `icaro.preview.port`") and is one line, which the refusal now spells out.
- Projects that keep ports or labels in an override file work without changes, and their previews now match `docker compose up`.
- Files pulled in through `include` or `extends` are read on every preview start, but editing them does not trigger a restart on its own; `icaro preview up --force` applies such an edit.
- The sample set stays in the repository as a regression test for the rule and its messages.
