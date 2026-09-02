# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder

ARG TARGETOS
ARG TARGETARCH

ENV GOTOOLCHAIN=auto

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY LICENSE ./LICENSE
COPY cmd ./cmd
COPY gen/go ./gen/go
COPY internal ./internal
COPY tools/collect-go-licenses ./tools/collect-go-licenses

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/dns ./cmd/dns && \
    go run ./tools/collect-go-licenses \
      -out /out/licenses \
      -project-license ./LICENSE \
      -target ./cmd/dns && \
    mkdir -p /out/data

FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

WORKDIR /

COPY --from=builder /out/dns /dns
COPY --from=builder --chown=65532:65532 /out/data /data
COPY --from=builder /out/licenses /licenses

LABEL org.opencontainers.image.licenses="Apache-2.0"

USER 65532:65532

EXPOSE 1053/udp 1053/tcp 8080/tcp

ENTRYPOINT ["/dns"]
