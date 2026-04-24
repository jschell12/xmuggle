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
	workDir    = filepath.Join(xmuggleDir, "work")
)

type Config struct {
	Interval  int    `json:"interval"`
	QueueRepo string `json:"queueRepo"`
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

func projectToURL(project string) string {
	if strings.HasPrefix(project, "git@") || strings.HasPrefix(project, "http") {
		return project
	}
	return fmt.Sprintf("git@github.com:%s.git", project)
}

// ── Queue message schema ──

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
	if m.Status == "" {
		m.Status = "pending"
	}
	return &m, nil
}

func writeTaskMeta(metaFile string, m *taskMeta) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(metaFile, append(data, '\n'), 0644)
}

func syncQueue() bool {
	gitDir := filepath.Join(queueDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return false
	}
	if out, err := runGit(queueDir, "pull", "--rebase", "origin", "main"); err != nil {
		logf("Queue pull failed: %s", out)
		return false
	}
	return true
}

func queueCommitPush(message string) {
	runGit(queueDir, "add", "-A")
	if _, err := runGit(queueDir, "commit", "-m", message); err == nil {
		runGit(queueDir, "pull", "--rebase", "origin", "main")
		if out, err := runGit(queueDir, "push", "origin", "main"); err != nil {
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
		return true
	}
	return syncQueue()
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

		if m.Status != "pending" {
			continue
		}

		if m.From == host {
			continue
		}

		remoteURL := projectToURL(m.Project)
		logf("Processing task %s for %s", taskID, m.Project)

		// Mark as processing
		m.Status = "processing"
		m.ProcessedBy = host
		writeTaskMeta(metaFile, m)
		queueCommitPush(fmt.Sprintf("processing: %s", taskID))

		// Clone target repo to temp dir
		_ = os.MkdirAll(workDir, 0755)
		cloneDir := filepath.Join(workDir, fmt.Sprintf("%s-%s", filepath.Base(m.Project), taskID))
		logf("  Cloning %s", m.Project)
		if out, err := runGit("", "clone", "--depth", "1", remoteURL, cloneDir); err != nil {
			logf("  Clone failed: %s", out)
			m.Status = "error"
			m.Result = fmt.Sprintf("Clone failed: %s", out)
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("error: %s — clone failed", taskID))
			continue
		}

		// Create branch
		branch := fmt.Sprintf("xmuggle-fix-%s", taskID)
		runGit(cloneDir, "checkout", "-b", branch)

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
			os.RemoveAll(cloneDir)
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

		// Spawn claude in the temp clone
		logf("  Spawning claude on branch %s", branch)
		cmd := exec.Command("claude", "--print", "--dangerously-skip-permissions", prompt)
		cmd.Dir = cloneDir
		cmd.Env = gitEnv()
		output, err := cmd.CombinedOutput()
		result := strings.TrimSpace(string(output))

		if err != nil {
			logf("  Claude failed: %v\n%s", err, result)
			os.RemoveAll(cloneDir)
			m.Status = "error"
			m.Result = fmt.Sprintf("Claude failed: %v", err)
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("error: %s", taskID))
			continue
		}

		logf("  Claude finished")

		// Check if claude made changes
		porcelain, _ := runGit(cloneDir, "status", "--porcelain")
		if porcelain == "" {
			logf("  No changes made")
			os.RemoveAll(cloneDir)
			m.Status = "done"
			m.Result = result
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("done: %s — no changes", taskID))
			logf("  Task %s complete (no changes)", taskID)
			continue
		}

		// Commit, push branch, create PR, merge, clean up
		runGit(cloneDir, "add", "-A")
		commitMsg := fmt.Sprintf("xmuggle: fix from task %s", taskID)
		runGit(cloneDir, "commit", "-m", commitMsg)

		// Unshallow so we can push the branch
		runGit(cloneDir, "fetch", "--unshallow")

		logf("  Pushing branch %s", branch)
		if out, err := runGit(cloneDir, "push", "origin", branch); err != nil {
			logf("  Push branch failed: %s", out)
			os.RemoveAll(cloneDir)
			m.Status = "error"
			m.Result = fmt.Sprintf("Push failed: %s", out)
			m.DoneAt = time.Now().Format(time.RFC3339)
			writeTaskMeta(metaFile, m)
			queueCommitPush(fmt.Sprintf("error: %s — push failed", taskID))
			continue
		}

		// Create and merge PR via gh CLI
		logf("  Creating PR")
		prCmd := exec.Command("gh", "pr", "create",
			"--title", commitMsg,
			"--body", fmt.Sprintf("Automated fix from xmuggle task %s\n\n%s", taskID, m.Message),
			"--head", branch,
			"--base", "main",
		)
		prCmd.Dir = cloneDir
		prCmd.Env = gitEnv()
		prOut, prErr := prCmd.CombinedOutput()
		prURL := strings.TrimSpace(string(prOut))

		if prErr != nil {
			logf("  PR create failed: %s", prURL)
			logf("  Falling back to direct push to main")
			runGit(cloneDir, "checkout", "main")
			runGit(cloneDir, "merge", branch)
			if out, err := runGit(cloneDir, "push", "origin", "main"); err != nil {
				logf("  Direct push also failed: %s", out)
			}
		} else {
			logf("  PR created: %s", prURL)
			logf("  Merging PR")
			mergeCmd := exec.Command("gh", "pr", "merge", "--merge", "--delete-branch", prURL)
			mergeCmd.Dir = cloneDir
			mergeCmd.Env = gitEnv()
			mergeOut, mergeErr := mergeCmd.CombinedOutput()
			if mergeErr != nil {
				logf("  PR merge failed: %s", strings.TrimSpace(string(mergeOut)))
			} else {
				logf("  PR merged")
				result = result + "\n\nPR: " + prURL
			}
		}

		// Clean up temp clone
		os.RemoveAll(cloneDir)

		// Mark as done in queue
		m.Status = "done"
		m.Result = result
		m.DoneAt = time.Now().Format(time.RFC3339)
		writeTaskMeta(metaFile, m)
		queueCommitPush(fmt.Sprintf("done: %s", taskID))

		logf("  Task %s complete", taskID)
	}
}

// ── Cycle ──

func runCycle() {
	cfg := loadConfig()
	processQueue(cfg)
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
		fmt.Print(`xmuggled — xmuggle queue daemon

Usage:
  xmuggled start                   Start daemon (background)
  xmuggled run                     Run a single sync cycle
  xmuggled stop                    Stop the daemon
  xmuggled status                  Show status
  xmuggled config                  Print config
  xmuggled edit                    Open config in $EDITOR
  xmuggled log [n]                 Show last n log lines (default 20)
  xmuggled set-queue <repo-url>    Set queue repo URL

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
