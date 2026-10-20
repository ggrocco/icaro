BIN      ?= bin/icaro
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X icaro/internal/cli.version=$(VERSION)

.PHONY: build test test-integration lint generate fmt tidy clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/icaro

test:
	go test -race -count=1 $(PKG)

# Needs a Docker daemon; set ICARO_TEST_POSTGRES_DSN to also run the Postgres matrix.
test-integration:
	go test -race -count=1 -tags integration $(PKG)

lint:
	golangci-lint run ./...

generate:
	go generate $(PKG)

fmt:
	gofmt -s -w $$(git ls-files '*.go')

tidy:
	go mod tidy

clean:
	rm -rf bin dist coverage.out

AGENT_IMAGE ?= ghcr.io/icaro-dev/agent-base:$(VERSION)
AGENT_ARCH  ?= $(shell go env GOARCH)

# The agent base image embeds a static Linux icaro for internal commands.
.PHONY: image-agent
image-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go build -ldflags '$(LDFLAGS)' -o bin/icaro-linux ./cmd/icaro
	docker build -f images/agent-base/Dockerfile -t $(AGENT_IMAGE) .
