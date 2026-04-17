package gitqueue

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jschell12/xmuggle/internal/ageutil"
	"github.com/jschell12/xmuggle/internal/config"
	"github.com/jschell12/xmuggle/internal/queue"
)

func gitLogf(format string, a ...any) {
	log.Printf("[git] "+format, a...)
}

// lookupSenderPubkey resolves a sender's pubkey via queue repo first, config fallback.
func lookupSenderPubkey(cfg *config.Config, sender string) string {
	if cfg.Git != nil {
		rel := fmt.Sprintf("pubkeys/%s.pub", sender)
		if FileExists(cfg.Git.CloneDir, rel) {
			data, err := ReadFile(cfg.Git.CloneDir, rel)
			if err == nil {
				return strings.TrimSpace(string(data))
			}
		}
	}
	return cfg.RecipientPubkey(sender)
}

// IngestTick pulls the queue repo, decrypts new tasks addressed to us, and
// writes them into ~/.xmuggle/queue/ in the existing format for the daemon.
func IngestTick(cfg *config.Config) error {
	if cfg.Git == nil || cfg.Age == nil {
		return nil
	}
	if err := EnsureCloned(cfg.Git.QueueRepo, cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return err
	}
	if err := PullRebase(cfg.Git.CloneDir, cfg.Git.Branch); err != nil {
		return err
	}

	files, err := ListFiles(cfg.Git.CloneDir, "queue")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	paths := config.GetPaths()
	var toCommit []string

	for _, rel := range files {
		if !strings.HasSuffix(rel, ".age") {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(rel), ".age")
		markerRel := fmt.Sprintf("processed/%s.marker", base)
		if FileExists(cfg.Git.CloneDir, markerRel) {
			continue
		}

		ct, err := ReadFile(cfg.Git.CloneDir, rel)
		if err != nil {
			gitLogf("read %s: %v", rel, err)
			continue
		}
		pt, err := ageutil.DecryptWithIdentity(ct, cfg.Age.IdentityFile)
		if err != nil {
			gitLogf("decrypt %s: %v", rel, err)
			continue
		}

		var p Payload
		if err := json.Unmarshal(pt, &p); err != nil {
			gitLogf("parse %s: %v", rel, err)
			continue
		}
		if p.RecipientHostname != cfg.Hostname {
			gitLogf("skip %s (addressed to %s, not us)", rel, p.RecipientHostname)
			continue
		}

		allShots := p.AllScreenshots()
		if len(allShots) == 0 {
			gitLogf("no screenshots in %s", rel)
			continue
		}

		task := queue.Task{
			Repo:      p.Repo,
			Message:   p.Message,
			Timestamp: p.Timestamp,
			Status:    queue.StatusPending,
		}

		if len(allShots) == 1 {
			shotBytes, err := base64.StdEncoding.DecodeString(allShots[0].DataB64)
			if err != nil {
				gitLogf("b64 decode %s: %v", rel, err)
				continue
			}
			if _, err := queue.WriteTaskFromBytes(paths.QueueDir, p.TaskID, task, allShots[0].Name, shotBytes); err != nil {
				gitLogf("write local task %s: %v", p.TaskID, err)
				continue
			}
		} else {
			var shots []queue.ScreenshotData
			for _, s := range allShots {
				b, err := base64.StdEncoding.DecodeString(s.DataB64)
				if err != nil {
					gitLogf("b64 decode %s/%s: %v", rel, s.Name, err)
					continue
				}
				shots = append(shots, queue.ScreenshotData{Name: s.Name, Data: b})
			}
			if _, err := queue.WriteTaskFromBytesMulti(paths.QueueDir, p.TaskID, task, shots); err != nil {
				gitLogf("write multi-image task %s: %v", p.TaskID, err)
				continue
			}
		}
		gitLogf("ingested task %s from %s (%d image(s))", p.TaskID, p.SenderHostname, len(allShots))

		meta := map[string]string{
			"sender_hostname": p.SenderHostname,
			"source_filename": rel,
		}
		if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(paths.QueueDir, p.TaskID, ".git-meta.json"), append(data, '\n'), 0o644)
		}

		gitLogf("ingested task %s from %s", p.TaskID, p.SenderHostname)

		if err := WriteFile(cfg.Git.CloneDir, markerRel, []byte("")); err == nil {
			toCommit = append(toCommit, markerRel)
		}
		if err := GitRm(cfg.Git.CloneDir, rel); err == nil {
			toCommit = append(toCommit, rel)
		}
	}

	if len(toCommit) > 0 {
		msg := fmt.Sprintf("Ingest %d task(s) on %s", len(toCommit)/2, cfg.Hostname)
		if err := CommitAndPush(cfg.Git.CloneDir, toCommit, msg, cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail); err != nil {
			gitLogf("commit/push ingest: %v", err)
		}
	}
	return nil
}

// PublishResultsTick encrypts and pushes any local results that were git-ingested.
func PublishResultsTick(cfg *config.Config) error {
	if cfg.Git == nil || cfg.Age == nil {
		return nil
	}
	paths := config.GetPaths()
	if _, err := os.Stat(paths.ResultsDir); os.IsNotExist(err) {
		return nil
	}

	entries, err := os.ReadDir(paths.ResultsDir)
	if err != nil {
		return err
	}

	var toCommit []string

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		resultDir := filepath.Join(paths.ResultsDir, taskID)
		resultFile := filepath.Join(resultDir, "result.json")
		publishedMarker := filepath.Join(resultDir, ".published")

		if _, err := os.Stat(resultFile); os.IsNotExist(err) {
			continue
		}
		if _, err := os.Stat(publishedMarker); err == nil {
			continue
		}

		metaPath := filepath.Join(paths.QueueDir, taskID, ".git-meta.json")
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			continue // not a git-ingested task
		}
		var meta map[string]string
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			continue
		}
		senderHost := meta["sender_hostname"]
		senderPub := lookupSenderPubkey(cfg, senderHost)
		if senderPub == "" {
			gitLogf("no pubkey for sender %s — skipping %s", senderHost, taskID)
			continue
		}

		data, err := os.ReadFile(resultFile)
		if err != nil {
			continue
		}
		var r queue.Result
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}

		payload := ResultPayload{
			Version:           1,
			TaskID:            taskID,
			SenderHostname:    cfg.Hostname,
			RecipientHostname: senderHost,
			Status:            r.Status,
			PRUrl:             r.PRUrl,
			Branch:            r.Branch,
			Summary:           r.Summary,
			Timestamp:         time.Now().UnixMilli(),
		}
		pt, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		ct, err := ageutil.EncryptToRecipients(pt, []string{senderPub, cfg.Age.Pubkey})
		if err != nil {
			gitLogf("encrypt result %s: %v", taskID, err)
			continue
		}

		rel := fmt.Sprintf("results/%s-%s.age", taskID, cfg.Hostname)
		if err := WriteFile(cfg.Git.CloneDir, rel, ct); err != nil {
			continue
		}
		toCommit = append(toCommit, rel)
		_ = os.WriteFile(publishedMarker, []byte(""), 0o644)
		gitLogf("published result for %s → %s", taskID, senderHost)
	}

	if len(toCommit) > 0 {
		_ = PullRebase(cfg.Git.CloneDir, cfg.Git.Branch)
		msg := fmt.Sprintf("Publish %d result(s) from %s", len(toCommit), cfg.Hostname)
		if err := CommitAndPush(cfg.Git.CloneDir, toCommit, msg, cfg.Git.Branch, cfg.Git.AuthorName, cfg.Git.AuthorEmail); err != nil {
			gitLogf("commit/push results: %v", err)
		}
	}
	return nil
}
