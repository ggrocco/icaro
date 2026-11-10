# ADR 0036 — A small local model distills assistant chats into memory

**Status:** Accepted. The open questions are settled below. Spike S12 runs in part: the CPU tier is measured on the build machine, and the Apple Silicon and GPU tiers wait for those machines. Builds on [ADR 0035](0035-workflow-assistant-chat.md) (assistant chats), [ADR 0004](0004-memory-mcp.md) and [ADR 0020](0020-portable-memory-search.md) (memory and BM25 search). Adds an optional model runtime the daemon manages; no Go dependency.

## Context

An assistant chat ([ADR 0035](0035-workflow-assistant-chat.md)) learns things the next chat should not have to rediscover:

- which secret a Slack action needed, and which host it is bound to;
- which validator issue the assistant made and how it was fixed;
- why the person rejected a proposal;
- what the workflow is for, and the constraints the person stated.

Today that knowledge ends with the conversation. The subscription model ([ADR 0035](0035-workflow-assistant-chat.md)) makes the obvious fix expensive: asking the agent CLI to summarize each chat spends the user's usage window on bookkeeping. Summarizing must therefore run **locally, for free, on any machine**, including a laptop without a GPU, and must never slow the user's agents down.

Two observations keep the model's job small:

1. **Most of the knowledge is already structured.** The daemon sees every tool call, every `validate_workflow` result, every draft accepted or discarded, every run and its status. It can record these without any model.
2. **What is left is short free text.** That means the person's intent, the reasons they gave, and the assistant's explanations. Summarizing short text into a fixed JSON shape is the task where very small models are good enough, especially with output constrained to a schema.

## Decision

### Two stages: deterministic first, model second

**Stage 1, always on, no model.** When a chat goes idle (`assistant.idle_timeout`) or is closed, `internal/assist` writes memories from the chat's event log:

| Memory | Kind | From |
|---|---|---|
| Validator lesson: the issue's JSON Pointer and message, and the change that cleared it | `finding` | a `validate_workflow` with issues followed by one without them |
| Integration fact: the action, its inputs, the secret name and host binding it needed | `finding` | `get_integration`, `list_secrets` and the saved workflow |
| Run outcome: status, the failing step and its error line, and whether a later draft fixed it | `finding` | `get_run` and the run that followed |
| Proposal outcome: accepted or discarded, with the workflow version it became | `decision` | the draft events |
| Working example: a saved workflow that later ran green | `finding` | `ApplyWorkflow` plus a succeeded run |

**Stage 2, opt-in, a small local model.** The model reads only the chat's user and assistant text (tool payloads are already summarized by stage 1, and run logs are never fed to it). It fills one JSON schema:

```json
{
  "intent": "what the workflow is for, in one or two sentences",
  "constraints": ["stated requirements, e.g. 'only on weekdays', 'notify #ops'"],
  "preferences": ["naming, images, timezones or channels the person chose"],
  "rejected": [{"what": "...", "why": "..."}],
  "open": ["questions left unanswered"],
  "resume": "a short handoff to start the next chat on this workflow"
}
```

Output is constrained to the schema by the runtime (llama.cpp's JSON-schema grammar), so a small model cannot produce an unparseable answer. Each field becomes a memory: `intent` and `constraints` attach to the workflow, `resume` is a `handoff`, `rejected` entries are `decision`s, and `open` entries are `task`s.

Every memory either stage writes carries `origin: distill:<chat>`, the chat and turn it came from as provenance, and the workflow name. Stage 2 memories are also marked `confidence: model`, so a person can tell which came from a model and remove them.

### The runtime: llama.cpp, managed or bring-your-own

- **Managed (default when enabled).** The daemon runs llama.cpp's `llama-server` as a helper container, using the upstream image pinned by digest (`ghcr.io/ggml-org/llama.cpp:server`).
  - It runs under the helper baseline with **no network at all** (`--network none`). It listens on a Unix socket in a directory only it and the daemon share, and it mounts the models directory read-only.
  - It has a CPU quota (`memory.distill.cpus`, default 2) and a memory limit sized from the model, so a running distillation never competes with agent sessions.
  - It is started for a batch of jobs and stopped after it. The daemon talks to it over HTTP on that socket (OpenAI-compatible API) with the standard library; there is no cgo and no new Go module.
- **Bring your own.** `memory.distill.endpoint` points at a llama-server or Ollama the user already runs on the host. This is the fast path on Apple Silicon, where containers in Docker Desktop cannot use the GPU but a host process can use Metal.
- **Jobs.** Distillation is a queued background job, one at a time, never on a chat's code path. A transcript longer than the model's context is cut into chunks, each chunk is summarized, and the summaries are merged in a second pass. A failed job is recorded and retried once; it never blocks a chat.
- **Models** are downloaded only on opt-in (`icaro memory model pull [<name>]`), pinned by SHA-256, stored under `~/.icaro/models/`, and listed by `icaro doctor`. There are no official GGUF builds of the Qwen3.5 small models; the catalog pins the community conversions it was checked against (Unsloth's Qwen3.5 and Gemma 4, IBM's own Granite), each Apache 2.0 like its base model, by repository, file and SHA-256.

### Model tiers

`memory.distill.model: auto` picks a tier from RAM and GPU, using the same RAM probe as [ADR 0031](0031-session-concurrency-cap.md). It can be set explicitly. Every default is Apache 2.0, so it can be pulled without a separate license decision.

| Tier | When | Default model (GGUF Q4_K_M) | Approximate size |
|---|---|---|---|
| Minimal | under 8 GB of RAM, no GPU | Qwen3.5-0.8B, or Granite 4.0 Nano 1B | under 1 GB |
| CPU (default) | 8 GB of RAM or more, no GPU | **Qwen3.5-2B** | under 2 GB |
| GPU | a GPU with 6 GB or more, or Apple Silicon through a host endpoint | Qwen3.5-4B or Gemma 4 E4B | about 3 GB |
| Large GPU | a GPU with 10 GB or more | Qwen3.5-9B | about 6 GB |

Candidates evaluated and not chosen as defaults:

- **LFM2-1.2B-Extract / LFM2.5** (Liquid AI) is built for this kind of extraction and is fast on CPU. However, its LFM Open License ends commercial rights at $10M of annual revenue, so it cannot be a default for a tool anyone may use at work. A user may still pick it by name.
- **Llama 3.2 1B/3B** is capable, but under the Llama community license; the Apache-licensed options cover the same sizes.

The sizes come from vendor documentation and third-party write-ups, not from Ícaro's own measurements. Spike S12 replaces them with measured numbers and may change the defaults.

### Using the memory in the next chat

- At the start of a chat about a workflow, the daemon runs a BM25 query ([ADR 0020](0020-portable-memory-search.md)) on the workflow's name, the integrations its draft uses and the first message. It adds the top results to the chat's preamble under a fixed budget (`assistant.memory_budget`, default 2,000 tokens): first the workflow's `resume` and `intent`, then validator lessons and integration facts, then examples.
- The assistant can query further with the read-only memory tools ADR 0035 already grants.
- Repeated validator lessons are counted. The most frequent ones across all chats feed a short "common mistakes" list in every chat's preamble.
- Memories are data, never instructions. The preamble frames them as notes from earlier chats.

### Measuring whether it helps

For each chat, the daemon records:

- the number of `validate_workflow` calls before the first clean result;
- the number of turns to an accepted proposal;
- the accept rate;
- the time from the first message to the first saved version.

`icaro assist stats` compares chats with and without retrieved memory. A memory that is never retrieved, or that comes before a discarded proposal more often than not, is a candidate for expiry.

Embeddings stay out, as ADR 0020 requires, until these numbers show that BM25 retrieval misses relevant memories. If they do, the candidates for local CPU embeddings are EmbeddingGemma-300M (smaller, faster) and Qwen3-Embedding-0.6B (Apache 2.0, stronger on benchmarks). That would be a separate ADR.

## Alternatives considered

- **Ask the agent CLI to summarize at the end of each chat.** It writes the best summaries, but it spends the subscription's usage window on every chat and adds a turn the person did not ask for. It stays possible: the person can say "remember this", and the assistant writes it through the memory tools.
- **Embed a model runtime in the daemon** (llama.cpp through cgo, or a pure-Go runtime). That would mean one process and no container, but cgo breaks the pure-Go build, and the pure-Go runtimes are much slower on CPU. Rejected under the [dependency policy](../05-engineering.md#dependency-policy).
- **A model for everything, no deterministic stage.** It is simpler to describe, but the structured facts are exact without a model, and a small model would only make them less reliable.

## Settled questions

1. **Stage 2 runs for every chat once a model is available.** Pulling a model (or setting `memory.distill.endpoint`) is the opt-in; `memory.distill.enabled: false` turns stage 2 off.
2. **Model-written memories expire** after `memory.distill.ttl` (default 90 days). Stage 1's exact facts stay until a person removes them.
3. **The managed runtime is CPU only.** GPU and Apple Silicon users point `memory.distill.endpoint` at their own llama-server or Ollama.

## Consequences

- Every chat leaves structured memories at no cost, even without a model, and the next chat on the same workflow starts with them.
- With a model pulled, a 2B model on a 4-core laptop CPU can summarize a typical chat in the background in about a minute or two (an estimate S12 must confirm). Chats never wait for it.
- Nothing leaves the machine: the managed runtime has no egress, and the transcript it reads is already secret-redacted.
- A distilled memory can be wrong. Provenance, `confidence: model` and the measurements are how a person finds out and removes it.
- A run log can carry a prompt injection. Stage 2 never reads run logs, and every memory is framed as data in the preamble; a poisoned memory can still mislead a later chat, which the draft and approval gates of ADR 0035 contain.
- The model download (1–6 GB) is opt-in and listed by `icaro doctor`.
