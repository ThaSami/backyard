# Backyard developer tasks. `make` (or `make help`) lists every target.
#
# Common flows:
#   make dev                    run a local server at http://localhost:8080 (no auth)
#   make test                   go test -race ./... (integration tests skip without services)
#   make services-up test       ... with postgres, redis, dynamodb-local and azurite
#   make lint sdk-test          what CI checks besides the Go tests
#   make cloudflare-setup       deploy the Workers edition to your Cloudflare account (free plan)

SHELL         := /bin/sh
.DEFAULT_GOAL := help

GO            ?= go
NODE          ?= node
NPM           ?= npm
DOCKER        ?= docker
COMPOSE       ?= $(DOCKER) compose
TERRAFORM     ?= terraform
HELM          ?= helm
GOLANGCI_LINT ?= golangci-lint
GORELEASER    ?= goreleaser

MODULE   := github.com/thasami/backyard
BIN_DIR  := bin
DIST_DIR := dist
EXE      := $(shell $(GO) env GOEXE 2>/dev/null)

# Version stamping (override on the command line: make build VERSION=1.2.3).
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
VERSION := $(VERSION)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
COMMIT  := $(COMMIT)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATE    := $(DATE)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Go tests.
TEST_PKGS  ?= ./...
TEST_FLAGS ?= -race -count=1 -timeout=20m

# Contributor test services (deploy/docker-compose/test-services.yml). `make
# test` exports these when the services are running and no BACKYARD_TEST_*
# variable is set already.
TEST_SERVICES          := deploy/docker-compose/test-services.yml
TEST_POSTGRES_DSN      := postgres://backyard:backyard@127.0.0.1:5432/backyard?sslmode=disable
TEST_REDIS_URL         := redis://127.0.0.1:6379/0
TEST_DYNAMODB_ENDPOINT := http://127.0.0.1:8000
TEST_AZURITE           := DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;

# Container image. DOCKER_BUILD_FLAGS adds flags, e.g. base images from a
# mirror: --build-arg GO_IMAGE=mirror.example.com/golang:1.26
IMAGE              ?= ghcr.io/thasami/backyard
IMAGE_TAG          ?= dev
PLATFORMS          ?= linux/amd64,linux/arm64
DOCKER_BUILD_FLAGS ?=

# Lambda deploy gateway (deploy/terraform/aws expects dist/backyard-lambda.zip, arm64).
LAMBDA_ARCH ?= arm64

# Infrastructure.
TF_DIRS    ?= $(patsubst %/,%,$(sort $(dir $(wildcard deploy/terraform/*/versions.tf deploy/terraform/*/main.tf))))
HELM_CHART := deploy/helm/backyard
CF_DIR     := deploy/cloudflare
# `make cloudflare-setup CF_SETUP_ARGS="--r2 --var URL_MODE:host --var BASE_DOMAIN:example.com"`
CF_SETUP_ARGS ?=
# dev:local runs offline (no Workers AI); dev uses your Cloudflare login for Workers AI.
CF_DEV_SCRIPT ?= dev:local

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@/ { printf "\n%s\n", substr($$0, 5); next } \
		/^[a-zA-Z0-9_.-]+:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

##@ Build

.PHONY: build
build: ## Build bin/backyard for this machine
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/backyard$(EXE) ./cmd/backyard

.PHONY: install
install: ## Install backyard into $GOBIN (or $GOPATH/bin)
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/backyard

.PHONY: lambda
lambda: ## Build the AWS Lambda deploy gateway: dist/backyard-lambda.zip (LAMBDA_ARCH=arm64)
	@mkdir -p $(DIST_DIR)/lambda
	CGO_ENABLED=0 GOOS=linux GOARCH=$(LAMBDA_ARCH) $(GO) build -trimpath -tags lambda.norpc \
		-ldflags "$(LDFLAGS)" -o $(DIST_DIR)/lambda/bootstrap ./cmd/backyard-lambda
	@rm -f $(DIST_DIR)/backyard-lambda.zip
	@if command -v zip >/dev/null 2>&1; then \
		chmod 0755 $(DIST_DIR)/lambda/bootstrap && \
		(cd $(DIST_DIR)/lambda && zip -q -X ../backyard-lambda.zip bootstrap); \
	else \
		$(GO) run ./scripts/mkzip -o $(DIST_DIR)/backyard-lambda.zip -mode 0755 $(DIST_DIR)/lambda/bootstrap; \
	fi
	@echo ">> $(DIST_DIR)/backyard-lambda.zip (linux/$(LAMBDA_ARCH))"

.PHONY: docker
docker: ## Build the container image for this machine (IMAGE, IMAGE_TAG)
	$(DOCKER) build $(DOCKER_BUILD_FLAGS) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) -t $(IMAGE):$(IMAGE_TAG) .

.PHONY: docker-multiarch
docker-multiarch: ## Build the image for PLATFORMS with buildx (no push)
	$(DOCKER) buildx build $(DOCKER_BUILD_FLAGS) --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG) .

.PHONY: release-snapshot
release-snapshot: ## Build all release artifacts locally with GoReleaser (no publish)
	@command -v $(GORELEASER) >/dev/null 2>&1 || { echo "goreleaser not found: https://goreleaser.com/install/"; exit 1; }
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean --skip=publish

##@ Develop

.PHONY: dev
dev: ## Run a local dev server (sqlite + files in ./.backyard-data, no auth)
	$(GO) run ./cmd/backyard serve --dev

.PHONY: sdk
sdk: ## Regenerate web/sdk/backyard.{js,mjs} from web/sdk/src
	$(NODE) web/sdk/build.mjs

.PHONY: fmt
fmt: ## Format Go code (gofmt -w)
	@gofmt -w $(GO_FILES)

##@ Test

GO_FILES = $(shell find . -name '*.go' -not -path './.git/*' -not -path '*/node_modules/*' -not -path '*/.terraform/*' -not -path './dist/*')

.PHONY: test
test: ## Run all Go tests with -race (uses `make services-up` services when running)
	@set -e; \
	if [ -z "$${BACKYARD_TEST_POSTGRES_DSN:-}$${BACKYARD_TEST_REDIS_URL:-}$${BACKYARD_TEST_DYNAMODB_ENDPOINT:-}$${BACKYARD_TEST_AZURITE:-}" ] && \
		$(COMPOSE) -f $(TEST_SERVICES) ps --status running --quiet 2>/dev/null | grep -q .; then \
		echo ">> using the test services from $(TEST_SERVICES)"; \
		BACKYARD_TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)'; export BACKYARD_TEST_POSTGRES_DSN; \
		BACKYARD_TEST_REDIS_URL='$(TEST_REDIS_URL)'; export BACKYARD_TEST_REDIS_URL; \
		BACKYARD_TEST_DYNAMODB_ENDPOINT='$(TEST_DYNAMODB_ENDPOINT)'; export BACKYARD_TEST_DYNAMODB_ENDPOINT; \
		BACKYARD_TEST_AZURITE='$(TEST_AZURITE)'; export BACKYARD_TEST_AZURITE; \
	fi; \
	echo "$(GO) test $(TEST_FLAGS) $(TEST_PKGS)"; \
	$(GO) test $(TEST_FLAGS) $(TEST_PKGS)

.PHONY: test-short
test-short: ## Run unit tests only (go test -short, no race detector)
	$(GO) test -short -count=1 $(TEST_PKGS)

.PHONY: cover
cover: ## Run Go tests with coverage → coverage.out / coverage.html
	$(GO) test -count=1 -covermode=atomic -coverprofile=coverage.out $(TEST_PKGS)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: sdk-test
sdk-test: ## Check the SDK build is in sync and run the SDK + hub tests (node --test)
	$(NODE) web/sdk/build.mjs --check
	$(NODE) --test 'web/sdk/test/*.test.mjs' 'web/test/*.test.mjs'

.PHONY: scripts-test
scripts-test: ## Test the release/CI tooling in scripts/ (Go and node --test)
	$(GO) test -count=1 ./scripts/...
	$(NODE) --test 'scripts/*.test.mjs'

CONFORMANCE_TARGET ?= go

.PHONY: conformance
conformance: ## Run the API conformance suite (CONFORMANCE_TARGET=go: serve --dev, worker: wrangler dev --env local)
	@if [ ! -d conformance ]; then echo "conformance/ does not exist: nothing to run"; exit 0; fi; \
	./scripts/conformance.sh $(CONFORMANCE_TARGET)

# E2E_TARGET: go (backyard serve --dev) or worker (wrangler dev --env local).
E2E_TARGET ?= go

.PHONY: e2e
e2e: ## Browser end-to-end test against a local server (E2E_TARGET=go|worker; needs playwright-core)
	@if [ -f e2e/package.json ]; then \
		cd e2e && $(NPM) ci && $(NPM) test; \
	else \
		if [ -z "$${PLAYWRIGHT_CORE:-}" ]; then echo "note: set PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs or the test skips (see web/test/browser.test.mjs)"; fi; \
		./scripts/with-server.sh $(E2E_TARGET) -- $(NODE) --test web/test/e2e.test.mjs; \
	fi

.PHONY: validate-configs
validate-configs: ## Validate the sample backyard.yaml files under deploy/ against the config schema
	$(GO) run ./scripts/checkconfig deploy/docker-compose/backyard.yaml deploy/systemd/backyard.yaml
	@if command -v $(HELM) >/dev/null 2>&1; then \
		for values in "" "-f $(HELM_CHART)/values-production.yaml"; do \
			$(HELM) template backyard $(HELM_CHART) $$values --show-only templates/configmap.yaml \
				| $(GO) run ./scripts/checkconfig -configmap - || exit 1; \
		done; \
	else echo "helm not found: skipping the Helm chart config"; fi

.PHONY: services-up
services-up: ## Start postgres, redis, dynamodb-local and azurite for integration tests
	$(COMPOSE) -f $(TEST_SERVICES) up -d --wait

.PHONY: services-down
services-down: ## Stop the test services and delete their data
	$(COMPOSE) -f $(TEST_SERVICES) down -v --remove-orphans

##@ Lint

.PHONY: lint
lint: ## gofmt check, go vet and golangci-lint (when installed)
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed (run make fmt):"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		echo "$(GOLANGCI_LINT) run"; $(GOLANGCI_LINT) run; \
	else \
		echo "golangci-lint not installed: skipping (https://golangci-lint.run/welcome/install/)"; \
	fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

##@ Deploy kits

.PHONY: tf-validate
tf-validate: ## terraform fmt -check, init -backend=false and validate for deploy/terraform/*
	@if [ -z "$(TF_DIRS)" ]; then echo "no Terraform modules found"; exit 0; fi; \
	set -e; for dir in $(TF_DIRS); do \
		echo ">> $$dir"; \
		$(TERRAFORM) -chdir=$$dir fmt -check -recursive; \
		$(TERRAFORM) -chdir=$$dir init -backend=false -input=false -no-color >/dev/null; \
		$(TERRAFORM) -chdir=$$dir validate -no-color; \
	done

.PHONY: helm-lint
helm-lint: ## helm lint + template the chart (default and production values)
	$(HELM) lint --strict $(HELM_CHART)
	$(HELM) lint --strict $(HELM_CHART) -f $(HELM_CHART)/values-production.yaml
	$(HELM) template backyard $(HELM_CHART) >/dev/null
	$(HELM) template backyard $(HELM_CHART) -f $(HELM_CHART)/values-production.yaml >/dev/null

.PHONY: compose-config
compose-config: ## Validate the docker-compose files (docker compose config)
	cd deploy/docker-compose && BACKYARD_SECRET=x POSTGRES_PASSWORD=x BACKYARD_DEPLOY_TOKEN_SHA256=x \
		$(COMPOSE) --env-file .env.example -f docker-compose.yml config --quiet
	$(COMPOSE) -f $(TEST_SERVICES) config --quiet

##@ Cloudflare (Workers edition, free plan)

.PHONY: cloudflare
cloudflare: ## Install, typecheck and test the Workers edition (deploy/cloudflare)
	@if [ ! -f $(CF_DIR)/package.json ]; then echo "$(CF_DIR) does not exist: nothing to test"; exit 0; fi; \
	cd $(CF_DIR) && $(NPM) ci && $(NPM) run typecheck && $(NPM) test

.PHONY: cloudflare-dev
cloudflare-dev: ## Run the Workers edition locally without a Cloudflare login (CF_DEV_SCRIPT=dev for Workers AI)
	cd $(CF_DIR) && $(NPM) ci && $(NPM) run $(CF_DEV_SCRIPT)

.PHONY: cloudflare-setup
cloudflare-setup: ## Deploy (or upgrade) the Workers edition on your Cloudflare account, free plan (CF_SETUP_ARGS)
	cd $(CF_DIR) && $(NPM) ci && $(NPM) run setup -- $(CF_SETUP_ARGS)

.PHONY: cloudflare-bundle
cloudflare-bundle: ## Bundle and validate the Worker like a CI deploy would (wrangler deploy --dry-run)
	cd $(CF_DIR) && $(NPM) ci
	$(NODE) scripts/cloudflare-ci.mjs deploy --dry-run

##@ Housekeeping

.PHONY: clean
clean: ## Remove build output (bin/, dist/, coverage files); keeps .backyard-data
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out coverage.html
