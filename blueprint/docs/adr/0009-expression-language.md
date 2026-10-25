# ADR 0009 — One expression language for workflows and manifests

**Status:** Accepted — resolves [open item O3](../02-decisions-risks.md#open-items)

## Context

`when`, trigger `filter`, manifest `match`, and every `${{ }}` interpolation need a grammar. O3 fixed the constraints: parsed at validation time, a missing key is an error, functions come from an allow-list with no env/file/exec access, and a step may reference only its `needs` ancestors.

## Decision

`internal/expr` implements one small language used by workflows and integration manifests.

- **Templates** are strings with `${{ expr }}` segments. A string that is exactly one `${{ }}` keeps the value's type (so `input: {n: "${{ trigger.pr_number }}"}` passes a number); mixed text renders values as strings, and objects as JSON.
- **Conditions** (`when`, `filter`, `match`) are bare expressions, optionally wrapped in one `${{ }}`, and must evaluate to a boolean — there is no truthiness.
- **Grammar:** literals (`'str'`, `"str"`, numbers, `true`, `false`, `null`), references (`steps.build.outputs.url`, `headers['x-github-event']`, `trigger.labels[0]`), `== != < <= > >=`, `&& || !`, parentheses, and calls to allow-listed functions. Identifiers may contain `-`, so step ids such as `build-app` work in paths. There is no arithmetic.
- **Functions:** `contains startsWith endsWith lower upper trim length toJSON fromJSON join split format coalesce string number`, plus `has(ref)`, the only construct for which an absent key is not an error.
- **Static analysis:** every expression exposes its references, so validators check roots, `needs` ancestry and declared outputs before a run starts; unknown names and functions carry nearest-match hints.
- **Roots:** workflows see `steps`, `trigger`, `env`, `secrets`, `run`, `workflow` (a `when` does not see `secrets`: a step's status would leak a condition on one; host-bound integration credentials are never interpolable); trigger filters see `trigger`; manifests see the roots their section defines (`headers`, `body`, `query`, `raw_body` for triggers; `input`, `config`, `response` for actions).

## Consequences

- The workflow validator rejects a reference to a missing output, an undeclared `needs` ancestor, or a typo'd function before the workflow is saved.
- Manifests and workflows read the same way, and the same fuzz-tested parser guards both.
- Adding a function is an ADR-level change to the allow-list, not an implementation detail.
