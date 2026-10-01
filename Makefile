BIN      ?= bin/icaro
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/ggrocco/icaro/internal/cli.version=$(VERSION)

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
