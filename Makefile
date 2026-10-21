# RavenDB Cloud operator.
#
# Every Go target runs inside a container via hack/go.sh, so a working Docker is
# the only prerequisite. Set GO_LOCAL=1 to use a host Go toolchain instead.

# The version of the last release, or of the one being prepared: the Prepare
# Release workflow sets it, the Release workflow reads it, and it is stamped
# into both binaries.
VERSION ?= 0.0.0-dev

# A local build, because docker-build and deploy are the development path. The
# published image is pinned by the Helm chart to the release it ships with, and
# nothing references a moving tag: an operator pinned to one reinstalls
# something different on every pod restart.
IMG ?= ravendb-cloud-operator:dev
RELEASE_IMG ?= docker.io/ravendb/ravendb-cloud-operator:$(VERSION)
LDFLAGS := -s -w -X github.com/ravendb/ravendb-cloud-operator/internal/version.Version=$(VERSION)
# controller-gen and setup-envtest are pinned in go.mod as tools, so they need
# no install step. golangci-lint is not: its dependencies would land in the
# operator's module graph.
GOLANGCI_LINT_VERSION ?= v2.14.0
ENVTEST_K8S_VERSION ?= 1.37.0
ifeq ($(GO_LOCAL),1)
GO :=
# The host sees the repository wherever it is checked out.
ENVTEST_DIR := $(CURDIR)/bin/envtest
else
GO := ./hack/go.sh
# hack/go.sh mounts the repository at /workspace, so paths handed to a command
# running inside the container have to be container paths, not $(CURDIR).
ENVTEST_DIR := /workspace/bin/envtest
endif

.PHONY: help
help: ## Show available targets.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

##@ Development

.PHONY: generate
generate: ## Regenerate deepcopy functions.
	$(GO) go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: manifests
manifests: ## Regenerate CRDs and RBAC from kubebuilder markers.
	$(GO) go tool controller-gen crd rbac:roleName=manager-role paths=./... \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt: ## Format the source.
	$(GO) gofmt -w ./api ./cmd ./internal

.PHONY: vet
vet: ## Run go vet.
	$(GO) go vet ./...

.PHONY: lint
lint: ## Run golangci-lint with .golangci.yml.
	$(GO) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: hooks
hooks: ## Install the pre-commit checks.
# core.hooksPath rather than copying into .git/hooks, so the hook stays a
# tracked file that an edit reaches everybody through.
	git config core.hooksPath hack/hooks
	@echo "  core.hooksPath = hack/hooks"

.PHONY: build
build: generate manifests ## Build the manager and the kubectl plugin into bin/.
	$(GO) go build -ldflags "$(LDFLAGS)" -o bin/manager ./cmd
	$(GO) go build -ldflags "$(LDFLAGS)" -o bin/kubectl-ravendb_cloud ./cmd/kubectl-ravendb_cloud

##@ Test

# Four levels, separated by the only thing that actually distinguishes them:
# what infrastructure each one needs. That is what decides where a level can
# run, how long it takes, and whether it can cost money.
#
#   unit         nothing but Go. In-process HTTP servers count as nothing:
#                they need no binary, no cluster and no network.
#   integration  the envtest kube-apiserver and etcd binaries.
#   e2e          Docker and a kind cluster, with the operator as a container.
#   e2e-cloud    a real RavenDB Cloud account and an API key. Read-only.
#
# `make test` stays the one to type: unit plus integration, which is
# everything reproducible on a laptop without Docker.

.PHONY: test
test: test-unit test-integration ## Run unit and integration tests. No cluster, no cloud.

.PHONY: test-unit
test-unit: generate manifests vet ## Unit: pure Go. Seconds, runs anywhere.
# The race detector needs cgo, which hack/go.sh otherwise leaves off so the
# manager binary stays static. Reconciles run concurrently and share clients,
# so this is the level that most needs it.
	CGO_ENABLED=1 $(GO) go test -race ./... -count=1
	@echo
	@echo "  Note: unless KUBEBUILDER_ASSETS is set in your environment, the CEL"
	@echo "  and defaulting tests in api/v1alpha1 just skipped. They need a real"
	@echo "  API server: 'make test-integration' provides one."
	@echo

# setup-envtest downloads the kube-apiserver and etcd binaries on first use and
# prints where they are, which differs by OS and architecture.
.PHONY: test-integration
test-integration: manifests ## CRD: CEL rules against a real kube-apiserver.
	$(GO) sh -c 'KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" go test ./api/... -count=1 -v'

# Kept as an alias because it is in muscle memory and in older notes.
.PHONY: test-crd
test-crd: test-integration ## Alias for test-integration.

.PHONY: test-e2e
test-e2e: ## Labs: the operator as a container, in a kind cluster.
	labs/run_all.sh

.PHONY: test-chart-install
test-chart-install: ## Install the chart into the lab cluster and check it works.
# Separate from the labs on purpose. They teach; this one closes the gap
# between a chart that renders and a chart that installs, and it is the only
# thing that exercises rbac.scope=namespace end to end.
	labs/chart_install.sh

.PHONY: test-e2e-cloud
test-e2e-cloud: ## Cloud: read-only requests against a real RavenDB Cloud account. Needs a key.
	labs/e2e_smoke.sh

.PHONY: test-all
test-all: test test-e2e test-chart-install ## Everything that does not touch the real cloud.

##@ Distribution

.PHONY: chart-sync
chart-sync: manifests ## Copy the generated CRDs and RBAC into the Helm chart.
	hack/chart-sync.sh

.PHONY: chart-verify
chart-verify: manifests ## Fail if the chart is stale relative to config/.
	hack/chart-sync.sh --verify

.PHONY: chart-lint
chart-lint: chart-verify ## Lint the chart and render every values file in ci/.
	helm lint charts/ravendb-cloud-operator
	helm template charts/ravendb-cloud-operator >/dev/null
# The defaults leave imagePullSecrets, extraEnv, watchNamespace and metrics
# empty or off, and an empty value is the case those templates skip. So the
# render above reaches none of them, and each file in ci/ is a set of values
# that does. Helm parses what it renders, so a broken nindent fails here.
	for f in charts/ravendb-cloud-operator/ci/*.yaml; do \
		echo "  rendering $$f"; \
		helm template charts/ravendb-cloud-operator -f "$$f" >/dev/null || exit 1; \
	done

##@ Build and deploy

.PHONY: docker-build
docker-build: ## Build the operator image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

# A kustomization written next to the output, rather than kustomize edit on
# config/manager, so building the installer never leaves the tree dirty.
.PHONY: build-installer
build-installer: manifests ## Write dist/install.yaml: CRDs, RBAC and the manager running RELEASE_IMG.
	mkdir -p dist
	printf 'resources:\n- ../config/default\nimages:\n- name: ravendb-cloud-operator\n  newName: %s\n  newTag: %s\n' \
		"$(firstword $(subst :, ,$(RELEASE_IMG)))" "$(lastword $(subst :, ,$(RELEASE_IMG)))" > dist/kustomization.yaml
	kubectl kustomize dist > dist/install.yaml
	rm dist/kustomization.yaml

.PHONY: install
install: manifests ## Install the CRDs into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: ## Remove the CRDs from the current cluster.
	kubectl delete --ignore-not-found -f config/crd/bases

.PHONY: deploy
deploy: manifests ## Deploy the operator into the current cluster, running IMG.
	kubectl apply -k config/default
	kubectl -n ravendb-cloud-operator-system set image \
		deploy/ravendb-cloud-operator-controller-manager manager=$(IMG)

.PHONY: undeploy
undeploy: ## Remove the operator from the current cluster.
	kubectl delete --ignore-not-found -k config/default

##@ Labs

.PHONY: lab-setup
lab-setup: ## Create a kind cluster and install the operator plus the lab control plane.
	labs/00_setup.sh

.PHONY: lab-run
lab-run: ## Run every lab against the current cluster.
	labs/run_all.sh

.PHONY: lab-teardown
lab-teardown: ## Delete the kind cluster the labs use.
	labs/00_setup.sh --teardown
