SERVICES := user-service doctor-service scheduling-service consultation-service \
            payment-service notification-service record-service admin-service api-gateway
REGISTRY  ?= ghcr.io/versalife26/telemed
VERSION   ?= 0.3.0

.PHONY: help build test vet lint fmt tidy images $(addprefix image-,$(SERVICES))

help: ## Show this help
	@grep -hE '^[a-z][a-zA-Z0-9_-]*:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}'

build: ## Build every entrypoint
	go build ./...

test: ## Unit tests, race detector on
	go test -race ./...

integration: ## Integration tests (needs Docker for testcontainers)
	go test -race -tags=integration -timeout=25m ./...

vet: ## go vet
	go vet ./...

fmt: ## Check gofmt
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

tidy: ## Assert go.mod/go.sum are tidy
	go mod tidy && git diff --exit-code go.mod go.sum

lint: ## golangci-lint
	golangci-lint run

images: $(addprefix image-,$(SERVICES)) ## Build all nine images

image-%: ## Build one service image: make image-user-service
	docker build --build-arg SERVICE=$* -t $(REGISTRY)/$*:$(VERSION) .
