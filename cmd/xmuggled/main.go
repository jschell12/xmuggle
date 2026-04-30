package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"sort"
	"log"
	"net/http"
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

// postCmdProcs tracks running post-command processes by repo path so they can
// be killed before re-running.
var postCmdProcs = struct {
	sync.Mutex
	m map[string][]*os.Process // key = repo path
}{m: make(map[string][]*os.Process)}

type RepoConfig struct {
	Path         string   `json:"path"`
	PostCommands []string `json:"postCommands,omitempty"`
	AICli        string   `json:"aiCli,omitempty"`        // per-repo override: "claude" or "cursor"
}

type Config struct {
	Interval     int          `json:"interval"`
	QueueRepo    string       `json:"queueRepo"`
	MaxWorkers   int          `json:"maxWorkers"`
	AQScriptsDir string       `json:"aqScriptsDir"`
	AICli        string       `json:"aiCli"`
	LogLevel     string       `json:"logLevel"` // trace, debug, info, warn, error
	Repos        []RepoConfig `json:"repos,omitempty"`
}

func defaultConfig() Config {
	return Config{
		Interval:     10,
		MaxWorkers:   3,
		AICli:        "claude",
		LogLevel:     "info",
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
	if cfg.AICli == "" {
		cfg.AICli = "claude"
	}
	// Fall back to queue-url file if queueRepo not set in daemon.json
	if cfg.QueueRepo == "" {
		queueURLFile := filepath.Join(xmuggleDir, "queue-url")
		if data, err := os.ReadFile(queueURLFile); err == nil {
			cfg.QueueRepo = strings.TrimSpace(string(data))
		}
	}
	return cfg
}

// buildAICommand returns an exec.Cmd for the configured AI CLI.
func buildAICommand(cliName, prompt string) *exec.Cmd {
	switch cliName {
	case "cursor":
		return exec.Command("agent", "--print", "--trust", "--force", "--output-format", "stream-json", prompt)
	default: // "claude"
		return exec.Command("claude", "--print", "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions", prompt)
	}
}

func shouldAutoMarkDone(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(normalized, "mark as done")
}

// resolveAICli returns the CLI to use: task setting > local repo override > global default.
func resolveAICli(cfg Config, m *taskMeta) string {
	// Task-level setting from sender takes priority
	if m.AICli != "" {
		return m.AICli
	}
	// Local per-repo override
	rc := findRepoConfig(cfg, m.Project)
	if rc != nil && rc.AICli != "" {
		return rc.AICli
	}
	return cfg.AICli
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

const (
	LevelTrace = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
)

var (
	logWriter  *os.File
	logLevel   = LevelInfo
	levelNames = map[int]string{
		LevelTrace: "TRACE",
		LevelDebug: "DEBUG",
		LevelInfo:  "INFO",
		LevelWarn:  "WARN",
		LevelError: "ERROR",
	}
)

func parseLogLevel(s string) int {
	switch strings.ToLower(s) {
	case "trace":
		return LevelTrace
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func setupLog() {
	_ = os.MkdirAll(xmuggleDir, 0755)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		logWriter = f
		log.SetOutput(f)
	}
	log.SetFlags(log.Ldate | log.Ltime)
}

func logAt(level int, format string, args ...any) {
	if level < logLevel {
		return
	}
	prefix := levelNames[level]
	msg := fmt.Sprintf("[%s] %s", prefix, fmt.Sprintf(format, args...))
	log.Println(msg)
	fmt.Println(msg)
}

func trace(format string, args ...any) { logAt(LevelTrace, format, args...) }
func debug(format string, args ...any) { logAt(LevelDebug, format, args...) }
func logf(format string, args ...any)  { logAt(LevelInfo, format, args...) }
func warn(format string, args ...any)  { logAt(LevelWarn, format, args...) }
func errorf(format string, args ...any) { logAt(LevelError, format, args...) }

// ── Git ──

func gitEnv() []string {
	env := os.Environ()
	tokenFile := filepath.Join(xmuggleDir, "gh-token")
	token := os.Getenv("GH_TOKEN")
	source := "env:GH_TOKEN"
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
		source = "env:GITHUB_TOKEN"
	}
	if token == "" {
		if data, err := os.ReadFile(tokenFile); err == nil {
			token = strings.TrimSpace(string(data))
			source = "file:" + tokenFile
		}
	}
	if token != "" {
		trace("Git token found via %s (len=%d)", source, len(token))
		env = append(env,
			"GH_TOKEN="+token,
			"GIT_ASKPASS=echo",
			"GIT_TERMINAL_PROMPT=0",
		)
	} else {
		trace("No git token found")
	}
	return env
}

func runGit(dir string, args ...string) (string, error) {
	trace("git %s (dir=%s)", strings.Join(args, " "), dir)
	cleanupStaleGitIndexLock(dir)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil && isIndexLockError(trimmed) && cleanupStaleGitIndexLock(dir) {
		warn("Retrying git after stale lock cleanup: %s", strings.Join(args, " "))
		retry := exec.Command("git", args...)
		retry.Dir = dir
		retry.Env = gitEnv()
		out, err = retry.CombinedOutput()
		trimmed = strings.TrimSpace(string(out))
	}
	if err != nil {
		trace("git %s failed: %s", args[0], trimmed)
	} else {
		trace("git %s ok: %s", args[0], truncate(trimmed, 200))
	}
	return trimmed, err
}

func isIndexLockError(out string) bool {
	return strings.Contains(out, "index.lock") && strings.Contains(out, "File exists")
}

func cleanupStaleGitIndexLock(dir string) bool {
	if dir == "" {
		return false
	}
	lockFile := filepath.Join(dir, ".git", "index.lock")
	info, err := os.Stat(lockFile)
	if err != nil {
		return false
	}
	// Avoid touching a lock that may still be in active use.
	if time.Since(info.ModTime()) < 2*time.Minute {
		return false
	}
	if err := os.Remove(lockFile); err != nil {
		return false
	}
	logf("Removed stale git lock: %s", lockFile)
	return true
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
	AICli       string   `json:"aiCli,omitempty"`
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

// collectPriorTaskContext scans the queue for completed tasks targeting the
// same project and returns a summary string the AI can use as context.
// Results are sorted chronologically (oldest first) and capped at 10 entries.
func collectPriorTaskContext(project, currentTaskID string) string {
	pendingDir := filepath.Join(queueDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return ""
	}

	type priorTask struct {
		id      string
		message string
		result  string
		doneAt  string
	}

	var prior []priorTask
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tid := entry.Name()
		if tid == currentTaskID {
			continue
		}
		metaFile := filepath.Join(pendingDir, tid, "meta.json")
		m, err := readTaskMeta(metaFile)
		if err != nil {
			continue
		}
		if m.Project != project {
			continue
		}
		if m.Status != "done" {
			continue
		}
		if m.Message == "" && m.Result == "" {
			continue
		}
		prior = append(prior, priorTask{
			id:      tid,
			message: m.Message,
			result:  m.Result,
			doneAt:  m.DoneAt,
		})
	}

	if len(prior) == 0 {
		return ""
	}

	// Sort by doneAt ascending (oldest first); task IDs are timestamp-prefixed
	// so lexicographic order is a reasonable fallback.
	sort.Slice(prior, func(i, j int) bool {
		if prior[i].doneAt != "" && prior[j].doneAt != "" {
			return prior[i].doneAt < prior[j].doneAt
		}
		return prior[i].id < prior[j].id
	})

	// Cap to most recent 10
	if len(prior) > 10 {
		prior = prior[len(prior)-10:]
	}

	var sb strings.Builder
	sb.WriteString("## Previous tasks completed for this project\n\n")
	sb.WriteString("Use the following completed task history as context if it helps solve the current task.\n\n")
	for _, p := range prior {
		sb.WriteString(fmt.Sprintf("### Task %s", p.id))
		if p.doneAt != "" {
			sb.WriteString(fmt.Sprintf(" (completed %s)", p.doneAt))
		}
		sb.WriteString("\n")
		if p.message != "" {
			sb.WriteString(fmt.Sprintf("**Request:** %s\n", p.message))
		}
		if p.result != "" {
			// Truncate very long results to avoid blowing up the prompt
			r := p.result
			if len(r) > 2000 {
				r = r[:2000] + "\n... (truncated)"
			}
			sb.WriteString(fmt.Sprintf("**Result:** %s\n", r))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func syncQueue() bool {
	debug("Syncing queue repo")
	gitDir := filepath.Join(queueDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		warn("Queue repo not found at %s", queueDir)
		return false
	}
	queueMu.Lock()
	defer queueMu.Unlock()
	if out, err := runGit(queueDir, "fetch", "origin", "main"); err != nil {
		errorf("Queue fetch failed: %s", out)
		return false
	}
	if out, err := runGit(queueDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		errorf("Queue reset failed: %s", out)
		return false
	}
	debug("Queue synced")
	return true
}

// queueCommitPushSafe wraps queue git ops in a mutex for concurrent workers.
func queueCommitPushSafe(message string) {
	debug("Queue commit+push: %s", message)
	queueMu.Lock()
	defer queueMu.Unlock()
	runGit(queueDir, "add", "-A")
	if _, err := runGit(queueDir, "commit", "-m", message); err == nil {
		debug("Queue commit ok, fetching before push")
		runGit(queueDir, "fetch", "origin", "main")
		if out, err := runGit(queueDir, "rebase", "FETCH_HEAD"); err != nil {
			warn("Queue rebase failed: %s", out)
		}
		if out, err := runGit(queueDir, "push", "origin", "main"); err != nil {
			errorf("Queue push failed: %s", out)
		} else {
			debug("Queue pushed: %s", message)
		}
	} else {
		trace("Queue commit skipped (nothing to commit): %s", message)
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

// Track tasks we've already run post-commands for (persisted to disk).
var postCmdDoneFile = filepath.Join(homeDir(), ".xmuggle", "post-cmd-done.json")

func loadPostCmdDone() map[string]bool {
	data, err := os.ReadFile(postCmdDoneFile)
	if err != nil {
		return make(map[string]bool)
	}
	var m map[string]bool
	if err := json.Unmarshal(data, &m); err != nil {
		return make(map[string]bool)
	}
	return m
}

func savePostCmdDone(m map[string]bool) {
	data, _ := json.Marshal(m)
	_ = os.WriteFile(postCmdDoneFile, data, 0644)
}

func processQueue(cfg Config) {
	trace("processQueue: starting cycle")
	if !ensureQueueClone(cfg) {
		return
	}

	pendingDir := filepath.Join(queueDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		warn("Cannot read pending dir: %v", err)
		return
	}

	host := hostname()
	postCmdDone := loadPostCmdDone()
	debug("processQueue: %d entries in pending/, host=%s, postCmdDone=%d tracked",
		len(entries), host, len(postCmdDone))

	// First pass: run post-commands for our tasks that completed
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskID := entry.Name()
		if postCmdDone[taskID] {
			trace("  [%s] Already ran post-commands, skipping", taskID)
			continue
		}
		metaFile := filepath.Join(pendingDir, taskID, "meta.json")
		m, err := readTaskMeta(metaFile)
		if err != nil {
			trace("  [%s] Cannot read meta.json: %v", taskID, err)
			continue
		}
		debug("  [%s] from=%s status=%s project=%s aiCli=%s", taskID, m.From, m.Status, m.Project, m.AICli)
		// Only run post-commands for tasks WE sent that are now done
		if m.From == host && m.Status == "done" {
			postCmdDone[taskID] = true
			savePostCmdDone(postCmdDone)
			logf("  [%s] Task completed by %s, running post-commands", taskID, m.ProcessedBy)
			runPostTaskCommands(cfg, m.Project, taskID)
		} else if m.From == host && m.Status != "done" {
			debug("  [%s] Our task, status=%s (waiting)", taskID, m.Status)
		}
	}

	// Second pass: dispatch pending tasks from other hosts
	debug("processQueue: scanning for dispatchable tasks")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		taskID := entry.Name()

		// Skip if already being processed by a worker
		activeTasksMu.Lock()
		if activeTasks[taskID] {
			activeTasksMu.Unlock()
			trace("  [%s] Already active, skipping", taskID)
			continue
		}
		activeTasksMu.Unlock()

		taskDir := filepath.Join(pendingDir, taskID)
		metaFile := filepath.Join(taskDir, "meta.json")

		m, err := readTaskMeta(metaFile)
		if err != nil {
			trace("  [%s] Cannot read meta: %v", taskID, err)
			continue
		}

		if m.Status != "pending" {
			trace("  [%s] Status=%s, skipping", taskID, m.Status)
			continue
		}

		if m.From == host {
			trace("  [%s] From self, skipping", taskID)
			continue
		}

		debug("  [%s] Found pending task: project=%s from=%s msg=%s aiCli=%s files=%v",
			taskID, m.Project, m.From, truncate(m.Message, 50), m.AICli, m.Filenames)

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

	if shouldAutoMarkDone(m.Message) {
		logf("  [%s] Auto-completing task from message instruction", taskID)
		markDone(m, metaFile, taskID, "Marked done from task instruction.")
		return
	}

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

	if shouldAutoMarkDone(strings.Join(textContent, "\n")) {
		logf("  [%s] Auto-completing task from attachment instruction", taskID)
		markDone(m, metaFile, taskID, "Marked done from task attachment instruction.")
		return
	}

	// Build prompt
	var promptParts []string
	promptParts = append(promptParts, "You are a code fix agent. Your job is to read the code in this repo, understand the issue described below, and fix it by editing files directly. Do not ask questions. Do not use xmuggle or any external tools. Just read the code, make the fix, and commit nothing — the caller handles git.")
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
	// Include prior completed tasks for the same project as context
	if priorContext := collectPriorTaskContext(m.Project, taskID); priorContext != "" {
		promptParts = append(promptParts, priorContext)
		logf("  [%s] Including prior task context for %s", taskID, m.Project)
	}
	prompt := strings.Join(promptParts, "\n\n")

	// Spawn AI CLI — stream output and log filtered lines to daemon log
	aiCli := resolveAICli(cfg, m)
	logf("  [%s] Spawning %s on branch %s", taskID, aiCli, branch)
	claudeCmd := buildAICommand(aiCli, prompt)
	claudeCmd.Dir = cloneDir
	claudeCmd.Env = gitEnv()

	stdoutPipe, _ := claudeCmd.StdoutPipe()
	claudeCmd.Stderr = claudeCmd.Stdout // merge stderr into stdout
	claudeCmd.Start()

	var resultBuf strings.Builder
	var totalIn, totalOut, totalCacheRead, totalCacheWrite int64
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	for scanner.Scan() {
		line := scanner.Text()
		resultBuf.WriteString(line)
		resultBuf.WriteString("\n")

		// Parse and log readable content
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				logf("  [%s] %s", taskID, trimmed)
			}
			continue
		}

		// Track tokens — Claude puts usage in assistant messages, Cursor in result
		if msg["type"] == "assistant" {
			if message, ok := msg["message"].(map[string]any); ok {
				// Claude token tracking
				if usage, ok := message["usage"].(map[string]any); ok {
					totalIn += toInt64(usage["input_tokens"])
					totalOut += toInt64(usage["output_tokens"])
					totalCacheRead += toInt64(usage["cache_read_input_tokens"])
					totalCacheWrite += toInt64(usage["cache_creation_input_tokens"])
				}
				if content, ok := message["content"].([]any); ok {
					for _, block := range content {
						b, ok := block.(map[string]any)
						if !ok {
							continue
						}
						switch b["type"] {
						case "text":
							if text, ok := b["text"].(string); ok && text != "" {
								logf("  [%s] %s", taskID, text)
							}
						case "tool_use":
							name, _ := b["name"].(string)
							inp, _ := b["input"].(map[string]any)
							switch name {
							case "Bash":
								if cmd, ok := inp["command"].(string); ok {
									logf("  [%s] > %s", taskID, cmd)
								}
							case "Edit":
								logf("  [%s] [edit] %s", taskID, inp["file_path"])
							case "Write":
								logf("  [%s] [write] %s", taskID, inp["file_path"])
							case "Read":
								logf("  [%s] [read] %s", taskID, inp["file_path"])
							case "Agent":
								logf("  [%s] [agent] %s", taskID, inp["description"])
							default:
								logf("  [%s] [%s]", taskID, name)
							}
						}
					}
				}
			}
		}
		// Cursor puts usage in the final result message
		if msg["type"] == "result" {
			if usage, ok := msg["usage"].(map[string]any); ok {
				totalIn += toInt64(usage["inputTokens"])
				totalOut += toInt64(usage["outputTokens"])
				totalCacheRead += toInt64(usage["cacheReadTokens"])
				totalCacheWrite += toInt64(usage["cacheWriteTokens"])
			}
		}
	}

	claudeErr := claudeCmd.Wait()
	result := strings.TrimSpace(resultBuf.String())

	if totalIn > 0 || totalOut > 0 {
		logf("  [%s] Tokens (%s): %d in / %d out / %d cache-read / %d cache-write",
			taskID, aiCli, totalIn, totalOut, totalCacheRead, totalCacheWrite)
	}

	if claudeErr != nil {
		// Exit code 143 = killed by SIGTERM (128+15), e.g. daemon restart.
		// Requeue instead of marking as permanent error so next cycle retries.
		if exitErr, ok := claudeErr.(*exec.ExitError); ok && exitErr.ExitCode() == 143 {
			logf("  [%s] Claude killed by SIGTERM, requeueing task", taskID)
			m.Status = "pending"
			m.ProcessedBy = ""
			writeTaskMeta(metaFile, m)
			queueCommitPushSafe(fmt.Sprintf("requeue (SIGTERM): %s", taskID))
			return
		}
		logf("  [%s] %s failed: %v", taskID, aiCli, claudeErr)
		markError(m, metaFile, taskID, fmt.Sprintf("%s failed: %v", aiCli, claudeErr))
		return
	}

	logf("  [%s] %s finished", taskID, aiCli)

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
	mergeCmd := exec.Command("python3", amScript, "merge", branch, "--delete-branch", "-p", projectName)
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

	markDone(m, metaFile, taskID, result)
}

// findRepoConfig matches a task's project (e.g. "jschell12/ai-enhance") to a
// configured repo by checking if the repo path ends with the project name.
func findRepoConfig(cfg Config, project string) *RepoConfig {
	projectName := filepath.Base(project)
	for i := range cfg.Repos {
		if filepath.Base(cfg.Repos[i].Path) == projectName {
			return &cfg.Repos[i]
		}
	}
	return nil
}

// killRepoProcesses finds and kills processes whose working directory matches
// repoPath. This catches processes not tracked by the daemon (e.g., manually
// started make run, electron, npm start, etc.) so the app can be properly
// restarted by post-commands.
func killRepoProcesses(repoPath, taskID string) {
	out, err := exec.Command("lsof", "-d", "cwd", "-Fpn").Output()
	if err != nil {
		return
	}

	myPid := os.Getpid()
	myPgid, _ := syscall.Getpgid(myPid)
	lines := strings.Split(string(out), "\n")

	var currentPid int
	killedGroups := make(map[int]bool)

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 'p':
			currentPid, _ = strconv.Atoi(line[1:])
		case 'n':
			cwdPath := line[1:]
			if cwdPath != repoPath || currentPid <= 0 || currentPid == myPid {
				continue
			}
			pgid, err := syscall.Getpgid(currentPid)
			if err != nil {
				continue
			}
			// Don't kill our own process group.
			if pgid == myPgid {
				continue
			}
			if killedGroups[pgid] {
				continue
			}
			killedGroups[pgid] = true
			if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
				logf("  [%s] Post-task: kill repo pgid %d (pid %d): %v", taskID, pgid, currentPid, err)
			} else {
				logf("  [%s] Post-task: killed repo process group %d (pid %d, cwd=%s)", taskID, pgid, currentPid, repoPath)
			}
		}
	}

	if len(killedGroups) > 0 {
		time.Sleep(2 * time.Second)
	}
}

// killPostCmdProcs kills any previously running post-command processes for the
// given repo path by sending SIGTERM to their process groups.
func killPostCmdProcs(repoPath, taskID string) {
	postCmdProcs.Lock()
	procs := postCmdProcs.m[repoPath]
	postCmdProcs.m[repoPath] = nil
	postCmdProcs.Unlock()

	for _, p := range procs {
		// Kill the entire process group (negative PID).
		if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil {
			// Process may have already exited — that's fine.
			logf("  [%s] Post-task: kill pgid %d: %v (may have already exited)", taskID, p.Pid, err)
		} else {
			logf("  [%s] Post-task: killed previous post-command process group %d", taskID, p.Pid)
		}
		// Reap the process so it doesn't become a zombie.
		p.Wait()
	}
}

// runPostTaskCommands pulls the latest changes into the local repo and runs
// any configured post-commands (e.g. make build, make install).
// Long-running commands are started in background process groups so they can
// be killed and restarted on the next task completion.
func runPostTaskCommands(cfg Config, project, taskID string) {
	rc := findRepoConfig(cfg, project)
	if rc == nil || rc.Path == "" {
		debug("  [%s] Post-task: no repo config for %s", taskID, project)
		return
	}

	if len(rc.PostCommands) == 0 {
		debug("  [%s] Post-task: no commands configured for %s", taskID, filepath.Base(rc.Path))
		reloadApp(taskID, "xmuggle", 24816)
		reloadApp(taskID, "ai-enhance", 24817)
		return
	}

	// Run post-commands in detached bash
	debug("  [%s] Post-task: running commands in detached bash", taskID)

	if _, err := os.Stat(rc.Path); err != nil {
		warn("  [%s] Post-task: local path %s not found", taskID, rc.Path)
		return
	}

	script := strings.Join(rc.PostCommands, " && ")
	logf("  [%s] Post-task: running %q in %s", taskID, script, rc.Path)

	postLog := filepath.Join(xmuggleDir, "post-cmd-"+taskID+".log")
	cmd := exec.Command("bash", "-lc", script)
	cmd.Dir = rc.Path
	cmd.Env = gitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	lf, _ := os.Create(postLog)
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		errorf("  [%s] Post-task: failed to start: %v", taskID, err)
		lf.Close()
	} else {
		logf("  [%s] Post-task: started (pid %d), log: %s", taskID, cmd.Process.Pid, postLog)
		go func() {
			err := cmd.Wait()
			lf.Close()
			if err != nil {
				warn("  [%s] Post-task: exited with error: %v", taskID, err)
			} else {
				logf("  [%s] Post-task: completed successfully", taskID)
			}
		}()
	}

	reloadApp(taskID, "xmuggle", 24816)
	reloadApp(taskID, "ai-enhance", 24817)
}

func reloadApp(taskID, name string, port int) {
	resp, err := http.Post(fmt.Sprintf("http://localhost:%d/reload", port), "application/json", nil)
	if err != nil {
		return // app not running, skip silently
	}
	resp.Body.Close()
	logf("  [%s] Post-task: %s UI reloaded", taskID, name)
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
		logLevel = parseLogLevel(cfg.LogLevel)
		workerSem = make(chan struct{}, cfg.MaxWorkers)
		_ = os.MkdirAll(xmuggleDir, 0755)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
		logf("Daemon starting (pid %d, interval %ds, maxWorkers %d, logLevel %s, aiCli %s)",
			os.Getpid(), cfg.Interval, cfg.MaxWorkers, cfg.LogLevel, cfg.AICli)
		debug("Config: %+v", cfg)
		debug("Queue repo: %s", cfg.QueueRepo)
		debug("AQ scripts: %s", cfg.AQScriptsDir)
		debug("Hostname: %s", hostname())
		for _, r := range cfg.Repos {
			debug("Repo: %s (aiCli=%s, postCmds=%v)", r.Path, r.AICli, r.PostCommands)
		}

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
		logLevel = parseLogLevel(cfg.LogLevel)
		workerSem = make(chan struct{}, cfg.MaxWorkers)
		logf("Running single cycle (maxWorkers %d, logLevel %s)", cfg.MaxWorkers, cfg.LogLevel)
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
		fmt.Printf("LogLevel:    %s\n", orDefault(cfg.LogLevel, "info"))
		fmt.Printf("Queue repo:  %s\n", orDefault(cfg.QueueRepo, "(none)"))
		fmt.Printf("AI CLI:      %s\n", cfg.AICli)
		fmt.Printf("AQ scripts:  %s\n", cfg.AQScriptsDir)
		if len(cfg.Repos) > 0 {
			fmt.Printf("Repos:\n")
			for _, r := range cfg.Repos {
				cli := r.AICli
				if cli == "" {
					cli = cfg.AICli
				}
				fmt.Printf("  %s (cli: %s)\n", r.Path, cli)
				for _, c := range r.PostCommands {
					fmt.Printf("    post: %s\n", c)
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

Repo config example (daemon.json):
  "repos": [
    {"path": "/path/to/local/repo", "postCommands": ["make build"]}
  ]
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

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
