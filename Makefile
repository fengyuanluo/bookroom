# BookRoom build entry points. `make help` lists the targets.
GO       ?= go
SQLC     ?= sqlc
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
BIN      := bin/bookroom

.PHONY: help generate build run test race bench lint check check-generated docker clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

generate: ## regenerate internal/api (oapi-codegen) and internal/store/db (sqlc)
	$(GO) generate ./internal/api
	$(SQLC) generate

build: ## build a static binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bookroom

run: build ## run the server with ./data as the data directory
	BOOKROOM_DATA_DIR=./data $(BIN) serve

test: ## run all tests
	$(GO) test ./...

race: ## run all tests with the race detector
	$(GO) test -race -count=1 ./...

bench: ## 20k-book listing and sync benchmarks
	$(GO) test ./internal/library ./internal/readest -run '^$$' -bench . -benchtime 50x -benchmem

lint: ## vet and formatting
	$(GO) vet ./...
	@test -z "$$(gofmt -l ./cmd ./internal)" || { gofmt -l ./cmd ./internal; echo 'run gofmt -w'; exit 1; }

check-generated: generate ## fail when generated code is out of date with the spec or SQL
	@git diff --exit-code -- internal/api internal/store/db || { echo 'generated code is stale: run make generate and commit'; exit 1; }

check: lint check-generated race ## everything CI runs

docker: ## build the container image
	docker build --build-arg VERSION=$(VERSION) -t bookroom:$(VERSION) .

clean:
	rm -rf bin
