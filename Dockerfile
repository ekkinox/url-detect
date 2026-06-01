# syntax=docker/dockerfile:1
#
# Builds a self-contained image: the llama.cpp libraries and the model are
# downloaded during the build (the "warmup" step), so a started container is
# ready to serve immediately with no runtime downloads.
#
# Build context is this directory only; the local "replace" pointing at a
# sibling kronk checkout is dropped so the published module is used instead.

ARG GO_IMAGE=golang:1.26-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
ARG MODEL=unsloth/Qwen3-1.7B-Q8_0

# -----------------------------------------------------------------------------
# Builder: compile the binary and warm the ~/.kronk cache (libs + model).

FROM ${GO_IMAGE} AS builder
ARG MODEL
ENV HOME=/root
ENV GOFLAGS=-mod=mod
WORKDIR /src

# Resolve dependencies against the published kronk module (no local replace).
COPY go.mod go.sum ./
RUN go mod edit -dropreplace=github.com/ardanlabs/kronk && go mod download

# Build the static (cgo-free) binary.
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/url-detect .

# Download the llama.cpp libraries and the model into /root/.kronk.
ENV MODEL=${MODEL}
RUN url-detect warmup

# -----------------------------------------------------------------------------
# Runtime: minimal image with just the shared libs the llama.cpp .so files need.

FROM ${RUNTIME_IMAGE}
ARG MODEL
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 libstdc++6 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV HOME=/root
COPY --from=builder /usr/local/bin/url-detect /usr/local/bin/url-detect
COPY --from=builder /root/.kronk /root/.kronk

# Runtime configuration (all overridable with `docker run -e`).
ENV MODEL=${MODEL}
ENV PORT=8080
ENV NSEQ=4

EXPOSE 8080
ENTRYPOINT ["url-detect"]
