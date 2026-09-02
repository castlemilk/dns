BUF ?= buf
GO ?= go
PNPM ?= pnpm
GO_FILES := $(shell find cmd internal gen/go -name '*.go' -type f)

.PHONY: build dev-api dev-web fmt generate lint proto-check test test-race tools web-build web-install web-lint

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

dev-web:
	$(PNPM) --dir apps/web dev

tools:
	@command -v buf
	@command -v golangci-lint
