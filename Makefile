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
# CI passes COVERFLAGS to gate the Docker adapter's coverage (ADR 0025).
test-integration:
	go test -race -count=1 -tags integration $(COVERFLAGS) $(PKG)

# Coverage of the whole test suite, not only unit tests (needs Docker; set
# ICARO_TEST_POSTGRES_DSN to include the Postgres matrix). Test binaries
# write to coverage/unit and coverage/integration; every icaro binary the
# suites build or launch writes to coverage/bin through GOCOVERDIR. The
# edge packages that unit tests barely reach are printed per suite.
COVERDIR   ?= $(CURDIR)/coverage
COVERPKG   := icaro/...
COVER_EDGE := icaro/internal/cli,icaro/internal/app,icaro/internal/container/docker

.PHONY: cover-e2e
cover-e2e:
	rm -rf $(COVERDIR) && mkdir -p $(COVERDIR)/unit $(COVERDIR)/integration $(COVERDIR)/bin
	CGO_ENABLED=0 go build -cover -coverpkg=$(COVERPKG) -ldflags '$(LDFLAGS)' -o $(COVERDIR)/icaro ./cmd/icaro
	go test -count=1 -cover -coverpkg=$(COVERPKG) $(PKG) -args -test.gocoverdir=$(COVERDIR)/unit
	GOCOVERDIR=$(COVERDIR)/bin go test -count=1 -tags integration -cover -coverpkg=$(COVERPKG) $(PKG) -args -test.gocoverdir=$(COVERDIR)/integration
	GOCOVERDIR=$(COVERDIR)/bin ICARO_E2E_BIN=$(COVERDIR)/icaro go test -count=1 -tags e2e ./hack/e2e ./hack/quickstart
	@echo "== unit";             go tool covdata percent -i=$(COVERDIR)/unit -pkg=$(COVER_EDGE)
	@echo "== unit+integration"; go tool covdata percent -i=$(COVERDIR)/unit,$(COVERDIR)/integration -pkg=$(COVER_EDGE)
	@echo "== all suites";       go tool covdata percent -i=$(COVERDIR)/unit,$(COVERDIR)/integration,$(COVERDIR)/bin -pkg=$(COVER_EDGE)
	go tool covdata textfmt -i=$(COVERDIR)/unit,$(COVERDIR)/integration,$(COVERDIR)/bin -o $(COVERDIR)/all.out
	@echo "merged profile: $(COVERDIR)/all.out (go tool cover -html=$(COVERDIR)/all.out)"

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
	cd desktop && go mod tidy
	go run ./hack/licenses -dir desktop -goos darwin -pkg . -out THIRD_PARTY_LICENSES.md

clean:
	rm -rf bin dist coverage.out coverage
	find web/dist -mindepth 1 ! -name README.txt -delete 2>/dev/null || true

# The desktop app (ADR 0027) is its own module under desktop/ and needs cgo:
# Xcode command line tools on macOS, GTK 4 and WebKitGTK 6 headers on Linux
# (libgtk-4-dev libwebkitgtk-6.0-dev). app-macos builds bin/Icaro.app.
APP_BIN ?= bin/icaro-app

.PHONY: app app-test app-macos
app:
	cd desktop && go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o ../$(APP_BIN) .

app-test:
	cd desktop && go vet ./... && go test -race -count=1 ./...

app-macos:
	desktop/build/bundle.sh bin $(VERSION)

AGENT_IMAGE ?= ghcr.io/icaro-dev/agent-base:$(VERSION)
AGENT_ARCH  ?= $(shell go env GOARCH)

# The agent base image embeds a static Linux icaro for internal commands.
.PHONY: image-agent
image-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go build -ldflags '$(LDFLAGS)' -o bin/icaro-linux ./cmd/icaro
	docker build -f images/agent-base/Dockerfile -t $(AGENT_IMAGE) .
