package engine

import (
	"sort"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

// ActionKind is the single instruction a caller of `crq next` executes. It is a
// CLOSED set: an agent driving a review loop reads exactly this field and does
// exactly what it says, so every judgement call that used to be improvised —
// how long to sleep, whether the head may move, whether the round is finished —
// is answered here instead of at the call site.
type ActionKind string

const (
	// ActionFix: actionable findings exist for this head. Fix them, validate,
	// then resolve or decline each thread.
	ActionFix ActionKind = "fix"
	// ActionHold: the caller has work to land but a required reviewer has not
	// answered for this head. Moving the head now would restart that review, so
	// hold it and re-check at Action.At.
	ActionHold ActionKind = "hold"
	// ActionPush: the head is released — commit and push the accumulated fixes.
	ActionPush ActionKind = "push"
	// ActionWait: nothing to do until Action.At.
	ActionWait ActionKind = "wait"
	// ActionDone: every required reviewer answered and no findings remain.
	ActionDone ActionKind = "done"
	// ActionBlocked: the loop cannot proceed without a human (PR closed).
	ActionBlocked ActionKind = "blocked"
)

// Action is the answer NextAction produces.
type Action struct {
	Kind   ActionKind
	Reason string
	// At is when the caller should call again; set for hold and wait, and
	// always strictly in the future (see NextInput.MinDelay).
	At time.Time
	// Pending lists the required bots with no review evidence for this head,
	// in the caller's configured order.
	Pending []string
	// Findings carries actionable feedback for ActionFix, and remains visible
	// when a live dispatch must temporarily hold its fixes for a reviewer.
	Findings []dialect.Finding
}

// NextInput is everything NextAction decides from. It is a struct rather than a
// long parameter list because the decision genuinely needs the whole picture:
// the round, what was observed, what the findings layer extracted, and the
// caller's own local state.
type NextInput struct {
	Round      state.Round
	Obs        Observation
	Completion CompletionStatus
	// Findings is the actionable feedback the crq layer extracted from the same
	// observation, already filtered to what can still be acted on.
	Findings []dialect.Finding
	Global   Global
	// Primary is the configured primary reviewer's login — the one whose review
	// spends account quota. It is the only bot an account block can delay, so
	// both the rate-limit degrade and the recheck arithmetic need to tell it
	// apart from the co-reviewers.
	Primary string
	// LocalWork reports that the caller holds changes the PR head does not have
	// yet (uncommitted, or committed but unpushed). It is what separates "push
	// your fixes" from "nothing left to do".
	LocalWork bool
	// Deferred marks a CodeRabbit rate-limit degrade: the co-reviewers answered
	// and CodeRabbit's review is still owed, firing at DeferredUntil.
	Deferred      bool
	DeferredUntil *time.Time
	// DeferredReady says the configured non-primary evidence is sufficient to
	// release a rate-limited primary. The caller owns that reviewer-specific
	// policy; the action engine only consumes its bot-agnostic verdict.
	DeferredReady bool
	// MinDelay is the floor for Action.At — the caller's poll interval. It makes
	// a hot loop unrepresentable: every wait is at least this long.
	MinDelay time.Duration
	// SettleUntil holds back a convergence verdict until the PR has been quiet
	// for the configured settle window. Bots deliver in waves — a co-reviewer
	// auto-reviews a pushed head minutes later, and a primary's detailed
	// comments can trail its own completion shell — so the first clean
	// observation is not yet an answer. Zero disables.
	SettleUntil *time.Time
}

const defaultMinDelay = 15 * time.Second

func (in NextInput) minDelay() time.Duration {
	if in.MinDelay > 0 {
		return in.MinDelay
	}
	return defaultMinDelay
}

// NextAction reduces the whole review protocol to one instruction.
//
// The order encodes the rules agents get wrong when left to their own devices:
// findings are cleared before anything else, the head is held while any
// required reviewer is still pending, and a CodeRabbit rate-limit degrade
// releases the head instead of stalling on it (the queued review fires on the
// new head by itself). Convergence is last, so "done" can only mean every
// required bot answered AND nothing is left to land.
func NextAction(in NextInput, now time.Time) Action {
	if !in.Obs.Open {
		return Action{Kind: ActionBlocked, Reason: "pr closed"}
	}
	if in.Obs.Head == "" {
		// A transient read failure, not a terminal state — come back shortly.
		return Action{Kind: ActionWait, Reason: "could not read head", At: in.nextCheck(now, nil, nil)}
	}

	pending := pendingBots(in.Completion)

	// 1. Findings first. An agent that starts a new review round on top of
	//    unresolved feedback burns account quota to be told the same thing.
	//
	//    One caller already knows it handled the findings even though GitHub
	//    cannot know yet: the live dispatch session must push before resolving
	//    its threads. If that session holds local work while another required
	//    reviewer is already reading this head, the safe next action is the hold,
	//    not another copy of the same findings. This is deliberately tied to the
	//    dispatch claim; ordinary dirty worktrees must still see every finding.
	//
	//    A threadless finding has no lever the caller can pull — nothing clears
	//    it but a push whose review supersedes it — so it can repeat. Suppressing
	//    it once the tree is dirty was tried and is WORSE: any unrelated dirty
	//    file then hides a finding the caller never saw at all, which for a
	//    threadless `review_skipped` is unrecoverable. A repeated instruction is
	//    visible and annoying; a swallowed finding is silent and harmful, so this
	//    deliberately errs toward repeating. Clearing them properly needs an
	//    explicit dismissal, not an inference from local state.
	if blocking := BlockingFindings(in.Findings, in.Obs.Head); len(blocking) > 0 {
		if in.LocalWork && in.Round.DispatchHeld(now) {
			if AutofixReady(in, now) {
				return Action{
					Kind:     ActionPush,
					Reason:   "autofix is complete and no active reviewer still gates this head",
					Pending:  pending,
					Findings: blocking,
				}
			}
			return Action{
				Kind:     ActionHold,
				Reason:   "do not push: a required reviewer has not answered for this head",
				At:       in.nextCheck(now, nil, pending),
				Pending:  pending,
				Findings: blocking,
			}
		}
		return Action{
			Kind:     ActionFix,
			Reason:   "actionable findings for this head",
			Pending:  pending,
			Findings: blocking,
		}
	}

	// 2. A rate-limit degrade releases the head deliberately. The primary's
	//    review is still owed, but it is queued and will fire against whatever
	//    head exists when the window opens — so holding the current head buys
	//    nothing and costs a whole window. This is checked BEFORE the generic
	//    hold below, which would otherwise stall the loop for the full block.
	//
	//    "Released" means released from the PRIMARY only. Every other required
	//    reviewer still gates the head exactly as it always does, so the degrade
	//    uses the same DoneExceptWithEvidence rule Feedback applies to
	//    convergence — otherwise a degraded round pushes out from under a Bugbot
	//    or Macroscope review that is still running.
	if in.Deferred {
		releasedByDegrade := in.DeferredReady
		// The co-reviewers' evidence is as fresh here as anywhere else, so the
		// same quiet period applies: a review shell can satisfy the degrade while
		// its inline findings are still arriving, and moving the head strands
		// them on the old commit.
		if in.LocalWork && releasedByDegrade && in.settling(now) {
			return Action{
				Kind:    ActionWait,
				Reason:  "co-reviewers answered; holding briefly in case trailing findings are still landing",
				At:      in.nextCheck(now, in.SettleUntil, pending),
				Pending: pending,
			}
		}
		switch {
		case in.LocalWork && releasedByDegrade:
			return Action{
				Kind:    ActionPush,
				Reason:  "co-reviewers answered; primary review deferred and will fire on the new head",
				Pending: pending,
			}
		case in.LocalWork:
			return Action{
				Kind:    ActionHold,
				Reason:  "do not push: the primary review is deferred but another required reviewer has not answered for this head",
				At:      in.nextCheck(now, in.DeferredUntil, pending),
				Pending: pending,
			}
		}
		return Action{
			Kind:    ActionWait,
			Reason:  "primary review deferred while the account quota is blocked",
			At:      in.nextCheck(now, in.DeferredUntil, pending),
			Pending: pending,
		}
	}

	// 3. Required reviewers still pending: the head must not move. Resolving a
	//    thread does not restart a review; pushing does.
	if !in.Completion.Done {
		// ...unless the wait already expired. Progress retires that round to keep
		// the queue moving, but missing required evidence still must not read as
		// convergence to a caller. Say so instead, and never re-fire: the head was
		// already acknowledged.
		if in.waitExpired(now) {
			return Action{
				Kind:    ActionBlocked,
				Reason:  "the review deadline elapsed before every required reviewer answered",
				Pending: pending,
			}
		}
		kind, reason := ActionWait, "awaiting review"
		if in.LocalWork {
			// Holding protects a review that is actually happening. Nothing has
			// been requested for this head while the round is only queued — or
			// absent entirely — so holding would stall the caller AND spend a
			// review window on code it is about to replace. Land the work first;
			// the round then covers the real head.
			if !reviewRequested(in.Round) {
				return Action{
					Kind:   ActionPush,
					Reason: "no review has been requested for this head yet; land your work before one is",
				}
			}
			kind, reason = ActionHold, "do not push: a required reviewer has not answered for this head"
		}
		return Action{Kind: kind, Reason: reason, At: in.nextCheck(now, nil, pending), Pending: pending}
	}

	// 4. Everything answered — but only once the PR has been quiet long enough
	//    that a trailing wave would have landed. This gates BOTH outcomes:
	//    declaring `done` early stops a loop moments before findings arrive, and
	//    pushing early moves the head out from under a reviewer whose inline
	//    comments are still landing, stranding them on the old commit.
	if in.settling(now) {
		return Action{
			Kind:    ActionWait,
			Reason:  "every required reviewer answered; holding briefly in case a trailing review is still landing",
			At:      in.nextCheck(now, in.SettleUntil, pending),
			Pending: pending,
		}
	}
	if in.LocalWork {
		return Action{Kind: ActionPush, Reason: "all required reviewers answered on this head"}
	}
	return Action{Kind: ActionDone, Reason: "converged: no findings and every required reviewer answered"}
}

// AutofixReady reports whether an unattended fixer can start without becoming
// a paid waiter after it has prepared local work.
//
// Interactive work may overlap a live review and hand its wait back to the
// harness. An unattended model process cannot: keeping it alive on `hold`
// repeatedly replays its entire context merely to poll a shell command. Delay
// dispatch until active reviewers answer, except when no review was requested,
// the primary is explicitly deferred, or its delivery deadline already passed.
func AutofixReady(in NextInput, now time.Time) bool {
	pending := pendingBots(in.Completion)
	// The durable deadline ends the whole attempt, including its settle window.
	// Once it expires, no reviewer from this round may keep a paid fixer alive.
	if in.waitExpired(now) {
		return true
	}
	if in.settling(now) {
		return false
	}
	if len(pending) == 0 || !pendingReviewRequested(in, pending) {
		return true
	}
	if !in.onlyPrimaryPending(pending) {
		return false
	}
	if in.Deferred && in.DeferredReady {
		return true
	}
	return false
}

func pendingReviewRequested(in NextInput, pending []string) bool {
	for _, login := range pending {
		if sameBot(login, in.Primary) {
			if reviewRequested(in.Round) {
				return true
			}
			continue
		}
		co := in.Round.Co(login)
		if co.CommandedAt != nil || co.ClaimedAt != nil ||
			(co.SeenActiveAt != nil && !co.ActivityCarried) {
			return true
		}
		if dialect.IsCodexBot(login) &&
			(in.Round.CodexCommandedAt != nil || in.Round.CodexClaimedAt != nil) {
			return true
		}
	}
	return false
}

// settling reports whether the quiet period after the last evidence is still
// running.
func (in NextInput) settling(now time.Time) bool {
	return in.SettleUntil != nil && in.SettleUntil.After(now)
}

// reviewRequested reports whether a review has actually been asked for at this
// round's head.
//
// A round with no phase does not exist; a queued one is only a place in the
// fleet's FIFO — no command posted, no quota spent — and a push supersedes it
// for free. The distinction matters because a round is no longer created only by
// an enqueue: `crq dismiss` creates one to record the decision, and reading that
// as "a review is running" made the documented fix flow hold its own fixes until
// a review of the pre-fix head finished.
func reviewRequested(r state.Round) bool {
	// Read THROUGH a dispatch hold. While a fix session holds a round, crq
	// mirrors it into awaiting_retry so binaries that do not understand the
	// claim still refuse to fire — but that is crq holding the round FOR the
	// session, not a review anybody requested.
	//
	// Counting it as one deadlocked the session that caused it: told to hold, it
	// waited for a reviewer that could not be asked — the claim makes the round
	// ineligible to fire — while its own heartbeat extended the window it was
	// waiting on. DispatchHoldPhase is what the round was before the hold, which
	// is the phase this question is about.
	phase := r.Phase
	if r.DispatchHoldPhase != "" {
		phase = r.DispatchHoldPhase
		// A retry records a request that was rejected, not one a reviewer is
		// currently reading. The dispatch claim prevents that retry from firing,
		// so the owning session must be allowed to land its replacement head.
		if phase == state.PhaseAwaitingRetry {
			return false
		}
	}
	return phase != "" && phase != state.PhaseQueued
}

// CanStillFire reports whether a round will ask for a review at some point.
//
// FireEligible answers "right now", which is the wrong question for callers
// guarding future work: a round cooling in awaiting_retry becomes eligible the
// moment its RetryAt passes.
func CanStillFire(r state.Round) bool {
	switch r.Phase {
	case state.PhaseQueued, state.PhaseReserved, state.PhaseAwaitingRetry:
		return true
	}
	return false
}

// waitExpired reports whether this head's persisted review deadline has passed.
// A completed phase may carry that deadline after Progress retires a silent
// round, while Next still needs to explain the missing required evidence.
func (in NextInput) waitExpired(now time.Time) bool {
	phase := in.Round.Phase
	if phase == state.PhaseAwaitingRetry && in.Round.DispatchHeld(now) && in.Round.DispatchHoldPhase != "" {
		phase = in.Round.DispatchHoldPhase
	}
	switch phase {
	case state.PhaseFired, state.PhaseReviewing, state.PhaseCompleted, state.PhaseExpired:
		return in.Round.WaitDeadline != nil && !now.Before(in.Round.WaitDeadline.UTC())
	}
	return false
}

// nextCheck is when the caller should call again. It is never sooner than
// MinDelay (so a caller cannot hot-loop) and never sooner than a gate that
// definitely prevents progress — the account-quota window and this round's own
// retry cooldown, both of which DecideFire enforces anyway.
//
// Two things narrow those gates, and both exist because sleeping through
// feedback is worse than one extra call:
//
//   - They only apply to a round still waiting to fire. A fired or reviewing
//     round's answer can land at any moment.
//   - They only apply when the primary is the ONLY reviewer still owed. Neither
//     the account window nor this round's fire cooldown has any hold over a
//     co-reviewer, so a round waiting on one must keep the short cadence — a
//     degraded round otherwise sleeps for the whole block and misses the very
//     co-reviewer findings the degrade exists to deliver.
func (in NextInput) nextCheck(now time.Time, extra *time.Time, pending []string) time.Time {
	at := now.Add(in.minDelay()).UTC()
	if !in.onlyPrimaryPending(pending) || coReviewActive(in.Round) {
		return at
	}
	gate := func(t *time.Time) {
		if t != nil && t.After(at) {
			at = t.UTC()
		}
	}
	gate(extra)
	switch in.Round.Phase {
	case state.PhaseQueued, state.PhaseAwaitingRetry:
		gate(in.Global.BlockedUntil)
		if in.Round.Phase == state.PhaseAwaitingRetry {
			gate(in.Round.RetryAt)
		}
	}
	return at
}

// coReviewActive reports whether a co-reviewer has been commanded for this
// round.
//
// Co-reviewers spend no account quota and take no fire slot, so neither the
// account window nor this round's own fire cooldown gates their answer. A round
// with one in flight must therefore keep the short cadence even when the only
// entry in ReviewedBy is the primary — which is the default configuration, and
// is exactly the degrade path: the caller would otherwise sleep until the
// account window opened, hours later, straight through the co-review findings
// the degrade exists to deliver.
func coReviewActive(r state.Round) bool {
	for _, co := range r.CoBots {
		if co.CommandedAt != nil {
			return true
		}
	}
	return false
}

// onlyPrimaryPending reports whether every reviewer still owed for this head is
// the primary. An empty pending set counts: there is nobody a short cadence
// would catch.
func (in NextInput) onlyPrimaryPending(pending []string) bool {
	primary := dialect.NormalizeBotName(in.Primary)
	for _, bot := range pending {
		if dialect.NormalizeBotName(bot) != primary {
			return false
		}
	}
	return true
}

// pendingBots lists the required bots with no review evidence yet, sorted for a
// stable answer.
func pendingBots(c CompletionStatus) []string {
	var out []string
	for bot, reviewed := range c.ReviewedBy {
		if !reviewed {
			out = append(out, bot)
		}
	}
	sort.Strings(out)
	return out
}
