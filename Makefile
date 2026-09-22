VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CLUSTER      ?= shpyrd
SERVER_IMAGE ?= shpyrd-server:dev
LDFLAGS      := -X shpyrd/pkg/version.Version=$(VERSION)

.PHONY: all build cli server ui image generate test vet lint clean \
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

## Build the server container image
image:
	docker build --build-arg VERSION=$(VERSION) -t $(SERVER_IMAGE) .

## Regenerate deepcopy code and the App CRD from api/
generate:
	go tool controller-gen object paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:dir=./deploy/components/shpyrd/base/crds

test:
	go test ./...

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

## Build the server image and load it into the kind cluster
dev-load: image
	kind load docker-image $(SERVER_IMAGE) --name $(CLUSTER)

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
