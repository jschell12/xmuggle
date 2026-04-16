---
name: look
description: Analyze screenshot(s) to identify bugs or UI issues and fix the code
---

# /look (Cursor Command)

Screenshots are auto-detected from ~/Desktop.

## Usage

```bash
# Latest screenshot, local
look --repo <repo> --msg "<message>"

# Specific images
look --repo <repo> --img "<name>" --msg "<message>"

# All unprocessed
look --repo <repo> --all --msg "<message>"

# Forward to another Mac on the LAN
look --repo <repo> --remote --msg "<message>"
```
