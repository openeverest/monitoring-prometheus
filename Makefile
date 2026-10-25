IMG ?= ghcr.io/openeverest/monitoring-prometheus:dev
CHART := charts/monitoring-prometheus

.PHONY: build
build: ## Build the controller binary.
	CGO_ENABLED=0 GOOS=linux go build -o bin/controller ./cmd/controller

.PHONY: docker-build
docker-build: build ## Build the controller image.
	docker build -t $(IMG) .

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART)

.PHONY: helm-template
helm-template: ## Render the chart.
	helm template monitoring-prometheus $(CHART) -n openeverest-system

.PHONY: test
test: ## Run unit tests.
	go test ./...
