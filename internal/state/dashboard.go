package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	stateBegin    = "<!-- crq:state"
	stateEnd      = "-->"
	crqProjectURL = "https://github.com/kristofferR/codereview-queue"
)

// firstStranded finds an in-flight round that reserved the slot but no longer
// holds it: it cannot receive feedback (no command was posted) and Pump cannot
// advance it (no slot), so it needs naming wherever it sits in the list.
//
// Holding the slot is the whole distinction. Every normal fire passes through
// PhaseReserved WITH a valid slot while the command is being posted, so testing
// the phase alone reported the happy path as stuck — and loudly, since a stranded
// round now outranks every other state.
func firstStranded(st State, inFlight []Round) *Round {
	slot := st.SlotRound()
	for i := range inFlight {
		if inFlight[i].Phase != PhaseReserved {
			continue
		}
		if slot != nil && slot.Repo == inFlight[i].Repo && slot.PR == inFlight[i].PR {
			continue // mid-fire, not stranded
		}
		return &inFlight[i]
	}
	return nil
}

func joinScope(scope []string) string {
	return strings.Join(scope, ",")
}

// dashboardLoc is the zone this issue renders its times in.
//
// The fleet record is asked first, and this host's CRQ_TZ only stands in for it.
// The issue is one shared artifact, so "how times are rendered" is a fleet
// answer by nature: taken from the rendering host's startup environment alone,
// a timezone saved from the settings page was reported as in force while every
// sync went on writing the zone of whichever machine happened to run it.
func dashboardLoc(st State, cfg StoreConfig) *time.Location {
	for _, name := range []string{st.Fleet.Env["CRQ_TZ"], cfg.Timezone} {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.UTC
}

// dashboardInterval is the pacing the queue is actually kept at: the fleet's
// recorded floor when there is one, and the rendering process's own
// configuration otherwise.
//
// Same reasoning as dashboardLoc, and the same failure. This issue is written by
// whichever host happens to save state, so reading its startup environment made
// ReadyAt, the queue's order and the "next at" headline advertise a fire time
// Pump — which resolves the setting from the record — does not follow.
func dashboardInterval(st State, cfg StoreConfig) time.Duration {
	// The typed field first: it is the refinement the generic env layer is
	// merged under, which is the precedence the configuration itself applies.
	for _, text := range []string{st.Fleet.MinInterval, st.Fleet.Env["CRQ_MIN_INTERVAL"]} {
		if d, err := time.ParseDuration(strings.TrimSpace(text)); err == nil && d >= 0 {
			return d
		}
	}
	return cfg.MinInterval
}

func fmtStamp(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "—"
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func minutesUntil(t time.Time, now time.Time) int {
	mins := int(t.Sub(now).Minutes()) + 1
	if mins < 1 {
		mins = 1
	}
	return mins
}

// inFlightRounds returns every round crq has already acted on and is still
// carrying: reserved (slot held, command not yet posted), fired, or reviewing —
// ordered by fire time.
//
// Together with State.Queue (queued + awaiting_retry) and heldRounds this
// PARTITIONS Active() by phase and administrative state, which is what makes
// "every active round is on the dashboard" true by construction. The previous
// single "Feedback wait" row showed reviewing[0] and silently dropped every
// round behind it, along with any reserved round whose fire slot Normalize had
// cleared.
func inFlightRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		switch r.Phase {
		case PhaseReserved, PhaseFired, PhaseReviewing:
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return firedAtOf(out[i]).Before(firedAtOf(out[j]))
	})
	return out
}

type heldRound struct {
	Round
	Hold Hold
}

// heldRounds returns work intentionally excluded from firing. Held work is not
// part of Queue because it is not waiting its turn, but it remains a decision
// somebody made and must stay visible.
//
// Built from the HOLDS, not from the rounds. Holding an untracked PR is the
// ordinary case — Enqueue deliberately refuses to create a round for one — so
// walking rounds showed nothing, and the dashboard said "Idle" immediately
// after a hold succeeded. A round's details are attached when there is one.
func heldRounds(st State) []heldRound {
	out := make([]heldRound, 0, len(st.Holds))
	for key, hold := range st.Holds {
		repo, pr, ok := splitHoldKey(key)
		if !ok {
			continue
		}
		entry := heldRound{Hold: hold, Round: Round{Repo: repo, PR: pr}}
		if r := st.Round(repo, pr); r != nil {
			entry.Round = *r
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].PR < out[j].PR
	})
	return out
}

// splitHoldKey reverses holdKey. A key it cannot read is skipped rather than
// rendered as a row pointing at a pull request that does not exist.
func splitHoldKey(key string) (repo string, pr int, ok bool) {
	repo, num, found := strings.Cut(key, "#")
	if !found || repo == "" {
		return "", 0, false
	}
	pr, err := strconv.Atoi(num)
	if err != nil || pr <= 0 {
		return "", 0, false
	}
	return repo, pr, true
}

// firedAtOf is when a round entered its in-flight life, for ordering the in-flight
// table. A reserved round has no FiredAt — the command is not posted yet — so
// falling straight through to EnqueuedAt sorted a long-queued PR that was reserved
// seconds ago ahead of reviews fired much earlier, contradicting the table's own
// ordering.
// firedTimeOf is what the in-flight table should print in its "fired" column: the
// time THIS attempt posted its command, or nothing when it has not.
//
// A retry deliberately keeps the previous attempt's FiredAt as history and Reserve
// does not clear it, so a reserved round would otherwise display an earlier
// attempt's timestamp as though the current command had gone out — and if the post
// hangs or the process dies, that misleading value is what stays on the dashboard
// beside the stranded reservation.
func firedTimeOf(r Round) *time.Time {
	if r.Phase == PhaseReserved {
		return nil
	}
	return r.FiredAt
}

func firedAtOf(r Round) time.Time {
	// A reserved round is BETWEEN attempts: a retry deliberately preserves the
	// previous FiredAt as history, so reading it here ordered the round by a fire
	// that may be hours old — and printed that stale time in a column describing a
	// command this attempt has not posted yet.
	if r.Phase == PhaseReserved && r.ReservedAt != nil {
		return *r.ReservedAt
	}
	if r.FiredAt != nil {
		return *r.FiredAt
	}
	if r.ReservedAt != nil {
		return *r.ReservedAt
	}
	return r.EnqueuedAt
}

// requestedRounds gathers every round for which crq actually REQUESTED a review
// (active or archived) for the "Recently requested" table, newest first, capped.
//
// CoOnly rounds are excluded: they carry a FiredAt because it anchors their
// evidence floor, but crq never asked the primary reviewer for anything. Listing
// them crowded the table with repos that cannot use the queue at all — on a
// CodeRabbit-Free private repo every push produced a row for a review that was
// never requested, pushing the real history off the end of the cap.
func requestedRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		if r.FiredAt != nil && !r.CoOnly {
			out = append(out, r)
		}
	}
	for _, r := range st.Archive {
		if r.FiredAt != nil && !r.CoOnly {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiredAt.After(*out[j].FiredAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// coBotMarks renders a round's co-reviewer trigger bookkeeping for the
// in-flight table's triggers column: ✓ = trigger posted/adopted, ⏳ = post
// claimed but not yet recorded. Empty when the round tracks no co-bots; the
// caller supplies the surrounding decoration.
func coBotMarks(r Round) string {
	if len(r.CoBots) == 0 {
		return ""
	}
	names := make([]string, 0, len(r.CoBots))
	for name := range r.CoBots {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		c := r.CoBots[name]
		switch {
		case c.CommandID != 0:
			parts = append(parts, name+" ✓")
		case c.ClaimedAt != nil:
			parts = append(parts, name+" ⏳")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// dash renders an empty cell as an em dash so a table row never collapses.
func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// hostName renders a round's writer id ("host=blue pid=4711 run=1a2b3c4d") as
// the machine name the host column has always shown. The round stores the writer
// id because that is what capabilities are keyed by; the pid and run id are
// bookkeeping for LaggingWriters, not something a reader of the table needs.
func hostName(writer string) string {
	rest, ok := strings.CutPrefix(writer, "host=")
	if !ok {
		return writer
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}

// hostCell renders the host column, including the case where there is no host
// to name.
//
// ByHost is written by Reserve, so a round that has never taken the fire slot
// has none — which is every row of the queue table until a round is retried.
// Formatting that as code produced an empty pair of backticks in the rendered
// issue, which reads as a bug in crq rather than as "no host has had this yet".
func hostCell(writer string) string {
	name := hostName(writer)
	if strings.TrimSpace(name) == "" {
		return dash("")
	}
	return "`" + name + "`"
}

// RenderDashboard renders the human-facing dashboard for the current state:
// rounds by phase instead of v2's queue/fired/awaiting maps.
func RenderDashboard(st State, cfg StoreConfig) string {
	loc := dashboardLoc(st, cfg)
	now := time.Now().UTC()
	queue := st.Queue(now, dashboardInterval(st, cfg))
	inFlight := inFlightRounds(st)
	held := heldRounds(st)
	slot := st.SlotRound()
	blocked := st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now)

	var b strings.Builder
	fmt.Fprintf(&b, "# 🐰 crq — Code Review Queue\n\n")

	stranded := firstStranded(st, inFlight)
	switch {
	case stranded != nil:
		// Reported before the transient states: a quota window or another PR's
		// review clears on its own, but a reserved round with no slot behind it
		// cannot be advanced by Pump at all, so hiding it behind them leaves
		// permanently stuck work looking ordinary.
		fmt.Fprintf(&b, "### 🟠 Stranded reservation on %s#%d — no fire slot backs it\n\n", stranded.Repo, stranded.PR)
	case blocked:
		fmt.Fprintf(&b, "### 🔴 Blocked — next review in ~%dm\n\n", minutesUntil(*st.Account.BlockedUntil, now))
	case slot != nil:
		fmt.Fprintf(&b, "### 🟡 Reviewing %s#%d\n\n", slot.Repo, slot.PR)
	case len(inFlight) > 0:
		fmt.Fprintf(&b, "### 🟡 Awaiting feedback for %s#%d\n\n", inFlight[0].Repo, inFlight[0].PR)
	case len(queue) > 0:
		// Nothing ready yet is still queued work, never idle — say when the front
		// of the queue opens instead of leaving the reader to guess.
		if next := queue[0].ReadyAt; !next.IsZero() {
			fmt.Fprintf(&b, "### 🟠 %d queued — next at %s\n\n", len(queue), fmtStamp(&next, loc))
		} else {
			fmt.Fprintf(&b, "### 🟠 %d queued\n\n", len(queue))
		}
	case len(held) > 0:
		fmt.Fprintf(&b, "### ⏸ %d held\n\n", len(held))
	default:
		fmt.Fprintf(&b, "### 🟢 Idle\n\n")
	}

	via := ""
	if st.Account.Source != "" && st.Account.Source != "init" {
		via = fmt.Sprintf("  _(via %s)_", st.Account.Source)
	}
	remaining := "available now"
	if st.Account.Remaining != nil {
		remaining = fmt.Sprintf("%d", *st.Account.Remaining)
	}
	if blocked {
		remaining = "0 — account blocked"
	}

	fmt.Fprintf(&b, "|   |   |\n|---|---|\n")
	fmt.Fprintf(&b, "| **Scope** | `%s` |\n", st.Account.Scope)
	fmt.Fprintf(&b, "| **Reviews remaining** | %s%s |\n", remaining, via)
	if blocked {
		fmt.Fprintf(&b, "| **CodeRabbit quota** | ⚠️ account blocked |\n")
	} else {
		fmt.Fprintf(&b, "| **CodeRabbit quota** | ✅ not currently blocked |\n")
	}
	coReviewers := cfg.CoReviewers
	if cfg.ResolveCoReviewers != nil {
		coReviewers = cfg.ResolveCoReviewers(st.Fleet)
	}
	if coReviewers != "" {
		fmt.Fprintf(&b, "| **Co-reviewers** | %s |\n", coReviewerCell(st, coReviewers))
	}
	fmt.Fprintf(&b, "| **Last review fired** | %s |\n", fmtStamp(st.LastFired, loc))
	if st.Autofix.Unhealthy() {
		fmt.Fprintf(&b, "\n> 🚨 fix sessions are not starting on %s — %d attempts in a row: %s\n",
			dash(st.Autofix.Host), st.Autofix.ConsecutiveFailures, st.Autofix.LastError)
	}
	if st.Warn != "" {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", st.Warn)
	}

	fmt.Fprintf(&b, "\n## 🔬 In flight — %d\n\n", len(inFlight))
	if len(inFlight) == 0 {
		fmt.Fprintf(&b, "_None._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | phase | fired | deadline | triggers | host |\n|---|---|---|---|---|---|---|\n")
		for _, r := range inFlight {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s | %s | %s | %s |\n",
				r.Repo, r.PR, r.Repo, r.PR, r.Head, r.Phase,
				fmtStamp(firedTimeOf(r), loc), fmtStamp(r.WaitDeadline, loc), dash(coBotMarks(r)), hostCell(r.ByHost))
		}
	}

	fmt.Fprintf(&b, "\n## ⏳ Queue — %d waiting\n\n", len(queue))
	if len(queue) == 0 {
		fmt.Fprintf(&b, "_Nothing queued._\n")
	} else {
		fmt.Fprintf(&b, "| # | PR | commit | ready | why | attempts | enqueued | host |\n|--:|---|---|---|---|--:|---|---|\n")
		for i, e := range queue {
			// Absolute stamps only: a relative "in 11m" would re-hash the dashboard
			// on every render, and fmtStamp already honours CRQ_TZ.
			// A zero ReadyAt means "now" only when nothing is holding the round.
			// With a gate whose end is unknowable — a slot held elsewhere, or a
			// round queued behind another — printing "now" contradicts the very
			// column next to it.
			ready := "now"
			switch {
			case !e.ReadyAt.IsZero():
				at := e.ReadyAt
				ready = fmtStamp(&at, loc)
			case e.Why != "":
				ready = "unknown"
			}
			// Only the front has a knowable position. What fires after it depends on
			// when its slot releases — the bot acknowledging, or the in-flight
			// timeout — so any number past the first is a guess. List them; do not
			// rank them.
			// A position is claimed only for a round that can fire now (see Queue):
			// anything else depends on when a pump runs and which windows have
			// opened by then.
			position := "—"
			if i == 0 && e.ReadyAt.IsZero() && e.Why == "" {
				position = strconv.Itoa(1)
			}
			fmt.Fprintf(&b, "| %s | [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s | %d | %s | %s |\n",
				position, e.Repo, e.PR, e.Repo, e.PR, e.Head, ready, dash(e.Why),
				e.Attempts, fmtStamp(&e.EnqueuedAt, loc), hostCell(e.ByHost))
		}
	}

	fmt.Fprintf(&b, "\n## ⏸ Held — %d\n\n", len(held))
	if len(held) == 0 {
		fmt.Fprintf(&b, "_None._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | phase | reason | held by | since |\n|---|---|---|---|---|---|\n")
		for _, e := range held {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s | `%s` | %s |\n",
				e.Repo, e.PR, e.Repo, e.PR, dash(e.Head), dash(string(e.Phase)), cell(dash(e.Hold.Reason)),
				cell(e.Hold.By), fmtStamp(&e.Hold.At, loc))
		}
	}

	requested := requestedRounds(st)
	fmt.Fprintf(&b, "\n## 📨 Recently requested — last %d\n\n", len(requested))
	if len(requested) == 0 {
		fmt.Fprintf(&b, "_None yet._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | requested | host |\n|---|---|---|---|\n")
		for _, r := range requested {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s |\n",
				r.Repo, r.PR, r.Repo, r.PR, r.Head, fmtStamp(r.FiredAt, loc), hostCell(r.ByHost))
		}
	}

	fmt.Fprintf(&b, "\n---\n")
	fmt.Fprintf(&b, "<sub>🤖 Managed by [crq](%s) · rev %d · updated %s · do not edit by hand (machine state is in the hidden block at the top).</sub>\n",
		crqProjectURL, st.Rev, fmtStamp(st.UpdatedAt, loc))
	return b.String()
}

// RenderTitle summarizes the state for the dashboard issue title. The queue
// count is the WHOLE queue, cooling-down rounds included: a state whose only
// work is not yet fire-eligible is queued, never idle.
func RenderTitle(st State, cfg StoreConfig) string {
	now := time.Now().UTC()
	queue := len(st.Queue(now, cfg.MinInterval))
	held := len(heldRounds(st))
	switch {
	case firstStranded(st, inFlightRounds(st)) != nil:
		// Same precedence as the body: permanently stuck work outranks states that
		// clear by themselves.
		return fmt.Sprintf("🐰 crq — stranded #%d · queue %d", firstStranded(st, inFlightRounds(st)).PR, queue)
	case st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		return fmt.Sprintf("🐰 crq — blocked · queue %d", queue)
	case st.SlotRound() != nil:
		return fmt.Sprintf("🐰 crq — reviewing #%d · queue %d", st.SlotRound().PR, queue)
	case len(inFlightRounds(st)) > 0:
		return fmt.Sprintf("🐰 crq — awaiting feedback · queue %d", queue)
	case queue > 0:
		return fmt.Sprintf("🐰 crq — %d queued", queue)
	case held > 0:
		return fmt.Sprintf("🐰 crq — %d held", held)
	default:
		return "🐰 crq — idle"
	}
}

func IssueBody(st State, cfg StoreConfig) (string, error) {
	machine, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\n%s\n%s\n\n%s", stateBegin, machine, stateEnd, RenderDashboard(st, cfg)), nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// cell makes a free-form value safe to put in a Markdown table. A hold reason is
// whatever an operator typed: a pipe ends the column early and a newline ends
// the row, so a reason like "waiting on API | security decision" rewrites the
// table around it.
func cell(v string) string {
	v = strings.ReplaceAll(v, "\r\n", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	return strings.ReplaceAll(v, "|", "\\|")
}

// coReviewerCell renders the co-reviewer row, which is a FLEET DEFAULT rather
// than the answer for any particular repository.
//
// Reviewers became per-repository, so a bare list here claims more than it
// knows: a reader of the issue sees "codex, bugbot, macroscope" and has no way
// to tell that one repository has been set to something else. The default is
// still worth showing — it governs everything nobody has ruled on — so it is
// labelled, and the repositories that differ are named beside it.
func coReviewerCell(st State, fleet string) string {
	overrides := make([]string, 0, len(st.Repos))
	for repo, override := range st.Repos {
		if !override.SetCoBots {
			continue
		}
		overrides = append(overrides, repo)
	}
	if len(overrides) == 0 {
		return fleet + " _(fleet default)_"
	}
	sort.Strings(overrides)
	// Named, not just counted: "3 repositories differ" sends the reader to the
	// CLI to find out which, and the answer is already here.
	const show = 3
	listed := overrides
	suffix := ""
	if len(listed) > show {
		listed, suffix = listed[:show], fmt.Sprintf(" +%d more", len(overrides)-show)
	}
	return fmt.Sprintf("%s _(fleet default; %s override%s)_", fleet,
		strings.Join(listed, ", ")+suffix, plural(len(overrides)))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
