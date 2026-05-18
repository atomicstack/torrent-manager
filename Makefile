.PHONY: help build test lint clean

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' Makefile | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## build both binaries for the local host
	go build ./cmd/...

test: ## run unit tests
	go test ./...

lint: ## go vet
	go vet ./...

clean: ## remove build artefacts
	rm -rf dist/ bin/
