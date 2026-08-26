#!/bin/bash
# Stops and removes the Mac Remote launchd agent.

set -uo pipefail

LABEL="com.salimarizi.mac-remote"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
STAGE="$HOME/Library/Application Support/MacRemote"

launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null && echo "Stopped ${LABEL}." || echo "${LABEL} was not running."
rm -f "$PLIST" && echo "Removed ${PLIST}."
# The staged copy holds a copy of .env, so removing the service removes the secret.
rm -rf "$STAGE" && echo "Removed ${STAGE}."
