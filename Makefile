.PHONY: help build build-tmgr build-seedctl build-tmweb test test-integration lint certs image image-local image-import image-local-tmweb image-import-tmweb push deploy deploy-seedhost install-mdns clean

SEEDCTL_HOST ?= tiny.example.com
IMAGE ?= ghcr.io/atomicstack/tmgr
TAG ?= dev
LOCAL_IMAGE ?= docker.io/local/tmgr:latest
LOCAL_IMAGE_TMWEB ?= docker.io/local/tmweb:latest
K3S_NODES ?= node-1.local node-2.local
MDNS_NODE ?= node-1.local

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' Makefile | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: build-tmgr build-seedctl build-tmweb ## build all binaries for the local host

build-tmgr: ## build tmgr (cross-compile arm64 for k3s)
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -tags=sqlite_fts5 -o dist/tmgr-linux-arm64 ./cmd/tmgr

build-seedctl: ## build seedctl (cross-compile amd64 for the seedbox)
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags=sqlite_fts5 -o dist/seedctl-linux-amd64 ./cmd/seedctl

build-tmweb: ## build tmweb (cross-compile arm64 for k3s; no cgo — pure http client)
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/tmweb-linux-arm64 ./cmd/tmweb

test: ## run unit tests
	go test -tags=sqlite_fts5 -race ./...

test-integration: ## run integration tests
	go test -tags=integration,sqlite_fts5 -race ./...

lint: ## go vet
	go vet -tags=sqlite_fts5 ./...

certs: ## generate ca + server + client certs into ./certs (idempotent)
	./scripts/make-certs.sh $(SEEDCTL_HOST)

image: build-tmgr ## build the tmgr container image (arm64) via docker
	docker buildx build --platform linux/arm64 -t $(IMAGE):$(TAG) --load .

push: image ## push the tmgr image
	docker push $(IMAGE):$(TAG)

image-local: ## build tmgr arm64 image into dist/tmgr-image.tar (podman; cgo)
	mkdir -p dist
	podman build --platform=linux/arm64 -t $(LOCAL_IMAGE) -f Dockerfile .
	podman save --format docker-archive -o dist/tmgr-image.tar $(LOCAL_IMAGE)

image-import: image-local ## scp dist/tmgr-image.tar and import on each k3s node
	@for h in $(K3S_NODES); do \
	  echo "==> $$h"; \
	  scp -q dist/tmgr-image.tar matt@$$h:/tmp/tmgr-image.tar && \
	  ssh -t matt@$$h "sudo /usr/local/bin/k3s ctr images import /tmp/tmgr-image.tar"; \
	done

image-local-tmweb: ## build tmweb arm64 image into dist/tmweb-image.tar (podman)
	mkdir -p dist
	podman build --platform=linux/arm64 -t $(LOCAL_IMAGE_TMWEB) -f Dockerfile.tmweb .
	podman save --format docker-archive -o dist/tmweb-image.tar $(LOCAL_IMAGE_TMWEB)

image-import-tmweb: image-local-tmweb ## scp dist/tmweb-image.tar and import on each k3s node
	@for h in $(K3S_NODES); do \
	  echo "==> $$h"; \
	  scp -q dist/tmweb-image.tar matt@$$h:/tmp/tmweb-image.tar && \
	  ssh -t matt@$$h "sudo /usr/local/bin/k3s ctr images import /tmp/tmweb-image.tar"; \
	done

deploy: ## apply k8s manifests
	kubectl apply -k deploy/k8s/

deploy-seedhost: build-seedctl ## copy seedctl + certs to the seed host and install
	scp dist/seedctl-linux-amd64 seedbox@$(SEEDCTL_HOST):bin/seedctl
	ssh seedbox@$(SEEDCTL_HOST) mkdir -p etc/seedctl
	scp deploy/seedhost/seedctl.example.yaml seedbox@$(SEEDCTL_HOST):etc/seedctl/seedctl.yaml
	scp certs/ca.crt certs/server.crt certs/server.key seedbox@$(SEEDCTL_HOST):etc/seedctl/
	ssh seedbox@$(SEEDCTL_HOST) chmod 600 etc/seedctl/server.key
	ssh seedbox@$(SEEDCTL_HOST) bash -s < deploy/seedhost/install.sh

install-mdns: ## install avahi-publish service on $(MDNS_NODE) so tmgr.local resolves
	scp scripts/install-mdns-on-pi.sh matt@$(MDNS_NODE):/tmp/install-mdns-on-pi.sh
	ssh -t matt@$(MDNS_NODE) "bash /tmp/install-mdns-on-pi.sh"

clean: ## remove dist/
	rm -rf dist/
