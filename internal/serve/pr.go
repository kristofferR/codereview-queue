package serve

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

// Observer is the expensive half of a PR view: everything that needs a round
// trip to GitHub. It is an interface so this package keeps working without one
// — the cheap state layer still renders, and the findings arrive when they can.
type Observer interface {
	Observe(ctx context.Context, repo string, pr int) (Observation, error)
}

// Observation is what one GitHub read tells us about a pull request.
type Observation struct {
	Head       string            `json:"head"`
	Converged  bool              `json:"converged"`
	Status     string            `json:"status,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	ReviewedBy map[string]bool   `json:"reviewed_by,omitempty"`
	Findings   []dialect.Finding `json:"findings"`
	Dismissed  int               `json:"dismissed,omitempty"`
	CheckedAt  time.Time         `json:"checked_at"`
}

// Coster estimates what one more round on a pull request would cost. Separate
// from Observer because it is a different question with a different failure
// mode: a page can show findings without a price, and a price without findings.
type Coster interface {
	Cost(ctx context.Context, repo string, pr int) (Cost, error)
}

// Cost mirrors crq.RoundCost on the wire. Ranges, per-reviewer reasoning and a
// prices-checked date, because a single confident figure would be the one
// output guaranteed to be wrong.
type Cost struct {
	Head            string         `json:"head"`
	Low             float64        `json:"low"`
	High            float64        `json:"high"`
	Exact           bool           `json:"exact,omitempty"`
	Unpriced        []string       `json:"unpriced,omitempty"`
	Summary         string         `json:"summary"`
	PricesCheckedAt string         `json:"prices_checked_at"`
	PricingNote     string         `json:"pricing_note"`
	Reviewers       []CostReviewer `json:"reviewers"`
	Diff            CostDiff       `json:"diff"`
}

type CostReviewer struct {
	Bot     string  `json:"bot"`
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Exact   bool    `json:"exact,omitempty"`
	Unknown bool    `json:"unknown,omitempty"`
	Basis   string  `json:"basis"`
}

type CostDiff struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// PRView is the two-layer page: state renders instantly, observation fills in.
type PRView struct {
	Repo  string     `json:"repo"`
	PR    int        `json:"pr"`
	Rev   int64      `json:"rev"`
	Round *RoundView `json:"round,omitempty"`
	Hold  *HeldRow   `json:"hold,omitempty"`

	// Observed is nil until a GitHub read succeeds. ObserveError explains why
	// it is still nil, so the page can say so rather than looking empty.
	Observed     *Observation `json:"observed,omitempty"`
	ObserveError string       `json:"observe_error,omitempty"`

	// Title is the pull request's own, so the page says what it is about rather
	// than only which number it is.
	Title string `json:"title,omitempty"`
	// Cost is what the NEXT round would cost. Nil when it could not be worked
	// out; CostError says why rather than leaving a blank where money goes.
	Cost      *Cost  `json:"cost,omitempty"`
	CostError string `json:"cost_error,omitempty"`

	History []HistoryEntry `json:"history"`
}

// RoundView is the live round, straight from state — no network needed.
type RoundView struct {
	Head       string      `json:"head"`
	Phase      string      `json:"phase"`
	Attempts   int         `json:"attempts,omitempty"`
	EnqueuedAt time.Time   `json:"enqueued_at"`
	FiredAt    *time.Time  `json:"fired_at,omitempty"`
	Deadline   *time.Time  `json:"deadline,omitempty"`
	RetryAt    *time.Time  `json:"retry_at,omitempty"`
	Note       string      `json:"note,omitempty"`
	Host       string      `json:"host,omitempty"`
	CoOnly     bool        `json:"co_only,omitempty"`
	Bots       []Bot       `json:"bots"`
	Fixing     *Session    `json:"fixing,omitempty"`
	Dismissed  []Dismissed `json:"dismissed,omitempty"`
	Next       string      `json:"next,omitempty"`
}

type Dismissed struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// HistoryEntry is an earlier head for this PR, newest first.
type HistoryEntry struct {
	Head    string     `json:"head"`
	Outcome string     `json:"outcome"`
	Note    string     `json:"note,omitempty"`
	At      *time.Time `json:"at,omitempty"`
	Current bool       `json:"current,omitempty"`
}

// observeCache keeps one observation per (repo, pr, head, state revision,
// reviewer config). Round state and dismissals shape convergence and findings
// even when the head and reviewer configuration stay unchanged.
type observeCache struct {
	mu      sync.Mutex
	entries map[string]observeEntry
}

type observeEntry struct {
	obs     Observation
	err     string
	fetched time.Time
}

const observeTTL = 60 * time.Second

// maxCacheEntries bounds each cache. The TTL alone only makes an entry
// unusable, it never removes one — and every push, reviewer change and
// allowance change mints a fresh key — so a dashboard left running kept every
// findings payload and every price it had ever served. The bound is generous
// enough that ordinary browsing never reaches it and small enough that reaching
// it costs nothing.
const maxCacheEntries = 512

func (c *observeCache) get(key string) (observeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.fetched) > observeTTL {
		return observeEntry{}, false
	}
	return e, true
}

func (c *observeCache) put(key string, e observeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]observeEntry{}
	}
	c.entries[key] = e
	// Expired entries first: none of them can ever be served again, so dropping
	// them is free. The bound is the backstop for a burst that outruns the TTL.
	pruneCache(c.entries, observeTTL, func(e observeEntry) time.Time { return e.fetched })
}

// pruneCache drops every entry past its TTL, then — if the map is still over
// the bound — the oldest of what is left, so a cache cannot grow without limit
// even while every entry in it is live.
func pruneCache[E any](entries map[string]E, ttl time.Duration, at func(E) time.Time) {
	now := time.Now()
	for key, e := range entries {
		if now.Sub(at(e)) > ttl {
			delete(entries, key)
		}
	}
	if len(entries) <= maxCacheEntries {
		return
	}
	type aged struct {
		key string
		at  time.Time
	}
	all := make([]aged, 0, len(entries))
	for key, e := range entries {
		all = append(all, aged{key, at(e)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(entries)-maxCacheEntries] {
		delete(entries, a.key)
	}
}

// costKey names everything a price depends on. The head alone is not enough:
// the estimate is a sum over the repository's EFFECTIVE reviewers priced
// against the account's remaining allowance, so adding a paid reviewer — or
// exhausting the allowance — at the same commit changes the answer. Keyed on
// the head alone, the page went on serving the old figure for the whole TTL,
// and an SSE reload could not shift it because it asked the same question.
func costKey(repo string, pr int, head string, bots []BotName, remaining *int) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(repo) + "#" + strconv.Itoa(pr) + "@" + head)
	for _, bot := range bots {
		b.WriteString("|" + bot.Login + ":primary=" + strconv.FormatBool(bot.Primary) + ":trigger=" + bot.Trigger)
	}
	b.WriteString("|allowance=")
	if remaining == nil {
		b.WriteString("unknown")
	} else {
		b.WriteString(strconv.Itoa(*remaining))
	}
	return b.String()
}

func observationKey(repo string, pr int, head string, rev int64, bots []BotName) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(repo) + "#" + strconv.Itoa(pr) + "@" + head +
		"|rev=" + strconv.FormatInt(rev, 10))
	for _, bot := range bots {
		b.WriteString("|" + bot.Login)
		if bot.Primary {
			b.WriteString(":primary")
		}
		if bot.Required {
			b.WriteString(":required")
		}
		b.WriteString(":trigger=" + bot.Trigger + ":command=" + bot.Command)
		b.WriteString(":grace=" + strconv.FormatInt(int64(bot.Grace), 10))
	}
	return b.String()
}

// costCache mirrors observeCache. Keyed by costKey: a price for a head that has
// been superseded — or for a reviewer set or allowance that has moved since —
// is a price for the wrong question.
type costCache struct {
	mu      sync.Mutex
	entries map[string]costEntry
}

type costEntry struct {
	cost    *Cost
	err     string
	fetched time.Time
}

// Longer than the observation TTL: the diff of a head does not change, so the
// only thing that can move a price is the account allowance running out.
const costTTL = 5 * time.Minute

func (c *costCache) get(key string) (costEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.fetched) > costTTL {
		return costEntry{}, false
	}
	return e, true
}

func (c *costCache) put(key string, e costEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]costEntry{}
	}
	c.entries[key] = e
	pruneCache(c.entries, costTTL, func(e costEntry) time.Time { return e.fetched })
}

// buildPRView assembles the cheap layer from state. maxAttempts is the same
// per-repository budget the overview reads, so the two renderings of one claim
// cannot disagree about how far along a session is; nil leaves it unsaid.
func buildPRView(st state.State, repo string, pr int, bots []BotName, inflight time.Duration, now time.Time, maxAttempts func(repo string) int) PRView {
	v := PRView{Repo: repo, PR: pr, Rev: st.Rev, History: []HistoryEntry{}}
	key := state.Key(repo, pr)
	v.Title = titleOf(st, repo, pr)

	if r, ok := st.Rounds[key]; ok {
		row := RoundRow{Key: key, Title: r.Title, Repo: r.Repo, PR: r.PR, Head: r.Head, Phase: string(r.Phase),
			FiredAt: r.FiredAt, Bots: botMarks(r, bots)}
		rv := &RoundView{
			Head: r.Head, Phase: string(r.Phase), Attempts: r.Attempts,
			EnqueuedAt: r.EnqueuedAt, FiredAt: r.FiredAt, RetryAt: r.RetryAt,
			Note: r.Note, Host: hostOf(r.ByHost), CoOnly: r.CoOnly, Bots: row.Bots,
		}
		if r.FiredAt != nil && inflight > 0 {
			d := r.FiredAt.Add(inflight)
			rv.Deadline = &d
		}
		if r.WaitDeadline != nil {
			rv.Deadline = r.WaitDeadline
		}
		for id, reason := range r.Dismissed {
			rv.Dismissed = append(rv.Dismissed, Dismissed{ID: id, Reason: reason})
		}
		sort.Slice(rv.Dismissed, func(i, j int) bool { return rv.Dismissed[i].ID < rv.Dismissed[j].ID })
		// Live, for the same reason the overview and the sessions table are: a
		// claim whose watcher died is not a running session, and the three views
		// must not disagree about that.
		if d, ok := st.Dispatches[key]; ok && d.Live(now) {
			// Every field the claim carries, because the card renders them: a
			// page that has the log path and the findings count in hand and
			// shows neither is the overview's session row with holes in it.
			s := Session{Key: key, Repo: repo, PR: pr, Head: r.Head, Host: hostOf(d.Host),
				Model: d.Model, Attempt: d.Attempts, Findings: d.Findings, Log: d.Log, Since: d.At}
			for i := len(st.Archive) - 1; i >= 0; i-- {
				archived := st.Archive[i]
				if strings.EqualFold(archived.Repo, repo) && archived.PR == pr &&
					archived.Dispatch != nil && archived.Dispatch.Token == d.Token {
					s.Head = archived.Head
					break
				}
			}
			if maxAttempts != nil {
				s.MaxAttempts = maxAttempts(repo)
			}
			if !d.Heartbeat.IsZero() {
				hb := d.Heartbeat
				s.Heartbeat = &hb
			}
			rv.Fixing = &s
		}
		row.Bots = rv.Bots
		rv.Next = nextForRound(r, row)
		v.Round = rv
		v.History = append(v.History, HistoryEntry{Head: r.Head, Outcome: string(r.Phase),
			Note: r.Note, At: r.FiredAt, Current: true})
	}

	if h, ok := st.Holds[key]; ok {
		v.Hold = &HeldRow{Key: key, Repo: repo, PR: pr, Reason: h.Reason, By: h.By, At: h.At}
		if v.Round != nil {
			v.Hold.Head = v.Round.Head
		}
	}

	for _, r := range st.Archive {
		if !strings.EqualFold(r.Repo, repo) || r.PR != pr {
			continue
		}
		v.History = append(v.History, HistoryEntry{Head: r.Head, Outcome: string(r.Phase),
			Note: r.Note, At: r.FiredAt})
	}
	sort.SliceStable(v.History, func(i, j int) bool {
		a, b := v.History[i].At, v.History[j].At
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		return a.After(*b)
	})
	return v
}

// handlePR serves one pull request. The state layer always answers; the
// observation is attempted but never allowed to fail the request — a page that
// shows the round and says "could not reach GitHub" beats a 500.
func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	if !s.allowDashboardRead(w, r) {
		return
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	pr, err := strconv.Atoi(r.PathValue("pr"))
	if owner == "" || name == "" || err != nil || pr <= 0 {
		http.NotFound(w, r)
		return
	}
	repo := owner + "/" + name

	s.mu.RLock()
	if !s.loaded {
		err := s.loadErr
		s.mu.RUnlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": firstLoadError(err)})
		return
	}
	st := s.lastState
	s.mu.RUnlock()

	bots := s.botsFor(&st)(repo)
	view := buildPRView(st, repo, pr, bots, s.pacing(st).Inflight, s.opts.Now(), s.maxAttempts(st))
	stateOnly := r.URL.Query().Get("state_only") == "1"

	if !stateOnly && s.observer != nil {
		// The round's head is what invalidates a cached entry, so an untracked
		// pull request has nothing to invalidate one WITH: its key never moves,
		// and a page reloaded after a push kept being served the previous head's
		// findings for the whole TTL. No proof of the head, no head-scoped reuse.
		head := ""
		if view.Round != nil {
			head = view.Round.Head
		}
		key := observationKey(repo, pr, head, st.Rev, bots)
		if r.URL.Query().Get("refresh") == "1" {
			s.observations.put(key, observeEntry{})
		}
		if e, ok := s.observations.get(key); ok && head != "" &&
			(e.err != "" || (!e.obs.CheckedAt.IsZero() && e.obs.Head == head)) {
			if e.err != "" {
				view.ObserveError = e.err
			} else {
				obs := e.obs
				view.Observed = &obs
			}
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			obs, err := s.observer.Observe(ctx, repo, pr)
			entry := observeEntry{obs: obs, fetched: time.Now()}
			if err != nil {
				entry.err = err.Error()
				view.ObserveError = entry.err
			} else if head != "" && obs.Head != "" && !sameHead(head, obs.Head) {
				entry.err = "pull request moved while this view was loading; refresh for current state"
				entry.fetched = time.Time{} // never cache a state/observation race
				view.ObserveError = entry.err
			} else {
				view.Observed = &obs
			}
			s.observations.put(key, entry)
		}
	} else if !stateOnly {
		view.ObserveError = "this server was started without GitHub access"
	}

	// Priced on the same trip and cached the same way: it costs one more pull
	// read, which is why the overview does not price every queue row.
	if !stateOnly && s.opts.Coster != nil {
		// The head GitHub just reported, when the observation above got one.
		// The five-minute TTL rests entirely on "the diff of a head does not
		// change", and keyed on a round's head that is empty or superseded that
		// reasoning does not hold: the page went on quoting the previous diff's
		// price for five minutes after a push.
		head := ""
		if view.Observed != nil {
			head = view.Observed.Head
		}
		key := costKey(repo, pr, head, bots, st.Account.Remaining)
		if r.URL.Query().Get("refresh") == "1" {
			s.costs.put(key, costEntry{})
		}
		if e, ok := s.costs.get(key); ok && head != "" && (e.err != "" || e.cost != nil) {
			view.Cost, view.CostError = e.cost, e.err
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			cost, err := s.opts.Coster.Cost(ctx, repo, pr)
			entry := costEntry{fetched: time.Now()}
			if err != nil {
				entry.err = err.Error()
				view.CostError = entry.err
			} else if head != "" && !sameHead(cost.Head, head) {
				entry.err = "pull request moved while pricing; refresh to price the observed head"
				view.CostError = entry.err
			} else {
				entry.cost = &cost
				view.Cost = &cost
			}
			cacheHead := head
			if entry.cost != nil {
				cacheHead = entry.cost.Head
			}
			if cacheHead != "" {
				s.costs.put(costKey(repo, pr, cacheHead, bots, st.Account.Remaining), entry)
			}
		}
	}

	writeJSON(w, http.StatusOK, view)
}

func sameHead(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}
