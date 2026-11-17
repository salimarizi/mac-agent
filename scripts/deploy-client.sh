#!/bin/bash
# Pushes the web client to both places that serve it: nginx on the VPS, and the
# staged copy the agent serves locally. The client is no longer a single file —
# the PWA manifest, icons and service worker have to travel with it — so copying
# index.html by hand now leaves the two copies out of step.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
CLIENT_DIR="$PROJECT_DIR/client"
STAGE="$HOME/Library/Application Support/MacRemote"

# Where nginx serves the client from on the VPS.
REMOTE_ROOT="${MAC_AGENT_WEB_ROOT:-/var/www/mac-agent/client}"

die() { echo "deploy-client: $*" >&2; exit 1; }

[ -d "$CLIENT_DIR" ] || die "no client directory at $CLIENT_DIR"
[ -f "$PROJECT_DIR/.env" ] || die "missing $PROJECT_DIR/.env"

# shellcheck disable=SC1091
set -a; . "$PROJECT_DIR/.env"; set +a

VPS_HOST="${MAC_AGENT_VPS_HOST:-}"
VPS_PORT="${MAC_AGENT_VPS_PORT:-22}"
DOMAIN="${MAC_AGENT_DOMAIN:-}"

# MAC_AGENT_VPS_USER is the tunnel account, which is deliberately nologin and
# cannot write anything — deploying needs a real login that owns the web root.
DEPLOY_USER="${MAC_AGENT_DEPLOY_USER:-root}"

[ -n "$VPS_HOST" ] || die "MAC_AGENT_VPS_HOST is not set"

ssh -p "$VPS_PORT" -o BatchMode=yes -o ConnectTimeout=10 "${DEPLOY_USER}@${VPS_HOST}" true 2>/dev/null \
  || die "cannot ssh as ${DEPLOY_USER}@${VPS_HOST}; set MAC_AGENT_DEPLOY_USER to an account that can write $REMOTE_ROOT"

echo "==> Staging locally into $STAGE/client"
mkdir -p "$STAGE"
rm -rf "$STAGE/client"
cp -R "$CLIENT_DIR" "$STAGE/client"

echo "==> Uploading to ${DEPLOY_USER}@${VPS_HOST}:${REMOTE_ROOT}"
ssh -p "$VPS_PORT" -o BatchMode=yes "${DEPLOY_USER}@${VPS_HOST}" "mkdir -p '$REMOTE_ROOT'"
scp -q -P "$VPS_PORT" -o BatchMode=yes \
  "$CLIENT_DIR"/* "${DEPLOY_USER}@${VPS_HOST}:${REMOTE_ROOT}/"

echo "==> Verifying"
fail=0
for f in "$CLIENT_DIR"/*; do
  name="$(basename "$f")"
  local_sum="$(shasum -a 256 "$f" | cut -d' ' -f1)"
  remote_sum="$(ssh -p "$VPS_PORT" -o BatchMode=yes "${DEPLOY_USER}@${VPS_HOST}" \
    "shasum -a 256 '$REMOTE_ROOT/$name' 2>/dev/null | cut -d' ' -f1" || true)"
  if [ "$local_sum" = "$remote_sum" ]; then
    printf '    ok   %s\n' "$name"
  else
    printf '    FAIL %s (local %.12s, remote %.12s)\n' "$name" "$local_sum" "${remote_sum:-missing}"
    fail=1
  fi
done
[ "$fail" -eq 0 ] || die "upload verification failed"

if [ -n "$DOMAIN" ]; then
  if curl -sf -m 8 "https://${DOMAIN}/healthz" | grep -q '"status":"online"'; then
    echo "==> https://${DOMAIN} is live"
  else
    echo "==> WARNING: https://${DOMAIN}/healthz did not answer; the tunnel may be down"
  fi
fi

echo
echo "Client deployed. The service keeps serving the old copy only for pages"
echo "already open — reload on the phone to pick this up."
echo "A service worker caches the shell, so if the phone looks stale, pull to"
echo "refresh or reopen the app once."
