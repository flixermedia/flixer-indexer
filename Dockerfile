FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/flixer-indexer ./cmd/flixer-indexer

FROM alpine:3.22 AS runtime
ARG APP_VERSION=dev
RUN apk add --no-cache ca-certificates curl ffmpeg \
    && adduser -D -H -u 65532 indexer \
    && mkdir -p /data/cache \
    && chown -R indexer:indexer /data/cache

COPY --from=build /out/flixer-indexer /usr/local/bin/flixer-indexer

ENV APP_VERSION=${APP_VERSION} \
    PORT=8092 \
    FFMPEG_PATH=ffmpeg \
    FFPROBE_PATH=ffprobe \
    CACHE_PATH=/data/cache

USER indexer
EXPOSE 8092

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/flixer-indexer"]
