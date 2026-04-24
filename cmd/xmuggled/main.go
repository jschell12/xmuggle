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
	"sync"
	"sync/atomic"
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

// Concurrency state
var (
	activeWorkers sync.WaitGroup
	workerCount   atomic.Int32
	workerSem     chan struct{}
	queueMu       sync.Mutex
	activeTasks   = make(map[string]bool)
	activeTasksMu sync.Mutex
)

type ProjectConfig struct {
	LocalPath    string   `json:"localPath"`
	PostCommands []string `json:"postCommands"`
}

type Config struct {
	Interval     int                      `json:"interval"`
	QueueRepo    string                   `json:"queueRepo"`
	MaxWorkers   int                      `json:"maxWorkers"`
	AQScriptsDir string                   `json:"aqScriptsDir"`
	PostCommands []string                 `json:"postCommands"`
	Projects     map[string]ProjectConfig `json:"projects"`
}

func defaultConfig() Config {
	return Config{
		Interval:     10,
		MaxWorkers:   3,
		AQScriptsDir: filepath.Join(homeDir(), "development", "github.com", "jschell12", "agent-queue", "scripts"),
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
		cfg.Interval = 10
	}
	if cfg.MaxWorkers < 1 {
		cfg.MaxWorkers = 3
	}
	if cfg.AQScriptsDir == "" {
		cfg.AQScriptsDir = defaultConfig().AQScriptsDir
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
	if out, err := runGit(queueDir, "fetch", "origin", "main"); err != nil {
		logf("Queue fetch failed: %s", out)
		return false
	}
	if out, err := runGit(queueDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		logf("Queue reset failed: %s", out)
		return false
	}
	return true
}

// queueCommitPushSafe wraps queue git ops in a mutex for concurrent workers.
func queueCommitPushSafe(message string) {
	queueMu.Lock()
	defer queueMu.Unlock()
	runGit(queueDir, "add", "-A")
	if _, err := runGit(queueDir, "commit", "-m", message); err == nil {
		runGit(queueDir, "fetch", "origin", "main")
		runGit(queueDir, "rebase", "FETCH_HEAD")
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

		// Skip if already being processed by a worker
		activeTasksMu.Lock()
		if activeTasks[taskID] {
			activeTasksMu.Unlock()
			continue
		}
		activeTasksMu.Unlock()

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

		// Try to acquire a worker slot
		select {
		case workerSem <- struct{}{}:
			// Got a slot
		default:
			logf("All %d worker slots occupied, deferring remaining tasks", cfg.MaxWorkers)
			return
		}

		// Mark as active
		activeTasksMu.Lock()
		activeTasks[taskID] = true
		activeTasksMu.Unlock()

		// Mark as processing in xmuggle-queue
		m.Status = "processing"
		m.ProcessedBy = host
		writeTaskMeta(metaFile, m)
		queueCommitPushSafe(fmt.Sprintf("processing: %s", taskID))

		logf("Dispatching task %s for %s (workers: %d/%d)",
			taskID, m.Project, workerCount.Load()+1, cfg.MaxWorkers)

		// Spawn worker goroutine
		workerCount.Add(1)
		activeWorkers.Add(1)
		go runWorker(cfg, m, taskID, taskDir)
	}
}

// ── Worker ──

func runWorker(cfg Config, m *taskMeta, taskID, taskDir string) {
	defer activeWorkers.Done()
	defer workerCount.Add(-1)
	defer func() { <-workerSem }()
	defer func() {
		activeTasksMu.Lock()
		delete(activeTasks, taskID)
		activeTasksMu.Unlock()
	}()

	metaFile := filepath.Join(taskDir, "meta.json")
	projectName := filepath.Base(m.Project)
	remoteURL := projectToURL(m.Project)
	aqScript := filepath.Join(cfg.AQScriptsDir, "agent-queue")
	amScript := filepath.Join(cfg.AQScriptsDir, "agent-merge")

	// Clone via agent-queue clone for isolated workspace
	sessionID := fmt.Sprintf("xmuggle-%s", taskID)
	_ = os.MkdirAll(workDir, 0755)

	logf("  [%s] Cloning %s", taskID, m.Project)
	cloneCmd := exec.Command("python3", aqScript, "clone", remoteURL, sessionID, "--parent", workDir)
	cloneCmd.Env = gitEnv()
	cloneOut, cloneErr := cloneCmd.CombinedOutput()
	if cloneErr != nil {
		logf("  [%s] Clone failed: %s", taskID, strings.TrimSpace(string(cloneOut)))
		markError(m, metaFile, taskID, fmt.Sprintf("Clone failed: %s", strings.TrimSpace(string(cloneOut))))
		return
	}

	// Parse clone output for clone_dir and branch
	var cloneResult struct {
		CloneDir string `json:"clone_dir"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(cloneOut, &cloneResult); err != nil {
		// Fallback: construct paths manually
		cloneResult.CloneDir = filepath.Join(workDir, fmt.Sprintf("%s-%s", projectName, sessionID))
		cloneResult.Branch = fmt.Sprintf("%s-%s", projectName, sessionID)
	}
	cloneDir := cloneResult.CloneDir
	branch := cloneResult.Branch

	defer os.RemoveAll(cloneDir)

	// Collect attachments
	var imgPaths []string
	var textContent []string
	for _, f := range m.Filenames {
		p := filepath.Join(taskDir, f)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f))
		if ext == ".txt" || ext == ".md" {
			data, err := os.ReadFile(p)
			if err == nil {
				textContent = append(textContent, string(data))
			}
		} else {
			imgPaths = append(imgPaths, p)
		}
	}

	if len(imgPaths) == 0 && len(textContent) == 0 && m.Message == "" {
		logf("  [%s] No content", taskID)
		markError(m, metaFile, taskID, "No content found in task")
		return
	}

	// Build prompt
	var promptParts []string
	if len(imgPaths) > 0 {
		promptParts = append(promptParts,
			fmt.Sprintf("Analyze the screenshot(s) at %s and fix any bugs or UI issues you find in this repo.", strings.Join(imgPaths, ", ")))
	}
	if len(textContent) > 0 {
		promptParts = append(promptParts, "Here is additional context:\n\n"+strings.Join(textContent, "\n\n"))
	}
	if m.Message != "" {
		promptParts = append(promptParts, m.Message)
	}
	prompt := strings.Join(promptParts, "\n\n")

	// Spawn Claude
	claudeLog := filepath.Join(xmuggleDir, "claude-"+taskID+".log")
	logf("  [%s] Spawning claude on branch %s", taskID, branch)
	logf("  [%s] Tail live: tail -f %s | python3 scripts/claude-log-filter.py", taskID, claudeLog)

	claudeCmd := exec.Command("claude", "--print", "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions", prompt)
	claudeCmd.Dir = cloneDir
	claudeCmd.Env = gitEnv()
	claudeLogFile, _ := os.Create(claudeLog)
	claudeCmd.Stdout = claudeLogFile
	claudeCmd.Stderr = claudeLogFile
	claudeErr := claudeCmd.Run()
	claudeLogFile.Close()
	outputBytes, _ := os.ReadFile(claudeLog)
	result := strings.TrimSpace(string(outputBytes))

	if claudeErr != nil {
		logf("  [%s] Claude failed: %v", taskID, claudeErr)
		markError(m, metaFile, taskID, fmt.Sprintf("Claude failed: %v", claudeErr))
		return
	}

	logf("  [%s] Claude finished", taskID)

	// Check for changes
	porcelain, _ := runGit(cloneDir, "status", "--porcelain")
	if porcelain == "" {
		logf("  [%s] No changes made", taskID)
		markDone(m, metaFile, taskID, result)
		return
	}

	// Commit changes
	runGit(cloneDir, "add", "-A")
	commitMsg := fmt.Sprintf("xmuggle: fix from task %s", taskID)
	runGit(cloneDir, "commit", "-m", commitMsg)

	// Merge via agent-merge (serialized per-project lock)
	logf("  [%s] Merging via agent-merge", taskID)
	mergeCmd := exec.Command("python3", amScript, branch, "--delete-branch", "-p", projectName)
	mergeCmd.Dir = cloneDir
	mergeCmd.Env = gitEnv()
	mergeOut, mergeErr := mergeCmd.CombinedOutput()
	mergeOutput := strings.TrimSpace(string(mergeOut))

	if mergeErr != nil {
		logf("  [%s] agent-merge failed: %s", taskID, mergeOutput)
		// Fallback: push branch and create PR
		logf("  [%s] Falling back to PR flow", taskID)
		runGit(cloneDir, "fetch", "--unshallow")
		runGit(cloneDir, "push", "origin", branch)

		prCmd := exec.Command("gh", "pr", "create",
			"--title", commitMsg,
			"--body", fmt.Sprintf("Automated fix from xmuggle task %s\n\n%s", taskID, m.Message),
			"--head", branch, "--base", "main",
		)
		prCmd.Dir = cloneDir
		prCmd.Env = gitEnv()
		prOut, prErr := prCmd.CombinedOutput()
		prURL := strings.TrimSpace(string(prOut))

		if prErr != nil {
			logf("  [%s] PR create also failed: %s", taskID, prURL)
		} else {
			logf("  [%s] PR created: %s", taskID, prURL)
			ghMerge := exec.Command("gh", "pr", "merge", "--merge", "--delete-branch", prURL)
			ghMerge.Dir = cloneDir
			ghMerge.Env = gitEnv()
			ghMerge.Run()
			result = result + "\n\nPR: " + prURL
		}
	} else {
		logf("  [%s] Merged to main via agent-merge", taskID)
	}

	runPostCommands(cfg, m.Project, cloneDir, taskID)
	markDone(m, metaFile, taskID, result)
}

// runPostCommands runs post-completion commands for a project.
// It looks up project-specific config first, falling back to global postCommands.
// If a localPath is configured, it runs git pull there first, then commands in that dir.
// Otherwise commands run in the clone dir.
func runPostCommands(cfg Config, project, cloneDir, taskID string) {
	// Resolve project config: try full project name, then base name
	var pc ProjectConfig
	var found bool
	if cfg.Projects != nil {
		pc, found = cfg.Projects[project]
		if !found {
			pc, found = cfg.Projects[filepath.Base(project)]
		}
	}

	// Determine which commands to run
	commands := pc.PostCommands
	if len(commands) == 0 {
		commands = cfg.PostCommands
	}

	// Determine working directory
	runDir := cloneDir
	if pc.LocalPath != "" {
		runDir = pc.LocalPath
		// Pull latest into local path first
		logf("  [%s] Pulling latest into %s", taskID, runDir)
		if out, err := runGit(runDir, "pull", "--rebase"); err != nil {
			logf("  [%s] git pull failed in %s: %s", taskID, runDir, out)
			return
		}
	}

	if len(commands) == 0 {
		return
	}

	for _, cmdStr := range commands {
		logf("  [%s] Running post-command: %s", taskID, cmdStr)
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = runDir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		outStr := strings.TrimSpace(string(out))
		if err != nil {
			logf("  [%s] Post-command failed: %s\n%s", taskID, err, outStr)
		} else if outStr != "" {
			logf("  [%s] Post-command output: %s", taskID, outStr)
		}
	}
}

func markDone(m *taskMeta, metaFile, taskID, result string) {
	m.Status = "done"
	m.Result = result
	m.DoneAt = time.Now().Format(time.RFC3339)
	writeTaskMeta(metaFile, m)
	queueCommitPushSafe(fmt.Sprintf("done: %s", taskID))
	logf("  [%s] Task complete", taskID)
}

func markError(m *taskMeta, metaFile, taskID, reason string) {
	m.Status = "error"
	m.Result = reason
	m.DoneAt = time.Now().Format(time.RFC3339)
	writeTaskMeta(metaFile, m)
	queueCommitPushSafe(fmt.Sprintf("error: %s", taskID))
	logf("  [%s] Task failed: %s", taskID, reason)
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
		workerSem = make(chan struct{}, cfg.MaxWorkers)
		_ = os.MkdirAll(xmuggleDir, 0755)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
		logf("Daemon starting (pid %d, interval %ds, maxWorkers %d)", os.Getpid(), cfg.Interval, cfg.MaxWorkers)

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
				logf("Received %s, waiting for %d workers...", s, workerCount.Load())
				activeWorkers.Wait()
				logf("All workers finished, shutting down")
				_ = os.Remove(pidFile)
				return
			}
		}

	case "run":
		ensureConfig()
		setupLog()
		cfg := loadConfig()
		workerSem = make(chan struct{}, cfg.MaxWorkers)
		logf("Running single cycle (maxWorkers %d)", cfg.MaxWorkers)
		runCycle()
		logf("Waiting for workers...")
		activeWorkers.Wait()
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
		fmt.Printf("Config:      %s\n", configFile)
		fmt.Printf("Interval:    %ds\n", cfg.Interval)
		fmt.Printf("MaxWorkers:  %d\n", cfg.MaxWorkers)
		fmt.Printf("Queue repo:  %s\n", orDefault(cfg.QueueRepo, "(none)"))
		fmt.Printf("AQ scripts:  %s\n", cfg.AQScriptsDir)
		if len(cfg.PostCommands) > 0 {
			fmt.Printf("Post cmds:   %s\n", strings.Join(cfg.PostCommands, "; "))
		}
		if len(cfg.Projects) > 0 {
			fmt.Printf("Projects:    %d configured\n", len(cfg.Projects))
			for name, pc := range cfg.Projects {
				fmt.Printf("  %s:\n", name)
				if pc.LocalPath != "" {
					fmt.Printf("    localPath:    %s\n", pc.LocalPath)
				}
				if len(pc.PostCommands) > 0 {
					fmt.Printf("    postCommands: %s\n", strings.Join(pc.PostCommands, "; "))
				}
			}
		}

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
