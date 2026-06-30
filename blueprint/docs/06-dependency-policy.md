# Dependency policy

## Default

Prefer the standard library. Any dependency must have:

- active maintenance or proven stability;
- a compatible permissive license;
- a relevant community/contributor base;
- a small scope and a mature API;
- a pinned version and automated updates.

## Candidates to evaluate in a spike

- JSON Schema: `github.com/santhosh-tekuri/jsonschema/v6`.
- Cron: evaluate `robfig/cron/v3` versus a maintained alternative; abstract it behind an interface.
- UI graph: React Flow, isolated in `web/`.
- Docker: CLI subprocess initially (more predictable for Compose); the Docker SDK only when it eliminates a concrete need.
- DB: `modernc.org/sqlite` (pure Go, CGO-free); use the standard `database/sql` interface and avoid CGO-based drivers (e.g. `mattn/go-sqlite3`) unless a concrete need arises.

## Forbidden without an ADR

A heavy HTTP framework, an ORM, an event bus, a DI container, a mutex in the domain, a dependency on a specific agent, or a library that requires CGO without an explicit need.
