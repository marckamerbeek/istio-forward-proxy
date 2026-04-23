# istio-forward-proxy Makefile

.PHONY: help build test lint docker docker-push install uninstall e2e clean

IMAGE_REPO ?= localhost/istio-forward-proxy
IMAGE_TAG  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
PROXY_NS   ?= istio-egress

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1;36m%-18s\033[0m %s\n", $$1, $$2}'

deps: ## Download Go dependencies
	go mod download
	go mod tidy

build: ## Build the Go binary locally
	CGO_ENABLED=0 go build -o bin/forward-proxy ./cmd

test: ## Run unit tests
	go test -v -race -cover ./internal/...

lint: ## Run linters
	go vet ./...
	@if command -v golangci-lint >/dev/null; then golangci-lint run ./...; else echo "golangci-lint not installed, skipping"; fi

docker: ## Build Docker image
	IMAGE_REPO=$(IMAGE_REPO) IMAGE_TAG=$(IMAGE_TAG) ./scripts/build.sh

docker-push: ## Build and push Docker image
	IMAGE_REPO=$(IMAGE_REPO) IMAGE_TAG=$(IMAGE_TAG) PUSH=1 ./scripts/build.sh

helm-lint: ## Lint the Helm chart
	helm lint deploy/helm/istio-forward-proxy

helm-template: ## Render the Helm chart locally
	helm template istio-forward-proxy deploy/helm/istio-forward-proxy \
		--namespace $(PROXY_NS) \
		--values deploy/helm/istio-forward-proxy/values.yaml

install: ## Install on cluster
	./scripts/install.sh

uninstall: ## Remove from cluster
	helm uninstall istio-forward-proxy --namespace $(PROXY_NS)

e2e: ## Run end-to-end tests on cluster
	./scripts/e2e-test.sh

verify-absolute-path: ## Prove that absolute paths are preserved
	./scripts/verify-absolute-path.sh

clean: ## Clean local build artifacts
	rm -rf bin/ dist/
	go clean -testcache

all: deps lint test helm-lint docker ## Full build (deps, lint, test, image)
