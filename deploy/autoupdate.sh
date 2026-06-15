#!/usr/bin/env bash
#
# poll-ci-autoupdate — pull the poll-ci engine image and, ONLY if its digest
# changed, restart poll-ci.service (whose ExecStartPre re-pulls + recreates the
# container). Driven by poll-ci-update.timer; runs as root. A no-op on days with
# no new image, so it never churns an in-flight gate needlessly.
#
# Image/name are read from the running poll-ci.service (the install drop-in sets
# POLL_CI_IMAGE / POLL_CI_NAME), with the published defaults as a fallback.
set -euo pipefail

env="$(systemctl show poll-ci.service -p Environment --value 2>/dev/null || true)"
image="$(printf '%s\n' $env | sed -n 's/^POLL_CI_IMAGE=//p')"; image="${image:-ghcr.io/extremeshok/poll-ci:latest}"
name="$(printf '%s\n' $env | sed -n 's/^POLL_CI_NAME=//p')";   name="${name:-poll-ci}"

before="$(docker inspect --format '{{.Image}}' "$name" 2>/dev/null || echo none)"
docker pull -q "$image" >/dev/null
after="$(docker image inspect --format '{{.Id}}' "$image")"

if [ "$before" = "$after" ]; then
  echo "poll-ci already on current image ($after)"
  exit 0
fi
echo "poll-ci image changed: $before -> $after — restarting ${name}.service"
systemctl restart "${name}.service"
