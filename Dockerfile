# syntax=docker/dockerfile:1
# =============================================================================
#  Sync — single image build (frontend + backend + runtime)
#  Stage 1: build the Vue 3 SPA (web/dist)
#  Stage 2: build the Go binary with the SPA embedded
#  Stage 3: minimal Alpine runtime (non-root, healthcheck)
#
#  Published by .github/workflows/build.yml as
#      ghcr.io/<owner>/sync:latest  and  ghcr.io/<owner>/sync:<version>
#  The workflow injects VERSION and COMMIT as build arguments.
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1 — frontend (Vue 3 + Vite + TailwindCSS)
# -----------------------------------------------------------------------------
FROM node:22-alpine AS frontend

WORKDIR /app/web

# Dependency layer: only manifests, so `npm ci` is cached between source edits.
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

# Sources (web/dist and web/node_modules are excluded by .dockerignore).
COPY web/ ./

ARG VERSION=dev
ARG COMMIT=unknown
ENV VITE_APP_VERSION=${VERSION} \
    VITE_APP_COMMIT=${COMMIT}
RUN rm -rf dist && npm run build

# -----------------------------------------------------------------------------
# Stage 2 — backend (Go + Gin + GORM)
# -----------------------------------------------------------------------------
FROM golang:1.27-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

RUN apk add --no-cache git ca-certificates tzdata

# Dependency layer: cached unless go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Sources.
COPY . ./

# The compiled SPA replaces the placeholder committed in web/dist (.gitkeep),
# so the runtime binary serves the real frontend through the embed FS.
COPY --from=frontend /app/web/dist ./web/dist

ENV CGO_ENABLED=0 \
    GOOS=linux
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/ivancarlosti/sync/internal/version.Version=${VERSION} \
        -X github.com/ivancarlosti/sync/internal/version.Commit=${COMMIT}" \
      -o /out/sync ./cmd/server

# -----------------------------------------------------------------------------
# Stage 3 — runtime
# -----------------------------------------------------------------------------
FROM alpine:3.22

# ca-certificates: OAuth/Graph/REST calls over TLS.
# tzdata: correct local time in logs, cron-like scheduling and token expiry.
# The account is called `app` and not `sync`: Alpine already ships a `sync` user
# (/bin/sync), so `adduser sync` would fail and the image would not build.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S app \
 && adduser -S -G app -h /app app

COPY --from=builder /out/sync /usr/local/bin/sync

WORKDIR /app
USER app

ENV APP_PORT=3000
EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O- "http://127.0.0.1:${APP_PORT}/api/health" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/sync"]
