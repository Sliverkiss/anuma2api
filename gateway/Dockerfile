# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Copy module manifests first for layer caching.
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# modernc.org/sqlite is pure Go; CGO_ENABLED=0 compiles it without cgo.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gateway ./cmd/server

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 anuma

WORKDIR /app

COPY --from=builder /out/gateway /app/gateway

# accounts.csv is mounted at runtime (see docker-compose.yml).
RUN mkdir -p /data /app/data && chown anuma:anuma /data /app/data

USER anuma

EXPOSE 7895

ENV ANUMA_DB_PATH=/app/data/gateway.db \
    ANUMA_PORT=7895 \
    ANUMA_MODEL_CACHE_TTL=600

# Health probe (SPEC-auth §2). wget ships with alpine:3.20.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=5s \
    CMD wget -q --spider http://127.0.0.1:7895/healthz || exit 1

ENTRYPOINT ["/app/gateway"]
