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
# Which llama.cpp build to bake and run: cpu | vulkan | rocm | cuda. This is
# fixed at build time because the matching llama.cpp libraries are downloaded
# into the image during warmup. The Makefile auto-detects it from the build
# host when not set. For rocm/cuda also pass a matching RUNTIME_IMAGE.
ARG PROCESSOR=cpu

# -----------------------------------------------------------------------------
# Builder: compile the binary and warm the ~/.kronk cache (libs + model).

FROM ${GO_IMAGE} AS builder
ARG MODEL
ARG PROCESSOR
ENV HOME=/root
ENV GOFLAGS=-mod=mod
# Pin the llama.cpp build the warmup downloads, so the baked libs match the
# processor the container will run on (instead of auto-detecting the build host).
ENV KRONK_PROCESSOR=${PROCESSOR}
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
ARG PROCESSOR
# Base runtime deps, plus the Vulkan loader + AMD/Mesa ICD when building the
# vulkan variant (the rocm/cuda variants get their runtime from a matching
# RUNTIME_IMAGE base instead).
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 libstdc++6 ca-certificates \
    && if [ "$PROCESSOR" = "vulkan" ]; then \
         apt-get install -y --no-install-recommends libvulkan1 mesa-vulkan-drivers; \
       fi \
    && rm -rf /var/lib/apt/lists/*

ENV HOME=/root
COPY --from=builder /usr/local/bin/url-detect /usr/local/bin/url-detect
COPY --from=builder /root/.kronk /root/.kronk

# Runtime configuration (all overridable with `docker run -e`).
ENV MODEL=${MODEL}
ENV PORT=8080
ENV NSEQ=4
# Load the same llama.cpp build that was baked in at warmup.
ENV KRONK_PROCESSOR=${PROCESSOR}

EXPOSE 8080
ENTRYPOINT ["url-detect"]
