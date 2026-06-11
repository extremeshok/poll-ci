# syntax=docker/dockerfile:1
#
# poll-ci engine image: the static binary + git + the Docker CLI. Checks
# themselves run in the repo-declared image via the mounted Docker socket, so
# this image stays tiny.

# --- build stage ------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/poll-ci .

# --- runtime stage ----------------------------------------------------------
# apk upgrade picks up post-release security fixes for the base packages — a
# stale base is exactly how image scans (trivy etc.) start blocking deploys.
FROM alpine:3.24
RUN apk -U upgrade --no-cache && \
    apk add --no-cache ca-certificates git docker-cli tini
COPY --from=build /out/poll-ci /usr/local/bin/poll-ci

# State (the tested-SHA set) lives here; mount a volume to persist it.
ENV WORK_DIR=/var/lib/poll-ci
VOLUME /var/lib/poll-ci

# tini reaps any stray children and forwards signals for a clean shutdown.
ENTRYPOINT ["/sbin/tini", "--", "poll-ci"]
