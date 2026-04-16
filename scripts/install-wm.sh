#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="$HOME/.screenshot-agent/config.json"
WATCHER_PLIST_LABEL="com.screenshot-agent.watcher"
WATCHER_PLIST_DEST="$HOME/Library/LaunchAgents/${WATCHER_PLIST_LABEL}.plist"
WATCHER_PLIST_TEMPLATE="$REPO_DIR/launchd/${WATCHER_PLIST_LABEL}.plist"

echo "=== Installing screenshot-agent (work machine) ==="
echo ""

# Check config exists
if [[ ! -f "$CONFIG_FILE" ]]; then
  echo "Error: No config found. Run 'make setup' first."
  exit 1
fi

SSH_HOST=$(python3 -c "import json; c=json.load(open('$CONFIG_FILE')); print(c['sshHost'])")

# Build
echo "Building..."
(cd "$REPO_DIR" && pnpm build)

# Link globally
echo "Linking globally..."
(cd "$REPO_DIR" && pnpm link --global 2>/dev/null || npm link 2>/dev/null || true)

# Check if screenshot-agent is on PATH
if command -v screenshot-agent &>/dev/null; then
  echo "screenshot-agent CLI installed on PATH"
else
  echo "Note: screenshot-agent not found on PATH."
  echo "You can run it directly: node $REPO_DIR/dist/index.js"
fi

# Create drop directories named after personal machine IP/hostname
DESKTOP_DROP="$HOME/Desktop/$SSH_HOST"
DOWNLOADS_DROP="$HOME/Downloads/$SSH_HOST"

echo ""
echo "Creating drop directories..."
mkdir -p "$DESKTOP_DROP" "$DOWNLOADS_DROP"
echo "  $DESKTOP_DROP"
echo "  $DOWNLOADS_DROP"

# Create sent + logs dirs
mkdir -p "$HOME/.screenshot-agent/sent" "$HOME/.screenshot-agent/logs"

# Ask for default repo
echo ""
read -rp "Default repo for dropped images (e.g., jschell12/my-app, or leave blank): " DEFAULT_REPO
if [[ -n "$DEFAULT_REPO" ]]; then
  # Update config with defaultRepo
  python3 -c "
import json
c = json.load(open('$CONFIG_FILE'))
c['defaultRepo'] = '$DEFAULT_REPO'
json.dump(c, open('$CONFIG_FILE', 'w'), indent=2)
print('  Default repo set to: $DEFAULT_REPO')
"
fi

# Install watcher launchd plist
echo ""
echo "Installing file watcher..."
WATCH_SCRIPT="$REPO_DIR/scripts/watch-drop.sh"
chmod +x "$WATCH_SCRIPT"

sed \
  -e "s|__WATCH_SCRIPT__|${WATCH_SCRIPT}|g" \
  -e "s|__DESKTOP_DROP__|${DESKTOP_DROP}|g" \
  -e "s|__DOWNLOADS_DROP__|${DOWNLOADS_DROP}|g" \
  -e "s|__HOME__|${HOME}|g" \
  "$WATCHER_PLIST_TEMPLATE" > "$WATCHER_PLIST_DEST"

launchctl unload "$WATCHER_PLIST_DEST" 2>/dev/null || true
launchctl load "$WATCHER_PLIST_DEST"
echo "File watcher installed and started."

# Install Claude Code skill
CLAUDE_SKILLS_DIR="$HOME/.claude/skills/look"
if [[ -d "$HOME/.claude" ]]; then
  echo ""
  echo "Detected Claude Code. Installing skill..."
  mkdir -p "$CLAUDE_SKILLS_DIR"
  cp "$REPO_DIR/skills/claude/SKILL.md" "$CLAUDE_SKILLS_DIR/SKILL.md"
  echo "Claude skill installed at $CLAUDE_SKILLS_DIR/SKILL.md"
fi

# Install Cursor command
CURSOR_COMMANDS_DIR="$HOME/.cursor/commands"
if [[ -d "$HOME/.cursor" ]]; then
  echo ""
  echo "Detected Cursor. Installing command..."
  mkdir -p "$CURSOR_COMMANDS_DIR"
  cp "$REPO_DIR/skills/cursor/command.md" "$CURSOR_COMMANDS_DIR/look.md"
  echo "Cursor command installed at $CURSOR_COMMANDS_DIR/look.md"
fi

echo ""
echo "=== Done! ==="
echo ""
echo "Drop directories (auto-sync to personal machine):"
echo "  ~/Desktop/$SSH_HOST/"
echo "  ~/Downloads/$SSH_HOST/"
echo ""
echo "How to use:"
echo "  1. Drop an image into either folder → auto-sent to personal machine"
echo "  2. For specific repo/message, add a sidecar JSON:"
echo "     bug.png + bug.json → {\"repo\": \"owner/repo\", \"msg\": \"fix the button\"}"
echo "  3. Or use subdirectories: ~/Desktop/$SSH_HOST/owner/repo/screenshot.png"
echo "  4. Or use the CLI: screenshot-agent --repo owner/repo --remote"
echo "  5. Or use /look in Claude Code or Cursor"
