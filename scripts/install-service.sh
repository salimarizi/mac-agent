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
cp "$PROJECT_DIR/mac-agent" "$STAGE/mac-agent"
cp "$SCRIPT_DIR/mac-remote-supervisor.sh" "$SUPERVISOR"
rm -rf "$STAGE/client"
cp -R "$PROJECT_DIR/client" "$STAGE/client"
cp "$PROJECT_DIR/.env" "$STAGE/.env"
chmod 600 "$STAGE/.env"
chmod +x "$SUPERVISOR" "$STAGE/mac-agent"

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

launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
launchctl enable "gui/$(id -u)/${LABEL}"

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
