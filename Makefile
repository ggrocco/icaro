BIN      ?= bin/icaro
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X icaro/internal/cli.version=$(VERSION)

.PHONY: build web web-test test test-integration lint generate fmt tidy upgrade clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/icaro

# Builds the React UI into web/dist, which the binary embeds.
web:
	cd web && npm ci && npm run build

web-test:
	cd web && npm run typecheck && npm test

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

# Upgrades Go modules and npm packages to their latest minor/patch releases
# and regenerates the third-party license lists. Major versions stay manual.
upgrade:
	go get -u -t $(PKG)
	go mod tidy
	cd web && npm update --save
	go run ./hack/licenses
	go run ./hack/licenses -npm web -out web/THIRD_PARTY_LICENSES.md

clean:
	rm -rf bin dist coverage.out
	find web/dist -mindepth 1 ! -name README.txt -delete 2>/dev/null || true

AGENT_IMAGE ?= ghcr.io/icaro-dev/agent-base:$(VERSION)
AGENT_ARCH  ?= $(shell go env GOARCH)

# The agent base image embeds a static Linux icaro for internal commands.
.PHONY: image-agent
image-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go build -ldflags '$(LDFLAGS)' -o bin/icaro-linux ./cmd/icaro
	docker build -f images/agent-base/Dockerfile -t $(AGENT_IMAGE) .
