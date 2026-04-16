---
name: look
description: Analyze screenshot(s) to identify bugs or UI issues and fix the code
---

# /look (Cursor Command)

Analyze screenshot(s), identify the problem, and fix the code. Screenshots are auto-detected from Desktop/Downloads.

## Usage

Gather:
1. **Repo**: GitHub repo (owner/name) or local path
2. **Message** (optional): what to fix or look for

Run in the terminal:

```bash
# Latest screenshot
screenshot-agent --repo <repo> --msg "<message>"

# All unprocessed screenshots
screenshot-agent --repo <repo> --all --msg "<message>"

# Specific image(s)
screenshot-agent --repo <repo> --img "<name>" --msg "<message>"

# Forward to remote machine
screenshot-agent --repo <repo> --msg "<message>" --remote
```
