package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	xmuggleDir = filepath.Join(homeDir(), ".xmuggle")
	configFile = filepath.Join(xmuggleDir, "daemon.json")
	pidFile    = filepath.Join(xmuggleDir, "daemon.pid")
	logFile    = filepath.Join(xmuggleDir, "daemon.log")
	inboxDir   = filepath.Join(xmuggleDir, "inbox")
	syncDir    = filepath.Join(xmuggleDir, "sync")
)

type RepoConfig struct {
	Path     string   `json:"path"`
	Pull     *bool    `json:"pull,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

func (r *RepoConfig) UnmarshalJSON(data []byte) error {
	// Accept plain string: "/path/to/repo"
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.Path = s
		return nil
	}
	// Accept object: {"path": "/path/to/repo", ...}
	type alias RepoConfig
	return json.Unmarshal(data, (*alias)(r))
}

func (r RepoConfig) ShouldPull() bool {
	return r.Pull == nil || *r.Pull
}

type Config struct {
	Interval  int          `json:"interval"`
	SyncRepo  string       `json:"syncRepo"`
	Repos     []RepoConfig `json:"repos"`
	Commands  []string     `json:"commands"`
	OnReceive []string     `json:"onReceive"`
}

func defaultConfig() Config {
	return Config{
		Interval: 30,
	}
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// ── Config ──

func loadConfig() Config {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return defaultConfig()
	}
	cfg := defaultConfig()
	_ = json.Unmarshal(data, &cfg)
	if cfg.Interval < 1 {
		cfg.Interval = 30
	}
	return cfg
}

func saveConfig(cfg Config) {
	_ = os.MkdirAll(xmuggleDir, 0755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configFile, append(data, '\n'), 0644)
}

func ensureConfig() {
	if _, err := os.Stat(configFile); err != nil {
		cfg := defaultConfig()
		// Seed syncRepo from existing file
		if data, err := os.ReadFile(filepath.Join(xmuggleDir, "sync-repo")); err == nil {
			cfg.SyncRepo = strings.TrimSpace(string(data))
		}
		saveConfig(cfg)
	}
}

// ── Logging ──

var logWriter *os.File

func setupLog() {
	_ = os.MkdirAll(xmuggleDir, 0755)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		logWriter = f
		log.SetOutput(f)
	}
	log.SetFlags(log.Ldate | log.Ltime)
}

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Println(msg)
	fmt.Println(msg)
}

// ── Git env ──

func gitEnv() []string {
	env := os.Environ()
	tokenFile := filepath.Join(xmuggleDir, "gh-token")
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		if data, err := os.ReadFile(tokenFile); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	if token != "" {
		env = append(env,
			"GH_TOKEN="+token,
			"GIT_ASKPASS=echo",
			"GIT_TERMINAL_PROMPT=0",
		)
	}
	return env
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runShell(command, dir string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ── Sync images via git ──

type meta struct {
	Filename  string `json:"filename"`
	Project   string `json:"project"`
	Message   string `json:"message"`
	From      string `json:"from"`
	Timestamp string `json:"timestamp"`
}

func syncImages(cfg Config) []string {
	if cfg.SyncRepo == "" {
		return nil
	}

	gitDir := filepath.Join(syncDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		logf("Cloning sync repo: %s", cfg.SyncRepo)
		_ = os.MkdirAll(syncDir, 0755)
		if out, err := runGit("", "clone", cfg.SyncRepo, syncDir); err != nil {
			logf("Sync clone failed: %s", out)
			return nil
		}
	} else {
		if out, err := runGit(syncDir, "pull", "--ff-only"); err != nil {
			logf("Sync pull failed: %s", out)
			return nil
		}
	}

	pendingDir := filepath.Join(syncDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil
	}

	host := hostname()
	var imported []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(pendingDir, entry.Name())
		metaFile := filepath.Join(dir, "meta.json")
		data, err := os.ReadFile(metaFile)
		if err != nil {
			continue
		}

		var m meta
		if err := json.Unmarshal(data, &m); err != nil || m.From == host {
			continue
		}

		srcImage := filepath.Join(dir, m.Filename)
		if _, err := os.Stat(srcImage); err != nil {
			continue
		}

		_ = os.MkdirAll(inboxDir, 0755)
		destImage := filepath.Join(inboxDir, m.Filename)
		if _, err := os.Stat(destImage); err == nil {
			continue // already imported
		}

		src, err := os.ReadFile(srcImage)
		if err != nil {
			continue
		}
		if err := os.WriteFile(destImage, src, 0644); err != nil {
			continue
		}
		if m.Message != "" {
			_ = os.WriteFile(destImage+".msg", []byte(m.Message), 0644)
		}
		imported = append(imported, m.Filename)
		logf("Imported: %s from %s", m.Filename, m.From)
	}

	return imported
}

// ── Repo sync ──

func syncRepos(cfg Config) {
	for _, repo := range cfg.Repos {
		if _, err := os.Stat(repo.Path); err != nil {
			logf("Repo not found: %s", repo.Path)
			continue
		}

		if repo.ShouldPull() {
			logf("Pulling %s", repo.Path)
			out, err := runGit(repo.Path, "pull", "--ff-only")
			if err != nil {
				logf("  Pull failed: %s", out)
			} else if out != "" && !strings.Contains(out, "Already up to date") {
				logf("  %s", out)
			}
		}

		// Process inbox after pulling
		processInbox(repo.Path)

		for _, cmd := range repo.Commands {
			runCommand(cmd, repo.Path)
		}
	}
}

// ── Inbox processing ──

type inboxMeta struct {
	Filenames []string `json:"filenames"`
	Message   string   `json:"message"`
	From      string   `json:"from"`
	Timestamp string   `json:"timestamp"`
}

func processInbox(repoPath string) {
	inboxPath := filepath.Join(repoPath, ".xmuggle", "inbox")
	metaFile := filepath.Join(inboxPath, "meta.json")

	data, err := os.ReadFile(metaFile)
	if err != nil {
		return // no inbox or no meta
	}

	var m inboxMeta
	if err := json.Unmarshal(data, &m); err != nil {
		logf("Bad inbox meta in %s: %v", repoPath, err)
		return
	}

	if len(m.Filenames) == 0 {
		return
	}

	// Check which images haven't been processed yet
	donePath := filepath.Join(repoPath, ".xmuggle", "done")
	_ = os.MkdirAll(donePath, 0755)

	var pending []string
	for _, f := range m.Filenames {
		src := filepath.Join(inboxPath, f)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		doneMarker := filepath.Join(donePath, f+".done")
		if _, err := os.Stat(doneMarker); err == nil {
			continue // already processed
		}
		pending = append(pending, f)
	}

	if len(pending) == 0 {
		return
	}

	logf("Processing %d image(s) in %s", len(pending), filepath.Base(repoPath))

	for _, f := range pending {
		imgPath := filepath.Join(inboxPath, f)
		logf("  Spawning claude for %s", f)

		prompt := fmt.Sprintf(
			"Analyze the screenshot at %s and fix any bugs or UI issues you find in this repo. %s",
			imgPath, m.Message,
		)

		cmd := exec.Command("claude", "--yes", "--print", prompt)
		cmd.Dir = repoPath
		cmd.Env = gitEnv()
		output, err := cmd.CombinedOutput()
		if err != nil {
			logf("  Claude failed for %s: %v\n%s", f, err, string(output))
			continue
		}

		logf("  Claude finished %s", f)

		// Commit and push any code changes Claude made
		if status, _ := runGit(repoPath, "status", "--porcelain"); status != "" {
			runGit(repoPath, "add", "-A")
			commitMsg := fmt.Sprintf("xmuggle: fix from %s", f)
			if _, err := runGit(repoPath, "commit", "-m", commitMsg); err == nil {
				logf("  Pushing code changes for %s", f)
				if out, err := runGit(repoPath, "push"); err != nil {
					logf("  Push failed: %s", out)
				}
			}
		}

		// Write result and mark as done
		resultFile := filepath.Join(donePath, f+".result")
		_ = os.WriteFile(resultFile, output, 0644)
		doneMarker := filepath.Join(donePath, f+".done")
		_ = os.WriteFile(doneMarker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0644)

		// Commit and push done markers
		runGit(repoPath, "add", "-A")
		doneCommitMsg := fmt.Sprintf("xmuggle: mark %s done", f)
		if _, err := runGit(repoPath, "commit", "-m", doneCommitMsg); err == nil {
			logf("  Pushing done marker for %s", f)
			if out, err := runGit(repoPath, "push"); err != nil {
				logf("  Push failed: %s", out)
			}
		}
	}
}

func runCommand(command, dir string) {
	logf("Running: %s", command)
	out, err := runShell(command, dir)
	if err != nil {
		logf("  Error: %s", out)
	} else if out != "" {
		logf("  %s", out)
	}
}

// ── Cycle ──

func runCycle() {
	cfg := loadConfig()

	imported := syncImages(cfg)
	syncRepos(cfg)

	for _, cmd := range cfg.Commands {
		runCommand(cmd, "")
	}

	if len(imported) > 0 && len(cfg.OnReceive) > 0 {
		logf("%d new image(s), running onReceive commands", len(imported))
		files := strings.Join(imported, " ")
		for _, cmd := range cfg.OnReceive {
			expanded := strings.ReplaceAll(cmd, "$FILES", files)
			runCommand(expanded, "")
		}
	}
}

// ── CLI ──

func main() {
	cmd := "help"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "start":
		ensureConfig()

		// Check if already running
		if pid, ok := readPid(); ok {
			fmt.Printf("Daemon already running (pid %d)\n", pid)
			return
		}

		// Re-exec as background process
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot find executable: %v\n", err)
			os.Exit(1)
		}
		child := exec.Command(exe, "_run-daemon")
		child.Env = os.Environ()
		// Detach from terminal
		child.Stdin = nil
		child.Stdout = nil
		child.Stderr = nil
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Daemon started (pid %d)\n", child.Process.Pid)

	case "_run-daemon":
		// Internal: the actual daemon loop, runs in background
		setupLog()
		cfg := loadConfig()

		_ = os.MkdirAll(xmuggleDir, 0755)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)

		logf("Daemon starting (pid %d, interval %ds)", os.Getpid(), cfg.Interval)
		logf("Config: %s", configFile)

		// Graceful shutdown
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

		// Run immediately
		runCycle()

		ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				runCycle()
			case s := <-sig:
				logf("Received %s, shutting down", s)
				_ = os.Remove(pidFile)
				return
			}
		}

	case "run":
		ensureConfig()
		setupLog()
		logf("Running single cycle")
		runCycle()
		logf("Done")

	case "stop":
		pid, ok := readPid()
		if !ok {
			fmt.Println("No daemon running")
			return
		}
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Printf("Could not stop pid %d: %v\n", pid, err)
		} else {
			_ = os.Remove(pidFile)
			fmt.Printf("Stopped daemon (pid %d)\n", pid)
		}

	case "status":
		if pid, ok := readPid(); ok {
			fmt.Printf("Daemon running (pid %d)\n", pid)
		} else {
			fmt.Println("Daemon not running")
		}
		cfg := loadConfig()
		fmt.Printf("Config:    %s\n", configFile)
		fmt.Printf("Interval:  %ds\n", cfg.Interval)
		fmt.Printf("Sync repo: %s\n", orDefault(cfg.SyncRepo, "(none)"))
		fmt.Printf("Repos:     %d\n", len(cfg.Repos))
		fmt.Printf("Commands:  %d\n", len(cfg.Commands))
		fmt.Printf("OnReceive: %d\n", len(cfg.OnReceive))

	case "config":
		ensureConfig()
		data, _ := os.ReadFile(configFile)
		fmt.Print(string(data))

	case "edit":
		ensureConfig()
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, configFile)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Run()

	case "add-repo":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: xmuggled add-repo <path> [command...]")
			os.Exit(1)
		}
		ensureConfig()
		cfg := loadConfig()
		abs, _ := filepath.Abs(os.Args[2])
		cmds := os.Args[3:]

		found := false
		for i, r := range cfg.Repos {
			if r.Path == abs {
				if len(cmds) > 0 {
					cfg.Repos[i].Commands = cmds
				}
				found = true
				fmt.Printf("Updated repo: %s\n", abs)
				break
			}
		}
		if !found {
			cfg.Repos = append(cfg.Repos, RepoConfig{Path: abs, Commands: cmds})
			fmt.Printf("Added repo: %s\n", abs)
		}
		saveConfig(cfg)

	case "add-command":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: xmuggled add-command <command>")
			os.Exit(1)
		}
		ensureConfig()
		cfg := loadConfig()
		cmd := strings.Join(os.Args[2:], " ")
		cfg.Commands = append(cfg.Commands, cmd)
		saveConfig(cfg)
		fmt.Printf("Added command: %s\n", cmd)

	case "on-receive":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: xmuggled on-receive <command>")
			os.Exit(1)
		}
		ensureConfig()
		cfg := loadConfig()
		cmd := strings.Join(os.Args[2:], " ")
		cfg.OnReceive = append(cfg.OnReceive, cmd)
		saveConfig(cfg)
		fmt.Printf("Added onReceive: %s\n", cmd)

	case "process-inbox":
		// Process inbox for a specific repo or all configured repos
		setupLog()
		if len(os.Args) > 2 {
			abs, _ := filepath.Abs(os.Args[2])
			processInbox(abs)
		} else {
			cfg := loadConfig()
			for _, repo := range cfg.Repos {
				processInbox(repo.Path)
			}
		}

	case "log":
		n := 20
		if len(os.Args) > 2 {
			n, _ = strconv.Atoi(os.Args[2])
			if n < 1 {
				n = 20
			}
		}
		data, err := os.ReadFile(logFile)
		if err != nil {
			fmt.Println("No log file")
			return
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		start := len(lines) - n
		if start < 0 {
			start = 0
		}
		fmt.Println(strings.Join(lines[start:], "\n"))

	default:
		fmt.Print(`xmuggled — xmuggle sync daemon

Usage:
  xmuggled start                   Start the daemon (background)
  xmuggled run                     Run a single sync cycle
  xmuggled stop                    Stop the running daemon
  xmuggled status                  Show daemon status and config summary
  xmuggled config                  Print current config
  xmuggled edit                    Open config in $EDITOR
  xmuggled log [n]                 Show last n log lines (default 20)
  xmuggled add-repo <path> [cmd]   Add a repo to sync
  xmuggled add-command <cmd>       Add a global command
  xmuggled on-receive <cmd>        Add a command to run on new images
  xmuggled process-inbox [path]    Process inbox images with Claude

Config: ~/.xmuggle/daemon.json
`)
	}
}

func readPid() (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(pidFile)
		return 0, false
	}
	return pid, true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
