// Package record captures screen frames at a configurable FPS using macOS screencapture.
package record

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Options configures a recording session.
type Options struct {
	Duration  time.Duration // 0 = record until Stop()
	FPS       float64       // default 1.0
	Format    string        // "jpg" (default) or "png"
	OutputDir string        // directory for frame files
}

func (o *Options) defaults() {
	if o.FPS <= 0 {
		o.FPS = 1.0
	}
	if o.Format == "" {
		o.Format = "jpg"
	}
	if o.OutputDir == "" {
		o.OutputDir = os.TempDir()
	}
}

// Recorder captures screen frames.
type Recorder struct {
	opts   Options
	prefix string
	stopCh chan struct{}

	mu     sync.Mutex
	frames []string
	seq    int
}

// New creates a Recorder with the given options.
func New(opts Options) *Recorder {
	opts.defaults()
	prefix := fmt.Sprintf("rec-%d", time.Now().UnixMilli())
	return &Recorder{
		opts:   opts,
		prefix: prefix,
		stopCh: make(chan struct{}),
	}
}

// Prefix returns the recording's prefix (e.g. "rec-1713234567890").
func (r *Recorder) Prefix() string { return r.prefix }

// Start begins capturing frames in a background goroutine.
// Returns an error if the first screencapture call fails (e.g. missing
// Screen Recording permission).
func (r *Recorder) Start() error {
	// Capture the first frame immediately so we fail fast on permission errors.
	if err := r.captureFrame(); err != nil {
		return fmt.Errorf("screencapture failed: %w\n  Grant Screen Recording permission to your terminal in System Settings > Privacy & Security > Screen Recording", err)
	}

	interval := time.Duration(float64(time.Second) / r.opts.FPS)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		deadline := time.Time{}
		if r.opts.Duration > 0 {
			deadline = time.Now().Add(r.opts.Duration)
		}

		for {
			select {
			case <-r.stopCh:
				return
			case <-ticker.C:
				if !deadline.IsZero() && time.Now().After(deadline) {
					return
				}
				_ = r.captureFrame()
			}
		}
	}()
	return nil
}

// Stop signals the recorder to stop and returns the list of captured frame paths.
func (r *Recorder) Stop() []string {
	select {
	case <-r.stopCh:
		// already stopped
	default:
		close(r.stopCh)
	}
	// Give the goroutine a moment to flush
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.frames))
	copy(out, r.frames)
	return out
}

// Frames returns the current count of captured frames (thread-safe).
func (r *Recorder) FrameCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *Recorder) captureFrame() error {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()

	name := fmt.Sprintf("%s-%03d.%s", r.prefix, seq, r.opts.Format)
	outPath := filepath.Join(r.opts.OutputDir, name)

	cmd := exec.Command("screencapture", "-x", "-t", r.opts.Format, outPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}

	if _, err := os.Stat(outPath); err != nil {
		return err
	}

	r.mu.Lock()
	r.frames = append(r.frames, outPath)
	r.mu.Unlock()
	return nil
}
