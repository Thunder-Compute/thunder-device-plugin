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

# Container images. Public, because an external install pulls exactly what
# Thunder's own clusters pull.
REGISTRY       ?= ghcr.io/thunder-compute
DAEMON_IMAGE   ?= $(REGISTRY)/thunder-device-plugin-daemon
OPERATOR_IMAGE ?= $(REGISTRY)/thunder-dra-operator
IMAGE_TAG      ?= $(VERSION)
PLATFORMS      ?= linux/amd64

# Helm.
CHART          := charts/thunder-device-plugin
CHART_REGISTRY ?= oci://ghcr.io/thunder-compute/charts
RELEASE        ?= thunder-device-plugin
NAMESPACE      ?= thunder-system

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
	@unformatted="$$(gofmt -l ./cmd ./internal hack/thunder-registry-stub.go)"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "gofmt is clean"

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...
	@# The registry stub carries a build-ignore tag, so ./... never reaches it.
	$(GO) vet hack/thunder-registry-stub.go

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
	$(HELM) lint $(CHART)
	$(HELM) lint charts/tests/pod
	$(HELM) lint charts/tests/vm

.PHONY: helm-template
helm-template: ## Render the main chart to stdout
	$(HELM) template $(RELEASE) $(CHART) --namespace $(NAMESPACE)

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

.PHONY: helm-push
helm-push: helm-package ## Package and push the chart to $(CHART_REGISTRY)
	@semver='$(patsubst v%,%,$(VERSION))'; \
	if [[ "$$semver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$$ ]]; then \
		package="thunder-device-plugin-$$semver.tgz"; \
	else \
		package="$$(ls -t thunder-device-plugin-*.tgz | head -1)"; \
	fi; \
	echo "pushing $$package to $(CHART_REGISTRY)"; \
	$(HELM) push "$$package" $(CHART_REGISTRY)

.PHONY: helm-schema
helm-schema: ## Check values.yaml validates against values.schema.json
	@jq empty $(CHART)/values.schema.json && echo "values.schema.json is valid JSON"
	$(HELM) template schema-check $(CHART) >/dev/null
	@echo "values.yaml validates against the schema"

##@ Verification

.PHONY: verify
verify: ## Offline checks: Go build/vet/test plus chart renders
	hack/verify.sh

.PHONY: test-local
test-local: ## Integration test on a throwaway local kind cluster
	hack/test-local.sh

.PHONY: release-version
release-version: ## Print the version this commit would be released as
	@hack/release-version.sh

.PHONY: verify-promotion
verify-promotion: ## Assert a release is its candidate re-tagged (CANDIDATE=0.2.0-rc.3 RELEASE_VERSION=0.2.0)
	hack/verify-promotion.sh "$(CANDIDATE)" "$(RELEASE_VERSION)"

.PHONY: preflight
preflight: ## Diagnose whether an existing cluster can run the driver (read-only)
	hack/preflight.sh

##@ End-to-end tests
#
# These run against the cluster your kubeconfig points at, which must already
# have the chart installed. They allocate real GPUs and enrol real Thunder
# clients. Point KUBECONFIG at the cluster under test.

E2E_NAMESPACE        ?= thunder-e2e
E2E_TIMEOUT          ?= 45m
E2E_STRESS_DURATION  ?= 5m
E2E_STRESS_WORKERS   ?= 6
E2E_FLAGS            ?=

E2E_TEST := $(GO) test -tags e2e -count=1 -v -timeout $(E2E_TIMEOUT) ./test/e2e/ \
	-e2e.namespace=$(E2E_NAMESPACE) $(E2E_FLAGS)

.PHONY: test-e2e
test-e2e: ## Basic and VM end-to-end tests against the current cluster
	$(E2E_TEST) -short

.PHONY: test-e2e-stress
test-e2e-stress: ## Stress the driver with concurrent claim churn (see E2E_STRESS_*)
	$(E2E_TEST) -run TestStress \
		-e2e.stress-duration=$(E2E_STRESS_DURATION) \
		-e2e.stress-workers=$(E2E_STRESS_WORKERS)

.PHONY: test-e2e-all
test-e2e-all: ## Every end-to-end test, including the stress run
	$(E2E_TEST) \
		-e2e.stress-duration=$(E2E_STRESS_DURATION) \
		-e2e.stress-workers=$(E2E_STRESS_WORKERS)

.PHONY: check
check: lint test verify ## Everything CI should run without a container runtime

.PHONY: check-all
check-all: check test-local ## Everything, including the local cluster test

##@ Deployment

.PHONY: install
install: ## Install or upgrade the chart (set THUNDER_API_TOKEN)
	@[[ -n "$${THUNDER_API_TOKEN:-}" ]] || { echo "THUNDER_API_TOKEN is required"; exit 1; }
	@# The token goes into a Secret directly; the chart never takes it as a value.
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(NAMESPACE) create secret generic thunder-api \
		--from-literal=THUNDER_API_TOKEN="$$THUNDER_API_TOKEN" \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(HELM) upgrade --install $(RELEASE) $(CHART) --namespace $(NAMESPACE) --wait

.PHONY: uninstall
uninstall: uninstall-confirm ## Uninstall the chart, leaving runtime state behind
	$(HELM) uninstall $(RELEASE) --namespace $(NAMESPACE) --ignore-not-found
	@echo
	@echo "The release is gone. Runtime state is not: ResourceSlices, DeviceClasses,"
	@echo "ThunderClients, the CRD and namespace $(NAMESPACE) all survive. Use"
	@echo "'make purge' to remove those too."

# uninstall-confirm names the cluster and spells out what survives, then blocks
# until the operator types the context name. Set YES=1 for automation.
#
# Uninstalling is not just "stop running pods": it removes the only two things
# that can revoke a Thunder enrollment or release a ThunderClient finalizer, and
# it leaves inventory published that no driver can serve.
.PHONY: uninstall-confirm
uninstall-confirm:
	@# `|| true` matters: with the CRD absent kubectl fails, and pipefail would
	@# abort the recipe with a bare "Error 1" instead of reading as zero clients.
	@clients="$$($(KUBECTL) get clients.thundercompute.com -A --no-headers --ignore-not-found 2>/dev/null | wc -l || true)"; \
	if [[ "$$clients" -gt 0 && "$${FORCE:-}" != "1" ]]; then \
		echo; \
		echo "$$clients ThunderClient(s) still exist, so claims are still prepared."; \
		echo "Removing the driver now would:"; \
		echo "  - leave those Thunder enrollments with nothing to revoke them"; \
		echo "  - leave their finalizers with nothing to release, so the resources"; \
		echo "    become undeletable and 'kubectl delete namespace $(NAMESPACE)' hangs"; \
		echo; \
		echo "Delete the workloads holding the claims first, while the driver is still"; \
		echo "installed. To uninstall anyway: make uninstall FORCE=1"; \
		exit 1; \
	fi
	@context="$$($(KUBECTL) config current-context 2>/dev/null || echo unknown)"; \
	echo; \
	echo "make uninstall will remove, in context '$$context':"; \
	echo "  - helm release $(RELEASE) in namespace $(NAMESPACE), so the daemon and"; \
	echo "    operator stop and no new claim can be prepared"; \
	echo; \
	echo "It will NOT remove:"; \
	echo "  - ResourceSlices and DeviceClasses, which keep advertising GPUs that"; \
	echo "    nothing can serve, so new claims will be allocated and then hang"; \
	echo "  - ThunderClients, the CRD, or namespace $(NAMESPACE)"; \
	echo; \
	$(KUBECTL) get resourceslices,deviceclasses -l app.kubernetes.io/name=thunder-dra-driver 2>/dev/null || true; \
	echo; \
	if [[ "$${YES:-}" == "1" ]]; then \
		echo "YES=1, skipping confirmation."; exit 0; \
	fi; \
	if [[ ! -t 0 ]]; then \
		echo "uninstall needs a terminal to confirm; rerun with YES=1 to skip the prompt."; exit 1; \
	fi; \
	read -r -p "Type the context name '$$context' to continue: " reply; \
	if [[ "$$reply" != "$$context" ]]; then echo "aborted"; exit 1; fi

.PHONY: purge
purge: purge-confirm ## Uninstall and delete everything the driver wrote at runtime (destructive)
	@# `helm uninstall` only removes what the release manifest owns. The operator
	@# and the daemon write ResourceSlices, DeviceClasses, ThunderClients and
	@# per-claim guest artifacts at runtime, and none of them carry an owner
	@# reference, so nothing garbage-collects them once the workloads are gone.
	@# Helm never deletes CRDs either, and the namespace came from `make install`.
	$(MAKE) uninstall YES=1 FORCE=1
	$(KUBECTL) delete resourceslices -l app.kubernetes.io/name=thunder-dra-driver --ignore-not-found
	$(KUBECTL) delete deviceclasses -l app.kubernetes.io/name=thunder-dra-driver --ignore-not-found
	$(KUBECTL) delete configmap,secret --all-namespaces -l app.kubernetes.io/name=thunder-dra-driver --ignore-not-found
	@# Dropping the CRD takes every remaining ThunderClient with it.
	$(KUBECTL) delete crd clients.thundercompute.com --ignore-not-found
	$(KUBECTL) delete namespace $(NAMESPACE) --ignore-not-found
	@echo
	@echo "Cluster state is gone. CDI specs and enrollment tokens still sit on the nodes"
	@echo "under the kubelet plugin dir; they are inert without the driver, and a"
	@echo "reinstall rewrites them."

# purge-confirm shows what purge is about to destroy and in which cluster, then
# blocks until the operator types the context name. Set YES=1 for automation.
.PHONY: purge-confirm
purge-confirm:
	@# `|| true` matters: with the CRD absent kubectl fails, and pipefail would
	@# abort the recipe with a bare "Error 1" instead of reading as zero clients.
	@clients="$$($(KUBECTL) get clients.thundercompute.com -A --no-headers --ignore-not-found 2>/dev/null | wc -l || true)"; \
	if [[ "$$clients" -gt 0 && "$${FORCE:-}" != "1" ]]; then \
		echo "$$clients ThunderClient(s) still exist, so their claims were never unprepared."; \
		echo "Deleting them here skips the unenroll call to the Thunder API and leaks those"; \
		echo "enrollments. Delete the workloads holding the claims while the driver is still"; \
		echo "installed, then rerun. To purge anyway: make purge FORCE=1"; \
		exit 1; \
	fi
	@context="$$($(KUBECTL) config current-context 2>/dev/null || echo unknown)"; \
	echo; \
	echo "make purge will delete, in context '$$context':"; \
	echo "  - helm release $(RELEASE) in namespace $(NAMESPACE)"; \
	echo "  - namespace $(NAMESPACE) and everything else in it"; \
	echo "  - CRD clients.thundercompute.com and all ThunderClient objects"; \
	echo "  - all ResourceSlices and DeviceClasses labelled thunder-dra-driver"; \
	echo "  - all per-claim guest ConfigMaps and Secrets, in every namespace"; \
	echo; \
	$(KUBECTL) get resourceslices,deviceclasses -l app.kubernetes.io/name=thunder-dra-driver 2>/dev/null || true; \
	$(KUBECTL) get clients.thundercompute.com -A --ignore-not-found 2>/dev/null || true; \
	$(KUBECTL) get configmap,secret --all-namespaces -l app.kubernetes.io/name=thunder-dra-driver 2>/dev/null || true; \
	echo; \
	if [[ "$${YES:-}" == "1" ]]; then \
		echo "YES=1, skipping confirmation."; exit 0; \
	fi; \
	if [[ ! -t 0 ]]; then \
		echo "purge needs a terminal to confirm; rerun with YES=1 to skip the prompt."; exit 1; \
	fi; \
	read -r -p "Type the context name '$$context' to continue: " reply; \
	if [[ "$$reply" != "$$context" ]]; then echo "aborted"; exit 1; fi

.PHONY: status
status: ## Show the driver's cluster state
	$(KUBECTL) -n $(NAMESPACE) get pods -o wide
	$(KUBECTL) get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver
	$(KUBECTL) get resourceslices -l app.kubernetes.io/name=thunder-dra-driver
	$(KUBECTL) get clients.thundercompute.com -A

.PHONY: logs
logs: ## Tail daemon and operator logs
	$(KUBECTL) -n $(NAMESPACE) logs -l app.kubernetes.io/name=thunder-device-plugin --all-containers --tail=100 -f
