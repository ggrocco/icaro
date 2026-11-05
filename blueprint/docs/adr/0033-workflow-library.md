# ADR 0033 — A workflow library of scripts and step presets

**Status:** Proposed. Would amend [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) by one step field, `preset`, and adds no step form. Takes the "workflow library" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs). Follows the pack rules of [ADR 0007](0007-integration-manifests.md) and [ADR 0016](0016-pack-loading-action-execution.md).

## Context

Workflows repeat themselves. The same helper script (summarize an issue, post a comment, wait for a port) is pasted into `command:` across workflows. The same step shape (an image pinned by digest, its command, its env and output capture) is copied from one file to the next, and an agent authoring a workflow regenerates both from scratch every time. Fixing a helper means editing every workflow that pasted it.

The superseded design ([ADR 0008](0008-blueprint-supersedes-engine-design.md); `git show 4fa63e8:docs/implementation-plan.md`, "Shared library") had a library of scripts and presets. It was versioned in the database, mounted read-only at `/icaro/lib`, and used through new `script:` and `preset:` step forms. The deferred-design row kept two ideas from it: materialize the library content-addressed, and pin one snapshot per run. This ADR fits those ideas to what the blueprint has since decided:

- content that changes behavior lives in git-backed packs, not in the database ([ADR 0007](0007-integration-manifests.md));
- a step is an `image`, a `uses` action or a pause (cut 8);
- third-party content gets a permission summary before it is activated ([ADR 0016](0016-pack-loading-action-execution.md)).

## Decision

### Library packs

The library is a set of **packs**: git repositories under `~/.icaro/library/<pack>/`, managed like integration packs.

- A pack holds `scripts/<name>` (any language, any file) and `presets/<name>.yaml`.
- `icaro library add <git-url>` clones into a hidden staging directory, prints a summary and activates only on confirmation. The summary lists every script, every preset and every image the presets would run. `icaro library update <pack>` is `git pull --ff-only` and prints the new summary.
- Edits made from the web UI, the API or MCP go to the `local` pack only. They are validated before any write and committed with a structured message (`Edited-By`, the items touched).
- **Names are flat**, so a workflow writes `summarize-issue`, not `<pack>/summarize-issue`. As with integrations, when two packs define the same script or preset, **neither** is loaded, and the collision is reported in `icaro library status`, the API and `icaro doctor`. Names match `^[a-z0-9][a-z0-9._-]{0,63}$`.
- A pack's last good version is kept in memory per item. An item that is already broken when the daemon starts is not loaded, as for manifests.
- A script's first comment line starting with `description:` is its one-line description, shown in listings. Ícaro parses nothing else in a script.

### Scripts are files on a path, not a step form

Every **image step** container gets the run's library snapshot bind-mounted **read-only** at `/icaro/lib`, with scripts at mode `0555`, plus a `MANIFEST.json` listing each script's name, pack, pack commit, SHA-256 and description.

A step uses a script by calling it, for example `command: summarize-issue --dry-run` or `command: /icaro/lib/summarize-issue`. `/icaro/lib` is **appended** to the image's `PATH`, which the engine reads from the image config. It is not prepended, so a pack cannot shadow `sh`, `git` or `curl`. A script named after a binary the image already has is reachable only by its full path.

A script runs with the image, profile, egress allowlist and env of the step that calls it, and nothing more. The mount gives it no credential, socket or network access of its own.

No `script:` step form is added. A script plus the image that runs it is what a preset is for.

### Presets are step defaults, merged before validation

A preset is a partial **image step**: `image`, `command`, `env`, `input`, `outputs`, `timeout` and `retry`, plus an `inputs` declaration of the input keys it accepts. Each key has `required`, `default` and `description`.

```yaml
# presets/summarize-issue.yaml
description: Summarize a GitHub issue for later steps
image: python:3.13-slim@sha256:<digest>
command: summarize-issue
inputs:
  issue_url: {required: true}
  dry_run: {default: "false"}
outputs: {summary: "json:summary"}
```

```yaml
- id: summarize
  preset: summarize-issue
  input:
    issue_url: ${{ trigger.issue_url }}
```

- **`preset` is a field, not a form.** It is allowed only on image steps; a step with `preset` but no `image` takes the preset's. It is the only cut-8 change.
- **Merge rule.**
  - The preset is applied first, then the step's own fields.
  - Scalar fields (`image`, `command`, `timeout`, `retry`) are replaced whole by the step.
  - `env`, `input` and `outputs` merge key by key, and the step wins.
  - `id`, `needs`, `when` and `repository` never come from a preset.
  - Presets do not nest.
- **Validation runs on the merged step.** Each issue's JSON Pointer is the step's path, and the message names the preset file when the offending field came from the preset. The rules are:
  - an unknown preset gets a nearest-match hint;
  - a missing required input is an error at `/steps/N/input/<key>`;
  - an input the preset does not declare is an error.
- **A preset's image must be pinned by digest.** A preset is code chosen by whoever wrote the pack, so a moving tag would let the pack change what runs without a commit.
- **Expressions in presets** may use `env`, `run` and `workflow`, but not `steps`, `trigger` or `secrets`. A preset is reusable, so it cannot know the workflow's step ids; the step's values reach the preset's command as inputs, through `/icaro/input.json` and `ICARO_INPUT_<NAME>`, with the declared defaults filled in. A preset from a third-party pack must not be able to reach a secret by name: a step that wants a secret in a preset's `env` passes it explicitly in its own `env`, where the reader of the workflow sees it.

### One snapshot per run

When a run starts, the engine **materializes** the loaded library into a content-addressed directory, `~/.icaro/runs/lib/<sha256>/`, where the hash is taken over the manifest. It records the hash on the run (`runs.library_hash`) and the manifest in `library_snapshots`.

- Presets are resolved from the same snapshot at run start.
- Every step of the run, retries included, sees that snapshot, even if a pack updates mid-run. A run parked on an approval, `wait` or `await` step resumes with the snapshot it started with.
- The same library content gives the same hash and the same directory, so unchanged runs build nothing.
- A snapshot directory is kept while a non-terminal run references it, and pruned after that.
- If the directory is missing when a run resumes (pruned by hand, or a new machine with the same database), it is rebuilt from the pack commits recorded in the manifest, with `git show <commit>:<path>`. A pack that no longer has that commit fails the run with that reason.
- Run history keeps the manifest. A finished run therefore always says which version of which script it ran, even after the directory is gone.

### Agents

- **Outside MCP** gains `list_library`, `get_library_item` and `put_library_item`. `put` writes to the `local` pack and returns every workflow that references the item, so an agent sees the blast radius of an edit. There is no delete tool, following "agents author and run; humans delete" ([architecture §10](../01-architecture.md#10-agent-interface)).
- `validate_workflow` resolves presets, so an agent's loop converges on the merged step.
- The `icaro-workflows` skill gains a step 0: list the library before writing a script, and put reusable helpers in it.
- **Inside a step**, the library is files only. In-container MCP stays [deferred](../02-decisions-risks.md#deferred-designs), so a step cannot change the library.

## Alternatives considered

- **The superseded design: library in the database, with `script:` and `preset:` step forms.** A database copy duplicates what git already gives packs (history, review, sharing), and a `script:` form duplicates what a preset does. Two step forms would also have to be lifted from cut 8 instead of one field.
- **Namespaced names (`<pack>/<name>`).** No collisions, but longer `command:` lines and a `PATH` with a directory per pack. The flat name with "both unload" matches integration packs, and a collision is loud.
- **Prepend `/icaro/lib` to `PATH`.** Shorter calls for names that clash, but any pack could shadow a system binary in every step. Appending costs nothing in the common case.
- **Mount only into steps that reference the library.** Smaller surface, but "references" would have to be inferred from free-form `command:` strings. The mount is read-only, and a script runs only when a step calls it.
- **Copy library files into each image.** Every helper change would need an image rebuild, which defeats the point.

## Consequences

- A helper fixed once is fixed for every workflow's next run, and a running workflow is never changed under it.
- A run's record names the exact pack commit of every script it could call.
- Third-party packs are code that runs in your steps. The add-time summary, the digest-pinned preset images, the appended `PATH` and the no-`secrets` rule bound what a pack can do without the workflow author seeing it. Within a step, a script is as trusted as the step's own command.
- A preset edit changes every workflow that uses it from their next run on, and validation runs again only when a workflow is validated or run. `put_library_item` and `icaro library status` list the workflows a change affects, and a run whose merged step no longer validates fails before any step starts, with the issues.
- New state: `runs.library_hash`, a `library_snapshots` table, and snapshot directories under `~/.icaro/runs/lib/`.
- Agent sessions (interactive, outside workflows) do not get `/icaro/lib` in this design.
- Implementation would be a new phase after Phase 11: packs and loading, the snapshot and mount, `preset` in the contract and validator, the CLI, API, MCP and web UI library views, the skill, and docs.
