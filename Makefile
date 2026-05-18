.PHONY: help build build-tmgr build-seedctl test test-integration lint certs image push deploy deploy-seedhost clean

SEEDCTL_HOST ?= tiny.example.com
IMAGE ?= ghcr.io/atomicstack/tmgr
TAG ?= dev

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' Makefile | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: build-tmgr build-seedctl ## build both binaries for the local host

build-tmgr: ## build tmgr (cross-compile arm64 for k3s)
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/tmgr-linux-arm64 ./cmd/tmgr

build-seedctl: ## build seedctl (cross-compile amd64 for the seedbox)
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/seedctl-linux-amd64 ./cmd/seedctl

test: ## run unit tests
	go test -race ./...

test-integration: ## run integration tests
	go test -tags=integration -race ./...

lint: ## go vet
	go vet ./...

certs: ## generate ca + server + client certs into ./certs (idempotent)
	./scripts/make-certs.sh $(SEEDCTL_HOST)

image: build-tmgr ## build the tmgr container image (arm64)
	docker buildx build --platform linux/arm64 -t $(IMAGE):$(TAG) --load .

push: image ## push the tmgr image
	docker push $(IMAGE):$(TAG)

deploy: ## apply k8s manifests
	kubectl apply -k deploy/k8s/

deploy-seedhost: build-seedctl ## copy seedctl + certs to the seed host and install
	scp dist/seedctl-linux-amd64 seedbox@$(SEEDCTL_HOST):bin/seedctl
	ssh seedbox@$(SEEDCTL_HOST) mkdir -p etc/seedctl
	scp deploy/seedhost/seedctl.example.yaml seedbox@$(SEEDCTL_HOST):etc/seedctl/seedctl.yaml
	scp certs/ca.crt certs/server.crt certs/server.key seedbox@$(SEEDCTL_HOST):etc/seedctl/
	ssh seedbox@$(SEEDCTL_HOST) chmod 600 etc/seedctl/server.key
	ssh seedbox@$(SEEDCTL_HOST) bash deploy/seedhost/install.sh

clean: ## remove dist/
	rm -rf dist/
