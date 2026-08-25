# syntax=docker/dockerfile:1.7

# ---------- frontend ----------
FROM docker.io/oven/bun:1.3 AS frontend-builder

# node-canvas is a build-time dep (frontend/vitePlugin.ts) and ships prebuilt
# binaries, so it needs the runtime shared libs only -- not the -dev headers.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && apt-get install -y --no-install-recommends \
    libcairo2 libpango-1.0-0 libpangocairo-1.0-0 libjpeg62-turbo libgif7 librsvg2-2

WORKDIR /app/frontend
COPY frontend/bun.lock frontend/package.json ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY frontend/ ./
RUN bunx vite build

# ---------- backend ----------
FROM docker.io/library/golang:1.25-alpine AS backend-builder

ARG GIT_COMMIT=unknown
ARG GIT_VERSION=dev

RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 \
    go build -ldflags="-w -s -X main.CommitSHA=${GIT_COMMIT} -X main.Version=${GIT_VERSION}" \
    -o /out/vault-server ./cmd/server

# ---------- runtime ----------
FROM docker.io/library/alpine:3.22

RUN apk add --no-cache ca-certificates ffmpeg sqlite wget && \
    addgroup -g 1000 vault && \
    adduser -D -u 1000 -G vault vault

USER 1000:1000
WORKDIR /app

COPY migrations ./migrations
COPY --from=frontend-builder /app/frontend/dist ./frontend/dist
COPY --from=backend-builder /out/vault-server .

VOLUME /app/data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/health || exit 1

CMD ["./vault-server"]
