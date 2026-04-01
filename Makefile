# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/kairos-io/cluster-api-provider-kairos:latest
# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:generateEmbeddedObjectMeta=true"

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	@# Fix namespace in webhook manifests (controller-gen generates 'system', we need 'kairos-capi-system')
	@if [ -f config/webhook/manifests.yaml ]; then \
		sed -i 's/namespace: system/namespace: kairos-capi-system/g' config/webhook/manifests.yaml; \
	fi
	@# Add contract version labels to all CRDs (required for Cluster API contract compliance)
	@# Note: Labels must be added AFTER annotations to match controller-gen output order
	@# Use sed to insert labels without reformatting (preserves controller-gen formatting)
	@for crd in config/crd/bases/bootstrap.cluster.x-k8s.io_kairosconfigs.yaml \
		config/crd/bases/bootstrap.cluster.x-k8s.io_kairosconfigtemplates.yaml \
		config/crd/bases/controlplane.cluster.x-k8s.io_kairoscontrolplanes.yaml \
		config/crd/bases/controlplane.cluster.x-k8s.io_kairoscontrolplanetemplates.yaml; do \
		if [ -f "$$crd" ]; then \
			if ! grep -q "cluster.x-k8s.io/provider: kairos" "$$crd" 2>/dev/null; then \
				sed -i '/^  name: /i\  labels:\n    cluster.x-k8s.io/provider: kairos\n    cluster.x-k8s.io/v1beta2: v1beta2' "$$crd"; \
			fi; \
		fi; \
	done

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: generate fmt vet ## Run tests (excludes integration tests).
	go test ./... -short -coverprofile cover.out

.PHONY: test-unit
test-unit: ## Run unit tests only.
	go test ./... -v -short

.PHONY: test-envtest
test-envtest: ## Run envtest-based integration tests.
	@echo "Installing/updating setup-envtest..."
	@go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@echo "Downloading CAPI CRDs..."
	@mkdir -p test/crd/capi
	@curl -L https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.8.0/cluster-api-components.yaml -o test/crd/capi/cluster-api-components.yaml || \
		(echo "Warning: Failed to download CAPI CRDs. Tests may fail." && rm -f test/crd/capi/cluster-api-components.yaml)
	@echo "Setting up kubebuilder tools..."
	@export PATH=$$(go env GOPATH)/bin:$$PATH && \
	eval $$(setup-envtest use -p env latest) && \
	go test ./test/envtest/... -v -timeout 120s

.PHONY: test-kubevirt
test-kubevirt: ## Run local KubeVirt e2e flow (requires kind and KubeVirt).
	./hack/kubevirt-e2e.sh

.PHONY: clean-envtest
clean-envtest: ## Clean envtest artifacts.
	rm -rf test/crd/capi

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: verify-generate
verify-generate: generate ## Verify that generated code is up to date.
	@git diff --exit-code || (echo "Error: Generated code is out of date. Run 'make generate' and commit the changes." && exit 1)

.PHONY: verify-manifests
verify-manifests: manifests ## Verify that manifests are up to date.
	@git diff --exit-code config/crd/bases config/rbac || (echo "Error: Manifests are out of date. Run 'make manifests' and commit the changes." && exit 1)

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: kubevirt-env
kubevirt-env: ## Build kubevirt-env helper CLI.
	go build -o bin/kubevirt-env ./cmd/kubevirt-env

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go

.PHONY: docker-build
docker-build: generate fmt vet ## Build docker image with the manager.
	docker build -t ${IMG} .
	@echo "Built image: ${IMG}"

.PHONY: docker-buildx
docker-buildx: generate fmt vet ## Build docker image with buildx for multi-platform.
	docker buildx build --platform linux/amd64,linux/arm64 -t ${IMG} --push .

.PHONY: docker-push
docker-push: docker-build ## Push docker image with the manager.
	docker push ${IMG}
	@echo "Pushed image: ${IMG}"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	kubectl delete --ignore-not-found=$(ignore-not-found) -f config/crd/bases

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && kustomize edit set image controller=${IMG}
	kustomize build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	kustomize build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.17.0
GOLANGCI_LINT_VERSION ?= v1.60.0

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@latest

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(LOCALBIN)/golangci-lint || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

.PHONY: verify
verify: fmt vet test ## Run all verification checks

