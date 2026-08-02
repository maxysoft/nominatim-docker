# Every target runs inside a container: nothing is installed on the host.
#
# Dependency and compiler caches are named volumes, otherwise each run
# recompiles the world.

GO_IMAGE   ?= golang:1.24-bookworm
BASE_IMAGE ?= debian:13.4-slim
UID        := $(shell id -u)
GID        := $(shell id -g)
PWD        := $(shell pwd)

GO_RUN = docker run --rm -u $(UID):$(GID) \
	-v $(PWD):/src -w /src \
	-v nominatim-gocache-mod:/gocache/mod \
	-v nominatim-gocache-build:/gocache/build \
	-e GOMODCACHE=/gocache/mod -e GOCACHE=/gocache/build -e GOFLAGS=-mod=mod \
	$(GO_IMAGE)

.PHONY: help tidy fmt vet test lint build requirements integration check clean

help:
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | column -t -s "$$(printf '\t')"

tidy: ## Resolve module dependencies and write go.sum
	$(GO_RUN) go mod tidy

fmt: ## Format Go sources
	$(GO_RUN) gofmt -l -w ./cmd ./internal

vet: ## Run go vet
	$(GO_RUN) go vet ./...

test: ## Run Go unit tests
	$(GO_RUN) go test -count=1 ./...

lint: ## Shell and Dockerfile linting
	docker run --rm -v $(PWD):/mnt -w /mnt koalaman/shellcheck:stable test/integration.sh   # default severity, same as CI
	docker run --rm -i hadolint/hadolint < Dockerfile

build: ## Build the container image
	DOCKER_BUILDKIT=1 docker build -t nominatim-docker:dev .

requirements: ## Regenerate requirements.txt with pinned versions and hashes
	# Resolved on the same base the image builds on, so the pins match reality.
	# pyicu is excluded: it has no wheel and is supplied by Debian's python3-icu.
	docker run --rm -v $(PWD):/src -w /src -e DEBIAN_FRONTEND=noninteractive $(BASE_IMAGE) sh -c '\
		apt-get -qq update && \
		apt-get -qq install -y --no-install-recommends \
			python3 python3-pip ca-certificates >/dev/null && \
		pip install --quiet --break-system-packages uv && \
		uv pip compile --generate-hashes --no-header --no-emit-package pyicu \
			--output-file requirements.txt requirements.in && \
		chown $(UID):$(GID) requirements.txt'

integration: ## Full local integration test (builds the image, imports Monaco)
	./test/integration.sh

check: vet test ## Fast pre-commit gate

clean:
	docker volume rm -f nominatim-gocache-mod nominatim-gocache-build
