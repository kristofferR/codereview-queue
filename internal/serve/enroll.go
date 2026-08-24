package serve

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

// Enrollment is one repository's answer to "does crq review this project", as
// resolved by internal/crq. serve does not decide it: the precedence between a
// shared record and a host's env file is a queue rule, and two answers to it
// would be one too many.
type Enrollment struct {
	Source       string     `json:"source"` // state|env|excluded|scope|off
	Enabled      bool       `json:"enabled"`
	EnvConflict  bool       `json:"env_conflict,omitempty"`
	ClearEnables bool       `json:"clear_enables,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	By           string     `json:"by,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

// EnrollFor resolves one repository against a loaded state.
type EnrollFor func(st state.State, repo string) Enrollment

// Candidate is one repository the picker offers. Everything here comes from the
// repository listing itself; whether crq already knows it is filled in locally.
type Candidate struct {
	Repo     string `json:"repo"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	// Issues is GitHub's open_issues_count, which counts issues AND pull
	// requests. Labelled as what it is rather than as an open-PR count, which
	// would cost one search per repository to know.
	Issues   int        `json:"issues"`
	PushedAt *time.Time `json:"pushed_at,omitempty"`
	Language string     `json:"language,omitempty"`
	// Enrollment is nil for a repository crq has no answer about yet.
	Enrollment *Enrollment `json:"enrollment,omitempty"`
}

// EnrollImpact is what enrolling a repository would do, before it is done.
type EnrollImpact struct {
	Rev             int64          `json:"rev"`
	Repo            string         `json:"repo"`
	Open            int            `json:"open"`
	Eligible        int            `json:"eligible"`
	Skipped         map[string]int `json:"skipped,omitempty"`
	Metered         int            `json:"metered"`
	Low             float64        `json:"low"`
	High            float64        `json:"high"`
	Unpriced        int            `json:"unpriced,omitempty"`
	Unexamined      int            `json:"unexamined,omitempty"`
	Summary         string         `json:"summary"`
	PricesCheckedAt string         `json:"prices_checked_at"`
}

// Previewer answers what enrolling a repository would cost. Separate from the
// Actor because it reads GitHub per open pull request: it is what a dialog asks
// when it opens, not something a list can carry.
type Previewer interface {
	PreviewEnroll(ctx context.Context, repo string) (EnrollImpact, error)
}

// Listing is one scope walk: the repositories it found, and the owners it could
// not finish. Truncation travels with the rows because a picker that shows a
// bounded list as if it were the whole of one is how a repository becomes
// impossible to add without anyone being told why.
type Listing struct {
	Repos []Candidate `json:"repos"`
	// Truncated names the owners whose listing hit the bound.
	Truncated []string `json:"truncated,omitempty"`
}

// Discoverer lists the repositories in the configured scope. It is a separate
// interface from Observer because it is the one call in the dashboard that is
// expensive enough to cache aggressively and to never make on a page load.
type Discoverer interface {
	Discover(ctx context.Context) (Listing, error)
}

// discoverCache holds the scope listing. Repositories appear when someone
// creates one, which is rare, and the picker has a Refresh button — so a long
// TTL costs a stale row and saves a multi-page REST walk on every open.
type discoverCache struct {
	mu      sync.Mutex
	at      time.Time
	listing Listing
	err     error
	flight  chan struct{}
}

const (
	discoverTTL     = 10 * time.Minute
	discoverTimeout = 60 * time.Second
)

func (c *discoverCache) get(ctx context.Context, d Discoverer, now time.Time, force bool) (Listing, error) {
	c.mu.Lock()
	if !force && c.at.After(now.Add(-discoverTTL)) && c.err == nil {
		listing := c.listing
		c.mu.Unlock()
		return listing, nil
	}
	if c.flight != nil {
		flight := c.flight
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return Listing{}, ctx.Err()
		case <-flight:
			c.mu.Lock()
			listing, err := c.listing, c.err
			c.mu.Unlock()
			return listing, err
		}
	}
	c.flight = make(chan struct{})
	flight := c.flight
	c.mu.Unlock()

	go c.fill(d, now, flight)
	select {
	case <-ctx.Done():
		return Listing{}, ctx.Err()
	case <-flight:
		c.mu.Lock()
		listing, err := c.listing, c.err
		c.mu.Unlock()
		return listing, err
	}
}

func (c *discoverCache) fill(d Discoverer, now time.Time, flight chan struct{}) {
	// The fetch belongs to every caller sharing this flight, not to whichever
	// browser happened to start it. A closed tab must not cancel discovery for
	// the other waiters.
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()
	listing, err := d.Discover(ctx)
	c.mu.Lock()
	c.at, c.listing, c.err = now, listing, err
	c.flight = nil
	close(flight)
	c.mu.Unlock()
}

// handleDiscover answers the repository picker. It never blocks the rest of the
// dashboard: nothing else calls it, and a failure here is reported as itself
// rather than as a broken page.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.allowDashboardRead(w, r) {
		return
	}
	if s.opts.Discoverer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this dashboard has no repository discovery configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()

	listing, err := s.discovered.get(ctx, s.opts.Discoverer, s.opts.Now(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	rows := listing.Repos

	// Annotate with what crq already knows, from the snapshot the server holds
	// — the picker's whole job is separating "not added yet" from "added, and
	// here is where that answer came from".
	s.mu.RLock()
	st := s.lastState
	loaded := s.loaded
	s.mu.RUnlock()
	out := make([]Candidate, 0, len(rows))
	for _, c := range rows {
		if loaded && s.opts.EnrollFor != nil {
			e := s.opts.EnrollFor(st, c.Repo)
			c.Enrollment = &e
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Recently pushed first: the repository somebody wants to add is
		// overwhelmingly the one they were just working in.
		switch {
		case a.PushedAt != nil && b.PushedAt != nil && !a.PushedAt.Equal(*b.PushedAt):
			return a.PushedAt.After(*b.PushedAt)
		case a.PushedAt != nil && b.PushedAt == nil:
			return true
		case a.PushedAt == nil && b.PushedAt != nil:
			return false
		}
		return strings.ToLower(a.Repo) < strings.ToLower(b.Repo)
	})
	writeJSON(w, http.StatusOK, map[string]any{"repos": out, "truncated": listing.Truncated})
}

// handleEnrollPreview answers the add-repo dialog. It is the one click in the
// product that can spend real money — a repository with a dozen open pull
// requests becomes a dozen metered reviews on the next pass — so the dialog
// asks before offering it, in the terms the bill arrives in.
func (s *Server) handleEnrollPreview(w http.ResponseWriter, r *http.Request) {
	if !s.allowDashboardRead(w, r) {
		return
	}
	if s.opts.Previewer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this dashboard cannot price an enrollment",
		})
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" || !strings.Contains(repo, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/name"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	impact, err := s.opts.Previewer.PreviewEnroll(ctx, repo)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

// allowDashboardRead protects GETs whose implementation spends authenticated
// GitHub quota. They are reads to the browser, but not side-effect free for the
// fleet: a hostile page could otherwise force refreshes without reading the
// response and starve every queue user.
func (s *Server) allowDashboardRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-CRQ-Dashboard") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing dashboard header"})
		return false
	}
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return false
	}
	return true
}
