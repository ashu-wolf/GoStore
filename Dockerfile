# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gostore ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 -S gostore \
    && adduser -u 10001 -S -G gostore -H -s /sbin/nologin gostore \
    && mkdir -p /data \
    && chown 10001:10001 /data

COPY --from=builder /out/gostore /usr/local/bin/gostore

ENV GOSTORE_ADDR=:9000 \
    GOSTORE_DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 9000

USER 10001:10001

HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:9000/-/readyz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/gostore"]
