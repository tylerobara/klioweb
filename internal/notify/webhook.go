package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Webhook POSTs a JSON document. The text/content fields make it work as-is
// for Slack, Mattermost and Discord.
type Webhook struct {
	URL    string
	Client *http.Client
}

func NewWebhook(url string) *Webhook {
	return &Webhook{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, e Event) error {
	icon := "✅"
	if e.Failed() {
		icon = "❌"
	}
	text := fmt.Sprintf("%s %s (%s)", icon, e.Title(), e.Backup.Name)
	if msg := e.firstError(); msg != "" {
		text += ": " + msg
	}
	b := e.Backup
	body, _ := json.Marshal(map[string]any{
		"text": text, "content": text,
		"event": "backup." + b.Phase, "status": b.Phase,
		"cluster": e.Cluster.Name, "namespace": e.Cluster.Namespace,
		"backup": b.Name, "error": b.Error,
		"startedAt": b.StartedAt, "stoppedAt": b.StoppedAt, "durationMs": b.DurationMs,
		"health": e.Cluster.Klio.Health,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
