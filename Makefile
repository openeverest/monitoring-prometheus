## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Image URL to use for all building/pushing image targets.
IMG ?= ghcr.io/openeverest/monitoring-prometheus-dev:latest
_IMG_REPO = $(firstword $(subst :, ,$(IMG)))
_IMG_TAG  = $(lastword $(subst :, ,$(IMG)))

CHART_DIR ?= charts/monitoring-prometheus
RELEASE_NAMESPACE ?= openeverest-system

GOLANGCI_LINT_VERSION ?= v2.14.0

# Branch of openeverest/openeverest whose CRDs the integration tests install.
# Switch to main once the monitoring API is merged there.
OPENEVEREST_BRANCH ?= monitoring/01-api
OPENEVEREST_CRD_URL = https://raw.githubusercontent.com/openeverest/openeverest/$(OPENEVEREST_BRANCH)/config/crd/bases
# Prometheus Operator release whose PodMonitor CRD the integration tests install.
PROMETHEUS_OPERATOR_VERSION ?= v0.94.2

# k3d cluster for the integration tests (dev/k3d_config.yaml).
K3D_CLUSTER_NAME ?= monitoring-prometheus-test

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: lint
lint: ## Run golangci-lint.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: tidy-check
tidy-check: ## Verify go.mod and go.sum are tidy.
	go mod tidy
	git diff --exit-code -- go.mod go.sum

.PHONY: run
run: ## Run the controller against the current kubeconfig.
	go run ./cmd/controller

##@ Testing

.PHONY: test-unit
test-unit: ## Run unit tests.
	go test -race -coverprofile=coverage.out ./...

.PHONY: test-integration
test-integration: ## Run the chainsaw integration tests against the current cluster.
	chainsaw test --config ./test/integration/.chainsaw.yaml ./test/integration

##@ Build

.PHONY: build
build: $(LOCALBIN) ## Build the controller binary.
	CGO_ENABLED=0 go build -o $(LOCALBIN)/controller ./cmd/controller

.PHONY: docker-build
docker-build: ## Build the controller image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the controller image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the chart.
	helm template monitoring-prometheus $(CHART_DIR) -n $(RELEASE_NAMESPACE)

##@ Integration test cluster

.PHONY: k3d-cluster-up
k3d-cluster-up: ## Create the k3d cluster for the integration tests.
	k3d cluster create $(K3D_CLUSTER_NAME) --config ./dev/k3d_config.yaml

.PHONY: k3d-cluster-down
k3d-cluster-down: ## Delete the k3d cluster.
	k3d cluster delete $(K3D_CLUSTER_NAME)

.PHONY: load-image
load-image: ## Import the controller image (IMG) into the k3d cluster.
	k3d image import $(IMG) -c $(K3D_CLUSTER_NAME)

.PHONY: install-crds
install-crds: ## Install the OpenEverest and Prometheus Operator CRDs the controller needs.
	kubectl apply --server-side -f $(OPENEVEREST_CRD_URL)/core.openeverest.io_providers.yaml
	kubectl apply --server-side -f $(OPENEVEREST_CRD_URL)/core.openeverest.io_instances.yaml
	kubectl apply --server-side -f $(OPENEVEREST_CRD_URL)/monitoring.openeverest.io_monitoringclasses.yaml
	kubectl apply --server-side -f $(OPENEVEREST_CRD_URL)/monitoring.openeverest.io_monitoringdestinations.yaml
	kubectl apply --server-side -f $(OPENEVEREST_CRD_URL)/monitoring.openeverest.io_monitoringbindings.yaml
	kubectl apply --server-side -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/$(PROMETHEUS_OPERATOR_VERSION)/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml
	kubectl wait --for=condition=Established --timeout=60s \
		crd/providers.core.openeverest.io crd/instances.core.openeverest.io \
		crd/monitoringclasses.monitoring.openeverest.io crd/monitoringdestinations.monitoring.openeverest.io \
		crd/monitoringbindings.monitoring.openeverest.io crd/podmonitors.monitoring.coreos.com

.PHONY: deploy-ci
deploy-ci: ## Install the chart with the image imported into k3d (IMG).
	helm upgrade --install monitoring-prometheus $(CHART_DIR) \
		--create-namespace \
		--namespace $(RELEASE_NAMESPACE) \
		--set image.repository=$(_IMG_REPO) \
		--set image.tag=$(_IMG_TAG) \
		--set image.pullPolicy=Never \
		--wait --timeout 2m
