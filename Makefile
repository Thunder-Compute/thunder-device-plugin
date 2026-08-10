# Thunder Device Plugin
#
# Run `make help` for the full target list. Every variable below can be
# overridden on the command line, e.g. `make image IMAGE_TAG=v1.2.3`.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE := github.com/Thunder-Compute/thunder-device-plugin

# Version metadata, linked into both binaries and stamped onto the images.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Container images.
REGISTRY       ?= thundercompute
DAEMON_IMAGE   ?= $(REGISTRY)/thunder-device-plugin-daemon
OPERATOR_IMAGE ?= $(REGISTRY)/thunder-dra-operator
IMAGE_TAG      ?= $(VERSION)
PLATFORMS      ?= linux/amd64

# Helm.
CHART     := charts/thunder-device-plugin
RELEASE   ?= thunder-device-plugin
NAMESPACE ?= thunder-system

# Build.
BIN_DIR := bin
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT)

GO      ?= go
DOCKER  ?= docker
HELM    ?= helm
KUBECTL ?= kubectl

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nThunder Device Plugin \033[90m($(VERSION))\033[0m\n\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo

.PHONY: version
version: ## Print the version metadata used for builds
	@echo "version:  $(VERSION)"
	@echo "commit:   $(COMMIT)"
	@echo "images:   $(DAEMON_IMAGE):$(IMAGE_TAG), $(OPERATOR_IMAGE):$(IMAGE_TAG)"

##@ Development

.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format Go sources
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source needs formatting
	@unformatted="$$(gofmt -l ./cmd ./internal)"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "gofmt is clean"

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: fmt-check vet ## Run all static checks

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run unit tests and write coverage.out
	$(GO) test -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: build
build: build-daemon build-operator ## Build both binaries into bin/

.PHONY: build-daemon
build-daemon: ## Build the node daemon
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/thunder-device-plugin-daemon ./cmd/daemon

.PHONY: build-operator
build-operator: ## Build the DRA operator
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/thunder-dra-operator ./cmd/thunder-dra-operator

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) coverage.out *.tgz

##@ Container images

.PHONY: image
image: image-daemon image-operator ## Build both container images

.PHONY: image-daemon
image-daemon: ## Build the daemon image
	$(DOCKER) build \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-f containers/daemon/Dockerfile \
		-t $(DAEMON_IMAGE):$(IMAGE_TAG) .

.PHONY: image-operator
image-operator: ## Build the operator image
	$(DOCKER) build \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-f containers/operator/Dockerfile \
		-t $(OPERATOR_IMAGE):$(IMAGE_TAG) .

.PHONY: push
push: ## Push both images
	$(DOCKER) push $(DAEMON_IMAGE):$(IMAGE_TAG)
	$(DOCKER) push $(OPERATOR_IMAGE):$(IMAGE_TAG)

.PHONY: image-buildx
image-buildx: ## Build and push multi-platform images (see PLATFORMS)
	$(DOCKER) buildx build --push --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-f containers/daemon/Dockerfile -t $(DAEMON_IMAGE):$(IMAGE_TAG) .
	$(DOCKER) buildx build --push --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-f containers/operator/Dockerfile -t $(OPERATOR_IMAGE):$(IMAGE_TAG) .

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint every chart
	$(HELM) lint $(CHART) --set thunder.apiToken=lint
	$(HELM) lint charts/tests/pod
	$(HELM) lint charts/tests/vm

.PHONY: helm-template
helm-template: ## Render the main chart to stdout
	$(HELM) template $(RELEASE) $(CHART) --namespace $(NAMESPACE) --set thunder.apiToken=template

.PHONY: helm-package
helm-package: ## Package the chart as a .tgz
	@# Chart versions must be semver. Stamp the release version only when
	@# VERSION is a tag; otherwise keep whatever Chart.yaml declares.
	@semver='$(patsubst v%,%,$(VERSION))'; \
	if [[ "$$semver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$$ ]]; then \
		echo "packaging $(CHART) as $$semver"; \
		$(HELM) package $(CHART) --version "$$semver" --app-version "$(VERSION)"; \
	else \
		echo "$(VERSION) is not semver; packaging with the Chart.yaml version"; \
		$(HELM) package $(CHART); \
	fi

.PHONY: helm-schema
helm-schema: ## Check values.yaml validates against values.schema.json
	@python3 -c "import json,sys; json.load(open('$(CHART)/values.schema.json'))" && echo "values.schema.json is valid JSON"
	$(HELM) template schema-check $(CHART) --set thunder.apiToken=schema >/dev/null
	@echo "values.yaml validates against the schema"

##@ Verification

.PHONY: verify
verify: ## Offline checks: Go build/vet/test plus chart renders
	hack/verify.sh

.PHONY: test-local
test-local: ## Integration test on a throwaway local kind cluster
	hack/test-local.sh

.PHONY: preflight
preflight: ## Diagnose whether an existing cluster can run the driver (read-only)
	hack/preflight.sh

.PHONY: check
check: lint test verify ## Everything CI should run without a container runtime

.PHONY: check-all
check-all: check test-local ## Everything, including the local cluster test

##@ Deployment

.PHONY: install
install: ## Install or upgrade the chart (set THUNDER_API_TOKEN)
	@[[ -n "$${THUNDER_API_TOKEN:-}" ]] || { echo "THUNDER_API_TOKEN is required"; exit 1; }
	$(HELM) upgrade --install $(RELEASE) $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--set-string thunder.apiToken="$$THUNDER_API_TOKEN" \
		--wait

.PHONY: uninstall
uninstall: ## Uninstall the chart
	$(HELM) uninstall $(RELEASE) --namespace $(NAMESPACE) --ignore-not-found

.PHONY: status
status: ## Show the driver's cluster state
	$(KUBECTL) -n $(NAMESPACE) get pods -o wide
	$(KUBECTL) get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver
	$(KUBECTL) get resourceslices -l app.kubernetes.io/name=thunder-dra-driver
	$(KUBECTL) get clients.thundercompute.com -A

.PHONY: logs
logs: ## Tail daemon and operator logs
	$(KUBECTL) -n $(NAMESPACE) logs -l app.kubernetes.io/name=thunder-device-plugin --all-containers --tail=100 -f
