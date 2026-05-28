# syntax=docker/dockerfile:1
#
# Multi-stage build. The builder stage clones blueship as a sibling directory
# so the `replace github.com/rasimio/blueship => ../blueship` directive in
# go.mod resolves. BLUESHIP_REV pins the blueship revision (defaults to main);
# override with `docker build --build-arg BLUESHIP_REV=<sha>` for reproducible
# builds.

FROM golang:1.26-alpine AS builder

ARG BLUESHIP_REPO=https://github.com/rasimio/blueship.git
ARG BLUESHIP_REV=main

RUN apk add --no-cache git ca-certificates

WORKDIR /src
RUN git clone --depth 1 --branch ${BLUESHIP_REV} ${BLUESHIP_REPO} /src/blueship

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
