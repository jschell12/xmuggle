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
	queueDir   = filepath.Join(xmuggleDir, "queue-repo")
	aqScripts  = filepath.Join(homeDir(), "development", "github.com", "jschell12", "agent-queue", "scripts")
)

type RepoConfig struct {
	Path     string   `json:"path"`
	Pull     *bool    `json:"pull,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

func (r *RepoConfig) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.Path = s
		return nil
	}
	type alias RepoConfig
	return json.Unmarshal(data, (*alias)(r))
}

func (r RepoConfig) ShouldPull() bool {
	return r.Pull == nil || *r.Pull
}

type Config struct {
	Interval  int          `json:"interval"`
	QueueRepo string       `json:"queueRepo"`
	Repos     []RepoConfig `json:"repos"`
	Commands  []string     `json:"commands"`
}

func defaultConfig() Config {
	return Config{Interval: 10}
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
		cfg.Interval = 10
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
		saveConfig(defaultConfig())
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

// ── Git ──

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

// ── Queue message schema ──
//
// Each task lives in pending/<id>/meta.json with these fields:
//   status: "pending" → "processing" → "done" | "error"
//   processedBy: hostname of the machine that processed it
//   result: summary of what was done
//
// Both daemons pull the queue repo. Only tasks with status "pending"
// and from != self are processed. Once done, the status is updated
// in place so the sender's daemon sees it and can update the UI.

type taskMeta struct {
	Filenames   []string `json:"filenames"`
	Project     string   `json:"project"`
	Message     string   `json:"message"`
	From        string   `json:"from"`
	Timestamp   string   `json:"timestamp"`
	Status      string   `json:"status"`
	ProcessedBy string   `json:"processedBy,omitempty"`
	Result      string   `json:"result,omitempty"`
	DoneAt      string   `json:"doneAt,omitempty"`
}

func readTaskMeta(metaFile string) (*taskMeta, error) {
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return nil, err
	}
	var m taskMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	// Default status for legacy tasks without a status field
	if m.Status == "" {
		m.Status = "pending"
	}
	return &m, nil
}

func writeTaskMeta(metaFile string, m *taskMeta) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(metaFile, append(data, '\n'), 0644)
}

func queueCommitPush(message string) {
	runGit(queueDir, "add", "-A")
	if _, err := runGit(queueDir, "commit", "-m", message); err == nil {
		runGit(queueDir, "pull", "--rebase")
		if out, err := runGit(queueDir, "push"); err != nil {
			logf("  Queue push failed: %s", out)
		}
	}
}

// ── Queue processing ──

func ensureQueueClone(cfg Config) bool {
	if cfg.QueueRepo == "" {
		return false
	}
	gitDir := filepath.Join(queueDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		logf("Cloning queue repo: %s", cfg.QueueRepo)
		_ = os.MkdirAll(queueDir, 0755)
		if out, err := runGit("", "clone", cfg.QueueRepo, queueDir); err != nil {
			logf("Queue clone failed: %s", out)
			return false
		}
	} else {
		if out, err := runGit(queueDir, "pull", "--rebase"); err != nil {
			logf("Queue pull failed: %s", out)
			return false
		}
	}
	return true
}

func resolveProject(cfg Config, project string) string {
	name := filepath.Base(project)
	for _, r := range cfg.Repos {
		if filepath.Base(r.Path) == name {
			return r.Path
		}
	}
	home := homeDir()
	candidates := []string{
		filepath.Join(home, "development", "github.com", project),
		filepath.Join(home, "dev", project),
		filepath.Join(home, project),
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, ".git")); err == nil {
			return c
		}
	}
	return ""
}

func processQueue(cfg Config) {
	if !ensureQueueClone(cfg) {
		return
	}

	pendingDir := filepath.Join(queueDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return
	}

	host := hostname()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		taskID := entry.Name()
		taskDir := filepath.Join(pendingDir, taskID)
		metaFile := filepath.Join(taskDir, "meta.json")

		m, err := readTaskMeta(metaFile)
		if err != nil {
			continue
		}

		// Skip tasks not in pending state
		if m.Status != "pending" {
			continue
		}

		// Skip our own submissions
		if m.From == host {
			continue
		}

		// Resolve project to local path
		repoPath := resolveProject(cfg, m.Project)
		if repoPath == "" {
			logf("Unknown project %q in task %s, skipping", m.Project, taskID)
			continue
		}

		logf("Processing task %s for %s", taskID, filepath.Base(repoPath))

		// Mark as processing
		m.Status = "processing"
		m.ProcessedBy = host
		writeTaskMeta(metaFile, m)
		queueCommitPush(fmt.Sprintf("processing: %s", taskID))

		// Pull latest for the target repo
		if out, err := runGit(repoPath, "pull", "--rebase"); err != nil {
			logf("  Pull failed for %s: %s", repoPath, out)
		}

		// Collect image paths
		var imgPaths []string
		for _, f := range m.Filenames {
			p := filepath.Join(taskDir, f)
			if _, err := os.Stat(p); err == nil {
				imgPaths = append(imgPaths, p)
			}
		}

		if len(imgPaths) == 0 {
			logf("  No images found in task %s", taskID)
			m.Status = "error"
			m.Result = "No images found in task"
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("error: %s — no images", taskID))
			continue
		}

		// Build prompt
		prompt := fmt.Sprintf(
			"Analyze the screenshot(s) at %s and fix any bugs or UI issues you find in this repo. %s",
			strings.Join(imgPaths, ", "), m.Message,
		)

		// Spawn claude
		logf("  Spawning claude in %s", filepath.Base(repoPath))
		cmd := exec.Command("claude", "--print", "--dangerously-skip-permissions", prompt)
		cmd.Dir = repoPath
		cmd.Env = gitEnv()
		output, err := cmd.CombinedOutput()
		result := strings.TrimSpace(string(output))

		if err != nil {
			logf("  Claude failed: %v\n%s", err, result)
			m.Status = "error"
			m.Result = fmt.Sprintf("Claude failed: %v", err)
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("error: %s", taskID))
			continue
		}

		logf("  Claude finished")

		// Commit and push code changes to the project repo
		if status, _ := runGit(repoPath, "status", "--porcelain"); status != "" {
			runGit(repoPath, "add", "-A")
			commitMsg := fmt.Sprintf("xmuggle: fix from task %s", taskID)
			if _, err := runGit(repoPath, "commit", "-m", commitMsg); err == nil {
				logf("  Pushing code changes to %s", filepath.Base(repoPath))
				if out, err := runGit(repoPath, "push"); err != nil {
					logf("  Push failed: %s", out)
				}
			}
		}

		// Mark as done in queue
		m.Status = "done"
		m.Result = result
		m.DoneAt = time.Now().Format(time.RFC3339)
		writeTaskMeta(metaFile, m)
		queueCommitPush(fmt.Sprintf("done: %s", taskID))

		logf("  Task %s complete", taskID)
	}
}

// ── Repo sync ──

func syncRepos(cfg Config) {
	for _, repo := range cfg.Repos {
		if _, err := os.Stat(repo.Path); err != nil {
			logf("Repo not found: %s", repo.Path)
			continue
		}
		if repo.ShouldPull() {
			out, err := runGit(repo.Path, "pull", "--rebase")
			if err != nil {
				logf("Pull failed %s: %s", filepath.Base(repo.Path), out)
			} else if out != "" && !strings.Contains(out, "Already up to date") {
				logf("Pulled %s: %s", filepath.Base(repo.Path), out)
			}
		}
		for _, cmd := range repo.Commands {
			runCommand(cmd, repo.Path)
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
	syncRepos(cfg)
	processQueue(cfg)
	for _, cmd := range cfg.Commands {
		runCommand(cmd, "")
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
		if pid, ok := readPid(); ok {
			fmt.Printf("Daemon already running (pid %d)\n", pid)
			return
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot find executable: %v\n", err)
			os.Exit(1)
		}
		child := exec.Command(exe, "_run-daemon")
		child.Env = os.Environ()
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
		setupLog()
		cfg := loadConfig()
		_ = os.MkdirAll(xmuggleDir, 0755)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
		logf("Daemon starting (pid %d, interval %ds)", os.Getpid(), cfg.Interval)

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

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
		fmt.Printf("Config:     %s\n", configFile)
		fmt.Printf("Interval:   %ds\n", cfg.Interval)
		fmt.Printf("Queue repo: %s\n", orDefault(cfg.QueueRepo, "(none)"))
		fmt.Printf("Repos:      %d\n", len(cfg.Repos))

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

	case "set-queue":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: xmuggled set-queue <repo-url>")
			os.Exit(1)
		}
		ensureConfig()
		cfg := loadConfig()
		cfg.QueueRepo = os.Args[2]
		saveConfig(cfg)
		fmt.Printf("Queue repo set: %s\n", cfg.QueueRepo)

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
  xmuggled start                   Start daemon (background)
  xmuggled run                     Run a single sync cycle
  xmuggled stop                    Stop the daemon
  xmuggled status                  Show status
  xmuggled config                  Print config
  xmuggled edit                    Open config in $EDITOR
  xmuggled log [n]                 Show last n log lines (default 20)
  xmuggled set-queue <repo-url>    Set queue repo URL
  xmuggled add-repo <path> [cmd]   Add a repo to sync
  xmuggled add-command <cmd>       Add a global command

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
