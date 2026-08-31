# syntax=docker/dockerfile:1
#
# Multi-stage build. The builder stage clones blueship as a sibling directory
# so the `replace github.com/rasimio/blueship => ../blueship` directive in
# go.mod resolves. BLUESHIP_REV pins the blueship revision; override with
# `docker build --build-arg BLUESHIP_REV=<sha>` for reproducible builds.

FROM golang:1.26-alpine AS builder

ARG BLUESHIP_REPO=https://github.com/rasimio/blueship.git
ARG BLUESHIP_REV=2779acdb62c240f9953cb108d21ff9a636b7c1e6

RUN apk add --no-cache git ca-certificates

WORKDIR /src
RUN git clone ${BLUESHIP_REPO} /src/blueship && \
    cd /src/blueship && \
    git checkout --detach ${BLUESHIP_REV}

WORKDIR /src/claude-proxy
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/claude-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/claude-proxy /usr/local/bin/claude-proxy

WORKDIR /app
ENV TOKEN_FILE=/data/anthropic-tokens.json
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/claude-proxy"]
CMD ["serve"]
