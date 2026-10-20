# Phase 10 — Documentation and landing page

**Objective:** public docs and mythological identity.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Build a static, lightweight GitHub Pages landing page ([`site/`](../../../site/index.html), [`pages.yml`](../../../.github/workflows/pages.yml)).
- [ ] Write quickstart, security, workflows, previews, and FAQ. *Index stub and [upgrading guide](../../../docs/upgrading.md) exist; the rest waits for the final CLI and APIs.*
- [x] Use the Ícaro artwork with a license and alt text ([`site/icaro.svg`](../../../site/icaro.svg), CC BY 4.0).
- [ ] Create reproducible examples.
- [x] Add CONTRIBUTING, CODEOWNERS, SECURITY, and a roadmap ([CONTRIBUTING](../../../CONTRIBUTING.md), [CODEOWNERS](../../../.github/CODEOWNERS), [SECURITY](../../../SECURITY.md), [roadmap](../04-roadmap.md)).

## Acceptance criteria

- [ ] The quickstart passes in the CI/documentation test.
- [x] No broken links (`hack/linkcheck` runs in the [docs workflow](../../../.github/workflows/docs.yml) on every PR).
- [ ] Examples use images pinned by digest.
