# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/cloudedits ./cmd/cloudedits

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 cloudedits && \
    mkdir -p /data /data/sidecars /library && \
    chown -R cloudedits:cloudedits /data && \
    chmod 1777 /data /data/sidecars

COPY --from=builder /out/cloudedits /usr/local/bin/cloudedits

USER cloudedits
WORKDIR /data

ENV CLOUDEDITS_DATA_DIR=/data
ENV CLOUDEDITS_HOSTNAME=library

VOLUME ["/data", "/library"]

# Pure userspace networking via tsnet — no NET_ADMIN, no /dev/net/tun, no root required.
# The default command serves the documented /library/photos mount over the
# tailnet; a bare `serve` would bind loopback inside the container.
ENTRYPOINT ["/usr/local/bin/cloudedits"]
CMD ["serve", "--tsnet", "--folder", "/library/photos"]
