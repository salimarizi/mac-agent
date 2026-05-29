#!/bin/bash
# MacRemote Launcher — double-click to start the remote control server.
# Uses osascript (AppleScript) for GUI dialogs, no terminal knowledge needed.

set -e

APP_NAME="Mac Remote"
CONFIG_FILE="$HOME/.mac-remote.conf"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$SCRIPT_DIR/mac-agent"
PID_FILE="/tmp/mac-remote.pid"
SSH_PID_FILE="/tmp/mac-remote-ssh.pid"
ENV_FILE="$SCRIPT_DIR/.env"

# ── Load .env file ───────────────────────────────────────────────────────────

if [ -f "$ENV_FILE" ]; then
  set -a
  source "$ENV_FILE"
  set +a
fi

# ── Helpers ───────────────────────────────────────────────────────────────────

notify() {
  osascript -e "display notification \"$1\" with title \"$APP_NAME\""
}

ask() {
  local prompt="$1" default="$2" hidden="${3:-false}"
  if [ "$hidden" = "true" ]; then
    osascript -e "text returned of (display dialog \"$prompt\" default answer \"$default\" with title \"$APP_NAME\" with hidden answer)" 2>/dev/null
  else
    osascript -e "text returned of (display dialog \"$prompt\" default answer \"$default\" with title \"$APP_NAME\")" 2>/dev/null
  fi
}

confirm() {
  osascript -e "button returned of (display dialog \"$1\" buttons {\"$2\", \"$3\"} default button \"$3\" with title \"$APP_NAME\")" 2>/dev/null
}

alert() {
  osascript -e "display dialog \"$1\" buttons {\"OK\"} default button \"OK\" with title \"$APP_NAME\" with icon $2" 2>/dev/null
}

# ── Stop running instance ─────────────────────────────────────────────────────

stop_all() {
  if [ -f "$PID_FILE" ]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
  if [ -f "$SSH_PID_FILE" ]; then
    kill "$(cat "$SSH_PID_FILE")" 2>/dev/null || true
    rm -f "$SSH_PID_FILE"
  fi
  # Also kill by name as fallback
  pkill -f "mac-agent.*-addr 127.0.0.1" 2>/dev/null || true
  pkill -f "ssh.*-fNR.*mac-remote-tunnel" 2>/dev/null || true
}

# ── Check if already running ──────────────────────────────────────────────────

if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  choice=$(confirm "Mac Remote is already running." "Settings" "Stop")
  if [ "$choice" = "Stop" ]; then
    stop_all
    notify "Stopped"
    exit 0
  fi
  # Fall through to settings if "Settings" chosen
  rm -f "$CONFIG_FILE"
  stop_all
fi

# ── Build binary if needed ────────────────────────────────────────────────────

if [ ! -f "$BINARY" ]; then
  notify "Building mac-agent... (first time only)"
  cd "$SCRIPT_DIR"
  CGO_ENABLED=1 go build -o mac-agent . 2>/tmp/mac-remote-build.log
  if [ $? -ne 0 ]; then
    alert "Build failed. Check that Go and ffmpeg are installed.\n\n$(cat /tmp/mac-remote-build.log)" "stop"
    exit 1
  fi
  notify "Build complete"
fi

# ── Load or create config ─────────────────────────────────────────────────────

load_config() {
  if [ -f "$CONFIG_FILE" ]; then
    source "$CONFIG_FILE"
    return 0
  fi
  return 1
}

save_config() {
  cat > "$CONFIG_FILE" << CONF
VPS_HOST="$VPS_HOST"
VPS_USER="$VPS_USER"
VPS_PORT="$VPS_PORT"
TUNNEL_PORT="$TUNNEL_PORT"
AGENT_PASSWORD="$AGENT_PASSWORD"
TURN_URL="$TURN_URL"
TURN_USER="$TURN_USER"
TURN_PASS="$TURN_PASS"
SCREEN_INDEX="$SCREEN_INDEX"
FPS="$FPS"
WIDTH="$WIDTH"
BITRATE="$BITRATE"
CONF
  chmod 600 "$CONFIG_FILE"
}

setup_wizard() {
  VPS_HOST=$(ask "VPS hostname or IP:" "${VPS_HOST:-$MAC_AGENT_VPS_HOST}") || exit 0
  VPS_USER=$(ask "VPS SSH username:" "${VPS_USER:-$MAC_AGENT_VPS_USER}") || exit 0
  VPS_PORT=$(ask "VPS SSH port:" "${VPS_PORT:-${MAC_AGENT_VPS_PORT:-22}}") || exit 0
  TUNNEL_PORT=$(ask "Tunnel port (must match nginx proxy_pass):" "${TUNNEL_PORT:-${MAC_AGENT_TUNNEL_PORT:-9443}}") || exit 0
  AGENT_PASSWORD=$(ask "Access password (for iPhone connection):" "${AGENT_PASSWORD:-$MAC_AGENT_PASSWORD}" "true") || exit 0

  if [ -z "$AGENT_PASSWORD" ]; then
    alert "Password is required." "stop"
    exit 1
  fi

  TURN_URL=$(ask "TURN server URL:" "${TURN_URL:-$MAC_AGENT_TURN_URL}") || exit 0
  TURN_USER=$(ask "TURN username:" "${TURN_USER:-$MAC_AGENT_TURN_USER}") || exit 0
  TURN_PASS=$(ask "TURN password:" "${TURN_PASS:-$MAC_AGENT_TURN_PASS}" "true") || exit 0
  SCREEN_INDEX=$(ask "Screen index (0=main, 1=second):" "${SCREEN_INDEX:-0}") || exit 0
  FPS=$(ask "FPS:" "${FPS:-30}") || exit 0
  WIDTH=$(ask "Output width (0=native):" "${WIDTH:-1600}") || exit 0
  BITRATE=$(ask "Bitrate:" "${BITRATE:-6M}") || exit 0

  save_config
}

if ! load_config; then
  setup_wizard
fi

# ── Offer to reconfigure ──────────────────────────────────────────────────────

choice=$(confirm "Connect to VPS: $VPS_HOST\nPassword: ••••••••\nScreen: $SCREEN_INDEX | FPS: $FPS | ${WIDTH}px" "Settings" "Start")
if [ "$choice" = "Settings" ]; then
  setup_wizard
fi

# ── Open SSH tunnel ───────────────────────────────────────────────────────────

# Kill any existing tunnel
pkill -f "ssh.*-fNR.*${TUNNEL_PORT}:localhost:${TUNNEL_PORT}" 2>/dev/null || true
sleep 1

notify "Opening SSH tunnel to $VPS_HOST..."

# Use -o ExitOnForwardFailure to fail fast if port is taken
ssh -f -N -R "${TUNNEL_PORT}:localhost:${TUNNEL_PORT}" \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=30 \
    -o ServerAliveCountMax=3 \
    -o ConnectTimeout=10 \
    -p "$VPS_PORT" \
    "${VPS_USER}@${VPS_HOST}" &
SSH_PID=$!
echo "$SSH_PID" > "$SSH_PID_FILE"

# Wait a moment for tunnel to establish
sleep 2

# Verify tunnel
if ! kill -0 "$SSH_PID" 2>/dev/null; then
  alert "SSH tunnel failed to connect.\nCheck your VPS credentials and network." "stop"
  rm -f "$SSH_PID_FILE"
  exit 1
fi

# ── Start mac-agent ───────────────────────────────────────────────────────────

export MAC_AGENT_PASSWORD="$AGENT_PASSWORD"
export MAC_AGENT_TURN_URL="$TURN_URL"
export MAC_AGENT_TURN_USER="$TURN_USER"
export MAC_AGENT_TURN_PASS="$TURN_PASS"

"$BINARY" \
  -screen "$SCREEN_INDEX" \
  -fps "$FPS" \
  -width "$WIDTH" \
  -bitrate "$BITRATE" \
  -addr "127.0.0.1:${TUNNEL_PORT}" &
AGENT_PID=$!
echo "$AGENT_PID" > "$PID_FILE"

sleep 2

if ! kill -0 "$AGENT_PID" 2>/dev/null; then
  alert "mac-agent failed to start.\nCheck Screen Recording and Accessibility permissions." "stop"
  stop_all
  exit 1
fi

notify "Running! Connect from your iPhone."

# ── Wait for user to stop ─────────────────────────────────────────────────────

CONNECT_URL="${MAC_AGENT_DOMAIN:+https://$MAC_AGENT_DOMAIN}"
alert "Mac Remote is running.\n\n${CONNECT_URL:+Connect at: $CONNECT_URL\n\n}Click OK to stop." "note"

# User clicked OK — stop everything
stop_all
notify "Stopped"
