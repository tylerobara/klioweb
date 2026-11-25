package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const pushoverEndpoint = "https://api.pushover.net/1/messages.json"

// Pushover sends via https://pushover.net/api.
type Pushover struct {
	Token, User string // application token and user/group key
	Device      string // optional device name(s), comma separated
	Sound       string // optional sound name
	// Priority per outcome, -2..2. Priority 2 (emergency) also needs Retry/Expire.
	FailurePriority, SuccessPriority int
	Retry, Expire                    int    // seconds; used only for priority 2
	LinkURL                          string // optional dashboard URL attached to the message

	Endpoint string
	Client   *http.Client
}

func NewPushover(token, user string) *Pushover {
	return &Pushover{Token: token, User: user, FailurePriority: 1, SuccessPriority: -1,
		Retry: 60, Expire: 3600, Endpoint: pushoverEndpoint, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *Pushover) Name() string { return "pushover" }

// Validate checks values against Pushover's documented limits.
func (p *Pushover) Validate() error {
	for _, pr := range []int{p.FailurePriority, p.SuccessPriority} {
		if pr < -2 || pr > 2 {
			return fmt.Errorf("pushover priority %d out of range -2..2", pr)
		}
		if pr == 2 && (p.Retry < 30 || p.Expire < 1 || p.Expire > 10800) {
			return fmt.Errorf("pushover priority 2 needs retry >= 30 and 1 <= expire <= 10800")
		}
	}
	return nil
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (p *Pushover) Send(ctx context.Context, e Event) error {
	b := e.Backup
	prio := p.SuccessPriority
	if e.Failed() {
		prio = p.FailurePriority
	}
	msg := b.Name
	if e.Failed() {
		if em := strings.TrimSpace(b.Error); em != "" {
			msg += "\n" + em
		}
	} else if b.DurationMs > 0 {
		msg += fmt.Sprintf("\nTook %.0fs", float64(b.DurationMs)/1000)
	}
	form := url.Values{
		"token": {p.Token}, "user": {p.User},
		"title":    {trunc(e.Title(), 250)},
		"message":  {trunc(msg, 1024)},
		"priority": {strconv.Itoa(prio)},
	}
	if b.StoppedAt != nil {
		form.Set("timestamp", strconv.FormatInt(b.StoppedAt.Unix(), 10))
	}
	if p.Device != "" {
		form.Set("device", p.Device)
	}
	if p.Sound != "" {
		form.Set("sound", p.Sound)
	}
	if p.LinkURL != "" {
		form.Set("url", p.LinkURL)
		form.Set("url_title", "Klio dashboard")
	}
	if prio == 2 {
		form.Set("retry", strconv.Itoa(p.Retry))
		form.Set("expire", strconv.Itoa(p.Expire))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("pushover returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	// 4xx means the request itself is bad (token, user, params): retrying can't help.
	// 429 (rate limit) and 5xx are transient.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %w", errPermanent, err)
	}
	return err
}
