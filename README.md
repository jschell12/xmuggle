# xmuggle

Screenshot-driven code fixes. Take a screenshot, and a Claude agent analyzes the image, finds the relevant code, and fixes it.

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
