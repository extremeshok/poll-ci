#!/usr/bin/env bash
#
# install.sh — install poll-ci as a systemd-managed Docker service.
#
#   sudo deploy/install.sh              # install + enable, incl. the daily self-update timer
#   sudo deploy/install.sh --build      # also build the image from this checkout
#   sudo deploy/install.sh --start      # also (re)start once config is in place
#   sudo deploy/install.sh --noautoupdate # skip the daily image self-update timer
#   sudo deploy/install.sh --heartbeat  # also install the dead-man's-switch timer
#
# Creates (config files only if missing — re-running never overwrites secrets):
#   /etc/poll-ci/poll-ci.env            0600, holds GITHUB_TOKEN
#   /etc/poll-ci/repos.yml              repos to watch
#   /etc/systemd/system/poll-ci.service + a drop-in pinning the image
#   /usr/local/bin/poll-ci-autoupdate                      the image-update script
#   /etc/systemd/system/poll-ci-update.{service,timer}     the daily upgrade check (--noautoupdate to skip)
# With --heartbeat, also:
#   /usr/local/bin/poll-ci-heartbeat                       the monitor script
#   /etc/poll-ci/heartbeat.env                             0640, HEARTBEAT_* config
#   /etc/systemd/system/poll-ci-heartbeat.{service,timer}  the periodic check
#
# Override the image/name:  POLL_CI_IMAGE=… POLL_CI_NAME=… sudo deploy/install.sh
set -euo pipefail

IMAGE="${POLL_CI_IMAGE:-ghcr.io/extremeshok/poll-ci:latest}"
NAME="${POLL_CI_NAME:-poll-ci}"
CONF_DIR=/etc/poll-ci
UNIT_DST=/etc/systemd/system/poll-ci.service
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_SRC="${SRC_DIR}/systemd/poll-ci.service"

DO_BUILD=false
DO_START=false
DO_HEARTBEAT=false
DO_AUTOUPDATE=true   # daily image self-update is on by default; --noautoupdate opts out
for a in "$@"; do
  case "$a" in
    --build) DO_BUILD=true ;;
    --start) DO_START=true ;;
    --heartbeat) DO_HEARTBEAT=true ;;
    --autoupdate) DO_AUTOUPDATE=true ;;
    --noautoupdate) DO_AUTOUPDATE=false ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $a" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
command -v docker    >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemd is required" >&2; exit 1; }
[ -f "$UNIT_SRC" ] || { echo "missing unit file: $UNIT_SRC" >&2; exit 1; }

if [ "$DO_BUILD" = true ]; then
  echo ">> building ${IMAGE} from ${SRC_DIR}/.."
  docker build -t "$IMAGE" "${SRC_DIR}/.."
fi

install -d -m 0750 "$CONF_DIR"

if [ ! -f "$CONF_DIR/poll-ci.env" ]; then
  ( umask 077
    cat > "$CONF_DIR/poll-ci.env" <<'ENV'
# poll-ci config — THIS FILE HOLDS THE TOKEN ONLY. Keep it 0600.
# Fine-grained PAT repository permissions:
#   Contents: Read and write        (read = clone + branch HEAD; write = promote:)
#   Commit statuses: Read and write
# The repo list lives in repos.yml; REPOS_FILE is set by the systemd unit. So
# this file can safely contain just GITHUB_TOKEN.
GITHUB_TOKEN=REPLACE_WITH_TOKEN
# Optional overrides:
# POLL_INTERVAL=60
# POLL_PRS=true
# DEFAULT_IMAGE=alpine:latest
ENV
  )
  chmod 600 "$CONF_DIR/poll-ci.env"
  echo ">> created $CONF_DIR/poll-ci.env  (edit it: set GITHUB_TOKEN)"
else
  echo ">> keeping existing $CONF_DIR/poll-ci.env"
fi

if [ ! -f "$CONF_DIR/repos.yml" ]; then
  cat > "$CONF_DIR/repos.yml" <<'YML'
repos:
  - repo: extremeshok/dnscontrol-ui
    branch: master
YML
  echo ">> created $CONF_DIR/repos.yml  (edit it for your repo[s])"
else
  echo ">> keeping existing $CONF_DIR/repos.yml"
fi

install -m 0644 "$UNIT_SRC" "$UNIT_DST"
# Drop-in pins the image/name this host runs (overrides the unit defaults).
install -d -m 0755 "${UNIT_DST}.d"
cat > "${UNIT_DST}.d/10-image.conf" <<EOF
[Service]
Environment=POLL_CI_IMAGE=${IMAGE}
Environment=POLL_CI_NAME=${NAME}
EOF

if [ "$DO_HEARTBEAT" = true ]; then
  install -m 0755 "${SRC_DIR}/heartbeat-monitor.sh" /usr/local/bin/poll-ci-heartbeat
  install -m 0644 "${SRC_DIR}/systemd/poll-ci-heartbeat.service" /etc/systemd/system/poll-ci-heartbeat.service
  install -m 0644 "${SRC_DIR}/systemd/poll-ci-heartbeat.timer"   /etc/systemd/system/poll-ci-heartbeat.timer
  if [ ! -f "$CONF_DIR/heartbeat.env" ]; then
    ( umask 027
      cat > "$CONF_DIR/heartbeat.env" <<'HENV'
# poll-ci-heartbeat config. The monitor also sources poll-ci.env for GITHUB_TOKEN.
HEARTBEAT_REPO=extremeshok/dnscontrol-ui
# Where to send alerts (apprise JSON endpoint or any webhook taking {title,body}):
# HEARTBEAT_ALERT_URL=
# Optional external dead-man's-switch pinged on every HEALTHY run (e.g. healthchecks.io):
# HEARTBEAT_PING_URL=
# Tuning (defaults shown). MAX_LAG=0: GRACE alone debounces an in-progress
# gate; a higher value would never alert on a promote failure affecting only
# the tip commit.
# HEARTBEAT_WATCH_BRANCH=master
# HEARTBEAT_TARGET_BRANCH=release
# HEARTBEAT_MAX_LAG=0
# HEARTBEAT_GRACE_SECONDS=2400
HENV
    )
    echo ">> created $CONF_DIR/heartbeat.env  (set HEARTBEAT_ALERT_URL)"
  else
    echo ">> keeping existing $CONF_DIR/heartbeat.env"
  fi
fi

if [ "$DO_AUTOUPDATE" = true ]; then
  install -m 0755 "${SRC_DIR}/autoupdate.sh" /usr/local/bin/poll-ci-autoupdate
  install -m 0644 "${SRC_DIR}/systemd/poll-ci-update.service" /etc/systemd/system/poll-ci-update.service
  install -m 0644 "${SRC_DIR}/systemd/poll-ci-update.timer"   /etc/systemd/system/poll-ci-update.timer
fi

systemctl daemon-reload
systemctl enable poll-ci.service >/dev/null
echo ">> installed + enabled poll-ci.service (image ${IMAGE})"
if [ "$DO_HEARTBEAT" = true ]; then
  systemctl enable --now poll-ci-heartbeat.timer >/dev/null
  echo ">> installed + enabled poll-ci-heartbeat.timer (edit ${CONF_DIR}/heartbeat.env, set HEARTBEAT_ALERT_URL)"
fi
if [ "$DO_AUTOUPDATE" = true ]; then
  systemctl enable --now poll-ci-update.timer >/dev/null
  echo ">> installed + enabled poll-ci-update.timer (daily image self-update; apply now with: systemctl start poll-ci-update.service)"
fi

if [ "$DO_START" = true ]; then
  systemctl restart poll-ci.service
  echo ">> (re)started — follow logs:  journalctl -u poll-ci -f"
else
  echo
  echo "Next:"
  echo "  1. edit ${CONF_DIR}/poll-ci.env   (set GITHUB_TOKEN)"
  echo "  2. edit ${CONF_DIR}/repos.yml      (your repo[s])"
  echo "  3. sudo systemctl start poll-ci"
  echo "  4. journalctl -u poll-ci -f"
fi
