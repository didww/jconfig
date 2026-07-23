BINARY  := jconfig
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
IMAGE     ?= ghcr.io/didww/jconfig
PLATFORMS ?= linux/amd64,linux/arm64
CHART     := charts/jconfig
CHART_REPO ?= oci://ghcr.io/didww/charts
# Chart versions must be bare SemVer: strip a leading v, and fall back to a
# placeholder when building outside a tag.
CHART_VERSION ?= $(shell echo "$(VERSION)" | sed 's/^v//' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+' \
	&& echo "$(VERSION)" | sed 's/^v//' || echo "0.0.0-dev")

GOOS   ?= linux
GOARCH ?= amd64

.PHONY: all build install test race vet lint fmt clean dist fakejunos \
	image image-push helm-lint helm-template helm-package helm-push release

all: test build

## build: static single binary for the host platform
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

## install: build into GOBIN
install:
	CGO_ENABLED=0 go install -trimpath -ldflags '$(LDFLAGS)' .

## test: run the unit and integration tests
test:
	go test ./...

## race: run the tests under the race detector
race:
	go test -race -count=2 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

## lint: golangci-lint if it is installed
lint:
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed, skipping"

## dist: cross-compiled release binaries
dist:
	@mkdir -p dist
	@for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 freebsd/amd64; do \
		os=$${pair%/*}; arch=$${pair#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-$$os-$$arch . ; \
	done

## fakejunos: build the fake device used for local trials
fakejunos:
	go build -trimpath -o fakejunos ./cmd/fakejunos

## image: build the container image for this host
image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## image-push: build and push a multi-arch image
image-push:
	docker buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest --push .

## helm-lint: lint the chart
helm-lint:
	helm lint $(CHART)

## helm-template: render the chart with default values
helm-template:
	helm template jconfig $(CHART)

## helm-package: package the chart into ./dist
helm-package: helm-lint
	@mkdir -p dist
	helm package $(CHART) -d dist \
		--version $(CHART_VERSION) --app-version $(CHART_VERSION)

## helm-push: push the packaged chart to the OCI registry
helm-push: helm-package
	helm push dist/jconfig-$(CHART_VERSION).tgz $(CHART_REPO)

## release: push the image and the chart
release: image-push helm-push

clean:
	rm -rf $(BINARY) fakejunos dist
