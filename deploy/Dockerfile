FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 10001 anuma

WORKDIR /app

COPY gateway/gateway /app/gateway

RUN mkdir -p /data /app/data && chown -R anuma:anuma /data /app/data

USER anuma

EXPOSE 17869

ENV ANUMA_DB_PATH=/app/data/gateway.db \
    ANUMA_PORT=17869 \
    ANUMA_MODEL_CACHE_TTL=600

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=5s \
    CMD wget -q --spider http://127.0.0.1:17869/healthz || exit 1

ENTRYPOINT ["/app/gateway"]
