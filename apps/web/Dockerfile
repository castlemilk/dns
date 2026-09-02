# syntax=docker/dockerfile:1.7

FROM node:24.19.0-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS base

ARG PNPM_VERSION=11.23.0

ENV NEXT_TELEMETRY_DISABLED=1

RUN npm install --global "pnpm@${PNPM_VERSION}"

FROM base AS dependencies

WORKDIR /app

COPY apps/web/package.json apps/web/pnpm-lock.yaml apps/web/pnpm-workspace.yaml ./
RUN --mount=type=cache,target=/pnpm/store,sharing=locked \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile

FROM base AS builder

WORKDIR /app

COPY --from=dependencies /app/node_modules ./node_modules
COPY apps/web/ ./
COPY LICENSE /project-license/LICENSE

RUN pnpm build && \
    node scripts/collect-standalone-licenses.mjs \
      --output .next/licenses \
      --project-license /project-license/LICENSE \
      --source-root . \
      --standalone .next/standalone

FROM gcr.io/distroless/nodejs24-debian13:nonroot@sha256:774b7d020b24214835769e24c3544835526cd0288f0b094eae48e8b2c2429a79 AS runner

ENV HOSTNAME=0.0.0.0 \
    NEXT_TELEMETRY_DISABLED=1 \
    NODE_ENV=production \
    PORT=3000

WORKDIR /app

COPY --from=builder --chown=65532:65532 /app/public ./public
COPY --from=builder --chown=65532:65532 /app/.next/standalone ./
COPY --from=builder --chown=65532:65532 /app/.next/static ./.next/static
COPY --from=builder /app/.next/licenses /licenses

LABEL org.opencontainers.image.licenses="Apache-2.0"

USER 65532:65532

EXPOSE 3000/tcp

CMD ["server.js"]
