# ADR 0015 — Integration manifest schema v1 and the webhook pipeline

**Status:** Accepted

## Context

[ADR 0007](0007-integration-manifests.md) made integrations declarative manifests, but left the manifest format and the exact handling of an inbound request unwritten. Phase 3 needs both frozen before third parties write packs.

## Decision

**Manifest schema v1** is published at [`schemas/integration-manifest.schema.json`](../schemas/integration-manifest.schema.json) and embedded in the binary; a drift test keeps the two identical. A manifest declares `name`, `hosts` (every host its actions may contact), `config` (defaults; only `base_url` is user-overridable), `auth` (named references: `bearer`, `header`, `basic`, `query`, `none`, each naming a credential `<integration>.<credential>`), `triggers` and `actions`. Expressions use the shared language ([ADR 0009](0009-expression-language.md)) with fixed roots: `headers`, `body`, `query`, `raw_body` in triggers; `input`, `config` in requests; plus `response` in outputs.

Semantic validation rejects what the schema cannot express: undeclared inputs, config keys or auth references (with nearest-match hints); action URLs that do not start with `https://<host>` or `${{ config.<key> }}`; a URL host missing from `hosts`; literal `Authorization` headers; bodies with other than exactly one of `json`, `form` and `raw`.

**Verification kinds:** `hmac-sha256` / `hmac-sha1` over a templated message (default the raw body; Slack signs `v0:<timestamp>:<body>`), with header, prefix and hex/base64 encoding; `shared-secret` (constant-time header compare); `url-token` for the generic `webhook` integration, whose per-workflow random token is the credential. Optional `timestamp` adds replay protection with a maximum skew. `challenges` answer provider handshakes (Slack `url_verification`) after verification without starting a workflow. Actions may declare a `success` condition for APIs that answer 200 with an error body.

**Pipeline**, in this order:

1. rate limit (token bucket per integration) and body cap;
2. verify, then persist the delivery record — rejected requests are recorded too, with credential headers masked;
3. dedupe: a verified delivery whose key matches an earlier *processed* one is recorded as `duplicate` and starts nothing. The key is the provider's delivery id, else a hash of the signed body, else (unsigned `url-token`) a fresh id;
4. challenges;
5. match events, then **normalize** each matched event's payload into `trigger.*`;
6. match subscribed workflows and evaluate each trigger's `filter` over the normalized payload — so filtering still happens before enqueue, but after normalization, because `filter` is defined over `trigger.*`;
7. enqueue with idempotency key `<integration>:<delivery key>:<workflow>`.

If enqueueing fails the delivery is recorded as `error` and answered with 503, so the provider retries; an `error` delivery does not count for dedupe, and the engine's idempotency key keeps partial successes from running twice. Redelivery re-runs steps 5–7 from a stored, verified record with key `redeliver:<new delivery id>:<workflow>`.

## Consequences

- Delivery records answer "why didn't my workflow fire?": verification error, duplicate, no matching event, filtered, skipped by concurrency policy, or the run id.
- Manual runs list an event's payload keys as the fields to prompt for, so every integration-triggered workflow stays testable by hand.
- The bundled manifests were checked against payload fixtures written from the providers' documentation. Validation against the live APIs (spike S8) is still open and may require extending the template language.
