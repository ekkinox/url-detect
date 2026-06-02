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

| Variable | Default                     | Meaning                                  |
|----------|-----------------------------|------------------------------------------|
| `MODEL`  | `unsloth/Qwen3-1.7B-Q4_K_M` | GGUF model to bake in and serve          |
| `PORT`   | `8080`                      | HTTP port                                |
| `NSEQ`   | `4`                         | Max concurrent model calls (parallelism) |

`MODEL` is baked in at build time, so pass it to **both** build and run:

```shell
make docker-build MODEL=unsloth/Qwen3-0.6B-Q8_0
make docker-run   MODEL=unsloth/Qwen3-0.6B-Q8_0 PORT=9000 NSEQ=8
```

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

| Model (`MODEL=`)               | Single | Batch | Single avg | Batch wall |
| ------------------------------ | ------ | ----- | ---------- | ---------- |
| `unsloth/Qwen3-1.7B-Q4_K_M`    | 67/68  | 67/68 | 412 ms     | 21.8 s     |
| `LiquidAI/LFM2-1.2B-Q4_K_M`    | 59/68  | 60/68 | 245 ms     | 15.3 s     |
| `unsloth/gemma-3-1b-it-Q4_K_M` | 59/68  | 59/68 | 347 ms     | 20.3 s     |

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
- The few-shot prompt is tuned for Qwen; those gains do **not** fully transfer to
  the smaller models, which is part of why they score lower.
