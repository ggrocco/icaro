# Phase 13 — Workflow library

**Objective:** shared scripts and step presets in git-backed library packs, mounted into every image step and pinned per run, as proposed in [ADR 0033](../adr/0033-workflow-library.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0033 **accepted**, with its open questions settled:
- flat names or pack-qualified names;
- presets on `uses` steps or on image steps only;
- `/icaro/lib` in interactive agent sessions;
- whether `put_library_item` may change an existing item.

A change to any of them is made in the ADR first, then here.

## Deliverables

- [ ] **Packs.** A new `internal/library` package loads packs under `~/.icaro/library/` (a new `paths.Layout` entry): `scripts/<name>` and `presets/<name>.yaml`.
  - **Shared git plumbing.** Clone into staging, fast-forward update, and commit to the `local` pack with `Edited-By`. This is shared with `internal/integration` packs: factor it into one package used by both, rather than copying it.
  - **Loading rules.** Owner-goroutine snapshots, the last good version kept in memory per item, and a broken-at-start item not loaded. Names must match `^[a-z0-9][a-z0-9._-]{0,63}$`, and a name defined by two packs unloads both.
  - **Wiring.** Done in `internal/app`.
- [ ] **Preset format.** A parser and JSON Schema for `presets/*.yaml`: `description`, `image` (digest-pinned), `command`, `env`, `input`, `outputs`, `timeout`, `retry` and `inputs` (`required`, `default`, `description`). Expressions are parsed at load time and limited to the roots `env`, `run` and `workflow`. The parser gets a `FuzzParse` target, since packs are untrusted input.
- [ ] **Contract.** Add the `preset` field (image steps only) to the spec types in `internal/workflow`, regenerate `schema/workflow.schema.json` with `go generate ./...`, and update the [blueprint contract](../schemas/workflow.schema.json) so the drift test passes.
- [ ] **Validation.** Resolve presets through a catalog interface (nil skips the check, as for integrations), merge them with ADR 0033's rule, then validate the merged step.
  - An issue keeps the step's JSON Pointer and names the preset file when the field came from it.
  - An unknown preset gets a nearest-match hint.
  - A missing required input is an error at `/steps/N/input/<key>`, and an undeclared input is an error.
  - `id`, `needs`, `when` and `repository` in a preset are an error at load time.
- [ ] **Snapshot.** At run start, the engine:
  - materializes the loaded library into `~/.icaro/runs/lib/<sha256>/` (scripts `0555`, `MANIFEST.json`);
  - records `runs.library_hash`, with the manifest in `library_snapshots`;
  - resolves presets from the same snapshot.

  The snapshot covers retries, restarts and parked runs. A missing directory is rebuilt from the recorded pack commits, or the run fails with the reason. Directories no non-terminal run references are pruned. Migrations go in both dialects.
- [ ] **Mount.**
  - **Read-only bind.** Every image step container gets `/icaro/lib` bind-mounted read-only over the run's snapshot. This goes through the typed specs in `internal/container`, as a daemon-owned mount outside the user mount policy, and that exception is documented in [security](../03-security.md).
  - **PATH.** The engine reads the image's `PATH` from its config and appends `/icaro/lib`.
- [ ] **Clients.**
  - **CLI and API.** `icaro library add|update|status|list|show|rm` (`rm` needs `--yes`) and the matching API endpoints. `status` reports load errors, collisions and, per item, the workflows that reference it. `go run ./hack/gendocs` regenerates `docs/cli.md`.
  - **Web UI.** A Library page: items, their pack and commit, and "used by".
  - **Run views.** The run page and `get_run` show the library hash and manifest.
- [ ] **MCP and skill.** `list_library`, `get_library_item` and `put_library_item`. `put` writes to the `local` pack, is validated before the write, and returns the workflows that reference the item. `validate_workflow` resolves presets. The `icaro-workflows` skill gains "check the library first". The library is exposed to agents through MCP only; no new in-container access.
- [ ] **Doctor.** Checks for library load errors, collisions, and a preset whose image is not pinned. They are mapped in the doctor coverage test, and a row is added to the [doctor checks](../03-security.md#icaro-doctor).
- [ ] **Docs.**
  - `docs/workflows.md`: a "Library" section with a preset example in a ```` ```yaml workflow ```` block, so `hack/docscheck` validates it.
  - A new `docs/library.md`, or a section of `docs/integrations.md`, covering pack management.
  - [Architecture §6](../01-architecture.md#6-workflow-engine) and §10.
  - The blueprint: ADR 0033 marked Accepted, a decision-index row, cut 8's wording, and the deferred-designs row removed.

## Acceptance criteria

- [ ] `icaro library add` on a test pack prints every script, preset and preset image, and activates nothing until confirmed. `update` refuses a non-fast-forward.
- [ ] A name defined by two packs loads from neither, and the collision shows in `icaro library status`, the API and `icaro doctor`.
- [ ] A pack edit that breaks a preset keeps the last good version serving, and the error is reported. A preset with a tag-only image, a `secrets`, `steps` or `trigger` reference, or an `id`/`needs`/`when`/`repository` field is refused at load time.
- [ ] Inside an image step:
  - `ls /icaro/lib` lists the scripts and `MANIFEST.json`;
  - a script runs by bare name;
  - writing under `/icaro/lib` fails;
  - a pack script named `sh` (or another binary the image has) does not shadow the image's binary.
- [ ] A step using a preset runs with the preset's image, command and outputs, and with the step's overrides. Its declared input defaults reach the container through `/icaro/input.json` and `ICARO_INPUT_<NAME>`.
- [ ] An invalid preset use returns structured issues with JSON Pointer paths: an unknown preset (with a hint), a missing required input, an undeclared input, and an issue in a preset-supplied field naming the preset file.
- [ ] Two concurrent runs started on either side of a library update record different hashes, and each sees one consistent snapshot across all its steps and retries.
- [ ] A run parked on an approval, `wait` or `await` step resumes after a library update and a daemon restart with its original snapshot. With the snapshot directory deleted, it is rebuilt from the recorded pack commits.
- [ ] A finished run's manifest still names each script's pack, commit and hash after its directory is pruned.
- [ ] Through MCP, an agent lists the library, puts a script into the `local` pack (which shows up as a git commit with `Edited-By`), and gets the referencing workflows back. No MCP tool deletes a library item.
- [ ] Every new store and engine decision has unit tests against the fake engine. The mount, `PATH` and rebuild paths have tests behind the `integration` build tag. Store changes pass on SQLite and Postgres, and coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.
