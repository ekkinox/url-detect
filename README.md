# url-detect

Extracts a low-cardinality **pattern** from a URL using a local LLM (via
[Kronk](https://github.com/ardanlabs/kronk)). Dynamic segments (ids, UUIDs,
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

## Run it

Everything goes through the Makefile. `make` on its own lists the targets.

```shell
# 1. Build the image (downloads llama.cpp + the model into the image,
#    so the container starts ready to serve — no runtime download).
make docker-build

# 2. Run it (serves on :8080).
make docker-run

# 3. In another terminal, send a sample request.
make query
```

Stop it with `Ctrl-C`, or `make docker-stop` if you started it detached
(`make docker-run-detached`).

## Configure

Override any of these on the `make` command line (defaults shown):

| Variable | Default                   | Meaning                                  |
| -------- | ------------------------- | ---------------------------------------- |
| `MODEL`  | `unsloth/Qwen3-1.7B-Q8_0` | GGUF model to bake in and serve          |
| `PORT`   | `8080`                    | HTTP port                                |
| `NSEQ`   | `4`                       | Max concurrent model calls (parallelism) |

`MODEL` is baked in at build time, so pass it to **both** build and run:

```shell
make docker-build MODEL=unsloth/Qwen3-0.6B-Q8_0
make docker-run   MODEL=unsloth/Qwen3-0.6B-Q8_0 PORT=9000 NSEQ=8
```

## Query

```shell
# single URL
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"url":"/api/v2/users/3"}'

# batch
curl -s -X POST http://localhost:8080/patterns \
  -H 'Content-Type: application/json' \
  -d '{"urls":["/users/7/","/orgs/acme/projects/12/builds/9f3a","/users/me"]}'
```

`GET /healthz` returns `ok`.

## Run without Docker

```shell
make run     # serves on :8080; first run downloads llama.cpp + the model
make test    # unit tests (no model needed)
```

This needs Go 1.26+ and the `kronk` repo checked out alongside this one
(`../kronk`, referenced by a `replace` in `go.mod`).
