# Phase 15 — Local memory distiller for assistant chats

**Objective:** turn every assistant chat into memories that make the next chat converge faster: structured facts with no model, plus an optional small local model for intent and handoffs, as proposed in [ADR 0036](../adr/0036-local-memory-distiller.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0036 **accepted** (done, with its questions settled). Spike **S12** runs alongside: the CPU tier on the build machine, the Mac and GPU tiers on those machines before release. The stage 1 deliverables need only [Phase 14](14-workflow-assistant.md); stage 2 needs S12.

## Spike S12 — small models on ordinary machines

- [ ] Build a test set of 30 recorded assistant chats, with a hand-written expected JSON for each (intent, constraints, preferences, rejected, open, resume).
- [ ] For each tier candidate (Qwen3.5-0.8B/2B/4B/9B, Granite 4.0 Nano 1B, Gemma 4 E2B/E4B) at Q4_K_M and Q8_0, measure on three reference machines (a 4-core x86 laptop with 8 GB and no GPU, an Apple Silicon Mac through a host endpoint, and a 6 GB NVIDIA GPU):
  - prompt and generation tokens per second;
  - peak RAM;
  - the time to distill a 6,000-token chat;
  - field-level quality against the expected JSON (a person grades a sample).
- [ ] Pass: on the 8 GB laptop, the CPU-tier default distills a 6,000-token chat in under 3 minutes with 2 cores, every output parses, and at least 80% of graded fields are correct with no invented secret, host or workflow name. Fail: the next smaller model, or stage 1 only on that tier.
- [ ] Record the results and the chosen defaults in ADR 0036.

## Deliverables

- [ ] **Stage 1 (no model).** In `internal/assist`, extractors from the chat's event log into memory: validator lessons, integration facts, run outcomes, proposal outcomes and working examples. Each uses a stable key per chat so re-running is idempotent, and records `origin: distill:<chat>` with chat and turn provenance.
- [ ] **Distill jobs.** A queue of distill jobs, run one at a time and persisted so a restart resumes them. Each job runs on chat idle or close, and is retried once. Migrations go in both dialects.
- [ ] **Runtime.** A helper container for `llama-server` (upstream image pinned by digest; no egress; CPU quota; started per batch), or `memory.distill.endpoint` for a host llama-server or Ollama. A small OpenAI-compatible client uses only the standard library, and output is constrained by a JSON schema. The model's output is parsed as untrusted input, with a `FuzzParse` target.
- [ ] **Models.** `icaro memory model pull|list|rm` (`rm` needs `--yes`), with SHA-256-pinned downloads under `~/.icaro/models/` and tier selection for `auto`. `go run ./hack/gendocs` regenerates `docs/cli.md`.
- [ ] **Stage 2.** Chunking for long transcripts, map and merge prompts, and the mapping of each field to a memory kind with `confidence: model`.
- [ ] **Retrieval.** A preamble at chat start built from a BM25 query under `assistant.memory_budget`, plus the "common mistakes" list.
- [ ] **Measurement.** Per-chat convergence metrics in the store, and `icaro assist stats` comparing chats with and without memory.
- [ ] **UI.** On the chat page, the memories a chat used and the ones it produced, each removable. On the workflow page, its intent and resume notes.
- [ ] **Doctor.** Checks that the distill model is present and its hash matches, that the endpoint is reachable, and the last job's error.
- [ ] **Docs.** A memory section in `docs/assistant.md` covering tiers, opt-in, privacy and removing memories; architecture §8; the blueprint (ADR 0036 marked Accepted, and a decision-index row).

## Acceptance criteria

- [ ] With no model pulled, a chat that fixed a validator issue and saved a workflow leaves a validator lesson, an integration fact and a proposal outcome in memory. The next chat on that workflow shows them in its preamble.
- [ ] With the CPU-tier model on a machine without a GPU, a closed chat gets `intent` and `resume` memories within the S12 time budget, while an agent session running at the same time keeps its CPU share.
- [ ] The managed runtime container has no route out; a test confirms a request to an outside host fails.
- [ ] No secret value from a chat reaches the model's input or a memory; a test plants one in a turn and checks both.
- [ ] A fixture with a prompt injection in a run log produces no stage 2 memory derived from it, because run logs are never in the model's input.
- [ ] Malformed or truncated model output fails the job with a recorded reason and writes nothing.
- [ ] Re-running a distill job does not duplicate memories.
- [ ] Removing a memory in the UI removes it from later preambles.
- [ ] `icaro assist stats` reports the convergence metrics for chats with and without memory.
- [ ] Unit tests use a fake endpoint (an HTTP edge mock) and never a real model. The container and download paths are behind the `integration` build tag, store changes pass on SQLite and Postgres, and coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.

## Where it lives

- Extractors, jobs and retrieval: `internal/assist/`; memory writes through `internal/memory`.
- Runtime container and client: `internal/assist/distill/`, with the container spec through `internal/container`.
- CLI: `internal/cli/memory_model.go`, `internal/cli/assist.go`.
