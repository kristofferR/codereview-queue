package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Co-reviewer evidence and gate algebra, keyed by login. These are the
// bot-shape-generic rules ("participated", "clean at SHA", "cannot finish",
// "was commanded") the Codex-specific helpers in codex.go now wrap; check
// runs count as evidence/activity alongside reviews and clean summaries
// because Bugbot's clean rounds exist ONLY as a check run.

// roundCutoff is the round-window floor: the fire time (UTC), or zero when the
// round has not fired.
func roundCutoff(r state.Round) time.Time {
	if r.FiredAt != nil {
		return r.FiredAt.UTC()
	}
	return time.Time{}
}

// roundCoCommandID reads the trigger comment recorded for login this round,
// falling back to the legacy Codex fields for rounds built directly (tests)
// rather than loaded through state.Normalize's fold.
func roundCoCommandID(r state.Round, login string) int64 {
	if c := r.Co(login); c.CommandID != 0 {
		return c.CommandID
	}
	if dialect.IsCodexBot(login) {
		return r.CodexCommandID
	}
	return 0
}

func roundCoCommandedAt(r state.Round, login string) *time.Time {
	if c := r.Co(login); c.CommandedAt != nil {
		return c.CommandedAt
	}
	if dialect.IsCodexBot(login) {
		return r.CodexCommandedAt
	}
	return nil
}

// eventConcerns reports whether a classified event concerns the co-reviewer
// login: by the classifier's For attribution when present, otherwise by author
// (a bot's own comments carry it even when For was not set).
func eventConcerns(ev dialect.BotEvent, login string) bool {
	if ev.For != "" {
		return sameBot(ev.For, login)
	}
	return sameBot(ev.Bot, login)
}

// coCutoff is the evidence floor for one co-reviewer: evidence produced in
// response to crq's own trigger command binds from the command time, which
// can precede a deferred CodeRabbit fire (the command posts while the round
// is still queued behind a rate-limit window or busy slot).
func coCutoff(r state.Round, login string) time.Time {
	cut := roundCutoff(r)
	if at := roundCoCommandedAt(r, login); at != nil {
		t := at.UTC()
		if cut.IsZero() || t.Before(cut) {
			return t
		}
	}
	return cut
}

// coSelfHealCutoff includes the observed head's full lifetime and the queued
// portion of this round. A co-reviewer can report that it is unable to finish
// before crq enqueues the head or while it waits for the primary fire slot;
// either answer must still suppress a later nudge.
func coSelfHealCutoff(r state.Round, obs Observation, login string) time.Time {
	cut := coCutoff(r, login)
	enqueued := r.EnqueuedAt.UTC()
	if !enqueued.IsZero() && (cut.IsZero() || enqueued.Before(cut)) {
		cut = enqueued
	}
	if r.Head == obs.Head {
		headAt := obs.HeadAt.UTC()
		if !headAt.IsZero() && (cut.IsZero() || headAt.Before(cut)) {
			return headAt
		}
	}
	return cut
}

// coChecks yields login's check runs from the observation.
func coChecks(obs Observation, login string) []CheckSeen {
	var out []CheckSeen
	for _, c := range obs.Checks {
		if sameBot(c.Bot, login) {
			out = append(out, c)
		}
	}
	return out
}

// coCheckAny reports whether login's REVIEW check is engaged with this head —
// running or finished. Posting a trigger alongside one would double-review.
//
// Two verdicts are deliberately excluded, because neither can ever satisfy the
// gate they would otherwise hold open. A FAILED run means the bot tried and did
// not deliver, so a self-heal trigger is the right response. An AUXILIARY check
// (Macroscope's Approvability or a repo-custom check) is not the review at all:
// treating it as engagement suppressed every trigger while the Correctness
// Check had not even started, leaving required rounds to time out with no
// recovery path.
//
// CheckUnable is NOT excluded: the bot answered — it cannot review this commit
// — and a trigger cannot change that answer, so the run suppresses the nudge
// even though it is not review evidence.
func coCheckAny(obs Observation, login string) bool {
	for _, c := range coChecks(obs, login) {
		switch c.Verdict {
		case dialect.CheckFailed, dialect.CheckAuxiliary:
			continue
		}
		return true
	}
	return false
}

// coCheckUnable reports whether login's own review run on this head says it
// could not review the commit (Macroscope's billing-issue skip) — the check-run
// twin of EvCoUnable, and read the same way: the bot cannot finish this round,
// so the dynamic gate must let go of it rather than wait out the deadline.
func coCheckUnable(obs Observation, login string) bool {
	for _, c := range coChecks(obs, login) {
		if c.Verdict == dialect.CheckUnable {
			return true
		}
	}
	return false
}

// coCheckActivity reports whether login has ANY check run on this head,
// including failed and auxiliary ones. Suppression and activity are separate
// questions: a bot whose only check crashed must not suppress the self-heal
// trigger (coCheckAny), but it is still demonstrably working on this PR — and
// if that were not activity, a bot whose FIRST check failed would have no
// signal at all, so self-heal could never fire and the round could only time
// out.
func coCheckActivity(obs Observation, login string) bool {
	return len(coChecks(obs, login)) > 0
}

// coCheckReviewedAt reports the newest COMPLETED check verdict for the head
// (Done or DoneClean — findings, if any, still gate via threads).
func coCheckReviewedAt(obs Observation, login string) (time.Time, bool) {
	var latest time.Time
	matched := false
	for _, c := range coChecks(obs, login) {
		if c.Verdict == dialect.CheckDone || c.Verdict == dialect.CheckDoneClean {
			matched = true
			if c.CompletedAt.After(latest) {
				latest = c.CompletedAt
			}
		}
	}
	return latest, matched
}

// coReviewedRound reports whether a submitted review by login binds to this
// round: one whose commit prefixes the head, or — SHA-less — one submitted
// at/after the fire.
func coReviewedRound(r state.Round, obs Observation, login string, cutoff time.Time) bool {
	for _, review := range obs.Reviews {
		if !sameBot(review.Bot, login) {
			continue
		}
		if r.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, r.Head) {
			return true
		}
		if review.Commit == "" && !review.SubmittedAt.IsZero() && notBefore(review.SubmittedAt, cutoff) {
			return true
		}
	}
	return false
}

// coCommentedRound reports whether login posted an actionable comment or a
// clean summary at/after the round's fire — the round-window evidence that
// means it is participating. Its notices (unable, acks, verdicts) do not
// count.
func coCommentedRound(obs Observation, login string, cutoff time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvOther && sameBot(ev.Bot, login) && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
	}
	return false
}

// coReviewedHeadAt reports the newest verdict by login explicitly bound to
// the observed head: a submitted review, a clean summary naming that SHA, or
// a completed check run (head-scoped by construction). The timestamp is the
// evidence floor used to ignore older unable notices.
func coReviewedHeadAt(obs Observation, login string) (time.Time, bool) {
	var latest time.Time
	matched := false
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, login) && obs.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			matched = true
			if review.SubmittedAt.After(latest) {
				latest = review.SubmittedAt
			}
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) && obs.Head != "" && dialect.SHAPrefixMatch(ev.SHA, obs.Head) {
			matched = true
			if at := ev.ObservedTime(); at.After(latest) {
				latest = at
			}
		}
	}
	if at, ok := coCheckReviewedAt(obs, login); ok {
		matched = true
		if at.After(latest) {
			latest = at
		}
	}
	return latest, matched
}

// coReviewedHead is the "login already reviewed this head" fire guard.
func coReviewedHead(obs Observation, login string) bool {
	_, matched := coReviewedHeadAt(obs, login)
	return matched
}

// CoActiveThisRound reports whether login shows activity bound to this round —
// a head review, a round-window comment/clean summary, a per-round verdict
// comment, or any head check run. observe() stores it on the Observation so
// the dynamic completion gate requires the bot when it participates without
// being configured-required. (Codex's thumbs-up quirk is layered on by
// CodexActiveThisRound.)
func CoActiveThisRound(r state.Round, obs Observation, login string) bool {
	cutoff := coCutoff(r, login)
	if coReviewedRound(r, obs, login, cutoff) || coCommentedRound(obs, login, cutoff) ||
		coVerdictSince(obs, login, cutoff) || coCheckActivity(obs, login) {
		return true
	}
	// Codex's thumbs-up quirk is applied here so callers never branch on a bot
	// identity themselves — engine owns which bots have quirks.
	return dialect.IsCodexBot(login) && obs.CodexThumbsUp
}

// coVerdictSince reports a per-round verdict comment (Macroscope's
// Approvability) by login at/after since. A verdict is round PARTICIPATION —
// it engages the dynamic gate so the bot's review is waited for — but never
// completion evidence: only reviews, clean summaries, and completed checks
// mark a bot reviewed.
func coVerdictSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoVerdict && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), since) {
			return true
		}
	}
	return false
}

// CoAutoActive reports whether login reviews this PR on its own right now: its
// most recent evidence — a submitted review, a clean summary, or a completed
// check — was not preceded by its trigger command. When true, crq must never
// post the trigger (the bot reviews unprompted). Only the LATEST evidence
// decides, so an old unprompted review from an epoch when auto-review was on
// no longer suppresses posting once a later commanded review lands;
// conversely a command posted before the latest evidence marks that evidence
// as commanded, not automatic.
func CoAutoActive(obs Observation, login string) bool {
	latest, prev, ok := latestCoEvidence(obs, login)
	if !ok {
		return false
	}
	// The latest evidence is automatic unless a command plausibly triggered it:
	// one posted in (prev, latest]. A command older than the previous evidence
	// belongs to an earlier round and does not explain this review — otherwise a
	// single manual trigger from three heads ago would suppress posting forever
	// even after the bot went back to reviewing on its own.
	return !coCommandInWindow(obs, login, prev, latest)
}

// latestCoEvidence returns the timestamps of the most recent and second-most
// recent review-or-clean-summary-or-completed-check events for login, and
// whether any exists. prev is zero when there is only one evidence item.
func latestCoEvidence(obs Observation, login string) (latest, prev time.Time, ok bool) {
	consider := func(at time.Time) {
		if at.IsZero() {
			return
		}
		switch {
		case !ok || at.After(latest):
			prev, latest, ok = latest, at, true
		case at.Equal(latest):
			// prev must stay strictly older: co-timestamped evidence (a review and
			// its clean summary in the same second) must not close the command
			// window to a point, or a command at that instant reads as absent and
			// a commanded review misclassifies as automatic.
		case at.After(prev):
			prev = at
		}
	}
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, login) {
			consider(review.SubmittedAt)
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) {
			consider(ev.PairTime())
		}
	}
	for _, c := range coChecks(obs, login) {
		if c.Verdict == dialect.CheckDone || c.Verdict == dialect.CheckDoneClean {
			consider(c.CompletedAt)
		}
	}
	return latest, prev, ok
}

// coCommandInWindow reports whether login's trigger command was posted after
// `after` and at or before `atOrBefore`. A zero `after` means no lower bound
// (the latest evidence is also the first — any command up to it counts).
func coCommandInWindow(obs Observation, login string, after, atOrBefore time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvCoCommand || !eventConcerns(ev, login) {
			continue
		}
		at := ev.PairTime()
		if at.After(atOrBefore) {
			continue
		}
		if !after.IsZero() && !at.After(after) {
			continue
		}
		return true
	}
	return false
}

// CoCommandSince reports whether login's trigger command comment exists
// at/after since. The self-heal retry uses it (with the round's fire time) to
// tell a fired round whose command is already on the PR from one whose post
// failed.
func CoCommandSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoCommand && eventConcerns(ev, login) && notBefore(ev.PairTime(), since) {
			return true
		}
	}
	return false
}

// coUnableSince reports whether login declared it cannot finish this round
// (EvCoUnable — Codex's usage-limit exhaustion) at/after since.
func coUnableSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoUnable && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), since) {
			return true
		}
	}
	return false
}

// CoOnlyEligible reports whether an account-blocked round may degrade to a
// co-reviewer-only round: the block is live AND login has evidence bound to
// THIS work — a review of the current head, or round-window activity anchored
// by the fire or by crq's own (possibly pre-fire) trigger command — AND no
// unable notice inside that same window. Auto-activity on older heads,
// configuration, or a live unanswered command merely predict evidence;
// degradation waits for the evidence itself, since before the bot responds
// there is nothing to return early anyway, and marking a round deferred stops
// the loop from extending its deadline over the block.
func CoOnlyEligible(r state.Round, obs Observation, login string, blockedUntil *time.Time, now time.Time) bool {
	if blockedUntil == nil || !blockedUntil.After(now) {
		return false
	}
	headEvidenceAt, headReviewed := coReviewedHeadAt(obs, login)
	anchored := r.FiredAt != nil || roundCoCommandedAt(r, login) != nil
	if !headReviewed && !(anchored && obs.co(login).ActiveThisRound) {
		return false
	}
	// The unable floor is the evidence window. For an unfired, uncommanded
	// round the cutoff is zero — floor it at the head review that qualified
	// the round instead, or any old exhaustion notice still on the PR would
	// suppress the degrade until the window expires.
	floor := coCutoff(r, login)
	if floor.IsZero() {
		floor = headEvidenceAt
	}
	return !coUnableSince(obs, login, floor) && !coCheckUnable(obs, login)
}

// DecideCoPost reports whether crq should post login's trigger command for
// this round. Common guards regardless of mode: a command is configured, the
// round has not already commanded this bot, no live command sits on the PR,
// the bot has not reviewed the head, and no check run of its exists for the
// head (including in-progress — a running check will deliver evidence).
//
// Modes: never — false. always — post whenever the common current-head guards
// above allow it. selfheal — post only for a bot observed active now or on a
// prior head that missed this one, once the anchor (the round's fire) is older
// than the grace period; the caller passes anchor and now so the fire path and
// the sweep share the rule.
func DecideCoPost(r state.Round, obs Observation, cp CoReviewerPolicy, commandPresent bool, anchor, now time.Time) bool {
	if roundCoCommandID(r, cp.Login) != 0 {
		return false
	}
	if strings.TrimSpace(cp.Command) == "" {
		return false
	}
	if commandPresent {
		return false
	}
	if coReviewedHead(obs, cp.Login) {
		return false
	}
	if coCheckAny(obs, cp.Login) {
		return false
	}
	switch cp.Trigger {
	case TriggerAlways:
		// "Always" is the operator's explicit instruction to ask this reviewer
		// on every head. AutoActive can come from an older head and therefore
		// cannot override that instruction; current-head review/check/command
		// evidence already suppresses duplicates in the common guards above.
		// Likewise, an unreadable checks endpoint is not evidence that a review
		// exists. Suppressing either case leaves a required reviewer pending
		// until the round times out with no recovery path.
		return true
	case TriggerSelfHeal:
		// Self-heal is deliberately conservative: when checks are unreadable,
		// the missing check is not evidence that an auto-review was missed.
		// Avoid double-asking a run that may already be in flight.
		if obs.co(cp.Login).ChecksUnknown {
			return false
		}
		if r.ForceCoReviewer(cp.Login) {
			return true
		}
		co := obs.co(cp.Login)
		if coUnableSince(obs, cp.Login, coSelfHealCutoff(r, obs, cp.Login)) {
			return false
		}
		if !co.AutoActive && !co.ActiveThisRound && r.Co(cp.Login).SeenActiveAt == nil {
			return false
		}
		if anchor.IsZero() {
			return false
		}
		return now.Sub(anchor) >= cp.selfHealGrace()
	default:
		return false
	}
}

// CoReviewerActive reports whether this reviewer has produced any observable
// activity on the pull request, regardless of which head it belongs to.
func CoReviewerActive(obs Observation, login string) bool {
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, login) {
			return true
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvCoCommand && eventConcerns(ev, login) {
			return true
		}
	}
	for _, check := range obs.Checks {
		if sameBot(check.Bot, login) {
			return true
		}
	}
	return false
}
