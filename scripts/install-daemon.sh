#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLIST_LABEL="com.look.daemon"
PLIST_DEST="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
PLIST_TEMPLATE="$REPO_DIR/launchd/${PLIST_LABEL}.plist"

echo "=== Installing look daemon ==="
echo ""
echo "This machine will process screenshot tasks pushed into ~/.look/queue/"
echo "by other Macs on the LAN (via scp/rsync using mac-link.sh or the /look skill)."
echo ""

# Create directories
echo "Creating directories..."
mkdir -p ~/.look/{queue,results,logs}

# Build
echo "Building..."
(cd "$REPO_DIR" && pnpm build)

# Link globally
echo "Linking globally..."
(cd "$REPO_DIR" && pnpm link --global 2>/dev/null || npm link 2>/dev/null || true)

# Install launchd plist
echo "Installing daemon..."

NODE_PATH="$(which node)"
DAEMON_JS="$REPO_DIR/dist/daemon.js"

if [[ ! -f "$DAEMON_JS" ]]; then
  echo "Error: dist/daemon.js not found. Build failed?"
  exit 1
fi

# Generate plist from template
sed \
  -e "s|__NODE_PATH__|${NODE_PATH}|g" \
  -e "s|__DAEMON_JS__|${DAEMON_JS}|g" \
  -e "s|__HOME__|${HOME}|g" \
  "$PLIST_TEMPLATE" > "$PLIST_DEST"

echo "Plist installed at $PLIST_DEST"

# Unload if already loaded, then load
launchctl unload "$PLIST_DEST" 2>/dev/null || true
launchctl load "$PLIST_DEST"

echo "Daemon started."
echo ""
echo "Verify with: launchctl list | grep com.look"
echo "Logs at: ~/.look/logs/"
echo ""
echo "Control:"
echo "  make daemon-start   # start daemon"
echo "  make daemon-stop    # stop daemon"
