# Icaro — Blueprint Analysis (superseded)

This document described the earlier server-side workflow-engine design. It is superseded by
[`blueprint/`](../blueprint/README.md), per
[ADR 0008](../blueprint/docs/adr/0008-blueprint-supersedes-engine-design.md).

- Ideas that fit the blueprint were merged into it: the step I/O contract, agent authoring over MCP, sandbox hardening, delivery records, portable SQL, and token auth.
- Compatible ideas blocked by an MVP cut are listed under [Deferred designs](../blueprint/docs/02-decisions-risks.md#deferred-designs).
- Code that still follows this design is scheduled in [existing code](../blueprint/docs/04-roadmap.md#existing-code).

The full original text is in git:

```bash
git show 4fa63e8:docs/blueprint-analysis.md
```
