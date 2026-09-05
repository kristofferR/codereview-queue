// Package serve renders the live web dashboard: an HTTP surface over the same
// state the Markdown issue dashboard is built from, plus the embedded SPA.
//
// It reads state and never decides anything. Every fire/hold/cancel rule stays
// in engine and crq — this package's whole job is to reduce a State into the
// shape a browser can paint, and to push a fresh reduction when the state ref
// moves. Anything that needs a GitHub round-trip (findings, convergence) is not
// here; it belongs on the per-PR endpoint that fetches on demand.
package serve

import (
	"sort"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

// Overview is everything the main screen paints, already reduced. It is a view
// model on purpose: the browser should not have to re-implement Queue()'s
// epistemics or the dashboard's headline precedence to draw a table.
type Overview struct {
	Now      time.Time  `json:"now"`
	Rev      int64      `json:"rev"`
	WroteAt  *time.Time `json:"wrote_at,omitempty"`
	Headline Headline   `json:"headline"`
	Quota    Quota      `json:"quota"`
	Slot     Slot       `json:"slot"`
	Leader   *Leader    `json:"leader,omitempty"`
	Counts   Counts     `json:"counts"`

	Attention []Attention `json:"attention"`
	InFlight  []RoundRow  `json:"in_flight"`
	Queue     []QueueRow  `json:"queue"`
	Held      []HeldRow   `json:"held"`
	Autofix   AutofixView `json:"autofix"`
	Finished  []DoneRow   `json:"finished"`

	// Stale mirrors Snapshot.Stale, so a caller reading only the overview can
	// still tell a live queue from the last one that loaded. Set by the handler,
	// not by BuildOverview: it describes the READ, not the state.
	Stale *Staleness `json:"stale,omitempty"`
}

// Headline is the one-line status, following the same precedence ladder the
// Markdown dashboard uses so the two can never disagree.
type Headline struct {
	Kind    string `json:"kind"` // stranded|blocked|reviewing|awaiting|queued|held|idle
	Text    string `json:"text"`
	Detail  string `json:"detail,omitempty"`
	Subject string `json:"subject,omitempty"` // repo#pr when the headline names one
}

type Quota struct {
	Scope string `json:"scope,omitempty"`
	// Remaining is a pointer because "unknown" and "zero left" are different
	// answers and must not render alike.
	Remaining    *int       `json:"remaining,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	Source       string     `json:"source,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	LastFired    *time.Time `json:"last_fired,omitempty"`
	// FairUse is the rolling-week count against the vendor's weekly throttle.
	// A different scarcity from the hourly block above: this one does not stop
	// reviews, it slows every one of them to a crawl, and crq only ever saw it
	// arrive. See state/firelog.go.
	FairUse state.WeeklyUsage `json:"fair_use"`
}

type Slot struct {
	Held      bool       `json:"held"`
	Key       string     `json:"key,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	HoldUntil *time.Time `json:"hold_until,omitempty"`
}

type Leader struct {
	Owner     string    `json:"owner"`
	Host      string    `json:"host"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired"`
}

type Counts struct {
	InFlight int `json:"in_flight"`
	Queued   int `json:"queued"`
	Held     int `json:"held"`
	Fixing   int `json:"fixing"`
}

// Attention is a problem that will not resolve itself. Ranked, most urgent
// first; each names its subject so the UI never has to guess a title.
// Attention is one thing worth acting on. Link points at the page that can act
// on it: an item that says a host is broken and leaves you to find the host
// page is a notification, not a control.
type Attention struct {
	Kind     string `json:"kind"` // stranded|host|leader|lagging|state
	Level    string `json:"level"`
	Subject  string `json:"subject,omitempty"`
	Text     string `json:"text"`
	Detail   string `json:"detail,omitempty"`
	Link     string `json:"link,omitempty"`
	LinkText string `json:"link_text,omitempty"`
}

// Bot is one reviewer's state on a round: commanded, claimed, or neither.
type Bot struct {
	Login    string     `json:"login"`
	Name     string     `json:"name"`
	Mark     string     `json:"mark"` // commanded|claimed|pending
	At       *time.Time `json:"at,omitempty"`
	Primary  bool       `json:"primary,omitempty"`
	Required bool       `json:"required,omitempty"`
}

type RoundRow struct {
	Key      string     `json:"key"`
	Title    string     `json:"title,omitempty"`
	Repo     string     `json:"repo"`
	PR       int        `json:"pr"`
	Head     string     `json:"head"`
	Phase    string     `json:"phase"`
	FiredAt  *time.Time `json:"fired_at,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	Bots     []Bot      `json:"bots"`
	Host     string     `json:"host,omitempty"`
	Note     string     `json:"note,omitempty"`
	Next     string     `json:"next,omitempty"` // the plain-language "what happens next"
	Fixing   bool       `json:"fixing,omitempty"`
}

type QueueRow struct {
	Key      string     `json:"key"`
	Title    string     `json:"title,omitempty"`
	Repo     string     `json:"repo"`
	PR       int        `json:"pr"`
	Head     string     `json:"head"`
	Position int        `json:"position,omitempty"` // only the front gets one
	ReadyAt  *time.Time `json:"ready_at,omitempty"` // only the front gets a time
	Why      string     `json:"why,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
	Host     string     `json:"host,omitempty"`
	CoOnly   bool       `json:"co_only,omitempty"`
	Next     string     `json:"next,omitempty"`
}

type HeldRow struct {
	Key    string    `json:"key"`
	Title  string    `json:"title,omitempty"`
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Head   string    `json:"head,omitempty"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
}

type AutofixView struct {
	Sessions []Session `json:"sessions"`
	Hosts    []Host    `json:"hosts"`
}

type Session struct {
	Key     string `json:"key"`
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	Head    string `json:"head,omitempty"`
	Host    string `json:"host,omitempty"`
	Model   string `json:"model,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// MaxAttempts is the budget this repository allows, so "attempt 2" can be
	// read as nearly-done or barely-started.
	MaxAttempts int        `json:"max_attempts,omitempty"`
	Findings    int        `json:"findings,omitempty"`
	Log         string     `json:"log,omitempty"`
	Since       time.Time  `json:"since"`
	Heartbeat   *time.Time `json:"heartbeat,omitempty"`
}

// Host health is deliberately three-valued: a host that has reported nothing is
// unknown, not healthy, and calling it healthy is how a dead watcher goes
// unnoticed.
type Host struct {
	Name        string     `json:"name"`
	Health      string     `json:"health"` // healthy|unhealthy|unknown
	Failures    int        `json:"failures,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
}

type DoneRow struct {
	Key     string     `json:"key"`
	Title   string     `json:"title,omitempty"`
	Repo    string     `json:"repo"`
	PR      int        `json:"pr"`
	Head    string     `json:"head"`
	Outcome string     `json:"outcome"`
	Note    string     `json:"note,omitempty"`
	At      *time.Time `json:"at,omitempty"`
}

// BuildOverview reduces a loaded State. minInterval comes from config because
// Queue() folds the pacing gate in, and reviewers names the bots so the UI can
// label marks without knowing any bot's identity itself.
func BuildOverview(st state.State, now time.Time, minInterval, inflight time.Duration, botsFor BotsFor, weeklyLimit int, maxAttempts func(repo string) int) Overview {
	ov := Overview{
		Now:     now,
		Rev:     st.Rev,
		WroteAt: st.UpdatedAt,
		Quota: Quota{
			Scope:        st.Account.Scope,
			Remaining:    st.Account.Remaining,
			Source:       st.Account.Source,
			BlockedUntil: st.Account.BlockedUntil,
			CheckedAt:    st.Account.CheckedAt,
			LastFired:    st.LastFired,
			FairUse:      st.FairUse(now, weeklyLimit),
		},
		Attention: []Attention{},
		InFlight:  []RoundRow{},
		Queue:     []QueueRow{},
		Held:      []HeldRow{},
		Finished:  []DoneRow{},
		Autofix:   AutofixView{Sessions: []Session{}, Hosts: []Host{}},
	}

	if slot := st.FireSlot; slot != nil {
		ov.Slot = Slot{Held: st.SlotHeld(now), Key: slot.Key, Since: &slot.Since, HoldUntil: slot.HoldUntil}
	} else {
		ov.Slot = Slot{Held: st.SlotHeld(now)}
	}
	if l := st.Leader; l != nil {
		ov.Leader = &Leader{Owner: l.Owner, Host: hostOf(l.Owner), ExpiresAt: l.ExpiresAt,
			Expired: !l.ExpiresAt.After(now)}
	}

	ov.InFlight = inFlight(st, now, inflight, botsFor)
	ov.Queue = queueRows(st, now, minInterval, botsFor)
	ov.Held = heldRows(st)
	ov.Autofix = autofixView(st, now, maxAttempts)
	ov.Finished = finishedRows(st)
	ov.Counts = Counts{len(ov.InFlight), len(ov.Queue), len(ov.Held), len(ov.Autofix.Sessions)}
	ov.Attention = attention(st, now, ov)
	ov.Headline = headline(st, now, ov)
	return ov
}

// BotName carries a reviewer's identity from config so this package never
// hardcodes one. Primary is the metered reviewer.
type BotName struct {
	Login   string
	Name    string
	Primary bool
	// Required says this bot gates convergence here. Runs is implied by
	// membership: a bot that does not review a repository is simply absent from
	// its list, because showing it as "off" on every row of every repository is
	// noise about a decision already made.
	Required bool
	Command  string
	Trigger  string
	Grace    Dur
}

// BotsFor answers "which reviewers actually run on this repository". The
// resolution itself lives in internal/crq (Config.ForRepo), and is passed in
// rather than reimplemented, so the dashboard cannot grow a second answer that
// disagrees with the one the queue decides from.
type BotsFor func(repo string) []BotName

func inFlight(st state.State, now time.Time, inflight time.Duration, botsFor BotsFor) []RoundRow {
	out := []RoundRow{}
	for key, r := range st.Rounds {
		switch r.Phase {
		case state.PhaseReserved, state.PhaseFired, state.PhaseReviewing:
		default:
			continue
		}
		row := RoundRow{
			Key: key, Title: r.Title, Repo: r.Repo, PR: r.PR, Head: r.Head, Phase: string(r.Phase),
			FiredAt: r.FiredAt, Host: hostOf(r.ByHost), Note: r.Note,
			Bots: botMarks(r, botsFor(r.Repo)),
		}
		if r.FiredAt != nil && inflight > 0 {
			d := r.FiredAt.Add(inflight)
			row.Deadline = &d
		}
		if r.WaitDeadline != nil {
			row.Deadline = r.WaitDeadline
		}
		// Live, not merely present: a claim a dead watcher never released stays
		// in Dispatches, and scheduling already treats it as free. Marking the
		// row would report a fixer that stopped existing.
		if d, fixing := st.Dispatches[key]; fixing && d.Live(now) {
			row.Fixing = true
		}
		row.Next = nextForRound(r, row)
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].FiredAt, out[j].FiredAt
		switch {
		case a == nil && b != nil:
			return false
		case a != nil && b == nil:
			return true
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		default:
			return out[i].Key < out[j].Key
		}
	})
	return out
}

// botMarks describes each reviewer's trigger bookkeeping for one round. bots is
// already the set that RUNS on this repository, so the empty mark means "not
// asked for this head yet", never "disabled" — a bot a repository turned off is
// absent from the row entirely rather than shown greyed out on every one.
func botMarks(r state.Round, bots []BotName) []Bot {
	out := make([]Bot, 0, len(bots))
	for _, b := range bots {
		mark := Bot{Login: b.Login, Name: b.Name, Mark: "pending", Primary: b.Primary, Required: b.Required}
		if b.Primary {
			// The primary is commanded by the fire itself, not by a co-bot entry.
			if !r.CoOnly && (r.CommandID != 0 || r.FiredAt != nil) {
				mark.Mark, mark.At = "commanded", r.FiredAt
			}
			out = append(out, mark)
			continue
		}
		co, ok := r.CoBots[dialect.NormalizeBotName(b.Login)]
		switch {
		case !ok:
		case co.CommandID != 0:
			mark.Mark = "commanded"
			if co.CommandedAt != nil {
				mark.At = co.CommandedAt
			}
		case co.ClaimedAt != nil:
			mark.Mark, mark.At = "claimed", co.ClaimedAt
		}
		out = append(out, mark)
	}
	return out
}

func queueRows(st state.State, now time.Time, minInterval time.Duration, botsFor BotsFor) []QueueRow {
	entries := st.Queue(now, minInterval)
	hasPrimary := func(repo string) bool {
		for _, bot := range botsFor(repo) {
			if bot.Primary {
				return true
			}
		}
		return false
	}
	for i := range entries {
		if !hasPrimary(entries[i].Repo) &&
			(entries[i].Why == state.WaitAccountBlocked ||
				entries[i].Why == state.WaitPacing) {
			entries[i].Why = ""
			entries[i].ReadyAt = time.Time{}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		iReady := entries[i].Why == "" && entries[i].ReadyAt.IsZero()
		jReady := entries[j].Why == "" && entries[j].ReadyAt.IsZero()
		if iReady != jReady {
			return iReady
		}
		return entries[i].Seq < entries[j].Seq
	})
	out := make([]QueueRow, 0, len(entries))
	for i, e := range entries {
		row := QueueRow{
			Key: state.Key(e.Repo, e.PR), Repo: e.Repo, PR: e.PR, Head: e.Head,
			Title: titleOf(st, e.Repo, e.PR),
			Why:   e.Why, Attempts: e.Attempts, Host: hostOf(e.ByHost), CoOnly: e.CoOnly,
		}
		// Only a round that is genuinely eligible now gets a number and a time —
		// anything behind it starts when the front finishes, which is unknowable.
		if i == 0 && e.Why == "" {
			row.Position = 1
		}
		if !e.ReadyAt.IsZero() {
			at := e.ReadyAt
			row.ReadyAt = &at
		}
		row.Next = nextForQueued(row)
		out = append(out, row)
	}
	return out
}

func heldRows(st state.State) []HeldRow {
	out := make([]HeldRow, 0, len(st.Holds))
	for key, h := range st.Holds {
		repo, pr := splitKey(key)
		row := HeldRow{Key: key, Repo: repo, PR: pr, Reason: h.Reason, By: h.By, At: h.At}
		if r, ok := st.Rounds[key]; ok {
			row.Head, row.Title = r.Head, r.Title
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

func autofixView(st state.State, now time.Time, maxAttempts func(repo string) int) AutofixView {
	v := AutofixView{Sessions: []Session{}, Hosts: []Host{}}
	for key, d := range st.Dispatches {
		// A claim past its TTL is a watcher that died without releasing it.
		// Scheduling already treats it as free; rendering it as a running session
		// left a dead "Fixing" on the page until something happened to replace the
		// entry, which on a quiet repository is never.
		if !d.Live(now) {
			continue
		}
		repo, pr := splitKey(key)
		s := Session{Key: key, Repo: repo, PR: pr, Host: hostOf(d.Host), Model: d.Model, Attempt: d.Attempts,
			Findings: d.Findings, Log: d.Log, Since: d.At}
		if maxAttempts != nil {
			s.MaxAttempts = maxAttempts(repo)
		}
		if r, ok := st.Rounds[key]; ok {
			s.Head = r.Head
		}
		// A session can push and supersede its round while its top-level claim
		// intentionally stays live to finish thread cleanup. The archived claim
		// still names the checkout/head that session owns.
		for i := len(st.Archive) - 1; i >= 0; i-- {
			r := st.Archive[i]
			if state.Key(r.Repo, r.PR) == key && r.Dispatch != nil && r.Dispatch.Token == d.Token {
				s.Head = r.Head
				break
			}
		}
		if !d.Heartbeat.IsZero() {
			hb := d.Heartbeat
			s.Heartbeat = &hb
		}
		v.Sessions = append(v.Sessions, s)
	}
	sort.Slice(v.Sessions, func(i, j int) bool { return v.Sessions[i].Since.Before(v.Sessions[j].Since) })

	for name, h := range st.AutofixByHost {
		health := h
		host := Host{Name: name, Failures: h.ConsecutiveFailures, LastError: h.LastError,
			LastFailure: h.LastFailureAt, LastSuccess: h.LastSuccessAt}
		switch {
		case health.Unhealthy():
			host.Health = "unhealthy"
		case h.LastSuccessAt == nil && h.ConsecutiveFailures == 0:
			host.Health = "unknown"
		default:
			host.Health = "healthy"
		}
		v.Hosts = append(v.Hosts, host)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Name < v.Hosts[j].Name })
	return v
}

func finishedRows(st state.State) []DoneRow {
	out := []DoneRow{}
	add := func(r state.Round) {
		outcome := string(r.Phase)
		if r.Merged() {
			outcome = state.NoteMerged
		}
		row := DoneRow{Key: state.Key(r.Repo, r.PR), Title: r.Title, Repo: r.Repo, PR: r.PR,
			Head: r.Head, Outcome: outcome, Note: r.Note}
		if r.FiredAt != nil {
			row.At = r.FiredAt
		}
		out = append(out, row)
	}
	for _, r := range st.Rounds {
		if r.Phase == state.PhaseCompleted || r.Phase == state.PhaseExpired {
			add(r)
		}
	}
	for _, r := range st.Archive {
		add(r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].At, out[j].At
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		return a.After(*b)
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// titleOf is the pull request's title from its round, when one is recorded.
// Queue entries come from State.Queue, which reduces a round to its scheduling
// facts; the title lives on the round itself.
func titleOf(st state.State, repo string, pr int) string {
	if r, ok := st.Rounds[state.Key(repo, pr)]; ok {
		return r.Title
	}
	return ""
}
