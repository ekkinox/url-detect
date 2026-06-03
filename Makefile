# url-detect — build and run the URL pattern extraction service.
#
# Override any variable on the command line, e.g.:
#   make docker-build MODEL=unsloth/Qwen3-0.6B-Q8_0
#   make docker-run PORT=9000 NSEQ=8

# ----- Configuration --------------------------------------------------------

# Model to bake into the image and serve (any GGUF model id Kronk can fetch).
MODEL     ?= unsloth/Qwen3-1.7B-Q4_K_M
# Host/container port the HTTP server listens on.
PORT      ?= 8080
# Max concurrent in-flight extractions (llama.cpp parallel sequences).
NSEQ      ?= 4
# eval mode: "single" (one request per URL, timed each) or "batch" (all at once).
MODE      ?= single
# URL to send with `make query`; empty uses a built-in sample batch.
URL       ?=

# Which llama.cpp build to bake and run: cpu | vulkan | rocm | cuda. Baked at
# build time (the matching libs are downloaded into the image), so pass the same
# value to docker-build and docker-run. When unset, it is auto-detected from the
# build host: NVIDIA -> cuda, AMD -> vulkan (works on the slim base; use
# PROCESSOR=rocm explicitly for the ROCm stack), a Vulkan loader -> vulkan, else
# cpu.
PROCESSOR ?= $(shell \
	if command -v nvidia-smi >/dev/null 2>&1; then echo cuda; \
	elif command -v rocminfo >/dev/null 2>&1 || [ -e /dev/kfd ]; then echo vulkan; \
	elif command -v vulkaninfo >/dev/null 2>&1; then echo vulkan; \
	else echo cpu; fi)

# Image / container naming.
IMAGE     ?= url-detect:latest
CONTAINER ?= url-detect

# Base images used by the Dockerfile. ROCm needs its runtime libraries in the
# image, so default the runtime base to a ROCm image for that processor.
GO_IMAGE      ?= golang:1.26-bookworm
ifeq ($(PROCESSOR),rocm)
RUNTIME_IMAGE ?= rocm/dev-ubuntu-24.04:6.2-complete
else
RUNTIME_IMAGE ?= debian:bookworm-slim
endif

# GPU passthrough flags for `docker run`, by processor.
ifeq ($(PROCESSOR),cuda)
GPU_FLAGS = --gpus all
else ifeq ($(PROCESSOR),rocm)
GPU_FLAGS = --device /dev/kfd --device /dev/dri --group-add video --group-add render --security-opt seccomp=unconfined
else ifeq ($(PROCESSOR),vulkan)
GPU_FLAGS = --device /dev/dri --group-add video --group-add render
else
GPU_FLAGS =
endif

.DEFAULT_GOAL := help

# ----- Local (no Docker) ----------------------------------------------------

.PHONY: build
build: ## Build the binary locally
	go build -o url-detect .

.PHONY: test
test: ## Run the unit tests
	go test ./...

.PHONY: run
run: ## Run locally (downloads libs+model on first use)
	MODEL=$(MODEL) PORT=$(PORT) NSEQ=$(NSEQ) go run .

# ----- Docker ---------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the image (downloads libs+model into the image)
	docker build \
		--build-arg MODEL=$(MODEL) \
		--build-arg PROCESSOR=$(PROCESSOR) \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		-t $(IMAGE) .

.PHONY: docker-run
docker-run: ## Run the container (ready to serve immediately; GPU flags per PROCESSOR)
	docker run --rm \
		--name $(CONTAINER) \
		$(GPU_FLAGS) \
		-p $(PORT):$(PORT) \
		-e MODEL=$(MODEL) \
		-e PORT=$(PORT) \
		-e NSEQ=$(NSEQ) \
		-e KRONK_PROCESSOR=$(PROCESSOR) \
		$(IMAGE)

.PHONY: docker-run-detached
docker-run-detached: ## Run the container in the background
	docker run -d --rm \
		--name $(CONTAINER) \
		$(GPU_FLAGS) \
		-p $(PORT):$(PORT) \
		-e MODEL=$(MODEL) \
		-e PORT=$(PORT) \
		-e NSEQ=$(NSEQ) \
		-e KRONK_PROCESSOR=$(PROCESSOR) \
		$(IMAGE)

.PHONY: docker-logs
docker-logs: ## Tail the container logs
	docker logs -f $(CONTAINER)

.PHONY: docker-stop
docker-stop: ## Stop the running container
	-docker stop $(CONTAINER)

.PHONY: query
query: ## Query the server. Pass URL='/your/url' for one URL, else a sample batch
	@if [ -n "$(URL)" ]; then \
		curl -s -X POST http://localhost:$(PORT)/patterns \
			-H 'Content-Type: application/json' \
			-d "{\"url\":\"$(URL)\"}"; \
	else \
		curl -s -X POST http://localhost:$(PORT)/patterns \
			-H 'Content-Type: application/json' \
			-d '{"urls":["/users/7/","/api/v2/users/john/sessions/a1b2c3d4e5f6a1b2?limit=10","/orgs/acme/projects/12/builds/9f3a","/users/me"]}'; \
	fi
	@echo

.PHONY: eval
eval: ## Evaluate eval.json against the server (MODE=single|batch; correctness + timing recap)
	@python3 eval.py --url http://localhost:$(PORT)/patterns --file eval.json --mode $(MODE)

# ----- Help -----------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Config: MODEL=$(MODEL) PROCESSOR=$(PROCESSOR) PORT=$(PORT) NSEQ=$(NSEQ) IMAGE=$(IMAGE)"
