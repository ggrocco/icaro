# Spike S12 — results on the build machine (CPU tier)

Run on 2026-11-10 with `go run ./hack/spike-s12` against llama.cpp's server (the pinned `container.DistillImage`), started the way the managed runtime starts it: no network, read-only root, all capabilities dropped, `--cpus 2`, `-t 2`, one slot. The 12k-context run also had the daemon's memory limit (model size plus 2 GiB).

**Machine:** a cloud VM with an Intel Xeon at 2.10 GHz, 4 vCPUs and 16 GB of RAM, no GPU. The container was held to 2 cores, so the timings stand in for the "4-core laptop, 2 cores for the distiller" case. They do not stand in for an 8 GB laptop's memory pressure, an Apple Silicon Mac or a GPU, which are still open.

**Test set:** the 12 chats in `testdata/`, written for this spike, with the expected fields for each. They range from 70 to 6,023 tokens (`long-incident-page` is the 6,000-token budget case). The spike asks for 30; the other 18 are still to be recorded from real chats.

**Grading:** each expected item is a regular expression that must match the field. *Strict* also requires a field expected empty to be empty; *lenient* ignores extra items. A person still has to grade a sample: the "reviewed" column is the strict misses checked by hand by the agent that wrote the set, not by a person.

## Results

| Model (Q4_K_M) | Context | Prompt tok/s | Gen tok/s | Parsed | Strict | Lenient | Reviewed | 6,023-token chat | Peak RAM |
|---|---|---|---|---|---|---|---|---|---|
| Qwen3.5-2B, first prompt | 8k | 108 | 7.6 | 12/12 | 46/72 (64%) | 61/72 (85%) | — | 85 s (3,908 tokens then) | 3.5 GiB* |
| Qwen3.5-2B, revised prompt | 8k | ~100 | 7.7 | 12/12 | 51/72 (71%) | 66/72 (92%) | — | 188 s, 2 parts and a merge, 3/6 fields | — |
| **Qwen3.5-2B, revised prompt** | **12k** | **109** | **7.4** | **12/12** | **53/72 (74%)** | **68/72 (94%)** | **63/72 (88%)** | **103 s, one call, 5/6 fields** | **2.4 GiB of 3.2 GiB** |
| Qwen3.5-0.8B, revised prompt | 8k | 207 | 15.4 | 12/12 | 46/72 (64%) | 58/72 (81%) | — | 92 s, 2 parts and a merge, 2/6 fields | 2.2 GiB* |

\* `docker stats` with no memory limit, which counts the page cache of the memory-mapped model.

Short chats (70–170 tokens) take 11–46 s on the 2B model and 8–22 s on the 0.8B model; generation dominates.

## CPU share

On the 4-core machine, a busybox container standing in for an agent session (`--cpus 2`, hashing in a loop for 40 s) did 4,651 rounds alone and 5,018 while the 2B model distilled the 6,023-token chat beside it. The difference is noise. The distill took 106 s, the same as alone. The distiller's quota of 2 cores leaves the rest of the machine to sessions.

## What changed because of it

- `distill.ParseSummary` drops placeholder items ("None", "N/A", "Not mentioned"), which both models wrote into empty lists.
- The system prompt defines `preferences` (taste, not the workflow's configuration), `rejected` (anything the person said no to, replaced or removed), `open` (questions the chat left, not the model's own) and `resume` (what was done last and whether it was saved).
- The runtime's context went from 8k to 12k tokens and `distill.ChunkChars` from 16,000 to 32,000, so a 6,000-token chat is one call. That was both faster (103 s instead of 188 s) and better (5/6 fields instead of 3/6) than two parts and a merge.

## Verdict for the CPU tier

The pass bar holds on this machine, with the caveats above. The 2B default distills a 6,000-token chat in under 3 minutes on 2 cores, every output parsed, 88% of fields are correct on review, and the model invented no secret, host or workflow name. The 0.8B model stays the minimal-tier default: it is twice as fast but clearly weaker.

The weakest field is `rejected`. In 3 of the 12 chats the 2B model reported a rejection the person never made, such as an extension of a proposal read as a rejection. These are `confidence: model` memories that expire, and the preamble frames every note as data. A person grading the 30-chat set should look at this field first.

## Reproduce

```sh
docker run -d --name s12 --network none --cpus 2 --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v ~/.icaro/models:/models:ro -v /tmp/s12:/sock <container.DistillImage> \
  -m /models/Qwen3.5-2B-Q4_K_M.gguf --host /sock/llama.sock -c 12288 -t 2 -np 1 --no-webui
go run ./hack/spike-s12 -socket /tmp/s12/llama.sock -label qwen3.5-2b -out s12.json
```
