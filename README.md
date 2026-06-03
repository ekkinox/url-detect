# URL Detect (PoC)

> Extracts a low-cardinality **pattern** from a URL using a mix of heuristic and local LLM based detections.

Dynamic segments (ids, UUIDs,
hashes, usernames, slugs) become named placeholders so the result is stable
enough to use as a metric/span label. Query strings and fragments are dropped.

```
/users/7/                              -> /users/{userId}/
/api/v2/users/3                        -> /api/v2/users/{userId}
/files/order-9f3a8821                  -> /files/{fileId}
/orders/42?limit=10&name=foo           -> /orders/{orderId}
/orgs/acme/projects/12/builds/9f3a     -> /orgs/{orgId}/projects/{projectId}/builds/{buildId}
```

It runs as an HTTP server: `POST /patterns` with one URL (`{"url":"..."}`) or
many (`{"urls":[...]}`) returns the pattern for each.

Uses [kronk](https://github.com/ardanlabs/kronk) for local LLM integration.

## Run

Everything goes through the [Makefile](Makefile). Run `make` to list the available targets.

```shell
# 1. Build the image (downloads llama.cpp + the model into the image,
#    so the container starts ready to serve — no runtime download).
make docker-build

# 2. Run it (serves on :8080 by default).
make docker-run

# 3. In another terminal, send a sample request.
make query
#    or via curl with your own URL
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"url":"/api/v2/users/3"}'
```

Stop it with `Ctrl-C`, or `make docker-stop` if you started it detached
(`make docker-run-detached`).

## Configure

Override any of these on the `make` command line (defaults shown):

| Variable    | Default                     | Meaning                                            |
|-------------|-----------------------------|----------------------------------------------------|
| `MODEL`     | `unsloth/Qwen3-1.7B-Q4_K_M` | GGUF model to bake in and serve                    |
| `PROCESSOR` | *auto-detected*             | llama.cpp build: `cpu` / `vulkan` / `rocm` / `cuda` |
| `PORT`      | `8080`                      | HTTP port                                          |
| `NSEQ`      | `4`                         | Max concurrent model calls (parallelism)           |

`MODEL` and `PROCESSOR` are baked in at build time, so pass them to **both**
build and run:

```shell
make docker-build MODEL=unsloth/Qwen3-0.6B-Q8_0
make docker-run   MODEL=unsloth/Qwen3-0.6B-Q8_0 PORT=9000 NSEQ=8
```

## GPU

`PROCESSOR` selects which llama.cpp build is downloaded into the image and which
GPU devices `docker-run` passes through. When unset, the **build host** is
probed: NVIDIA → `cuda`, AMD → `vulkan`, a Vulkan loader → `vulkan`, else `cpu`.
Because the libraries are baked at build time, set the same `PROCESSOR` on
`docker-build` and `docker-run` (and build *on*, or for, the target GPU host).

```shell
# AMD (e.g. Radeon 7900XTX) — Vulkan: works on the slim base, no ROCm stack.
make docker-build PROCESSOR=vulkan
make docker-run   PROCESSOR=vulkan NSEQ=4

# AMD via the ROCm stack (auto-selects a ROCm runtime base image).
make docker-build PROCESSOR=rocm
make docker-run   PROCESSOR=rocm NSEQ=2

# NVIDIA (requires the NVIDIA Container Toolkit on the host).
make docker-build PROCESSOR=cuda
make docker-run   PROCESSOR=cuda
```

The model offloads all layers to the GPU automatically; no model is too small to
run on CPU instead (`PROCESSOR=cpu`). `NSEQ` is the concurrency knob — each slot
pre-allocates its own KV-cache partition, so it is bounded by VRAM (≈1–2 for a
large model in 24 GB, 4–8 for a small one). The host must expose its GPU: AMD
needs `/dev/dri` (and `/dev/kfd` for ROCm) with the `video`/`render` groups —
the `docker-run` targets add these flags for you. On some hosts the in-container
`render` group GID differs; if you hit a permissions error, pass the host's GID
explicitly, e.g. `--group-add $(getent group render | cut -d: -f3)`.

## Query

```shell
make query                              # a built-in sample batch
make query URL='/api/v2/users/3'        # your own URL
```

Or with curl directly:

```shell
# single URL
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"url":"/api/v2/users/3"}'

# batch URLs
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"urls":["/users/7/","/orgs/acme/projects/12/builds/9f3a","/users/me"]}'
```

`GET /healthz` returns `ok`.

## Evaluate

[eval.json](eval.json) is a labeled dataset (`[{"url": ..., "pattern": ...}]`) of diverse,
multilingual URLs with hard-to-classify static/dynamic segments. `make eval`
sends them to a running server, checks each result against the expected
pattern, and prints a per-URL PASS/FAIL/ERROR with timing plus a recap.

```shell
make eval              # single mode: one request per URL, each timed
make eval MODE=batch   # batch mode: all URLs in one request, timed as a whole
make eval PORT=9000    # target a non-default port
```

`single` measures per-URL latency; `batch` exercises the in-server concurrency.
Pattern comparison ignores a leading slash, so expected patterns may be written
with or without it.

## Benchmark

Head-to-head on the 68-case [eval.json](eval.json) set (CPU, `NSEQ=4`), via `make eval`
in both modes:

| Model (`MODEL=`)                     | Single | Batch | Single avg | Batch wall |
| ------------------------------------ | ------ | ----- | ---------- | ---------- |
| `unsloth/Qwen3-1.7B-Q4_K_M`          | 67/68  | 67/68 | 412 ms     | 21.8 s     |
| `LiquidAI/LFM2-1.2B-Q4_K_M`          | 59/68  | 60/68 | 245 ms     | 15.3 s     |
| `unsloth/gemma-3-1b-it-Q4_K_M`       | 59/68  | 59/68 | 347 ms     | 20.3 s     |
| `unsloth/Qwen3.6-35B-A3B-UD-Q2_K_XL` | 66/68  | 66/68 | 1165 ms    | 56.0 s     |

`Single`/`Batch` are correct cases out of 68; `Qwen3-1.7B-Q4_K_M` is the current
default. Latency is per *model call*; most URLs are settled by the Go rules with
no call at all (see [Configuration](#configure) to switch `MODEL`).

Scores have ±2 of run-to-run jitter: the model is not fully deterministic
(concurrent decoding + KV-cache reuse), so a couple of borderline judgments
(e.g. is `checkout` an action or a value?) flip between runs.

Takeaways:

- **Qwen3-1.7B-Q4 is the accuracy winner (67/68).** Its one miss is a deep
  `/collection/{TypeName}/{opaqueId}` case (a known limitation of the
  "word-after-id stays static" rule, not the model).
- **LFM2-1.2B is the speed winner (~1.7× faster)** but ~59/68: it misses keyword
  demotions (`active`, `stripe`, `api`) and occasionally drops a real id
  (`/utilisateur/marie`). A good fallback when throughput matters more than the
  last ~12% of accuracy.
- **gemma-3-1b** is slower than LFM2 and no more accurate here.
- **Qwen3.6-35B-A3B (2-bit Q2_K_XL)** does not pay off on this task: ~2.6–2.9×
  slower on CPU and slightly *less* accurate (66/68). The LLM only adjudicates a
  few ambiguous segments, so the extra capacity has little to bite on, and the
  aggressive 2-bit quant plus a Qwen3-tuned prompt erase any edge. A GPU makes it
  fast but not more accurate.
- The few-shot prompt is tuned for Qwen; those gains do **not** fully transfer to
  the smaller models, which is part of why they score lower.
