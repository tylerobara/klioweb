// Package notify delivers notifications when a Klio backup finishes.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tylerobara/klioweb/internal/collector"
)

type Mode string

const (
	OnFailure Mode = "failure"
	OnSuccess Mode = "success"
	OnBoth    Mode = "both"
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return OnFailure, nil
	case OnFailure, OnSuccess, OnBoth:
		return m, nil
	}
	return "", fmt.Errorf("invalid notification mode %q (want success, failure or both)", s)
}

func (m Mode) wants(phase string) bool {
	switch phase {
	case "completed":
		return m == OnSuccess || m == OnBoth
	case "failed":
		return m == OnFailure || m == OnBoth
	}
	return false
}

// Event describes one finished backup.
type Event struct {
	Cluster collector.ClusterStatus
	Backup  collector.Backup
}

func (e Event) Failed() bool { return e.Backup.Phase == "failed" }

// Title is a short one-line summary.
func (e Event) Title() string {
	verb := "succeeded"
	if e.Failed() {
		verb = "failed"
	}
	return fmt.Sprintf("Klio backup %s: %s/%s", verb, e.Cluster.Namespace, e.Cluster.Name)
}

func (e Event) firstError() string {
	return strings.SplitN(e.Backup.Error, "\n", 2)[0]
}

// Sender delivers an event to one destination.
type Sender interface {
	Name() string
	Send(ctx context.Context, e Event) error
}

// errPermanent marks failures that retrying won't fix (e.g. a rejected token).
var errPermanent = errors.New("permanent")

type Source interface {
	Snapshot(ctx context.Context) (*collector.Snapshot, error)
}

type Notifier struct {
	senders []Sender
	mode    Mode
	src     Source
	// pending[backup] holds the senders that still need to deliver it.
	// Present-but-empty means fully handled.
	pending map[string]map[string]bool
	primed  bool
}

func New(mode Mode, src Source, senders ...Sender) *Notifier {
	return &Notifier{senders: senders, mode: mode, src: src, pending: map[string]map[string]bool{}}
}

// Run polls until ctx is done. Backups that already finished when the process
// starts are recorded without notifying, so restarts don't replay history.
func (n *Notifier) Run(ctx context.Context, every time.Duration) {
	names := make([]string, len(n.senders))
	for i, s := range n.senders {
		names[i] = s.Name()
	}
	log.Printf("notifications enabled (on=%s, via %s)", n.mode, strings.Join(names, ", "))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *Notifier) tick(ctx context.Context) {
	snap, err := n.src.Snapshot(ctx)
	if err != nil {
		log.Printf("notify: snapshot: %v", err)
		return
	}
	current := map[string]bool{}
	for _, c := range snap.Clusters {
		if c.Klio == nil {
			continue
		}
		// oldest first so messages arrive in order
		for i := len(c.Klio.Recent) - 1; i >= 0; i-- {
			b := c.Klio.Recent[i]
			if b.Phase != "completed" && b.Phase != "failed" {
				continue
			}
			k := c.Namespace + "/" + b.Name
			current[k] = true
			todo, known := n.pending[k]
			if !known {
				todo = map[string]bool{}
				if n.primed && n.mode.wants(b.Phase) {
					for _, s := range n.senders {
						todo[s.Name()] = true
					}
				}
				n.pending[k] = todo
			}
			for _, s := range n.senders {
				if !todo[s.Name()] {
					continue
				}
				err := s.Send(ctx, Event{Cluster: c, Backup: b})
				switch {
				case err == nil:
					delete(todo, s.Name())
				case errors.Is(err, errPermanent):
					log.Printf("notify: %s: %s: %v (not retrying)", s.Name(), k, err)
					delete(todo, s.Name())
				default:
					log.Printf("notify: %s: %s: %v (will retry)", s.Name(), k, err)
				}
			}
		}
	}
	for k := range n.pending { // forget backups that have aged out
		if !current[k] {
			delete(n.pending, k)
		}
	}
	n.primed = true
}
