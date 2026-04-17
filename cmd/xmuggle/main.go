// Command xmuggle is the primary CLI — runs a Claude Code agent against a
// screenshot to fix code in a target repo.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jschell12/xmuggle/internal/ageutil"
	"github.com/jschell12/xmuggle/internal/config"
	"github.com/jschell12/xmuggle/internal/daemoninstall"
	"github.com/jschell12/xmuggle/internal/discover"
	"github.com/jschell12/xmuggle/internal/gitqueue"
	"github.com/jschell12/xmuggle/internal/images"
	"github.com/jschell12/xmuggle/internal/prompt"
	"github.com/jschell12/xmuggle/internal/queue"
	"github.com/jschell12/xmuggle/internal/record"
	"github.com/jschell12/xmuggle/internal/remote"
	"github.com/jschell12/xmuggle/internal/spawn"
)

const usage = `Usage: xmuggle [<subcommand>] [flags]

Main flags:
  --repo  <repo>   GitHub repo (owner/name or URL) or local path
  --img   <name>   Select image by fuzzy match (repeatable)
  --all            Process ALL unprocessed images
  --msg   <msg>    Optional context
  --list           Show images in ~/.xmuggle/ and status
  --scan           Ingest ALL images from ~/Desktop (not just screenshots)

Transports:
  (default)        process locally
  --remote         SSH/rsync to a Mac on the LAN
    --host <host>  specific hostname (otherwise dns-sd discovery)
    --user <user>  SSH user (default: $USER)
  --remote --git   age-encrypted via private GitHub queue repo
    --to <host>    recipient hostname (overrides default_recipient)

Subcommands:
  xmuggle rec [--duration 30s] [--fps 1] [--format jpg] [--repo <repo>] [--msg <msg>] [transport flags]
  xmuggle rm <name>... [--all-done]           # remove images from ~/.xmuggle/ by fuzzy name
  xmuggle init-recv <owner/repo> [--peer <sender>] [--json]    # receiver: setup + daemon
  xmuggle init-send <owner/repo> [--peer <receiver>] [--json]  # sender: setup
  xmuggle peers                               # list receivers and senders in the queue repo
  xmuggle add-recipient <host> [--pubkey age1...] [--default]
  xmuggle list-recipients

Examples:
  xmuggle --repo jschell12/my-app                              # latest screenshot locally
  xmuggle --repo jschell12/my-app --all --msg "fix alignment"  # all pending
  xmuggle --repo jschell12/my-app --remote --git               # encrypted via git
  xmuggle --list
`

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		// No args means: use default behavior (like --list-ish?). Keep it explicit.
		fmt.Print(usage)
		os.Exit(0)
	}

	first := os.Args[1]
	switch first {
	case "-h", "--help":
		fmt.Print(usage)
		return
	case "init-recv":
		cmdInitRecv(os.Args[2:])
		return
	case "init-send":
		cmdInitSend(os.Args[2:])
		return
	case "peers":
		cmdPeers()
		return
	case "rm":
		cmdRm(os.Args[2:])
		return
	case "rec":
		cmdRec(os.Args[2:])
		return
	case "add-recipient":
		cmdAddRecipient(os.Args[2:])
		return
	case "list-recipients":
		cmdListRecipients()
		return
	}

	runMain(os.Args[1:])
}

type mainArgs struct {
	repo, message, host, user, to string
	remote, useGit, list, scan, all bool
	imgs []string
}

func parseMainArgs(args []string) *mainArgs {
	a := &mainArgs{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			i++
			if i < len(args) {
				a.repo = args[i]
			}
		case "--msg":
			i++
			if i < len(args) {
				a.message = args[i]
			}
		case "--img":
			i++
			if i < len(args) {
				a.imgs = append(a.imgs, args[i])
			}
		case "--host":
			i++
			if i < len(args) {
				a.host = args[i]
			}
		case "--user":
			i++
			if i < len(args) {
				a.user = args[i]
			}
		case "--to":
			i++
			if i < len(args) {
				a.to = args[i]
			}
		case "--remote":
			a.remote = true
		case "--git":
			a.useGit = true
		case "--list":
			a.list = true
		case "--scan":
			a.scan = true
		case "--all":
			a.all = true
		}
	}
	return a
}

func runMain(rawArgs []string) {
	a := parseMainArgs(rawArgs)

	if a.scan {
		n, err := images.IngestAll()
		if err != nil {
			die("scan: %v", err)
		}
		fmt.Printf("Ingested %d image(s) into ~/.xmuggle/\n", n)
		if a.repo == "" {
			return
		}
	}

	if a.list {
		n, _ := images.AutoIngest()
		if n > 0 {
			fmt.Printf("Auto-ingested %d new screenshot(s)\n\n", n)
		}
		imgs, err := images.ListAll()
		if err != nil {
			die("list: %v", err)
		}
		if len(imgs) == 0 {
			fmt.Println("No images in ~/.xmuggle/")
			fmt.Println("Take a screenshot, or run --scan to ingest all images from ~/Desktop.")
			return
		}
		unprocessed := 0
		for _, img := range imgs {
			if !img.IsProcessed {
				unprocessed++
			}
		}
		fmt.Printf("%d image(s) in ~/.xmuggle/ (%d unprocessed):\n\n", len(imgs), unprocessed)
		for _, img := range imgs {
			status := "pending"
			if img.IsProcessed {
				status = "done"
			}
			fmt.Printf("  [%s] %s\n", status, img.Name)
		}
		return
	}

	if a.repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --repo is required")
		fmt.Println(usage)
		os.Exit(1)
	}

	var shotPaths []string
	switch {
	case len(a.imgs) > 0:
		for _, q := range a.imgs {
			img, err := images.FindByName(q)
			if err != nil || img == nil {
				die("no image matching %q in ~/.xmuggle/ (run --list)", q)
			}
			shotPaths = append(shotPaths, img.Path)
		}
	case a.all:
		ups, err := images.AllUnprocessed()
		if err != nil {
			die("find unprocessed: %v", err)
		}
		if len(ups) == 0 {
			die("No unprocessed images. Take a screenshot or run --scan.")
		}
		for _, img := range ups {
			shotPaths = append(shotPaths, img.Path)
		}
		fmt.Printf("Found %d unprocessed image(s)\n", len(shotPaths))
	default:
		img, err := images.Latest()
		if err != nil {
			die("find latest: %v", err)
		}
		if img == nil {
			die("No unprocessed images. Take a screenshot or run --scan.")
		}
		shotPaths = []string{img.Path}
	}

	var names []string
	for _, p := range shotPaths {
		names = append(names, filepath.Base(p))
	}
	fmt.Printf("Screenshot(s): %s\n", strings.Join(names, ", "))
	fmt.Printf("Target repo: %s\n", a.repo)
	if a.message != "" {
		fmt.Printf("Context: %s\n", a.message)
	}
	mode := "local"
	if a.useGit {
		mode = "remote (git)"
	} else if a.remote {
		mode = "remote (ssh)"
	}
	fmt.Printf("Mode: %s\n---\n", mode)

	switch {
	case a.useGit:
		runRemoteGit(shotPaths, a)
	case a.remote:
		runRemoteSSH(shotPaths, a)
	default:
		runLocal(shotPaths, a)
	}
}

func runLocal(shotPaths []string, a *mainArgs) {
	p := prompt.Build(prompt.Options{
		ScreenshotPaths: shotPaths,
		Repo:            a.repo,
		Message:         a.message,
	})
	code, err := spawn.Interactive(p, "")
	if err != nil {
		die("%v", err)
	}
	for _, sp := range shotPaths {
		_ = images.MarkProcessed(sp)
	}
	os.Exit(code)
}

func runRemoteSSH(shotPaths []string, a *mainArgs) {
	host := a.host
	if host == "" {
		fmt.Println("Discovering Macs on the LAN...")
		svcs, err := discover.DiscoverAll(4 * time.Second)
		if err != nil || len(svcs) == 0 {
			die("no Macs discovered. Pass --host <hostname>")
		}
		fmt.Println("Discovered SSH hosts:")
		for i, s := range svcs {
			fmt.Printf("  [%d] %s -> %s\n", i+1, s.Instance, s.Host)
		}
		fmt.Print("Choose target: ")
		var choice int
		_, _ = fmt.Scanln(&choice)
		if choice < 1 || choice > len(svcs) {
			die("invalid choice")
		}
		host = svcs[choice-1].Host
	}

	target := remote.Target{Host: host, User: a.user}
	fmt.Printf("Remote: %s\n", host)

	taskID := queue.NewTaskID()
	tmpBase := filepath.Join(os.TempDir(), "xmuggle-tasks")
	_ = os.MkdirAll(tmpBase, 0o755)

	t := queue.Task{
		Repo:      a.repo,
		Message:   a.message,
		Timestamp: time.Now().UnixMilli(),
		Status:    queue.StatusPending,
	}

	var taskDir string
	var err error
	if len(shotPaths) == 1 {
		taskDir, err = queue.WriteTask(tmpBase, taskID, t, shotPaths[0])
	} else {
		taskDir, err = queue.WriteTaskMulti(tmpBase, taskID, t, shotPaths)
	}
	if err != nil {
		die("write task: %v", err)
	}
	fmt.Printf("Sending task %s (%d image(s))...\n", taskID, len(shotPaths))
	if err := remote.SendTask(target, taskDir, taskID); err != nil {
		die("send: %v", err)
	}
	for _, sp := range shotPaths {
		_ = images.MarkProcessed(sp)
	}

	fmt.Println("Waiting for result...")
	pollForResults([]string{taskID}, func(id string) (*queue.Result, error) {
		return remote.PollForResult(target, id, 10*time.Minute, 5*time.Second)
	}, a.repo)
}

func runRemoteGit(shotPaths []string, a *mainArgs) {
	cfg, err := config.Load()
	if err != nil {
		die("load config: %v", err)
	}
	if cfg.Git == nil {
		die("git transport not configured. Run: xmuggle init-send <owner/repo> (or init-recv on the processing machine)")
	}
	if cfg.Age == nil {
		die("no age keypair. Run: xmuggle init-send <owner/repo> (or init-recv on the processing machine)")
	}

	recipient := a.to
	if recipient == "" {
		recipient = cfg.DefaultRecipient
	}
	fmt.Printf("Queue repo: %s\n", cfg.Git.QueueRepo)
	fmt.Printf("Recipient: %s\n", recipient)

	taskID := queue.NewTaskID()
	fmt.Printf("Encrypting and pushing task %s (%d image(s))...\n", taskID, len(shotPaths))
	err = gitqueue.SendTask(cfg, gitqueue.SendArgs{
		TaskID:          taskID,
		Repo:            a.repo,
		Message:         a.message,
		ScreenshotPaths: shotPaths,
		Recipient:       a.to,
	})
	if err != nil {
		die("send (git): %v", err)
	}
	for _, sp := range shotPaths {
		_ = images.MarkProcessed(sp)
	}

	fmt.Println("Waiting for result...")
	pollForResults([]string{taskID}, func(id string) (*queue.Result, error) {
		r, err := gitqueue.PollForResult(cfg, id, 10*time.Minute)
		if err != nil {
			return nil, err
		}
		return &queue.Result{
			Status:    r.Status,
			PRUrl:     r.PRUrl,
			Branch:    r.Branch,
			Summary:   r.Summary,
			Timestamp: r.Timestamp,
		}, nil
	}, a.repo)
}

// pollForResults drains results serially and prints summaries, then optionally git-pulls in a local repo.
func pollForResults(taskIDs []string, poll func(string) (*queue.Result, error), repo string) {
	failed := false
	for _, id := range taskIDs {
		r, err := poll(id)
		fmt.Printf("\n--- Task %s ---\n", id)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
			continue
		}
		if r.Status == "success" {
			fmt.Println("Fix applied successfully!")
			if r.PRUrl != "" {
				fmt.Println("PR:", r.PRUrl)
			}
			if r.Branch != "" {
				fmt.Println("Branch:", r.Branch)
			}
		} else {
			fmt.Fprintln(os.Stderr, "Agent reported an error:")
			fmt.Fprintln(os.Stderr, r.Summary)
			failed = true
		}
	}

	if _, err := os.Stat(repo); err == nil {
		fmt.Printf("\nPulling latest in %s...\n", repo)
		cmd := exec.Command("git", "pull")
		cmd.Dir = repo
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
	}
	if failed {
		os.Exit(1)
	}
}

//
// Subcommands
//

// setupQueueRepo registers the queue repo and scaffolds its directories.
func setupQueueRepo(cfg *config.Config, slug string) {
	cfg.SetGit(slug)
	if err := config.Save(cfg); err != nil {
		die("save config: %v", err)
	}

	fmt.Printf("Cloning %s into %s...\n", slug, cfg.Git.CloneDir)
	if err := gitqueue.EnsureCloned(slug, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		die("clone: %v", err)
	}

	var touched []string
	for _, d := range []string{"queue", "results", "pubkeys", "processed", "roles/recv", "roles/send"} {
		rel := d + "/.gitkeep"
		if !gitqueue.FileExists(cfg.Git.CloneDir, rel) {
			_ = gitqueue.WriteFile(cfg.Git.CloneDir, rel, []byte(""))
			touched = append(touched, rel)
		}
	}
	if len(touched) > 0 {
		if err := gitqueue.CommitAndPush(cfg.Git.CloneDir, touched, "Scaffold queue repo directories", cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail); err != nil {
			die("commit: %v", err)
		}
	}
	fmt.Printf("Queue repo ready: %s\n", slug)
}

// ensureKeypair generates a keypair if missing, otherwise reads the existing one.
// Returns the public key.
func ensureKeypair(cfg *config.Config) string {
	identityPath := config.DefaultIdentityPath()
	if cfg.Age != nil && cfg.Age.IdentityFile != "" {
		identityPath = cfg.Age.IdentityFile
	}

	var pub string
	if _, err := os.Stat(identityPath); err == nil {
		p, err := ageutil.ReadIdentityPubkey(identityPath)
		if err != nil {
			die("read existing identity: %v", err)
		}
		pub = p
		fmt.Printf("Identity exists at %s\n", identityPath)
	} else {
		fmt.Printf("Generating age keypair at %s...\n", identityPath)
		p, err := ageutil.GenerateKeypair(identityPath)
		if err != nil {
			die("generate keypair: %v", err)
		}
		pub = p
	}

	cfg.SetAge(identityPath, pub)
	if err := config.Save(cfg); err != nil {
		die("save config: %v", err)
	}
	fmt.Printf("Public key: %s\n", pub)
	return pub
}

// baseInit performs the setup common to both init-recv and init-send:
// ensures dirs, loads config, scaffolds queue repo, ensures age keypair,
// publishes pubkey. Returns the loaded config. Idempotent.
func baseInit(slug string) *config.Config {
	if !strings.Contains(slug, "/") {
		die("Invalid slug: expected owner/repo")
	}
	if err := config.EnsureDirs(); err != nil {
		die("ensure dirs: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		die("load config: %v", err)
	}

	setupQueueRepo(cfg, slug)
	ensureKeypair(cfg)
	publishOwnPubkey(cfg)
	return cfg
}

// initArgs holds the parsed result of init-recv / init-send arguments.
type initArgs struct {
	slug    string
	peer    string
	useJSON bool
}

// parseInitArgs parses <slug> [--peer <host>] [--json]. `cmd` is used in the
// usage error string only.
func parseInitArgs(args []string, cmd string) initArgs {
	var a initArgs
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--peer":
			if i+1 >= len(args) {
				die("Usage: xmuggle %s <owner/repo> [--peer <hostname>] [--json]", cmd)
			}
			a.peer = args[i+1]
			i += 2
		case "--json":
			a.useJSON = true
			i++
		default:
			if a.slug == "" && !strings.HasPrefix(args[i], "--") {
				a.slug = args[i]
				i++
			} else {
				i++
			}
		}
	}
	if a.slug == "" {
		die("Usage: xmuggle %s <owner/repo> [--peer <hostname>] [--json]", cmd)
	}
	return a
}

// cmdInitRecv sets up this machine as a receiver: base init + installs and
// loads the launchd daemon so it starts processing incoming queue tasks.
//
// Optionally accepts --peer <sender-host> to cache that sender's pubkey
// locally, and --json for non-interactive (AI-driven) use.
func cmdInitRecv(args []string) {
	opts := parseInitArgs(args, "init-recv")
	cfg := baseInit(opts.slug)
	publishRoleMarker(cfg, "recv")

	fmt.Println()
	fmt.Println("Installing daemon...")
	if err := daemoninstall.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		fmt.Fprintln(os.Stderr, "Fallback: run `make daemon-install` from the repo.")
		os.Exit(1)
	}
	fmt.Println("Daemon installed and running.")

	peers, _ := discoverPeers(cfg, "send")

	if opts.useJSON {
		emitPeersJSON(cfg, "sender", peers)
		return
	}

	var selected string
	switch {
	case opts.peer != "":
		selected = opts.peer
	case interactive() && len(peers) > 0:
		selected = promptSelectPeer("sender", peers)
	default:
		fmt.Println()
		printDiscoveredPeers("sender", peers)
		fmt.Println()
		fmt.Println("This machine will now process queue tasks addressed to it.")
		fmt.Println("Tell senders to run:")
		fmt.Printf("  xmuggle init-send %s\n", opts.slug)
		fmt.Printf("  xmuggle add-recipient %s --default\n", mustHostname())
		return
	}

	if selected == "" {
		return
	}
	pub, err := fetchPeerPubkey(cfg, selected)
	if err != nil {
		die("fetch pubkey for %s: %v", selected, err)
	}
	cfg.UpsertRecipient(config.Recipient{Hostname: selected, Pubkey: pub})
	if err := config.Save(cfg); err != nil {
		die("save config: %v", err)
	}
	fmt.Printf("Cached sender pubkey for: %s\n", selected)
}

// publishRoleMarker writes roles/<role>/<hostname> to the queue repo
// (idempotent — no-op if the file is already there).
func publishRoleMarker(cfg *config.Config, role string) {
	if cfg.Git == nil {
		return
	}
	rel := fmt.Sprintf("roles/%s/%s", role, cfg.Hostname)
	if gitqueue.FileExists(cfg.Git.CloneDir, rel) {
		return
	}
	if err := gitqueue.WriteFile(cfg.Git.CloneDir, rel, []byte("")); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not write role marker: %v\n", err)
		return
	}
	msg := fmt.Sprintf("Register %s as %s", cfg.Hostname, role)
	if err := gitqueue.CommitAndPush(cfg.Git.CloneDir, []string{rel}, msg, cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not push role marker: %v\n", err)
	}
}

// cmdInitSend sets up this machine as a sender: base init + role marker.
// Optionally accepts --peer <receiver-host> to set default_recipient in one
// shot, and --json for non-interactive (AI-driven) use.
func cmdInitSend(args []string) {
	opts := parseInitArgs(args, "init-send")
	cfg := baseInit(opts.slug)
	publishRoleMarker(cfg, "send")

	peers, _ := discoverPeers(cfg, "recv")

	if opts.useJSON {
		emitPeersJSON(cfg, "receiver", peers)
		return
	}

	var selected string
	switch {
	case opts.peer != "":
		selected = opts.peer
	case interactive() && len(peers) > 0:
		selected = promptSelectPeer("receiver", peers)
	default:
		fmt.Println()
		printDiscoveredPeers("receiver", peers)
		fmt.Println()
		fmt.Println("Pick one with:")
		fmt.Println("  xmuggle add-recipient <hostname> --default")
		return
	}

	if selected == "" {
		return
	}
	pub, err := fetchPeerPubkey(cfg, selected)
	if err != nil {
		die("fetch pubkey for %s: %v", selected, err)
	}
	cfg.UpsertRecipient(config.Recipient{Hostname: selected, Pubkey: pub})
	cfg.DefaultRecipient = selected
	if err := config.Save(cfg); err != nil {
		die("save config: %v", err)
	}
	fmt.Printf("Default recipient: %s\n", selected)
	fmt.Println()
	fmt.Println("Ready to send. Try:")
	fmt.Println("  xmuggle --repo <repo> --remote --git --msg \"fix this\"")
}

// discoverPeers reads roles/<role>/* from the queue repo clone and returns
// hostnames excluding this machine's own hostname.
func discoverPeers(cfg *config.Config, role string) ([]string, error) {
	if cfg.Git == nil {
		return nil, fmt.Errorf("git transport not configured")
	}
	dir := filepath.Join(cfg.Git.CloneDir, "roles", role)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() == cfg.Hostname {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// fetchPeerPubkey pulls the queue repo and returns pubkeys/<host>.pub.
func fetchPeerPubkey(cfg *config.Config, host string) (string, error) {
	if cfg.Git == nil {
		return "", fmt.Errorf("git transport not configured")
	}
	if err := gitqueue.EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return "", err
	}
	_ = gitqueue.PullRebase(cfg.Git.CloneDir, cfg.Git.Branch)
	rel := fmt.Sprintf("pubkeys/%s.pub", host)
	if !gitqueue.FileExists(cfg.Git.CloneDir, rel) {
		return "", fmt.Errorf("no pubkey at %s in %s (has that machine run 'xmuggle init-*' yet?)", rel, cfg.Git.QueueRepo)
	}
	data, err := gitqueue.ReadFile(cfg.Git.CloneDir, rel)
	if err != nil {
		return "", err
	}
	pub := strings.TrimSpace(string(data))
	if !strings.HasPrefix(pub, "age1") {
		return "", fmt.Errorf("%s contains something that is not an age pubkey", rel)
	}
	return pub, nil
}

// promptSelectPeer shows a numbered menu and returns the chosen hostname,
// or "" if skipped / invalid.
func promptSelectPeer(role string, peers []string) string {
	labelPlural := role + "s"
	fmt.Printf("\nRegistered %s:\n", labelPlural)
	for i, p := range peers {
		fmt.Printf("  [%d] %s\n", i+1, p)
	}
	fmt.Printf("  [0] skip\n")
	fmt.Printf("Select one [0]: ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "0" {
		return ""
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(peers) {
		fmt.Println("(invalid selection — skipping)")
		return ""
	}
	return peers[n-1]
}

// printDiscoveredPeers lists peers informationally (no prompt).
func printDiscoveredPeers(role string, peers []string) {
	labelPlural := role + "s"
	fmt.Printf("Registered %s:\n", labelPlural)
	if len(peers) == 0 {
		fmt.Printf("  (none found — the %s needs to run 'xmuggle init-%s' first)\n", role, shortRole(role))
		return
	}
	for _, p := range peers {
		fmt.Printf("  - %s\n", p)
	}
}

func shortRole(role string) string {
	switch role {
	case "sender":
		return "send"
	case "receiver":
		return "recv"
	}
	return role
}

// emitPeersJSON prints a machine-readable choice summary.
func emitPeersJSON(cfg *config.Config, peerRole string, peers []string) {
	if peers == nil {
		peers = []string{}
	}
	action := "select-peer"
	hint := fmt.Sprintf("Re-run with --peer <hostname> to proceed.")
	if len(peers) == 0 {
		action = "no-peers"
		hint = fmt.Sprintf("No %ss registered yet. Base setup is complete — add a peer later with 'xmuggle add-recipient <host> --default' or re-run init with --peer once a %s has registered.", peerRole, peerRole)
	}
	payload := map[string]any{
		"action":     action,
		"role":       peerRole,
		"peers":      peers,
		"queue_repo": cfg.Git.QueueRepo,
		"hint":       hint,
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(b))
}

// interactive returns true if stdin is connected to a terminal.
func interactive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func mustHostname() string {
	h, _ := os.Hostname()
	return config.NormalizeHostname(h)
}

// cmdPeers lists registered receivers and senders from the queue repo.
func cmdPeers() {
	cfg, err := config.Load()
	if err != nil {
		die("load config: %v", err)
	}
	if cfg.Git == nil {
		die("git transport not configured. Run: xmuggle init-send <owner/repo> or init-recv <owner/repo>")
	}
	if err := gitqueue.EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		die("reach queue repo: %v", err)
	}
	_ = gitqueue.PullRebase(cfg.Git.CloneDir, cfg.Git.Branch)

	listRole := func(role string) []string {
		dir := filepath.Join(cfg.Git.CloneDir, "roles", role)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, e.Name())
			}
		}
		return out
	}

	recvs := listRole("recv")
	sends := listRole("send")

	fmt.Printf("Queue repo: %s\n\n", cfg.Git.QueueRepo)

	fmt.Println("Receivers (process incoming tasks):")
	if len(recvs) == 0 {
		fmt.Println("  (none registered)")
	}
	for _, h := range recvs {
		marker := ""
		if h == cfg.Hostname {
			marker = "  ← this machine"
		}
		fmt.Printf("  %s%s\n", h, marker)
	}

	fmt.Println()
	fmt.Println("Senders (submit tasks):")
	if len(sends) == 0 {
		fmt.Println("  (none registered)")
	}
	for _, h := range sends {
		marker := ""
		if h == cfg.Hostname {
			marker = "  ← this machine"
		}
		fmt.Printf("  %s%s\n", h, marker)
	}
}

func publishOwnPubkey(cfg *config.Config) {
	if cfg.Git == nil || cfg.Age == nil {
		return
	}
	if err := gitqueue.EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not reach queue repo: %v\n", err)
		return
	}
	_ = gitqueue.PullRebase(cfg.Git.CloneDir, cfg.Git.Branch)

	rel := fmt.Sprintf("pubkeys/%s.pub", cfg.Hostname)
	existing := ""
	if gitqueue.FileExists(cfg.Git.CloneDir, rel) {
		data, err := gitqueue.ReadFile(cfg.Git.CloneDir, rel)
		if err == nil {
			existing = strings.TrimSpace(string(data))
		}
	}
	if existing == cfg.Age.Pubkey {
		fmt.Printf("Pubkey already published at %s\n", rel)
		return
	}
	if err := gitqueue.WriteFile(cfg.Git.CloneDir, rel, []byte(cfg.Age.Pubkey+"\n")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if err := gitqueue.CommitAndPush(cfg.Git.CloneDir, []string{rel}, "Publish pubkey for "+cfg.Hostname, cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Printf("Published %s to %s\n", rel, cfg.Git.QueueRepo)
}

func cmdAddRecipient(args []string) {
	if len(args) < 1 {
		die("Usage: xmuggle add-recipient <hostname> [--pubkey age1...] [--default]")
	}
	hostname := args[0]
	var pubkey string
	asDefault := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--pubkey":
			i++
			if i < len(args) {
				pubkey = args[i]
			}
		case "--default":
			asDefault = true
		}
	}
	cfg, err := config.Load()
	if err != nil {
		die("load config: %v", err)
	}

	if pubkey == "" {
		if cfg.Git == nil {
			die("no pubkey given and no queue repo. Pass --pubkey or run 'xmuggle init-send <owner/repo>' first.")
		}
		p, err := fetchPeerPubkey(cfg, hostname)
		if err != nil {
			die("%v", err)
		}
		pubkey = p
	}

	if !strings.HasPrefix(pubkey, "age1") {
		die("%q doesn't look like an age pubkey", pubkey)
	}

	cfg.UpsertRecipient(config.Recipient{Hostname: hostname, Pubkey: pubkey})
	if asDefault || cfg.DefaultRecipient == "" {
		cfg.DefaultRecipient = hostname
		fmt.Printf("Default recipient: %s\n", hostname)
	}
	if err := config.Save(cfg); err != nil {
		die("save: %v", err)
	}
	fmt.Printf("Added recipient %s\n", hostname)
}

// cmdRec captures screen frames and optionally auto-submits them as one task.
func cmdRec(args []string) {
	var (
		durStr  string
		fpsVal  float64 = 1.0
		format  string  = "jpg"
		repo    string
		message string
		useGit  bool
		isRemote bool
		host    string
		user    string
		to      string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--duration":
			i++
			if i < len(args) {
				durStr = args[i]
			}
		case "--fps":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err == nil {
					fpsVal = v
				}
			}
		case "--format":
			i++
			if i < len(args) {
				format = args[i]
			}
		case "--repo":
			i++
			if i < len(args) {
				repo = args[i]
			}
		case "--msg":
			i++
			if i < len(args) {
				message = args[i]
			}
		case "--remote":
			isRemote = true
		case "--git":
			useGit = true
		case "--host":
			i++
			if i < len(args) {
				host = args[i]
			}
		case "--user":
			i++
			if i < len(args) {
				user = args[i]
			}
		case "--to":
			i++
			if i < len(args) {
				to = args[i]
			}
		}
	}

	var dur time.Duration
	if durStr != "" {
		d, err := time.ParseDuration(durStr)
		if err != nil {
			die("invalid duration %q: %v", durStr, err)
		}
		dur = d
	}

	tmpDir := filepath.Join(os.TempDir(), "xmuggle-rec")
	_ = os.MkdirAll(tmpDir, 0o755)

	rec := record.New(record.Options{
		Duration:  dur,
		FPS:       fpsVal,
		Format:    format,
		OutputDir: tmpDir,
	})

	if dur > 0 {
		fmt.Fprintf(os.Stderr, "Recording for %s (%.0f fps, %s)...\n", dur, fpsVal, format)
	} else {
		fmt.Fprintf(os.Stderr, "Recording (%.0f fps, %s)... Press Ctrl+C to stop.\n", fpsVal, format)
	}

	if err := rec.Start(); err != nil {
		die("%v", err)
	}

	// Wait for Ctrl+C or duration expiry
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	if dur > 0 {
		select {
		case <-sigCh:
		case <-time.After(dur + 500*time.Millisecond):
		}
	} else {
		<-sigCh
	}
	signal.Stop(sigCh)

	frames := rec.Stop()
	fmt.Fprintf(os.Stderr, "\nCaptured %d frame(s)\n", len(frames))

	if len(frames) == 0 {
		fmt.Println("No frames captured.")
		return
	}

	// Move frames to ~/.xmuggle/
	storeDir := config.GetPaths().ConfigDir
	var storePaths []string
	for _, f := range frames {
		dst := filepath.Join(storeDir, filepath.Base(f))
		if err := os.Rename(f, dst); err != nil {
			// Cross-device? copy instead
			data, err2 := os.ReadFile(f)
			if err2 != nil {
				die("move frame: %v", err)
			}
			if err2 := os.WriteFile(dst, data, 0o644); err2 != nil {
				die("write frame: %v", err2)
			}
			_ = os.Remove(f)
		}
		storePaths = append(storePaths, dst)
	}

	fmt.Printf("Frames saved to ~/.xmuggle/ (%s-*)\n", rec.Prefix())

	if repo == "" {
		fmt.Println("\nTo submit:")
		fmt.Printf("  xmuggle --repo <repo> --img %s\n", rec.Prefix())
		return
	}

	// Auto-submit
	a := &mainArgs{
		repo:    repo,
		message: message,
		useGit:  useGit,
		remote:  isRemote,
		host:    host,
		user:    user,
		to:      to,
	}

	fmt.Printf("\nSubmitting %d frames to %s...\n", len(storePaths), repo)
	switch {
	case useGit:
		runRemoteGit(storePaths, a)
	case isRemote:
		runRemoteSSH(storePaths, a)
	default:
		runLocal(storePaths, a)
	}
}

// cmdRm removes images from ~/.xmuggle/ by fuzzy name match.
// Accepts one or more <name> args, plus --all-done to delete every processed image.
func cmdRm(args []string) {
	if len(args) == 0 {
		die("Usage: xmuggle rm <name>... [--all-done]")
	}

	var names []string
	allDone := false
	for _, a := range args {
		if a == "--all-done" {
			allDone = true
			continue
		}
		names = append(names, a)
	}

	var failed bool

	for _, q := range names {
		name, err := images.RemoveByName(q)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			failed = true
			continue
		}
		fmt.Printf("Removed: %s\n", name)
	}

	if allDone {
		removed, err := images.RemoveAllDone()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			failed = true
		}
		if len(removed) == 0 {
			fmt.Println("No processed images to remove.")
		} else {
			fmt.Printf("Removed %d processed image(s):\n", len(removed))
			for _, n := range removed {
				fmt.Println("  " + n)
			}
		}
	}

	if failed {
		os.Exit(1)
	}
}

func cmdListRecipients() {
	cfg, err := config.Load()
	if err != nil {
		die("load config: %v", err)
	}
	fmt.Println("Hostname:", cfg.Hostname)
	if cfg.Age != nil {
		fmt.Println("Self pubkey:", cfg.Age.Pubkey)
	}
	if cfg.Git != nil {
		fmt.Println("Queue repo:", cfg.Git.QueueRepo)
	}
	fmt.Println()

	if len(cfg.Recipients) == 0 {
		fmt.Println("No recipients configured.")
		return
	}
	fmt.Println("Recipients:")
	for _, r := range cfg.Recipients {
		marker := ""
		if r.Hostname == cfg.DefaultRecipient {
			marker = " (default)"
		}
		fmt.Printf("  %s%s\n    %s\n", r.Hostname, marker, r.Pubkey)
	}
	_ = json.Marshal // silence unused if imports get cleaned
}
