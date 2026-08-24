package serve

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

// splitKey undoes state.Key. A malformed key keeps its whole text as the repo
// rather than being dropped: a row with a strange name is debuggable, a missing
// row is not.
func splitKey(key string) (string, int) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return key, 0
	}
	pr, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return key, 0
	}
	return key[:i], pr
}

// hostOf reduces "host=mbp pid=42 run=abc" to "mbp". The pid and run id matter
// when reading a state dump; on screen they are noise.
func hostOf(by string) string {
	return state.WriterHost(by)
}

// nextForRound says what happens next in words. The phase vocabulary stays
// visible in the UI, but nobody should need it to understand the row.
func nextForRound(r state.Round, row RoundRow) string {
	var waiting []string
	for _, b := range row.Bots {
		switch b.Mark {
		case "commanded":
			if b.Primary {
				waiting = append(waiting, b.Name)
			}
		case "claimed":
			waiting = append(waiting, b.Name)
		}
	}
	switch r.Phase {
	case state.PhaseReserved:
		return "Reserved the fire slot; the review command goes out next."
	case state.PhaseFired:
		return "Review requested; waiting for the bot to acknowledge it."
	case state.PhaseReviewing:
		if len(waiting) > 0 {
			return fmt.Sprintf("Acknowledged and the slot is released; waiting on %s.",
				joinWords(waiting))
		}
		return "Acknowledged and the slot is released; waiting for the review to land."
	}
	return ""
}

// nextForQueued turns Queue()'s why-reason into a sentence. It never invents a
// start time for a round behind the front — that is exactly what Queue refuses
// to claim, and the UI must not claim it either.
func nextForQueued(row QueueRow) string {
	quota := ""
	if row.CoOnly {
		quota = " Co-reviewers only, so it spends no quota."
	}
	switch row.Why {
	case "":
		return "Ready — fires on the daemon's next pass." + quota
	case state.WaitAccountBlocked:
		return "Waits for the CodeRabbit quota window, then its turn in the queue."
	case state.WaitSlotBusy:
		return "Another PR holds the fire slot; this one moves when that finishes."
	case state.WaitPacing:
		return "Pacing between fires — the queue spaces reviews out." + quota
	case state.WaitCoolingDown:
		return fmt.Sprintf("Cooling down after %s; it retries when the window opens.",
			plural(row.Attempts, "attempt"))
	case state.WaitBehind:
		return "Ready, but queued behind an earlier round." + quota
	}
	return ""
}

// headline follows the Markdown dashboard's precedence exactly. Two dashboards
// disagreeing about what matters most would be worse than either alone.
func headline(st state.State, now time.Time, ov Overview) Headline {
	if s := stranded(st, now); s != "" {
		return Headline{Kind: "stranded", Subject: s,
			Text:   "Stranded reservation on " + s,
			Detail: "It holds a reservation with no fire slot behind it, and will not clear on its own."}
	}
	if b := st.Account.BlockedUntil; b != nil && b.After(now) {
		return Headline{Kind: "blocked",
			Text:   "CodeRabbit quota blocked",
			Detail: "Only the metered lane is paused — co-review and autofix continue."}
	}
	if r := st.SlotRound(); r != nil {
		key := state.Key(r.Repo, r.PR)
		return Headline{Kind: "reviewing", Subject: key, Text: "Reviewing " + key}
	}
	if len(ov.InFlight) > 0 {
		return Headline{Kind: "awaiting",
			Text: fmt.Sprintf("Awaiting feedback on %s", plural(len(ov.InFlight), "pull request"))}
	}
	if len(ov.Queue) > 0 {
		return Headline{Kind: "queued", Text: fmt.Sprintf("%s waiting", plural(len(ov.Queue), "round"))}
	}
	if len(ov.Held) > 0 {
		return Headline{Kind: "held", Text: fmt.Sprintf("%s held", plural(len(ov.Held), "pull request"))}
	}
	return Headline{Kind: "idle", Text: "Idle", Detail: "Nothing in flight, nothing queued."}
}

// attention collects the things that will not fix themselves, worst first.
func attention(st state.State, now time.Time, ov Overview) []Attention {
	out := []Attention{}
	if s := stranded(st, now); s != "" {
		repo, pr := splitKey(s)
		out = append(out, Attention{Kind: "stranded", Level: "bad", Subject: s,
			Text:   "Stranded reservation on " + s,
			Detail: "Cancel the round to release it, or wait for the daemon to normalise.",
			Link:   prLink(repo, pr), LinkText: "Open the pull request"})
	}
	for _, h := range ov.Autofix.Hosts {
		if h.Health == "unhealthy" {
			out = append(out, Attention{Kind: "host", Level: "bad", Subject: h.Name,
				Text: fmt.Sprintf("Autofix failing on %s — %s in a row",
					h.Name, plural(h.Failures, "attempt")),
				Detail: h.LastError, Link: "#/setup", LinkText: "Open hosts"})
		}
	}
	if ov.Leader == nil {
		out = append(out, Attention{Kind: "leader", Level: "warn",
			Text:   "No daemon holds the leader lease",
			Detail: "Enqueued work will not fire on its own until one starts.",
			Link:   "#/setup", LinkText: "Open setup"})
	} else if ov.Leader.Expired {
		out = append(out, Attention{Kind: "leader", Level: "warn", Subject: ov.Leader.Host,
			Text:   "The leader lease has expired",
			Detail: "The last daemon was " + ov.Leader.Host + "; nothing is driving the queue.",
			Link:   "#/setup", LinkText: "Open setup"})
	}
	// The weekly fair-use threshold does not stop reviews, it slows every one
	// of them to about one an hour — an ~80% collapse that used to arrive with
	// no warning at all. It earns a place here only once it is close.
	if fu := ov.Quota.FairUse; fu.Limit > 0 && fu.Level != "ok" {
		level := "warn"
		text := fmt.Sprintf("%d of %d metered reviews used this week", fu.Fires, fu.Limit)
		if fu.Level == "over" {
			level = "bad"
			text = fmt.Sprintf("Past the weekly fair-use threshold (%d of %d)", fu.Fires, fu.Limit)
		}
		out = append(out, Attention{Kind: "fairuse", Level: level, Text: text, Detail: fu.Note,
			Link: "#/settings", LinkText: "Fleet settings"})
	}
	if st.Warn != "" {
		out = append(out, Attention{Kind: "state", Level: "warn", Text: st.Warn})
	}
	return out
}

// stranded is the reserved-round-with-no-slot case: it never clears itself, so
// it outranks everything else on the screen.
func stranded(st state.State, now time.Time) string {
	if st.SlotHeld(now) {
		return ""
	}
	keys := make([]string, 0, len(st.Rounds))
	for key, r := range st.Rounds {
		if r.Phase == state.PhaseReserved {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func joinWords(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// prLink is the dashboard's own route for a pull request, so an attention item
// can point at the page that can act on it rather than only describing it.
func prLink(repo string, pr int) string {
	if repo == "" || pr <= 0 {
		return ""
	}
	return fmt.Sprintf("#/pr/%s/%d", repo, pr)
}
