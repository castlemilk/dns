BUF ?= buf
GO ?= go
PNPM ?= pnpm
GO_FILES := $(shell find cmd internal gen/go -name '*.go' -type f)

.PHONY: build dev-api dev-web fmt generate lint proto-check test test-race \
	test-integration test-integration-deephost test-integration-stalwart \
	tools web-build web-install web-lint web-observability

# Local engines used by the integration targets. Override on the command line:
#   make test-integration-deephost SIMPLE_TEST_DEEPHOST_URL=http://127.0.0.1:30081
SIMPLE_TEST_DEEPHOST_URL ?= http://127.0.0.1:30081
SIMPLE_TEST_STALWART_URL ?=
SIMPLE_TEST_STALWART_TOKEN ?=
# Set by scripts/dev/stalwart-up.sh and scripts/dev/stalwart-webhook.sh; the
# SMTP address and the webhook trio are optional, and the tests that need them
# skip when they are empty.
SIMPLE_TEST_STALWART_SMTP ?=
SIMPLE_TEST_STALWART_WEBHOOK_SECRET ?=
SIMPLE_TEST_STALWART_WEBHOOK_PORT ?=
SIMPLE_TEST_STALWART_WEBHOOK_PATH ?=

generate:
	$(BUF) generate

proto-check:
	$(BUF) lint
	$(BUF) generate
	git diff --exit-code -- gen/go apps/web/gen

fmt:
	gofmt -w $(GO_FILES)

lint:
	golangci-lint run ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

# Integration tests carry no build tag: they compile under `go test ./...`,
# `go vet` and golangci-lint, and skip themselves when their engine URL is
# unset. These targets set the URL, so they actually run.
#
# Every recipe line that mentions SIMPLE_TEST_STALWART_TOKEN is prefixed with
# `@`: make echoes a recipe before running it, so an un-prefixed line would
# print the live Stalwart API key to the terminal and into any CI log.

test-integration-deephost:
	SIMPLE_TEST_DEEPHOST_URL=$(SIMPLE_TEST_DEEPHOST_URL) \
		$(GO) test -count=1 -run Integration ./internal/hosting/...

test-integration-stalwart:
	@test -n "$(SIMPLE_TEST_STALWART_URL)" || \
		{ echo "set SIMPLE_TEST_STALWART_URL (and SIMPLE_TEST_STALWART_TOKEN); start one with scripts/dev/stalwart-up.sh" >&2; exit 1; }
	@SIMPLE_TEST_STALWART_URL=$(SIMPLE_TEST_STALWART_URL) \
	SIMPLE_TEST_STALWART_TOKEN=$(SIMPLE_TEST_STALWART_TOKEN) \
	SIMPLE_TEST_STALWART_SMTP=$(SIMPLE_TEST_STALWART_SMTP) \
	SIMPLE_TEST_STALWART_WEBHOOK_SECRET=$(SIMPLE_TEST_STALWART_WEBHOOK_SECRET) \
	SIMPLE_TEST_STALWART_WEBHOOK_PORT=$(SIMPLE_TEST_STALWART_WEBHOOK_PORT) \
	SIMPLE_TEST_STALWART_WEBHOOK_PATH=$(SIMPLE_TEST_STALWART_WEBHOOK_PATH) \
		$(GO) test -count=1 -run Integration ./internal/mail/...

test-integration:
	@SIMPLE_TEST_DEEPHOST_URL=$(SIMPLE_TEST_DEEPHOST_URL) \
	SIMPLE_TEST_STALWART_URL=$(SIMPLE_TEST_STALWART_URL) \
	SIMPLE_TEST_STALWART_TOKEN=$(SIMPLE_TEST_STALWART_TOKEN) \
		$(GO) test -count=1 -run Integration ./...

build:
	$(GO) build ./...

dev-api:
	$(GO) run ./cmd/dns

web-install:
	$(PNPM) --dir apps/web install --frozen-lockfile

web-lint:
	$(PNPM) --dir apps/web lint

web-build:
	$(PNPM) --dir apps/web build

web-observability: web-build
	$(PNPM) --dir apps/web test:observability

dev-web:
	$(PNPM) --dir apps/web dev

tools:
	@command -v buf
	@command -v golangci-lint
