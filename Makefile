VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CLUSTER      ?= shpyrd
SERVER_IMAGE ?= shpyrd-server:dev
LDFLAGS      := -X shpyrd/pkg/version.Version=$(VERSION)

.PHONY: all build cli server ui image dev-image generate test vet lint clean \
        dev-cluster dev-load dev-deploy dev-destroy installclint commitlint

all: build

## Build both binaries into bin/
build: cli server

cli:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/shpyrd ./cmd/shpyrd

server:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/shpyrd-server ./cmd/shpyrd-server

## Build the dashboard into ui/dist (embedded by `make server`)
ui:
	cd ui && npm ci --no-audit --no-fund && npm run build

## Build the server container image (full multi-stage build)
image:
	docker build --build-arg VERSION=$(VERSION) -t $(SERVER_IMAGE) .

## Fast development image: compile the server on the host (UI embedded from
## ui/dist), then package it with Dockerfile.dev. Run `make ui` first when
## the dashboard changed.
GOARCH_HOST := $(shell go env GOARCH)
dev-image:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_HOST) go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o bin/shpyrd-server-linux-$(GOARCH_HOST) ./cmd/shpyrd-server
	docker build -f Dockerfile.dev --build-arg TARGETARCH=$(GOARCH_HOST) -t $(SERVER_IMAGE) .

## Regenerate deepcopy code and the App CRD from api/
generate:
	go tool controller-gen object paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:dir=./deploy/components/shpyrd/base/crds

## Go tests and the dashboard's unit tests (needs `npm ci` in ui/ once,
## which `make ui` does)
test:
	go test ./...
	cd ui && npm run test

vet:
	go vet ./...

lint: vet
	cd ui && npm run lint

clean:
	rm -rf bin ui/dist/*
	touch ui/dist/.gitkeep

## Local development loop -----------------------------------------------------

## Create the kind cluster and install everything except the shpyrd server
dev-cluster: cli
	./bin/shpyrd cluster create --name $(CLUSTER) --skip shpyrd

## Build the server image and load it into every node of the kind cluster.
## This is what `kind load docker-image` does (import into containerd's k8s.io
## namespace with the node's snapshotter), without needing a kind binary on
## PATH: the cluster is created by the kind library in go.mod, and a kind CLI
## older than that library cannot read the node's containerd config.
dev-load: dev-image
	@set -e; \
	nodes=$$(docker ps -q --filter "label=io.x-k8s.kind.cluster=$(CLUSTER)"); \
	if [ -z "$$nodes" ]; then \
		echo "no kind cluster named $(CLUSTER); run 'make dev-cluster'" >&2; exit 1; \
	fi; \
	archive=$$(mktemp); \
	trap 'rm -f "$$archive"' EXIT; \
	docker save -o "$$archive" $(SERVER_IMAGE); \
	for node in $$nodes; do \
		name=$$(docker inspect --format '{{.Name}}' $$node | cut -c2-); \
		snapshotter=$$(docker exec $$node containerd config dump 2>/dev/null \
			| sed -n "s/^[[:space:]]*snapshotter = '\(..*\)'/\1/p" | head -1); \
		echo "Loading $(SERVER_IMAGE) into $$name ($${snapshotter:-overlayfs})..."; \
		docker exec -i $$node ctr --namespace=k8s.io images import \
			--all-platforms --digests --snapshotter="$${snapshotter:-overlayfs}" - \
			< "$$archive" >/dev/null; \
	done

## Load the image and (re)apply the shpyrd component
dev-deploy: cli dev-load
	./bin/shpyrd cluster init --context kind-$(CLUSTER) --only shpyrd --set SHPYRD_SERVER_IMAGE=$(SERVER_IMAGE)
	kubectl --context kind-$(CLUSTER) -n shpyrd-system rollout restart deployment/shpyrd-server

dev-destroy: cli
	./bin/shpyrd cluster destroy --name $(CLUSTER) --yes

## Tooling ---------------------------------------------------------------------

installclint:
	npm install -g @commitlint/cli @commitlint/config-conventional

commitlint:
	commitlint --from=HEAD~1

## Build both CLI binaries
cli-all: cli
	go build -ldflags "-X shpyrd/pkg/version.Version=$(VERSION)" -o bin/shpyrd-ctl ./cmd/shpyrd-ctl
