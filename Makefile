# One binary now. TELEMED_DOMAINS picks a subset at run time; there is nothing
# to select at build time.
DOMAINS   := user doctor scheduling consultation payment notification record admin
REGISTRY  ?= ghcr.io/versalife26/telemed
VERSION   ?= 0.3.0

.PHONY: help build run test integration vet lint fmt tidy image migrate seed

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

run: ## Run the whole platform locally
	go run ./cmd/telemed

image: ## Build the image
	docker build -t $(REGISTRY)/backend:$(VERSION) .

migrate: ## Apply the bootstrap schemas then every domain's migrations
	./scripts/migrate.sh

seed: ## Load fake doctors, patients and appointments into a migrated dev database
	psql "$(DATABASE_URL)" -f scripts/seed.sql
