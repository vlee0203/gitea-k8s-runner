// Package audit contains a leaked registration token: it periodically lists the
// runners in this controller's scope and deletes any it did not register itself.
//
// Gitea's runner registration token is long-lived and reusable, so a leaked one
// lets someone register a runner that would receive this scope's jobs (and with
// them the repos' CI secrets). Gitea has no JIT/ephemeral registration, so the
// containment is to make such a runner short-lived: a foreign runner that merely
// bootstrapped its own identity survives at most one audit interval.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Auditor deletes runners in scope that this controller did not register.
type Auditor struct {
	baseURL  string // e.g. https://gitea.example.com
	path     string // e.g. /api/v1/user/actions/runners
	token    string // Gitea API token with permission to manage runners in that scope
	known    func(context.Context) (map[string]bool, error)
	interval time.Duration
	client   *http.Client
}

// New returns an Auditor. baseURL is the Gitea instance, path the runners API
// path for the scope to police (e.g. "/api/v1/user/actions/runners"), known
// reports the uuids this controller has registered.
func New(baseURL, path, token string, known func(context.Context) (map[string]bool, error), interval time.Duration) *Auditor {
	return &Auditor{
		baseURL:  strings.TrimRight(baseURL, "/"),
		path:     path,
		token:    token,
		known:    known,
		interval: interval,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Run audits on every interval until ctx is cancelled.
func (a *Auditor) Run(ctx context.Context) {
	log.Printf("runner audit enabled: %s every %s", a.path, a.interval)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := a.Once(ctx); err != nil {
				log.Printf("runner audit: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

type runner struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

// Once performs a single audit pass.
func (a *Auditor) Once(ctx context.Context) error {
	known, err := a.known(ctx)
	if err != nil {
		return fmt.Errorf("read known runners: %w", err)
	}

	var list struct {
		Runners []runner `json:"runners"`
	}
	if err := a.do(ctx, http.MethodGet, a.path, &list); err != nil {
		return fmt.Errorf("list runners: %w", err)
	}

	mine, foreign := 0, 0
	for i := range list.Runners {
		r := list.Runners[i]
		// Identify our own runners by id: Gitea assigns it, the runner list API
		// exposes it, and it is unique — matching by name would spare a stale
		// record left behind by an earlier registration under the same name.
		if known[strconv.FormatInt(r.ID, 10)] {
			mine++
			continue
		}
		foreign++
		log.Printf("runner audit: WARNING deleting runner %q (id %d) — not registered by this controller; a registration token has probably leaked",
			r.Name, r.ID)
		if err := a.do(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", a.path, r.ID), nil); err != nil {
			log.Printf("runner audit: delete runner %d: %v", r.ID, err)
		}
	}
	log.Printf("runner audit: %s — %d runner(s): %d ours, %d foreign", a.path, len(list.Runners), mine, foreign)
	return nil
}

// DeleteRunner removes one runner record by id, used when rotating an identity
// so the credential it replaces stops working.
func (a *Auditor) DeleteRunner(ctx context.Context, id int64) error {
	return a.do(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", a.path, id), nil)
}

func (a *Auditor) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+a.token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
