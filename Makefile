BUF ?= buf
GO ?= go
PNPM ?= pnpm
GO_FILES := $(shell find cmd internal gen/go -name '*.go' -type f)

.PHONY: build dev-api dev-web fmt generate lint proto-check test test-race \
	test-integration test-integration-deephost test-integration-stalwart \
	tools web-build web-install web-lint web-observability

# Local engines used by the integration targets. Override on the command line:
#   make test-integration-deephost DEEPHOST_TEST_DEEPHOST_URL=http://127.0.0.1:30081
DEEPHOST_TEST_DEEPHOST_URL ?= http://127.0.0.1:30081
DEEPHOST_TEST_STALWART_URL ?=
DEEPHOST_TEST_STALWART_TOKEN ?=
# Set by scripts/dev/stalwart-up.sh and scripts/dev/stalwart-webhook.sh; the
# SMTP address and the webhook trio are optional, and the tests that need them
# skip when they are empty.
DEEPHOST_TEST_STALWART_SMTP ?=
DEEPHOST_TEST_STALWART_WEBHOOK_SECRET ?=
DEEPHOST_TEST_STALWART_WEBHOOK_PORT ?=
DEEPHOST_TEST_STALWART_WEBHOOK_PATH ?=

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

# The end-to-end suites: the real components over real sockets, no engine URLs
# and no cluster required, so this runs anywhere `make test` does. They carry no
# build tag either, so `make test` already includes them — this target exists to
# run just them while working on the paths they cover.
#
# TestDNSJourney*  a record from the bbolt store, through the real snapshot feed
#                  over HTTP, into a real authority answering on a real UDP and
#                  TCP socket. It exists because every layer once agreed with the
#                  others while all of them disagreed with what the customer
#                  typed, which only a test spanning them can catch.
# TestEndToEnd*    the mail journey: SMTP in, POP3 out, real queue, real relay.
test-e2e:
	$(GO) test -race -count=1 -run 'TestDNSJourney|TestEndToEnd' ./internal/app/... ./internal/maild/...

# Integration tests carry no build tag: they compile under `go test ./...`,
# `go vet` and golangci-lint, and skip themselves when their engine URL is
# unset. These targets set the URL, so they actually run.
#
# Every recipe line that mentions DEEPHOST_TEST_STALWART_TOKEN is prefixed with
# `@`: make echoes a recipe before running it, so an un-prefixed line would
# print the live Stalwart API key to the terminal and into any CI log.

test-integration-deephost:
	DEEPHOST_TEST_DEEPHOST_URL=$(DEEPHOST_TEST_DEEPHOST_URL) \
		$(GO) test -count=1 -run Integration ./internal/hosting/...

test-integration-stalwart:
	@test -n "$(DEEPHOST_TEST_STALWART_URL)" || \
		{ echo "set DEEPHOST_TEST_STALWART_URL (and DEEPHOST_TEST_STALWART_TOKEN); start one with scripts/dev/stalwart-up.sh" >&2; exit 1; }
	@DEEPHOST_TEST_STALWART_URL=$(DEEPHOST_TEST_STALWART_URL) \
	DEEPHOST_TEST_STALWART_TOKEN=$(DEEPHOST_TEST_STALWART_TOKEN) \
	DEEPHOST_TEST_STALWART_SMTP=$(DEEPHOST_TEST_STALWART_SMTP) \
	DEEPHOST_TEST_STALWART_WEBHOOK_SECRET=$(DEEPHOST_TEST_STALWART_WEBHOOK_SECRET) \
	DEEPHOST_TEST_STALWART_WEBHOOK_PORT=$(DEEPHOST_TEST_STALWART_WEBHOOK_PORT) \
	DEEPHOST_TEST_STALWART_WEBHOOK_PATH=$(DEEPHOST_TEST_STALWART_WEBHOOK_PATH) \
		$(GO) test -count=1 -run Integration ./internal/mail/...

test-integration:
	@DEEPHOST_TEST_DEEPHOST_URL=$(DEEPHOST_TEST_DEEPHOST_URL) \
	DEEPHOST_TEST_STALWART_URL=$(DEEPHOST_TEST_STALWART_URL) \
	DEEPHOST_TEST_STALWART_TOKEN=$(DEEPHOST_TEST_STALWART_TOKEN) \
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
