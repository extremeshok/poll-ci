#!/usr/bin/env bash
#
# poll-ci-heartbeat — dead-man's-switch for the poll-ci promotion pipeline.
#
# poll-ci is a single container on one VPS holding one token; if it dies, the
# token expires, or it silently stops promoting, `release` just stops advancing
# and nobody is paged. This monitor (run from a systemd timer) closes that gap.
#
# It alerts when EITHER:
#   1. the poll-ci container is not running, OR
#   2. the watched branch has been ahead of the target branch by more than
#      MAX_LAG commits AND the target has not advanced for longer than GRACE
#      (i.e. the gate/promote has genuinely stalled — not merely a gate that is
#      legitimately in progress, which clears within one gate duration).
#
# It also (optionally) pings an external dead-man's-switch URL on every HEALTHY
# run, so that if this monitor or the whole host dies, that external service
# alerts you too (defence against the monitor itself failing).
#
# Config is read from the environment; if HEARTBEAT_ENV_FILE (default
# /etc/poll-ci/poll-ci.env) is readable it is sourced first, so GITHUB_TOKEN /
# GITHUB_API / POLL_CI_NAME already set for poll-ci are reused.
#
#   Required: HEARTBEAT_REPO=owner/name   (the repo poll-ci watches)
#   Common:   HEARTBEAT_ALERT_URL=...     (apprise JSON / generic webhook; POST {title,body})
#   Optional: HEARTBEAT_WATCH_BRANCH=master  HEARTBEAT_TARGET_BRANCH=release
#             HEARTBEAT_MAX_LAG=0           (commits master may lead release by before the
#                                            stall clock starts; GRACE alone debounces an
#                                            in-progress gate. >0 would treat a promote
#                                            failure on a single tip commit as healthy
#                                            forever — the exact incident this monitors.)
#             HEARTBEAT_GRACE_SECONDS=2400  (40m > slowest gate; stall threshold)
#             HEARTBEAT_PING_URL=...        (external dead-man's-switch, pinged when healthy)
#             HEARTBEAT_STATE_FILE=/var/lib/poll-ci/heartbeat.state
#             GITHUB_TOKEN=...              (for private repos / higher rate limit)
#
# Exit 0 always (a monitor must not crash the timer); problems are alerted, not raised.

set -uo pipefail

ENV_FILE="${HEARTBEAT_ENV_FILE:-/etc/poll-ci/poll-ci.env}"
# shellcheck disable=SC1090
[ -r "$ENV_FILE" ] && . "$ENV_FILE"

REPO="${HEARTBEAT_REPO:-}"
WATCH="${HEARTBEAT_WATCH_BRANCH:-master}"
TARGET="${HEARTBEAT_TARGET_BRANCH:-release}"
MAX_LAG="${HEARTBEAT_MAX_LAG:-0}"
GRACE="${HEARTBEAT_GRACE_SECONDS:-2400}"
CONTAINER="${POLL_CI_NAME:-poll-ci}"
ALERT_URL="${HEARTBEAT_ALERT_URL:-}"
PING_URL="${HEARTBEAT_PING_URL:-}"
STATE="${HEARTBEAT_STATE_FILE:-/var/lib/poll-ci/heartbeat.state}"
API="${GITHUB_API:-https://api.github.com}"
TOKEN="${GITHUB_TOKEN:-}"
DOCKER="${DOCKER_BIN:-docker}"

log()  { printf '%s poll-ci-heartbeat: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# JSON-escape stdin's first arg for the webhook body.
json_str() { printf '%s' "$1" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' 2>/dev/null \
             || printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }

alert() {
  log "ALERT: $1"
  [ -n "$ALERT_URL" ] || return 0
  curl -fsS -m 15 -X POST -H 'Content-Type: application/json' \
    -d "{\"title\":\"poll-ci heartbeat\",\"body\":$(json_str "$1")}" \
    "$ALERT_URL" >/dev/null 2>&1 || log "warn: alert POST to HEARTBEAT_ALERT_URL failed"
}

ping_ok() {  # external dead-man's-switch: only pinged when everything is healthy
  [ -n "$PING_URL" ] || return 0
  curl -fsS -m 15 "$PING_URL" >/dev/null 2>&1 || log "warn: heartbeat ping to HEARTBEAT_PING_URL failed"
}

if [ -z "$REPO" ]; then
  log "HEARTBEAT_REPO is not set (owner/name); nothing to check."
  exit 0
fi

# --- check 1: container liveness -------------------------------------------
running="$("$DOCKER" inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null | tr -d '[:space:]')"
[ -n "$running" ] || running="missing"
if [ "$running" != "true" ]; then
  alert "poll-ci container '$CONTAINER' is not running (state=$running) on $(hostname) — promotions are stopped."
  exit 0
fi

# --- check 2: promotion progress (master vs release) -----------------------
auth_hdr=()
[ -n "$TOKEN" ] && auth_hdr=(-H "Authorization: Bearer $TOKEN")

cmp="$(curl -fsS -m 25 "${auth_hdr[@]}" -H 'Accept: application/vnd.github+json' \
        "$API/repos/$REPO/compare/$TARGET...$WATCH" 2>/dev/null)"
if [ -z "$cmp" ]; then
  log "warn: compare API call failed (network/token/rate-limit); skipping lag check this run."
  exit 0
fi

# ahead_by = commits WATCH leads TARGET by (pending promotion);
# base_commit.sha = the TARGET (release) tip. Without jq, take the FIRST sha
# after the "base_commit" key — a greedy sed over the whole body would match
# the LAST sha in the payload instead (a nested tree/parent sha).
if command -v jq >/dev/null 2>&1; then
  ahead="$(printf '%s' "$cmp" | jq -r '.ahead_by // 0')"
  release_sha="$(printf '%s' "$cmp" | jq -r '.base_commit.sha // empty')"
else
  ahead="$(printf '%s' "$cmp" | sed -n 's/.*"ahead_by":[[:space:]]*\([0-9]*\).*/\1/p' | head -1)"
  release_sha="$(printf '%s' "$cmp" | tr -d '\n' | awk -F'"base_commit"' 'NF>1{print $2}' \
                 | grep -o '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]\{40\}"' | head -1 \
                 | grep -o '[0-9a-f]\{40\}')"
fi
[ -n "$ahead" ] || ahead=0

now="$(date +%s)"

if [ "$ahead" -le "$MAX_LAG" ] 2>/dev/null; then
  : > "$STATE" 2>/dev/null || true          # healthy: clear stall state
  log "ok: $WATCH is $ahead commit(s) ahead of $TARGET (<= MAX_LAG=$MAX_LAG)."
  ping_ok
  exit 0
fi

# master is ahead of release by > MAX_LAG. Debounce on whether release is
# ADVANCING: if its tip changed since we last looked, promotions are happening,
# so reset the stall clock. Only alert if release is stuck for > GRACE.
mkdir -p "$(dirname "$STATE")" 2>/dev/null || true
prev_sha=""; prev_since=""
[ -r "$STATE" ] && read -r prev_sha prev_since < "$STATE" 2>/dev/null || true

if [ "$prev_sha" != "$release_sha" ] || [ -z "$prev_since" ]; then
  printf '%s %s\n' "$release_sha" "$now" > "$STATE" 2>/dev/null || true
  log "note: $WATCH ahead of $TARGET by $ahead; release tip moved/first-seen — starting stall clock."
  exit 0
fi

stuck_for=$(( now - prev_since ))
if [ "$stuck_for" -gt "$GRACE" ]; then
  alert "$WATCH is $ahead commit(s) ahead of $TARGET and $TARGET has not advanced for $((stuck_for/60))m (> $((GRACE/60))m). poll-ci gate/promote appears stalled (red gate, dead runner, or expired token) on $(hostname)."
else
  log "note: $WATCH ahead by $ahead; $TARGET unchanged for $((stuck_for/60))m (grace $((GRACE/60))m) — gate likely in progress."
fi
exit 0
