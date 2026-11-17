#!/bin/bash
# Installs Mac Remote as a launchd user agent so the Mac stays reachable after a
# reboot, a crash, or a network drop — without anyone logging in and clicking.

set -euo pipefail

LABEL="com.salimarizi.mac-remote"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$HOME/Library/Logs/MacRemote"

# launchd agents get no TCC grant for ~/Documents, ~/Desktop or ~/Downloads, so a
# checkout in any of those is unreadable to the job — it fails with "Operation not
# permitted" before the script even runs. Run from a staged copy in Application
# Support, which is outside those boundaries.
STAGE="$HOME/Library/Application Support/MacRemote"
SUPERVISOR="$STAGE/scripts/mac-remote-supervisor.sh"

[ -f "$PROJECT_DIR/.env" ] || { echo "Missing $PROJECT_DIR/.env — copy .env.example and fill it in first." >&2; exit 1; }

mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR" "$STAGE/scripts"

if [ ! -x "$PROJECT_DIR/mac-agent" ]; then
  echo "Building mac-agent…"
  ( cd "$PROJECT_DIR" && CGO_ENABLED=1 PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH" go build -o mac-agent . )
fi

echo "Staging runtime into ${STAGE}…"
# The agent is ad-hoc signed, so macOS identifies it by the hash of its contents.
# Every rebuild produces a new hash, and TCC quietly drops the Screen Recording and
# Accessibility grants attached to the old one — the stream keeps running but shows
# only the wallpaper, and nothing responds to input. Say so rather than let it be
# rediscovered from the symptoms.
# Compare against a stamp of the unsigned source, not the staged copy: staging
# re-signs the binary, so the two never match byte for byte.
STAMP="$STAGE/.agent-sha"
NEW_SHA="$(shasum -a 256 "$PROJECT_DIR/mac-agent" | cut -d' ' -f1)"
BINARY_CHANGED=0
if [ -f "$STAMP" ] && [ "$(cat "$STAMP")" != "$NEW_SHA" ]; then
  BINARY_CHANGED=1
fi
cp "$PROJECT_DIR/mac-agent" "$STAGE/mac-agent"
printf '%s' "$NEW_SHA" > "$STAMP"
cp "$SCRIPT_DIR/mac-remote-supervisor.sh" "$SUPERVISOR"
rm -rf "$STAGE/client"
cp -R "$PROJECT_DIR/client" "$STAGE/client"
cp "$PROJECT_DIR/.env" "$STAGE/.env"
chmod 600 "$STAGE/.env"
chmod +x "$SUPERVISOR" "$STAGE/mac-agent"

# Go linker-signs binaries as "a.out"; TCC then cannot tell this agent apart from
# any other Go build, and every rebuild looks like a new program. A stable
# identifier keeps the Screen Recording grant attached to this agent.
codesign --force --sign - --identifier com.salimarizi.mac-agent "$STAGE/mac-agent"

cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>

  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>${SUPERVISOR}</string>
  </array>

  <key>WorkingDirectory</key>
  <string>${STAGE}</string>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <true/>

  <key>ThrottleInterval</key>
  <integer>10</integer>

  <key>ProcessType</key>
  <string>Interactive</string>

  <key>StandardOutPath</key>
  <string>${LOG_DIR}/mac-remote.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/mac-remote.log</string>

  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
</dict>
</plist>
PLISTEOF

GUI="gui/$(id -u)"

launchctl bootout "${GUI}/${LABEL}" 2>/dev/null || true

# bootout returns before launchd has finished unloading the job, and bootstrapping
# into a domain that still holds it fails with "Input/output error" — leaving the
# Mac unreachable until someone notices. Wait for the old job to actually go.
for _ in $(seq 1 20); do
  launchctl print "${GUI}/${LABEL}" >/dev/null 2>&1 || break
  sleep 0.5
done

for attempt in $(seq 1 5); do
  if launchctl bootstrap "$GUI" "$PLIST" 2>/dev/null; then
    break
  fi
  if launchctl print "${GUI}/${LABEL}" >/dev/null 2>&1; then
    break   # already loaded; nothing to retry
  fi
  [ "$attempt" -lt 5 ] || { echo "Failed to bootstrap ${LABEL}." >&2; exit 1; }
  sleep 2
done

launchctl enable "${GUI}/${LABEL}"

# Confirm the agent actually came up rather than reporting success blindly.
for _ in $(seq 1 30); do
  if curl -sf -m 2 http://127.0.0.1:9443/healthz >/dev/null 2>&1; then
    echo "Agent is answering on 127.0.0.1:9443."
    break
  fi
  sleep 1
done

if [ "$BINARY_CHANGED" = 1 ]; then
  echo
  echo "⚠️  The agent binary changed, so macOS has invalidated its privacy grants."
  echo "    Expect the stream to show only the wallpaper, and input to do nothing,"
  echo "    until you re-approve it in System Settings > Privacy & Security:"
  echo "      • Screen & System Audio Recording"
  echo "      • Accessibility"
  echo "    Toggle the entry off and on again (or re-add it), then re-run this script."
  echo "    Granting /bin/bash in both lists avoids this on future rebuilds."
  echo
fi

echo "Installed and started ${LABEL}."
echo
echo "  Status:  launchctl print gui/$(id -u)/${LABEL} | grep state"
echo "  Logs:    tail -f ${LOG_DIR}/mac-remote.log"
echo "  Stop:    launchctl bootout gui/$(id -u)/${LABEL}"
echo "  Remove:  $SCRIPT_DIR/uninstall-service.sh"
echo
echo "The service runs from the staged copy in:"
echo "  $STAGE"
echo "Re-run this script after changing .env, the client, or the Go code."
echo
echo "Grant Screen Recording and Accessibility to /bin/bash in"
echo "System Settings → Privacy & Security, then run this script again."
