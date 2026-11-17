#!/bin/bash
# Runs mac-agent and its reverse SSH tunnel as one supervised unit.
#
# Any unrecoverable problem exits non-zero; launchd (KeepAlive) restarts us, so
# recovery is always a clean cold start rather than a half-torn-down state.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="$SCRIPT_DIR/mac-agent"
ENV_FILE="$SCRIPT_DIR/.env"

# launchd starts us with a minimal PATH; ffmpeg and go live in Homebrew prefixes.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*"; }
die() { log "FATAL: $*"; exit 1; }

[ -f "$ENV_FILE" ] || die "missing $ENV_FILE"
set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

VPS_HOST="${MAC_AGENT_VPS_HOST:-}"
VPS_USER="${MAC_AGENT_VPS_USER:-root}"
VPS_PORT="${MAC_AGENT_VPS_PORT:-22}"
TUNNEL_PORT="${MAC_AGENT_TUNNEL_PORT:-9443}"
DOMAIN="${MAC_AGENT_DOMAIN:-}"
SSH_KEY="${MAC_AGENT_SSH_KEY:-$HOME/.ssh/id_ed25519}"

[ -n "$VPS_HOST" ] || die "MAC_AGENT_VPS_HOST is not set"
[ -n "${MAC_AGENT_PASSWORD:-}" ] || die "MAC_AGENT_PASSWORD is not set"

SCREEN_INDEX="${MAC_AGENT_SCREEN_INDEX:-0}"
FPS="${MAC_AGENT_FPS:-30}"
WIDTH="${MAC_AGENT_WIDTH:-1600}"
BITRATE="${MAC_AGENT_BITRATE:-6M}"

AGENT_PID=""
SSH_PID=""

# The agent handles SIGTERM by releasing the viewer, which closes ffmpeg's stdin
# and lets the encoder exit by itself. Only escalate if it does not take the hint.
stop_pid() {
  local pid="$1"
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill "$pid" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.5
  done
  kill -9 "$pid" 2>/dev/null
  return 0
}

cleanup() {
  stop_pid "$AGENT_PID"
  stop_pid "$SSH_PID"
  return 0
}
trap 'log "signalled, shutting down"; cleanup; exit 0' TERM INT
trap cleanup EXIT

ssh_base=(
  ssh
  -i "$SSH_KEY"
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o StrictHostKeyChecking=accept-new
  -o ConnectTimeout=10
  -p "$VPS_PORT"
)

# ── Build if needed ───────────────────────────────────────────────────────────

if [ ! -x "$BINARY" ]; then
  # The staged install ships a prebuilt binary and no sources; only a run straight
  # out of the checkout can rebuild.
  [ -f "$SCRIPT_DIR/go.mod" ] || die "missing $BINARY — run scripts/install-service.sh to rebuild and re-stage"
  log "building mac-agent"
  ( cd "$SCRIPT_DIR" && CGO_ENABLED=1 go build -o "$BINARY" . ) \
    || die "build failed — is Go installed?"
fi

# ── Start the agent ───────────────────────────────────────────────────────────

# An agent that survived an unclean exit still holds the listen port, so the new
# one would fail to bind and we would restart forever. Clear it first.
if pkill -f "^${BINARY}" 2>/dev/null; then
  log "killed a leftover mac-agent from a previous run"
  sleep 1
fi

log "starting mac-agent on 127.0.0.1:$TUNNEL_PORT"
"$BINARY" \
  -screen "$SCREEN_INDEX" \
  -fps "$FPS" \
  -width "$WIDTH" \
  -bitrate "$BITRATE" \
  -web "$SCRIPT_DIR/client" \
  -addr "127.0.0.1:${TUNNEL_PORT}" &
AGENT_PID=$!

for _ in $(seq 1 30); do
  if curl -sf -m 2 "http://127.0.0.1:${TUNNEL_PORT}/healthz" >/dev/null 2>&1; then
    log "mac-agent is listening"
    break
  fi
  kill -0 "$AGENT_PID" 2>/dev/null || die "mac-agent exited during startup — check Screen Recording and Accessibility permissions"
  sleep 1
done

curl -sf -m 2 "http://127.0.0.1:${TUNNEL_PORT}/healthz" >/dev/null 2>&1 \
  || die "mac-agent did not become healthy"

# ── Open the reverse tunnel ───────────────────────────────────────────────────

start_tunnel() {
  "${ssh_base[@]}" \
    -N \
    -R "${TUNNEL_PORT}:127.0.0.1:${TUNNEL_PORT}" \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=15 \
    -o ServerAliveCountMax=3 \
    "${VPS_USER}@${VPS_HOST}" &
  SSH_PID=$!
  sleep 3
  kill -0 "$SSH_PID" 2>/dev/null
}

# A tunnel that died uncleanly leaves the port bound on the VPS until sshd reaps
# the session, and the tunnel account is nologin so no remote helper can clear
# it. Wait the bind out here — exiting instead just makes launchd restart us into
# the same bind every ThrottleInterval.
tunnel_up=0
for attempt in $(seq 1 10); do
  log "opening reverse tunnel ${TUNNEL_PORT} -> ${VPS_USER}@${VPS_HOST} (attempt ${attempt}/10)"
  if start_tunnel; then
    tunnel_up=1
    break
  fi
  stop_pid "$SSH_PID"
  SSH_PID=""
  backoff=$(( attempt < 6 ? attempt * 10 : 60 ))
  log "tunnel did not come up; retrying in ${backoff}s"
  sleep "$backoff"
done

[ "$tunnel_up" = 1 ] || die "reverse tunnel never came up after 10 attempts"

if [ -n "$DOMAIN" ]; then
  confirmed=0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf -m 8 "https://${DOMAIN}/healthz" | grep -q '"status":"online"'; then
      confirmed=1
      break
    fi
    sleep 2
  done
  if [ "$confirmed" = 1 ]; then
    log "end-to-end check passed: https://${DOMAIN} is serving this Mac"
  else
    log "WARNING: https://${DOMAIN}/healthz did not confirm the tunnel"
  fi
fi

log "running (agent pid $AGENT_PID, ssh pid $SSH_PID)"

# ── Supervise ─────────────────────────────────────────────────────────────────

local_health() { curl -sf -m 2 "http://127.0.0.1:${TUNNEL_PORT}/healthz" 2>/dev/null; }

viewer_connected() {
  local body
  body="$(local_health)" || return 1
  local n
  n="$(printf '%s' "$body" | sed -n 's/.*"viewers":\([0-9][0-9]*\).*/\1/p')"
  [ -n "$n" ] && [ "$n" -gt 0 ]
}

health_failures=0

while :; do
  sleep 20

  if ! kill -0 "$AGENT_PID" 2>/dev/null; then
    log "mac-agent died — restarting service"
    exit 1
  fi
  if ! kill -0 "$SSH_PID" 2>/dev/null; then
    log "ssh tunnel died — restarting service"
    exit 1
  fi

  # The process can be alive while the HTTP server is wedged, so ask it directly.
  if ! local_health >/dev/null; then
    log "mac-agent is not answering locally — restarting service"
    exit 1
  fi

  # The tunnel can stay "up" while the far end is a black hole, so probe the
  # public URL rather than trusting the process list.
  if [ -n "$DOMAIN" ]; then
    if curl -sf -m 8 "https://${DOMAIN}/healthz" | grep -q '"status":"online"'; then
      health_failures=0
    elif viewer_connected; then
      # Someone is watching right now, so the path they came in on works. The
      # public probe is wrong, and restarting would drop a working session.
      log "public health check failed but a viewer is connected — not restarting"
      health_failures=0
    else
      health_failures=$((health_failures + 1))
      log "health check failed (${health_failures}/3)"
      if [ "$health_failures" -ge 3 ]; then
        log "tunnel is not carrying traffic — restarting service"
        exit 1
      fi
    fi
  fi
done
