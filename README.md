# url-detect

Extracts a low-cardinality **pattern** from a URL path using a local LLM via
[Kronk](https://github.com/ardanlabs/kronk). Dynamic, high-cardinality segments
(numeric IDs, UUIDs, hashes, usernames, id-bearing slugs) are replaced with
named placeholders so the result is stable enough to use as a label for
metrics, spans, etc.

```
/users/7/                              -> /users/{userId}/
/users/7#profile                       -> /users/{userId}
/api/v2/users/3                        -> /api/v2/users/{userId}
/files/order-9f3a8821                  -> /files/{fileId}
/orders/42?limit=10&name=foo           -> /orders/{orderId}
/users/john/sessions/a1b2c3d4e5f6a1b2  -> /users/{userId}/sessions/{sessionId}
/orgs/acme/projects/12/builds/9f3a     -> /orgs/{orgId}/projects/{projectId}/builds/{buildId}
```

The query string (`?...`) and fragment (`#...`) are dropped entirely — they are
high-cardinality and not part of the route pattern. The path's trailing slash is
preserved.

## Design: Go owns structure, the LLM only classifies

A small local model can't reliably do byte-exact string surgery, so it doesn't
have to. The work is split:

**Go (deterministic, in `pattern.go`):**
- Parses the URL, preserving the leading/trailing slash **byte-for-byte** and
  splitting the path into segments. The `?query` and `#fragment` are split off
  and discarded.
- A heuristic confidently flags obvious identifiers: numbers, UUIDs, hashes,
  short hex tokens (`9f3a`), and id-bearing slugs (`order-9f3a8821`).
- It also applies the REST convention: the segment right after a plural
  collection noun is that collection's identifier, anywhere in the path
  (`users/john` → `john`, `api/v2/users/john` → `john`). A stop-list keeps
  sub-resources and actions static (`users/settings`, `users/me`,
  `users/sessions`). This catches human-readable IDs deterministically, since
  small models are unreliable at it.
- Names every placeholder from the nearest preceding static segment
  (`users` → `{userId}`, `sessions` → `{sessionId}`), with a counter to keep
  consecutive placeholders unique. **The model never produces names**, so it
  can't emit a high-cardinality name like `{a1b2c3...}`.
- Reconstructs the final pattern from the segments + preserved structure.

**LLM (per-segment classification only):**
- Receives the segments plus the heuristic guesses and returns a single
  boolean per segment: dynamic or not.
- It is a best-effort refinement on top of the heuristic — catching identifiers
  that don't follow a plural collection (e.g. after a singular resource like
  `/user/john`) or other shapes the Go rules miss.
- Runs with `enable_thinking: false`, `temperature: 0`, a `json_schema`
  constraining output to `{"dynamic":[...]}`, and few-shot examples.

**Guardrail + fallback:**
- The model output is validated (exactly one boolean per segment); on failure
  it retries up to `maxAttempts`, then falls back to the pure-Go heuristic.
- Results are combined as `heuristic OR model`: the model can *promote* a
  segment to dynamic but can never *demote* one the heuristic is sure about, so
  a weak model can't drop an obvious ID.

The model is `unsloth/Qwen3-1.7B-Q8_0` — cheap and fast, and reliable here
because Go carries the structural load. The 0.6B model works structurally but
misses human-readable identifiers; set `MODEL` to trade speed for quality.

## Configuration

All settings are environment variables (also exposed as Makefile / Docker
variables):

| Var     | Default                     | Meaning                                   |
| ------- | --------------------------- | ----------------------------------------- |
| `MODEL` | `unsloth/Qwen3-1.7B-Q8_0`   | GGUF model id Kronk downloads and serves  |
| `PORT`  | `8080`                      | HTTP listen port                          |
| `NSEQ`  | `4`                         | Max concurrent extractions (parallelism)  |
| `URL_DETECT_DEBUG` | `1`              | Per-request resolution trace (`0` to mute)|

## Run locally

### Prerequisites

- **Go 1.26+** (`go version`).
- **Internet access on first run** — Kronk downloads the llama.cpp libraries
  (~40 MB) and the model (~1.8 GB for the default 1.7B) into `~/.kronk`, then
  reuses them. Allow a few GB of free disk.
- **The `kronk` source as a sibling checkout.** `go.mod` contains
  `replace github.com/ardanlabs/kronk => ../kronk`, so the build expects the
  repo one directory up. Either provide it:

  ```shell
  # from the parent directory that contains this repo
  git clone https://github.com/ardanlabs/kronk ../kronk
  ```

  …or, if you prefer the published module instead of a local checkout, drop the
  replace (this is exactly what the Docker build does):

  ```shell
  go mod edit -dropreplace=github.com/ardanlabs/kronk
  go mod tidy
  ```

  No cgo toolchain is needed — the binary is built with `CGO_ENABLED=0` and
  loads llama.cpp via `purego`.

### Steps

```shell
# 1. (optional) run the unit tests — these need neither the model nor kronk libs
go test ./...

# 2. start the server (first run downloads libs+model; later runs are instant)
go run .
#    └─ or: make run
#    └─ with overrides: MODEL=unsloth/Qwen3-0.6B-Q8_0 PORT=9000 NSEQ=8 go run .
```

The model and llama.cpp libraries are loaded **once** at startup, then an HTTP
server is exposed on `:8080` (override with `PORT`). Wait for the log line:

```
listening on :8080 — POST /patterns (max 4 concurrent)
```

Then query it (see [HTTP API](#http-api) below, or `make query`) and stop it
with `Ctrl-C` (the model is unloaded gracefully on shutdown).

To run inside a container instead — with the libraries and model already baked
in so there is no first-run download — see [Docker](#docker).

## Docker

The image downloads the libraries and the model **at build time** (the `warmup`
step), so a started container serves immediately with no runtime download.

```shell
# Build (bakes in MODEL). Configure anything via Make variables:
make docker-build                                   # default 1.7B model
make docker-build MODEL=unsloth/Qwen3-0.6B-Q8_0     # smaller/faster

# Run (override PORT / NSEQ / MODEL as needed):
make docker-run PORT=8080 NSEQ=4
```

Equivalent raw Docker commands:

```shell
docker build --build-arg MODEL=unsloth/Qwen3-1.7B-Q8_0 -t url-detect:latest .

docker run --rm -p 8080:8080 \
  -e MODEL=unsloth/Qwen3-1.7B-Q8_0 -e PORT=8080 -e NSEQ=4 \
  url-detect:latest
```

The build context is this directory only; the local `replace` pointing at a
sibling `kronk` checkout is dropped so the published module is used. The image
is `linux/amd64` (CPU llama.cpp libraries).

## HTTP API

`POST /patterns` accepts a single URL via `"url"` and/or a batch via `"urls"`,
and returns the extracted pattern for each. `GET /healthz` returns `ok`.

Batch:

```shell
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{
    "urls": [
      "/users/7/",
      "/users/7#profile",
      "/api/v2/users/3",
      "/files/order-9f3a8821",
      "/orders/42?limit=10&name=foo",
      "/api/v2/users/john/sessions/a1b2c3d4e5f6a1b2?limit=10",
      "/orgs/acme/projects/12/builds/9f3a",
      "/users/settings",
      "/users/me"
    ]
  }'
```

Response:

```json
{
  "results": [
    {"url": "/users/7/", "pattern": "/users/{userId}/"},
    {"url": "/users/7#profile", "pattern": "/users/{userId}"},
    {"url": "/api/v2/users/3", "pattern": "/api/v2/users/{userId}"},
    {"url": "/files/order-9f3a8821", "pattern": "/files/{fileId}"},
    {"url": "/orders/42?limit=10&name=foo", "pattern": "/orders/{orderId}"},
    {"url": "/api/v2/users/john/sessions/a1b2c3d4e5f6a1b2?limit=10", "pattern": "/api/v2/users/{userId}/sessions/{sessionId}"},
    {"url": "/orgs/acme/projects/12/builds/9f3a", "pattern": "/orgs/{orgId}/projects/{projectId}/builds/{buildId}"},
    {"url": "/users/settings", "pattern": "/users/settings"},
    {"url": "/users/me", "pattern": "/users/me"}
  ]
}
```

Single URL:

```shell
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"url":"/api/v2/users/3"}'
```

### Concurrency

The single loaded model is created with `NSeqMax` parallel sequences, so the
server handles concurrent requests in parallel rather than serializing them.
A semaphore bounds in-flight extractions to `NSeqMax`; extra requests wait for a
free slot. URLs within a single batch are also processed in parallel under the
same limit, and results keep their input order. A per-URL error (if any) is
reported in that URL's result entry rather than failing the whole batch.

Set the parallelism with `NSEQ` (default 4); the context window scales with it:

```shell
NSEQ=8 PORT=9000 go run .
```

Note: on CPU the parallel sequences share compute cores, so concurrency
improves throughput modestly; on GPU it scales further. The code is verified
data-race-free under `go run -race`.

A step-by-step resolution trace (model used, parse, heuristic vs. model
classification, the combine, and final tokens) prints by default. Disable it
with `URL_DETECT_DEBUG=0`:

```
--- resolution ---
model     : unsloth/Qwen3-1.7B-Q8_0
url       : /orgs/acme/projects/12/builds/9f3a
parse     : segments=[orgs acme projects 12 builds 9f3a] leading=true trailing=false stripped=""
heuristic : orgs=static acme=static projects=static 12=DYN builds=static 9f3a=DYN
model     : orgs=static acme=DYN projects=static 12=DYN builds=static 9f3a=DYN (attempt 1)
combined  : orgs=static acme=DYN projects=static 12=DYN builds=static 9f3a=DYN [heuristic OR model]
tokens    : [orgs {orgId} projects {projectId} builds {buildId}]
pattern   : /orgs/{orgId}/projects/{projectId}/builds/{buildId}
------------------
```

## Test

The deterministic core (parsing, heuristic, naming, guardrail) has unit tests
that run without the model:

```shell
go test ./...
```
