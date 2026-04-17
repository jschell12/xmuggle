// Command xmuggled is the xmuggle daemon: watches ~/.xmuggle/queue, enqueues tasks to
// agent-queue, spawns workers, and optionally syncs with a private git queue repo.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jschell12/xmuggle/internal/aq"
	"github.com/jschell12/xmuggle/internal/config"
	"github.com/jschell12/xmuggle/internal/gitqueue"
	"github.com/jschell12/xmuggle/internal/prompt"
	"github.com/jschell12/xmuggle/internal/queue"
	"github.com/jschell12/xmuggle/internal/spawn"
)

var (
	prURLRE  = regexp.MustCompile(`https://github\.com/[^\s]+/pull/\d+`)
	branchRE = regexp.MustCompile(`xmuggle-fix/\d+`)
)

func projectName(repo string) string {
	s := strings.TrimPrefix(repo, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimSuffix(s, ".git")
	return filepath.Base(s)
}

func repoURL(repo string) string {
	if strings.HasPrefix(repo, "http") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	// "owner/name" short form
	if m, _ := regexp.MatchString(`^[\w-]+/[\w.-]+$`, repo); m {
		return "https://github.com/" + repo + ".git"
	}
	return repo
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type daemon struct {
	cfg        *config.Config
	paths      config.Paths
	maxWorkers int
	interval   time.Duration

	mu sync.Mutex
}

func (d *daemon) logf(format string, a ...any) {
	log.Printf(format, a...)
}

// enqueueTask converts a local pending task into an agent-queue item.
func (d *daemon) enqueueTask(taskDir string) (projectAndRepo [2]string, ok bool) {
	taskID := filepath.Base(taskDir)
	t, err := queue.ReadTask(taskDir)
	if err != nil {
		d.logf("read %s: %v", taskDir, err)
		_ = queue.UpdateTaskStatus(taskDir, queue.StatusError)
		return
	}
	shots := queue.FindScreenshots(taskDir)
	if len(shots) == 0 {
		d.logf("no screenshots in %s", taskDir)
		_ = queue.UpdateTaskStatus(taskDir, queue.StatusError)
		return
	}
	project := projectName(t.Repo)

	if err := aq.Init(project); err != nil {
		d.logf("aq init %s: %v", project, err)
		return
	}
	title := "xmuggle-fix:" + taskID
	desc := t.Message
	if desc == "" {
		desc = "Screenshot-driven fix"
	}
	if err := aq.Add(project, title, desc, []string{"screenshot"}); err != nil {
		d.logf("aq add %s: %v", taskID, err)
		return
	}
	d.logf("enqueued task %s to agent-queue project %s", taskID, project)
	_ = queue.UpdateTaskStatus(taskDir, queue.StatusProcessing)
	return [2]string{project, t.Repo}, true
}

// spawnWorker clones the repo, runs the worker prompt, returns once done (fire-and-forget caller).
func (d *daemon) spawnWorker(project, repo string) {
	agentID, err := aq.NextAgentID(project)
	if err != nil {
		d.logf("next agent id: %v", err)
		return
	}

	info, err := aq.Clone(repoURL(repo), agentID, os.TempDir())
	if err != nil {
		d.logf("aq clone for %s: %v", agentID, err)
		return
	}

	d.logf("spawning worker %s in %s on branch %s", agentID, info.CloneDir, info.Branch)

	p := prompt.BuildWorker(prompt.WorkerOptions{
		AgentID:            agentID,
		Project:            project,
		RepoURL:            repoURL(repo),
		CloneDir:           info.CloneDir,
		Branch:             info.Branch,
		AQScripts:          aq.ScriptsDir(),
		ScreenshotQueueDir: d.paths.QueueDir,
		ResultsDir:         d.paths.ResultsDir,
	})

	go func() {
		result, err := spawn.Capture(p, info.CloneDir)
		if err != nil {
			d.logf("worker %s error: %v", agentID, err)
			return
		}
		if result.ExitCode != 0 {
			d.logf("worker %s exited with code %d", agentID, result.ExitCode)
			return
		}
		combined := result.Stdout + "\n" + result.Stderr
		prURL := prURLRE.FindString(combined)
		branch := branchRE.FindString(combined)
		d.logf("worker %s completed. PR: %s, branch: %s", agentID, prURL, branch)
	}()
}

func (d *daemon) tick() {
	d.mu.Lock()
	defer d.mu.Unlock()

	pending := queue.ListPending(d.paths.QueueDir)
	if len(pending) == 0 {
		return
	}

	groups := map[string][2]string{} // project → [project, repo]
	enqueuedByProject := map[string]int{}

	for _, taskDir := range pending {
		info, ok := d.enqueueTask(taskDir)
		if !ok {
			continue
		}
		project := info[0]
		groups[project] = info
		enqueuedByProject[project]++
	}

	for project, info := range groups {
		repo := info[1]
		active, err := aq.ActiveWorkerCount(project)
		if err != nil {
			d.logf("active count for %s: %v", project, err)
			active = 0
		}
		needed := enqueuedByProject[project]
		if needed > d.maxWorkers {
			needed = d.maxWorkers
		}
		toSpawn := needed - active
		for i := 0; i < toSpawn; i++ {
			d.spawnWorker(project, repo)
		}
		d.logf("project %s: %d enqueued, %d workers active, %d spawned", project, enqueuedByProject[project], active, max(0, toSpawn))
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (d *daemon) run() {
	d.logf("Daemon started. Watching %s every %s", d.paths.QueueDir, d.interval)
	d.logf("Agent-queue scripts: %s", aq.ScriptsDir())
	d.logf("Max workers: %d", d.maxWorkers)

	// Initial check
	d.tick()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	// Git sync tickers (separate intervals, guarded against overlap)
	var gitTicker *time.Ticker
	if d.cfg.Git != nil && d.cfg.Age != nil {
		pollMS := d.cfg.Git.PollIntervalMS
		if pollMS == 0 {
			pollMS = 10_000
		}
		gitInterval := time.Duration(pollMS) * time.Millisecond
		gitTicker = time.NewTicker(gitInterval)
		d.logf("Git sync enabled (repo=%s, poll=%s)", d.cfg.Git.QueueRepo, gitInterval)
	} else {
		d.logf("Git sync disabled (not configured)")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	var gitC <-chan time.Time
	if gitTicker != nil {
		gitC = gitTicker.C
		// Kick off initial git sync so new tasks get ingested promptly
		go d.gitSync()
	}

	for {
		select {
		case <-ticker.C:
			d.tick()
		case <-gitC:
			d.gitSync()
		case s := <-sig:
			d.logf("Received %s, shutting down", s)
			return
		}
	}
}

type busyGuard struct {
	mu   sync.Mutex
	busy bool
}

func (g *busyGuard) tryLock() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy {
		return false
	}
	g.busy = true
	return true
}

func (g *busyGuard) unlock() {
	g.mu.Lock()
	g.busy = false
	g.mu.Unlock()
}

var gitGuard busyGuard

func (d *daemon) gitSync() {
	if !gitGuard.tryLock() {
		return
	}
	defer gitGuard.unlock()

	if err := gitqueue.IngestTick(d.cfg); err != nil {
		d.logf("[git] ingest: %v", err)
	}
	if err := gitqueue.PublishResultsTick(d.cfg); err != nil {
		d.logf("[git] publish: %v", err)
	}
}

func main() {
	if err := config.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "ensure dirs:", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	p := config.GetPaths()

	log.SetFlags(log.LstdFlags | log.LUTC)

	d := &daemon{
		cfg:        cfg,
		paths:      p,
		maxWorkers: envInt("MAX_WORKERS", 3),
		interval:   time.Duration(envInt("POLL_INTERVAL_MS", 5000)) * time.Millisecond,
	}
	d.run()
}
