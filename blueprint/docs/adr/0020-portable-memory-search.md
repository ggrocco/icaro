# ADR 0020 — Memory search is BM25 over a portable inverted index

**Status:** Accepted

## Context

Architecture §8 asks for text/FTS retrieval first, and §4 requires portable SQL so the SQLite/Postgres swap stays honest. SQLite FTS5 and Postgres `tsvector` are both dialect-only features.

## Decision

- Memory text is tokenized in Go (lowercase letter/digit runs, camelCase and snake_case split, light plural stemming, a short stopword list) and stored in `memory_terms(term, memory_id, project_id, tf, doc_len)` with a covering index on `(project_id, term, tf, doc_len, memory_id)`. Title terms count double. The same SQL runs on both backends.
- Queries are ranked with Okapi BM25 (k1 = 1.2, b = 0.75) using project-wide statistics. For each query term the engine reads its document frequency and its 2000 highest-tf postings from the index alone, scores candidates in Go, then fetches the best candidates by primary key and applies filters (kind, branch, agent, since, source prefix, status, TTL) in chunks. When filters exhaust a capped candidate list, an exact join over every posting runs instead.
- **Latency target:** on a 10k-document project (≈60 words per document, Zipf-distributed vocabulary), p95 query latency stays under **50 ms** on SQLite, measured by `TestQueryLatency10k` without the race detector (`go test -run QueryLatency10k ./internal/memory`). The reference run measured p50 ≈ 14 ms, p95 ≈ 34 ms. The test skips under `-race`, which slows the pure-Go SQLite driver by an order of magnitude.
- Vectors stay out until a local benchmark shows text retrieval is not good enough (architecture §8).

## Consequences

- No dialect-specific search code; store tests cover both backends.
- Ranking of very common terms is approximate (only their highest-tf postings compete), which matters little because their idf is near zero.
- Bulk ingest writes one posting per distinct term per memory; a 10k-document load takes on the order of 15 s on SQLite. Ingest is incremental after the first run.
