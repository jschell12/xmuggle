---
name: look
description: >-
  Analyze screenshot(s) to identify bugs or UI issues and fix the code.
  Auto-detects new screenshots from Desktop/Downloads. Works locally
  or forwards to a remote machine for processing.
---

# /look

Analyze screenshot(s), identify the problem, and fix the code. New screenshots are auto-detected from your Desktop and Downloads via macOS Spotlight — just take a screenshot and invoke `/look`.

## When to trigger

- User invokes `/look`
- User says "look at this", "fix this screenshot", "check my screenshot"
- User provides a screenshot and mentions a repo that needs changes
- User drops an image and describes a bug or desired change

## Steps

1. **Gather information**:
   - **Repo** (required): GitHub repo (`owner/name` or URL) or local path. Ask if not provided.
   - **Message** (optional): What's wrong, what to look for, or what to fix.
   - **Image selection** (optional): Specific image name(s), `--all` for all unprocessed, or omit for latest.

2. **Check for new screenshots**:

```bash
screenshot-agent --list
```

This auto-ingests any new screenshots from Desktop/Downloads and shows what's available.

3. **Run the fix**:

```bash
# Latest unprocessed screenshot
screenshot-agent --repo <repo> --msg "<message>"

# All unprocessed screenshots
screenshot-agent --repo <repo> --all --msg "<message>"

# Specific image(s)
screenshot-agent --repo <repo> --img "<name>" --msg "<message>"
screenshot-agent --repo <repo> --img "<name1>" --img "<name2>" --msg "<message>"

# Forward to remote machine instead of processing locally
screenshot-agent --repo <repo> --msg "<message>" --remote
```

4. **Report the result** to the user. Mention what was fixed and that they can `git pull` to get changes.

## Flags reference

| Flag | Purpose |
|---|---|
| `--repo <repo>` | Target repo (required) |
| `--msg "<text>"` | Context for the agent |
| `--img "<name>"` | Select specific image (repeatable, fuzzy matches) |
| `--all` | Process all unprocessed images |
| `--remote` | Forward to remote machine daemon |
| `--list` | Show all images and status |
| `--scan` | Ingest ALL images from Desktop/Downloads (not just screenshots) |

## Prerequisites

- `screenshot-agent` CLI on PATH
- `claude` and `gh` CLIs on PATH
- For `--remote`: SSH configured (`make setup`) + daemon running on remote (`make daemon-start`)
