# xmuggle

Screenshot-driven code fixes. Take a screenshot, run `xmuggle`, and a Claude Code agent analyzes the image, finds the relevant code, fixes it, opens a PR, and merges it.

Written in Go. Single static binary. age encryption for git-based remote transport.

## Install

```bash
git clone git@github.com:jschell12/xmuggle.git
cd xmuggle
make install    # builds, installs binaries, adds /xmuggle skill
```

## Quick start (local — single machine)

```bash
# Take a screenshot, then:
xmuggle --list                                              # see what's pending
xmuggle --repo jschell12/my-app                             # fix the latest screenshot
xmuggle --repo jschell12/my-app --msg "button overlaps footer"   # with context
xmuggle --repo jschell12/my-app --all                       # fix all pending screenshots
xmuggle --repo jschell12/my-app --img bug1 --img bug2       # specific images, one task

# Screen recording (captures at 1fps, auto-submits)
xmuggle rec --duration 30s --repo jschell12/my-app --msg "UI glitch demo"
```

### What happens

1. Agent reads the screenshot(s) — Claude sees images natively
2. Analyzes what's wrong, using your message for context
3. Clones the repo, creates a branch
4. Finds and fixes the relevant code
5. Pushes, opens a PR, and merges it

## Remote setup (two machines, encrypted via GitHub)

For when you can't (or don't want to) run the agent locally — e.g. a VPN-locked work laptop forwarding tasks to your personal Mac.

### Step 1: Create a private queue repo (once)

```bash
gh repo create jschell12/xmuggle-queue --private
```

### Step 2: Set up the RECEIVING machine (personal laptop)

This machine runs the daemon that processes incoming tasks.

```bash
git clone git@github.com:jschell12/xmuggle.git && cd xmuggle
make install

# Run init-recv from inside the repo you want to fix:
cd ~/dev/my-app
xmuggle init-recv jschell12/xmuggle-queue
```

`init-recv` does everything in one command:
- Clones the queue repo + scaffolds directories
- Generates an age keypair + publishes your pubkey
- Installs + starts the daemon (launchd)
- If run from a git repo, registers that repo with this receiver (so senders don't need `--repo`)
- If senders are already registered, prompts you to cache their pubkey

Run `init-recv` again from other git repos to register additional repos with this receiver.

### Step 3: Set up the SENDING machine (work laptop)

This machine submits screenshot tasks.

```bash
git clone git@github.com:jschell12/xmuggle.git && cd xmuggle
make install
xmuggle init-send jschell12/xmuggle-queue
xmuggle add-recipient <receiver-hostname> --default
```

`init-send` sets up the queue repo + keypair and lists available receivers. Then `add-recipient` fetches the receiver's pubkey and sets it as the default target.

### Step 4: Start sending

```bash
# Take a screenshot on the work laptop, then:
xmuggle send --remote --git --msg "fix the login form"        # repo inferred from receiver
xmuggle send --remote --git --screenshots                      # pick screenshots interactively

# Override repo if needed:
xmuggle send --repo jschell12/other-app --remote --git --msg "fix something else"

# Or record the screen and send the frames:
xmuggle rec --duration 30s --remote --git --msg "watch the sidebar"
```

`--repo` is optional when using `--remote --git` — the target repo is resolved from whatever the receiver registered during `init-recv`. If the receiver has multiple repos, you'll be prompted to pick one.

The task is age-encrypted to the receiver's pubkey, committed to the queue repo. The receiver's daemon picks it up, spawns a Claude agent, fixes the code, pushes a PR, merges it, and encrypts the result back.

### Step 5: Verify

```bash
xmuggle peers         # see who's registered as sender/receiver
xmuggle list-recipients  # see configured pubkeys
```

## Remote (SSH/rsync — same LAN, no encryption)

If both Macs are on the same LAN without VPN issues:

```bash
# Bonjour discovers Macs advertising SSH
xmuggle --repo jschell12/my-app --remote

# Or specify directly
xmuggle --repo jschell12/my-app --remote --host macmini.local
```

Target Mac needs the daemon: `make daemon-install` or `xmuggle init-recv <queue-repo>`.

## Examples

### Local (single machine)

```bash
xmuggle --list                                               # see pending
xmuggle --repo jschell12/my-app --msg "fix the button"       # latest screenshot
xmuggle --repo jschell12/my-app --img bug1 --img bug2        # multi-image, one task
xmuggle rec --duration 30s --repo jschell12/my-app            # screen record + submit
```

### Remote (full session — receiver + sender + send)

```bash
# --- On your personal laptop (receiver) ---
cd ~/dev/my-app                                              # a git repo
xmuggle init-recv jschell12/xmuggle-queue
#   ✓ Queue repo cloned
#   ✓ Age keypair generated + pubkey published
#   ✓ Daemon installed and running
#   ✓ Registered my-app repo with this receiver

# --- On your work laptop (sender) ---
xmuggle init-send jschell12/xmuggle-queue
#   ✓ Queue repo cloned
#   ✓ Age keypair generated + pubkey published
#   Lists available receivers (with their repos) — pick one:
xmuggle add-recipient joshs-macbook-pro --default

# --- Now send from the work laptop (--repo is optional) ---
xmuggle send --remote --git --msg "fix the login form"      # repo from receiver
xmuggle send --all --remote --git --msg "fix all pending"
xmuggle send --repo jschell12/other-app --remote --git       # override repo
xmuggle rec --duration 30s --remote --git --msg "UI glitch"

# --- Check status ---
xmuggle peers                                                # who's registered
xmuggle list                                                 # pending images
```

### Cleanup

```bash
xmuggle rm "Screenshot 2026-04-12"                           # remove by name
xmuggle rm --all-done                                        # remove all processed
```

## Image detection

Screenshots are auto-detected from `~/Desktop` via macOS Spotlight (`kMDItemIsScreenCapture`) and copied into `~/.xmuggle/` on every run. No manual step needed — just take a screenshot and go.

- `~/.xmuggle/.tracked` — processed filenames
- `~/.xmuggle/.seen` — source paths we've ingested (prevents re-copying)
- `--scan` — ingest ALL images from ~/Desktop (not just screenshots)
- `xmuggle rm <name>...` — remove images; `xmuggle rm --all-done` for bulk cleanup

## Screen recording

```bash
xmuggle rec                                  # record until Ctrl+C
xmuggle rec --duration 30s                   # fixed duration
xmuggle rec --duration 1m --fps 2            # 2 frames/sec
xmuggle rec --duration 30s --repo jschell12/my-app --msg "UI glitch"        # auto-submit locally
xmuggle rec --duration 30s --repo jschell12/my-app --remote --git --msg "demo"  # auto-submit via git
```

Requires Screen Recording permission for your terminal app (System Settings > Privacy & Security > Screen Recording).

## Architecture

```
xmuggle (CLI)                       xmuggled (daemon)
────────────                        ────────────
Local mode:                         Watches ~/.xmuggle/queue/ every 5s
  spawn claude + prompt             Enqueues tasks to agent-queue
                                    Spawns workers (up to MAX_WORKERS)
Remote (SSH):                       Workers: claim → fix → agent-merge → complete
  rsync task → polling
                                    Git sync (if configured):
Remote (git):                         Pulls queue repo every N seconds
  age-encrypt + commit + push         Decrypts new tasks addressed to us
  poll for encrypted result           Encrypts + pushes results back
```

## Running while the Mac is asleep

When a MacBook sleeps on battery, macOS suspends userland processes — the daemon
can't poll the queue. Two knobs, both configurable via `make`:

```bash
# Wrap the daemon in `caffeinate -i` so idle sleep is prevented while it runs.
# Note: this does NOT keep the Mac awake when the lid is closed on battery —
# that's system sleep, which no userland process can block.
make daemon-install SLEEP_MODE=awake

# Schedule a daily wake-from-sleep (uses `pmset repeat wakeorpoweron`, sudo).
# On wake the daemon drains the queue, then the Mac re-sleeps.
sudo make daemon-wake-schedule WAKE_TIMES=09:00:00
sudo make daemon-wake-schedule WAKE_TIMES=12:00:00 WAKE_DAYS=MTWRF
sudo make daemon-wake-unschedule
```

`pmset repeat` only supports one recurring time per day; for higher-frequency
polling, run the receiver on an always-on machine (Mac mini) — that's what the
remote architecture is designed for.

## Make targets

| Command | Purpose |
|---|---|
| `make install` | Build, install binaries + `/xmuggle` skill |
| `make install-skill` | Install just the skill files (no build) |
| `make daemon-install [SLEEP_MODE=awake]` | Install queue-processing daemon (launchd) |
| `make daemon-start / -stop / -logs` | Control the daemon |
| `make daemon-uninstall` | Remove the daemon |
| `sudo make daemon-wake-schedule WAKE_TIMES=HH:MM:SS` | Schedule daily wake-from-sleep |
| `sudo make daemon-wake-unschedule` | Cancel the pmset wake schedule |
| `make link` | Interactive LAN discovery (mac-link.sh) |
| `make uninstall-tool` | Remove all xmuggle binaries, plists, skills |

## Requirements

- macOS (uses `mdfind`, `dns-sd`, `screencapture`)
- Go 1.26+ (to build)
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code)
- [GitHub CLI](https://cli.github.com/) (`gh`) authenticated
- [agent-queue](https://github.com/jschell12/agent-queue) (for daemon)
- `git`, `rsync`, `ssh` (stdlib on macOS)

No dependency on `age` CLI — the age protocol is embedded in the binary via `filippo.io/age`.
