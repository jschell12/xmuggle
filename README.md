# xmuggle

Code changes from text prompts or screenshots, with GitHub and GitLab repositories. Describe a feature or fix, optionally attach screenshots, and an AI agent edits the code.

Electron UI for managing screenshots and projects. Go daemon (`xmuggled`) for git sync and background tasks.

## Install

```bash
git clone git@github.com:jschell12/xmuggle.git
cd xmuggle
make install       # builds daemon, installs to ~/.local/bin, restarts via launchd
npm install        # install Electron dependencies
```

## Usage

### Electron UI

```bash
npm start
```

The UI scans `~/Desktop` for screenshots and text files, lets you pick a project (any local git repo), add context, and send to Claude for analysis.

### Daemon

The Go daemon (`xmuggled`) runs in the background via launchd, handling:

- **Git sync** — pulls a shared sync repo, imports screenshots sent from other machines
- **Repo pulls** — keeps configured repos up to date
- **Custom commands** — runs shell commands on each cycle
- **onReceive hooks** — triggers commands when new images arrive

```bash
xmuggled start          # start daemon (or use launchd)
xmuggled stop           # stop daemon
xmuggled status         # show PID and config
xmuggled log 50         # tail daemon log
xmuggled config         # print config
xmuggled edit           # open config in $EDITOR
```

Config lives at `~/.xmuggle/daemon.json`:

```json
{
  "interval": 30,
  "syncRepo": "git@github.com:user/xmuggle-sync.git",
  "repos": [
    "/path/to/repo",
    { "path": "/path/to/repo", "pull": true, "commands": ["make test"] }
  ],
  "commands": ["echo hello"],
  "onReceive": ["notify $FILES"]
}
```

### Logs

```bash
xmuggled log 50                                  # via CLI
tail -f ~/.xmuggle/daemon.log                    # daemon log
tail -f ~/.xmuggle/logs/daemon.stderr.log        # launchd stderr
```

## Make targets

| Command | Purpose |
|---|---|
| `make install` | Build daemon, install to `~/.local/bin`, restart via launchd |
| `make build` | Build `xmuggled` binary |
| `make run` | Install and launch Electron UI |
| `make run-daemon` | Run daemon in foreground |
| `make daemon-stop` | Stop daemon |
| `make daemon-restart` | Restart daemon via launchd |
| `make daemon-status` | Show daemon status |
| `make daemon-log` | Tail daemon log |

## Requirements

- macOS
- Go 1.22+ (to build daemon)
- Node.js + npm (for Electron UI)
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code)

## GitLab and text prompts

Add a local checkout with its GitLab origin (SSH or HTTPS, including nested groups
and self-hosted GitLab). You can override the Git URL when adding a project.
The queue repository can also be hosted on GitLab.

Click **+ Prompt**, select a project, describe the requested change, and click
**Queue prompt** (or Cmd/Ctrl+Enter). No image is required. Text prompts remain
visible in the grid with task status and results. Screenshot submissions still work.
The processing daemon runs on another machine, as with screenshot tasks.

GitLab tasks clone an isolated branch, push changes, and open a merge request
against the project's default branch using `glab`. Review and merge it in GitLab.
Install and authenticate the GitLab CLI on the processing machine, including the
appropriate host for self-hosted GitLab. GitHub tasks retain their existing flow.

Use SSH keys or Git credential helpers for Git authentication. For environment
credentials, launch the UI and daemon through `scredmgr run --path <project> --
<command>`. Tokens are no longer stored or read from `~/.xmuggle/gh-token`.

## Response inbox

Open **Responses** to see completed and failed submissions, including the worker's
result and merge-request link. The badge counts unread responses; opening a
response marks it read. The inbox is stored locally in `~/.xmuggle/responses.json`
and survives restarts and deletion of original attachments.

While xmuggle is running, it checks the shared queue every 10 seconds and after
wake. Newly collected responses trigger an in-app alert and an OS notification
when supported and permitted by system settings. Clicking the notification opens
the inbox. Tasks completed while the app was closed appear on the next sync.
Only tasks submitted by this machine's hostname are collected. Existing completed
tasks are imported on the first sync; subsequent syncs do not repeat their alerts.
