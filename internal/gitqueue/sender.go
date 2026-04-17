package gitqueue

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jschell12/xmuggle/internal/ageutil"
	"github.com/jschell12/xmuggle/internal/config"
)

type SendArgs struct {
	TaskID          string
	Repo            string
	Message         string
	ScreenshotPath  string   // single (legacy callers)
	ScreenshotPaths []string // multi-image
	Recipient       string   // hostname override; empty uses default
}

// screenshotPaths returns the canonical list: multi if set, else wraps single.
func (a *SendArgs) screenshotPaths() []string {
	if len(a.ScreenshotPaths) > 0 {
		return a.ScreenshotPaths
	}
	if a.ScreenshotPath != "" {
		return []string{a.ScreenshotPath}
	}
	return nil
}

// SendTask encrypts and pushes a task to the queue repo.
// Supports both single and multi-image tasks.
func SendTask(cfg *config.Config, args SendArgs) error {
	if cfg.Git == nil {
		return fmt.Errorf("git transport not configured; run: xmuggle init-send <owner/repo> (or init-recv on the processing machine)")
	}
	if cfg.Age == nil {
		return fmt.Errorf("no age keypair; run: xmuggle init-send <owner/repo> (or init-recv on the processing machine)")
	}

	recipientHost := args.Recipient
	if recipientHost == "" {
		recipientHost = cfg.DefaultRecipient
	}
	if recipientHost == "" {
		return fmt.Errorf("no recipient specified; run: xmuggle add-recipient <host> --default or pass --to <host>")
	}
	recPubkey := cfg.RecipientPubkey(recipientHost)
	if recPubkey == "" {
		return fmt.Errorf("no pubkey configured for recipient %q; run: xmuggle add-recipient %s", recipientHost, recipientHost)
	}

	if err := EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return err
	}
	if err := PullRebase(cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return err
	}

	paths := args.screenshotPaths()
	if len(paths) == 0 {
		return fmt.Errorf("no screenshot paths given")
	}

	payload := Payload{
		Version:           2,
		TaskID:            args.TaskID,
		SenderHostname:    cfg.Hostname,
		RecipientHostname: recipientHost,
		Repo:              args.Repo,
		Message:           args.Message,
		Timestamp:         time.Now().UnixMilli(),
	}

	if len(paths) == 1 {
		// Single image — backward-compatible path
		shotBytes, err := os.ReadFile(paths[0])
		if err != nil {
			return fmt.Errorf("read screenshot: %w", err)
		}
		ext := strings.ToLower(filepath.Ext(paths[0]))
		if ext == "" {
			ext = ".png"
		}
		payload.Screenshot.Name = "screenshot" + ext
		payload.Screenshot.DataB64 = base64.StdEncoding.EncodeToString(shotBytes)
	} else {
		// Multi-image
		for i, p := range paths {
			shotBytes, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read screenshot %s: %w", p, err)
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext == "" {
				ext = ".png"
			}
			payload.Screenshots = append(payload.Screenshots, ScreenshotEntry{
				Name:    fmt.Sprintf("screenshot-%03d%s", i+1, ext),
				DataB64: base64.StdEncoding.EncodeToString(shotBytes),
			})
		}
	}

	plaintext, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ciphertext, err := ageutil.EncryptToRecipients(plaintext, []string{recPubkey, cfg.Age.Pubkey})
	if err != nil {
		return err
	}

	rel := fmt.Sprintf("queue/%s-%s.age", args.TaskID, cfg.Hostname)
	if err := WriteFile(cfg.Git.CloneDir, rel, ciphertext); err != nil {
		return err
	}

	msg := fmt.Sprintf("Queue task %s (%d image(s)) from %s", args.TaskID, len(paths), cfg.Hostname)
	return CommitAndPush(cfg.Git.CloneDir, []string{rel}, msg, cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail)
}

// PollForResult waits for results/<taskID>-*.age and returns the decrypted result.
func PollForResult(cfg *config.Config, taskID string, timeout time.Duration) (*ResultPayload, error) {
	if cfg.Git == nil || cfg.Age == nil {
		return nil, fmt.Errorf("git transport not configured")
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	pollMS := cfg.Git.PollIntervalMS
	if pollMS == 0 {
		pollMS = 10_000
	}
	pollInterval := time.Duration(pollMS) * time.Millisecond

	if err := EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = PullRebase(cfg.Git.CloneDir, cfg.Git.Branch)

		files, _ := ListFiles(cfg.Git.CloneDir, "results")
		for _, rel := range files {
			name := filepath.Base(rel)
			if !strings.HasPrefix(name, taskID+"-") || !strings.HasSuffix(name, ".age") {
				continue
			}
			ct, err := ReadFile(cfg.Git.CloneDir, rel)
			if err != nil {
				continue
			}
			pt, err := ageutil.DecryptWithIdentity(ct, cfg.Age.IdentityFile)
			if err != nil {
				continue
			}
			var r ResultPayload
			if err := json.Unmarshal(pt, &r); err != nil {
				continue
			}
			if r.TaskID == taskID {
				return &r, nil
			}
		}

		_, _ = io.WriteString(os.Stderr, ".")
		time.Sleep(pollInterval)
	}
	return nil, fmt.Errorf("timed out waiting for result after %s", timeout)
}
