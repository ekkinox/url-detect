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

# Image / container naming.
IMAGE     ?= url-detect:latest
CONTAINER ?= url-detect

# Base images used by the Dockerfile.
GO_IMAGE      ?= golang:1.26-bookworm
RUNTIME_IMAGE ?= debian:bookworm-slim

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
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		-t $(IMAGE) .

.PHONY: docker-run
docker-run: ## Run the container (ready to serve immediately)
	docker run --rm \
		--name $(CONTAINER) \
		-p $(PORT):$(PORT) \
		-e MODEL=$(MODEL) \
		-e PORT=$(PORT) \
		-e NSEQ=$(NSEQ) \
		$(IMAGE)

.PHONY: docker-run-detached
docker-run-detached: ## Run the container in the background
	docker run -d --rm \
		--name $(CONTAINER) \
		-p $(PORT):$(PORT) \
		-e MODEL=$(MODEL) \
		-e PORT=$(PORT) \
		-e NSEQ=$(NSEQ) \
		$(IMAGE)

.PHONY: docker-logs
docker-logs: ## Tail the container logs
	docker logs -f $(CONTAINER)

.PHONY: docker-stop
docker-stop: ## Stop the running container
	-docker stop $(CONTAINER)

.PHONY: query
query: ## Send a sample batch request to the running server
	curl -s -X POST http://localhost:$(PORT)/patterns \
		-H 'Content-Type: application/json' \
		-d '{"urls":["/users/7/","/api/v2/users/john/sessions/a1b2c3d4e5f6a1b2?limit=10","/orgs/acme/projects/12/builds/9f3a","/users/me"]}'
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
	@echo "Config: MODEL=$(MODEL) PORT=$(PORT) NSEQ=$(NSEQ) IMAGE=$(IMAGE)"
