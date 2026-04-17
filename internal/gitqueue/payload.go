package gitqueue

// ScreenshotEntry is a single image within a task payload.
type ScreenshotEntry struct {
	Name    string `json:"name"`
	DataB64 string `json:"data_b64"`
}

// Payload is the encrypted task sent from sender to receiver.
type Payload struct {
	Version           int               `json:"version"`
	TaskID            string            `json:"task_id"`
	SenderHostname    string            `json:"sender_hostname"`
	RecipientHostname string            `json:"recipient_hostname"`
	Repo              string            `json:"repo"`
	Message           string            `json:"message,omitempty"`
	Timestamp         int64             `json:"timestamp"`
	Screenshot        ScreenshotEntry   `json:"screenshot"`                // single (backward compat)
	Screenshots       []ScreenshotEntry `json:"screenshots,omitempty"`     // multi-image
}

// AllScreenshots returns the canonical list of screenshots. If Screenshots
// (multi) is populated, it is returned; otherwise the single Screenshot
// field is wrapped into a one-element slice.
func (p *Payload) AllScreenshots() []ScreenshotEntry {
	if len(p.Screenshots) > 0 {
		return p.Screenshots
	}
	if p.Screenshot.Name != "" {
		return []ScreenshotEntry{p.Screenshot}
	}
	return nil
}

// ResultPayload is the encrypted result sent from receiver back to sender.
type ResultPayload struct {
	Version           int    `json:"version"`
	TaskID            string `json:"task_id"`
	SenderHostname    string `json:"sender_hostname"`
	RecipientHostname string `json:"recipient_hostname"`
	Status            string `json:"status"` // "success" or "error"
	PRUrl             string `json:"pr_url,omitempty"`
	Branch            string `json:"branch,omitempty"`
	Summary           string `json:"summary"`
	Timestamp         int64  `json:"timestamp"`
}
