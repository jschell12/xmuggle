#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "=== Installing /look skill ==="
echo ""

# Build
echo "Building..."
(cd "$REPO_DIR" && pnpm build)

# Link globally
echo "Linking globally..."
(cd "$REPO_DIR" && pnpm link --global 2>/dev/null || npm link 2>/dev/null || true)

if command -v look &>/dev/null; then
  echo "look CLI installed on PATH"
else
  echo "Note: look not found on PATH."
  echo "You can run it directly: node $REPO_DIR/dist/index.js"
fi

# Install Claude Code skill
if [[ -d "$HOME/.claude" ]]; then
  echo ""
  echo "Detected Claude Code. Installing /look skill..."
  mkdir -p "$HOME/.claude/skills/look"
  cp "$REPO_DIR/skills/claude/SKILL.md" "$HOME/.claude/skills/look/SKILL.md"
  echo "  → ~/.claude/skills/look/SKILL.md"
fi

# Install Cursor command
if [[ -d "$HOME/.cursor" ]]; then
  echo ""
  echo "Detected Cursor. Installing /look command..."
  mkdir -p "$HOME/.cursor/commands"
  cp "$REPO_DIR/skills/cursor/command.md" "$HOME/.cursor/commands/look.md"
  echo "  → ~/.cursor/commands/look.md"
fi

echo ""
echo "Done! Use /look in Claude Code or Cursor."
echo ""
echo "Examples:"
echo "  look --list                              # see screenshots"
echo "  look --repo jschell12/my-app             # fix latest screenshot"
echo "  look --repo jschell12/my-app --all       # fix all unprocessed"
