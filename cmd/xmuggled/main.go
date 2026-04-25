package main

import (
	"bufio"
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

// postCmdProcs tracks running post-command processes by repo path so they can
// be killed before re-running.
var postCmdProcs = struct {
	sync.Mutex
	m map[string][]*os.Process // key = repo path
}{m: make(map[string][]*os.Process)}

type RepoConfig struct {
	Path         string   `json:"path"`
	PostCommands []string `json:"postCommands,omitempty"`
	AICli        string   `json:"aiCli,omitempty"` // per-repo override: "claude" or "cursor"
}

type Config struct {
	Interval     int          `json:"interval"`
	QueueRepo    string       `json:"queueRepo"`
	MaxWorkers   int          `json:"maxWorkers"`
	AQScriptsDir string       `json:"aqScriptsDir"`
	AICli        string       `json:"aiCli"`
	Repos        []RepoConfig `json:"repos,omitempty"`
}

func defaultConfig() Config {
	return Config{
		Interval:     10,
		MaxWorkers:   3,
		AICli:        "claude",
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

// resolveAICli returns the CLI to use for a given project, checking per-repo override first.
func resolveAICli(cfg Config, project string) string {
	rc := findRepoConfig(cfg, project)
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
	// Hold queueMu so we don't race with queueCommitPushSafe in worker
	// goroutines.  Without this, reset --hard can destroy a worker's
	// unpushed "done" commit, causing the sender to never see the
	// completion and never run post-task commands.
	queueMu.Lock()
	defer queueMu.Unlock()
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
	if !ensureQueueClone(cfg) {
		return
	}

	pendingDir := filepath.Join(queueDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return
	}

	host := hostname()
	postCmdDone := loadPostCmdDone()

	// First pass: run post-commands for our tasks that completed
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskID := entry.Name()
		if postCmdDone[taskID] {
			continue
		}
		metaFile := filepath.Join(pendingDir, taskID, "meta.json")
		m, err := readTaskMeta(metaFile)
		if err != nil {
			continue
		}
		// Only run post-commands for tasks WE sent that are now done
		if m.From == host && m.Status == "done" {
			postCmdDone[taskID] = true
			savePostCmdDone(postCmdDone)
			logf("  [%s] Task completed by %s, running post-commands", taskID, m.ProcessedBy)
			runPostTaskCommands(cfg, m.Project, taskID)
		}
	}

	// Second pass: dispatch pending tasks from other hosts
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
	prompt := strings.Join(promptParts, "\n\n")

	// Spawn Claude — stream output and log filtered lines to daemon log
	logf("  [%s] Spawning claude on branch %s", taskID, branch)

	aiCli := resolveAICli(cfg, m.Project)
	logf("  [%s] Using AI CLI: %s", taskID, aiCli)
	claudeCmd := buildAICommand(aiCli, prompt)
	claudeCmd.Dir = cloneDir
	claudeCmd.Env = gitEnv()

	stdoutPipe, _ := claudeCmd.StdoutPipe()
	claudeCmd.Stderr = claudeCmd.Stdout // merge stderr into stdout
	claudeCmd.Start()

	var resultBuf strings.Builder
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	for scanner.Scan() {
		line := scanner.Text()
		resultBuf.WriteString(line)
		resultBuf.WriteString("\n")

		// Parse and log readable content
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg["type"] == "assistant" {
			if message, ok := msg["message"].(map[string]any); ok {
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
	}

	claudeErr := claudeCmd.Wait()
	result := strings.TrimSpace(resultBuf.String())

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
		return
	}

	// Check local path exists
	if _, err := os.Stat(rc.Path); err != nil {
		logf("  [%s] Post-task: local path %s not found, skipping", taskID, rc.Path)
		return
	}

	// Kill any running processes in the repo directory (manually started apps, etc.)
	killRepoProcesses(rc.Path, taskID)

	// Kill any still-running post-commands for this repo before restarting.
	killPostCmdProcs(rc.Path, taskID)

	// Pull latest changes
	logf("  [%s] Post-task: git pull --rebase in %s", taskID, rc.Path)
	if out, err := runGit(rc.Path, "pull", "--rebase"); err != nil {
		logf("  [%s] Post-task: git pull failed: %s", taskID, out)
	}

	// Run post-commands
	var newProcs []*os.Process
	for _, cmdStr := range rc.PostCommands {
		logf("  [%s] Post-task: running %q in %s", taskID, cmdStr, rc.Path)
		cmd := exec.Command("bash", "-c", cmdStr)
		cmd.Dir = rc.Path
		cmd.Env = os.Environ()
		// Start in its own process group so we can kill the whole tree later.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			logf("  [%s] Post-task: %q failed to start: %v", taskID, cmdStr, err)
			continue
		}
		logf("  [%s] Post-task: %q started (pid %d)", taskID, cmdStr, cmd.Process.Pid)
		newProcs = append(newProcs, cmd.Process)

		// Wait in the background so we log completion / reap zombies.
		go func(cmdStr string, c *exec.Cmd) {
			if err := c.Wait(); err != nil {
				logf("  [%s] Post-task: %q exited: %v", taskID, cmdStr, err)
			} else {
				logf("  [%s] Post-task: %q succeeded", taskID, cmdStr)
			}
		}(cmdStr, cmd)
	}

	postCmdProcs.Lock()
	postCmdProcs.m[rc.Path] = newProcs
	postCmdProcs.Unlock()
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

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
