#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

is_on_path() {
  case ":$PATH:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

BIN_DIR="$HOME/.local/bin"
mkdir -p "$BIN_DIR"

# Clean up old installs in other locations
for old_dir in /opt/homebrew/bin /usr/local/bin "$HOME/bin"; do
  for bin in xmuggle xmuggled; do
    if [[ -f "$old_dir/$bin" ]]; then
      rm -f "$old_dir/$bin" 2>/dev/null || sudo rm -f "$old_dir/$bin" 2>/dev/null || true
      echo "Removed old $old_dir/$bin"
    fi
  done
done

echo "=== Installing /xmuggle skill + CLI ==="
echo "Install dir: $BIN_DIR"
echo ""

# Build
echo "Building..."
(cd "$REPO_DIR" && make build)

# Install binaries
for bin in xmuggle xmuggled; do
  install -m 0755 "$REPO_DIR/bin/$bin" "$BIN_DIR/$bin"
  echo "  $BIN_DIR/$bin"
done

# Ensure ~/.local/bin is in all shell profiles (idempotent)
PATH_LINE='export PATH="$HOME/.local/bin:$PATH"'
for RC_FILE in "$HOME/.zshrc" "$HOME/.zprofile" "$HOME/.bashrc" "$HOME/.bash_profile"; do
  if ! grep -qF '.local/bin' "$RC_FILE" 2>/dev/null; then
    echo "" >> "$RC_FILE"
    echo '# Added by xmuggle install' >> "$RC_FILE"
    echo "$PATH_LINE" >> "$RC_FILE"
    echo "Added ~/.local/bin to PATH in $RC_FILE"
  fi
done
export PATH="$BIN_DIR:$PATH"

# Claude Code skill
if [[ -d "$HOME/.claude" ]]; then
  mkdir -p "$HOME/.claude/skills/xmuggle"
  cp "$REPO_DIR/skills/claude/SKILL.md" "$HOME/.claude/skills/xmuggle/SKILL.md"
  echo "  ~/.claude/skills/xmuggle/SKILL.md"
fi

# Cursor command
if [[ -d "$HOME/.cursor" ]]; then
  mkdir -p "$HOME/.cursor/commands"
  cp "$REPO_DIR/skills/cursor/command.md" "$HOME/.cursor/commands/xmuggle.md"
  echo "  ~/.cursor/commands/xmuggle.md"
fi

echo ""
echo "Done! Use /xmuggle in Claude Code or Cursor."
echo ""
echo "Quick start:"
echo "  cd ~/dev/my-app && xmuggle init"
echo "  xmuggle send --screenshots"
