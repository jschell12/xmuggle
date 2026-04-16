# look

Screenshot-driven code fixes. Take a screenshot, invoke `/look` or run `look`, and a Claude Code agent analyzes the image, finds the relevant code, fixes it, opens a PR, and merges it.

## Install

```bash
git clone git@github.com:jschell12/look.git
cd look
pnpm install
make install    # builds, links CLI, installs /look skill
```

That's it. Take a screenshot and run:

```bash
look --repo jschell12/my-app
```

## Usage

```bash
# Fix the latest unprocessed screenshot
look --repo jschell12/my-app

# With context
look --repo jschell12/my-app --msg "the submit button overlaps the footer"

# Specific image (fuzzy name match)
look --repo jschell12/my-app --img "Screenshot 2026-04-14"

# Multiple images
look --repo jschell12/my-app --img bug1 --img bug2 --msg "same issue on different pages"

# All unprocessed screenshots
look --repo jschell12/my-app --all

# See what's in the store
look --list
```

### What happens

1. Agent reads the screenshot(s) (Claude sees images natively)
2. Analyzes what's wrong, using your message for context
3. Clones the repo, creates a branch
4. Finds and fixes the relevant code
5. Pushes, opens a PR, and merges it

## Remote processing

Forward a task to another Mac on the LAN — useful when your work laptop can't run the agent but your personal Mac can:

```bash
# Interactive: Bonjour/mDNS discovers Macs advertising SSH
look --repo jschell12/my-app --remote

# Or specify directly
look --repo jschell12/my-app --remote --host macmini.local
```

The target Mac needs the daemon running:

```bash
# On the machine that will process tasks
make daemon-install
```

The daemon watches `~/.look/queue/` and dispatches tasks to an agent-queue worker pool (up to 3 parallel workers with merge locking).

## Image detection

New screenshots are **auto-detected** via macOS Spotlight (`kMDItemIsScreenCapture`) from `~/Desktop` and `~/Downloads`, and copied into `~/.look/`. No manual step needed — just take a screenshot and run the command.

Already-processed images are tracked in `~/.look/.tracked`.

Use `--scan` if you want to ingest non-screenshot images (downloaded files, etc.) as well.

## mac-link.sh

Bundled script for general-purpose LAN work between Macs:

```bash
make link                                # interactive: discover + pick action
bash scripts/mac-link.sh tunnel          # set up local SSH port forward
bash scripts/mac-link.sh rtunnel         # reverse tunnel
bash scripts/mac-link.sh push            # rsync local → remote
bash scripts/mac-link.sh pull            # rsync remote → local
bash scripts/mac-link.sh discover        # print discovered host
```

## Commands

| Command | Purpose |
|---|---|
| `make install` | Build, link CLI, install `/look` skill (Claude + Cursor) |
| `make daemon-install` | Install the queue-processing daemon (launchd) |
| `make daemon-start/stop/logs` | Control the daemon |
| `make daemon-uninstall` | Remove the daemon |
| `make link` | Interactive SSH tunnel / file transfer |

## Requirements

- macOS (uses `mdfind` for screenshot detection, `dns-sd` for discovery)
- Node.js 22+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code)
- [GitHub CLI](https://cli.github.com/) (`gh`) authenticated
- [agent-queue](https://github.com/jschell12/agent-queue) — for daemon mode only
