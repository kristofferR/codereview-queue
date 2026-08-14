package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// FireVerdict is what Pump should do with a fire-eligible round. Nothing
// outside DecideFire may conclude "post the review command" — this is the
// single owner of that decision.
type FireVerdict int

const (
	FireNo           FireVerdict = iota // skip this pass (Reason says why)
	FirePost                            // reserve the slot and post the command
	FireAdopt                           // a command is already on the PR — adopt it
	FireDedupe                          // bot already reviewed this head — complete without firing
	FireCoOnly                          // CodeRabbit reviewed the head but a gating co-reviewer still must — post only its trigger
	FireCoReviewWait                    // CodeRabbit reviewed the head; a gating co-bot has not — wait for it, bounded, without posting or holding the slot
	FireCoDeferred                      // account blocked — post only co-reviewer triggers now; the round stays queued so CodeRabbit fires when the window opens
	FireSupersede                       // observed head differs — supersede the round first
	FireDrop                            // PR closed/merged — abandon the round
)

type FireDecision struct {
	Verdict FireVerdict
	Reason  string
	// Adopt fields identify the existing command comment (FireAdopt).
	AdoptCommandID int64
	AdoptAt        time.Time
	// PostCo lists the co-reviewer logins whose trigger commands the apply
	// layer posts alongside this verdict (FirePost/FireAdopt/FireCoOnly/
	// FireCoDeferred). See DecideCoPost.
	PostCo []string
	// AdoptCo identifies live co-reviewer trigger comments to record as this
	// round's command anchors instead of posting duplicates (FireCoDeferred).
	AdoptCo map[string]CommandSeen
}

// Global is the cross-PR state a fire decision needs.
type Global struct {
	SlotFree     bool
	BlockedUntil *time.Time // CodeRabbit account quota block
	LastFired    *time.Time // global pacing anchor
}

// coReviewers resolves the effective co-reviewer policies.
func (p Policy) coReviewers() []CoReviewerPolicy { return p.CoReviewers }

// CoReviewerPolicies exposes the effective co-reviewer list to the apply
// layer (self-heal sweeps, trigger posting), which shares DecideCoPost with
// the fire path and so must iterate the same resolved entries.
func (p Policy) CoReviewerPolicies() []CoReviewerPolicy { return p.coReviewers() }

// CoReviewedHead reports whether login already has head review evidence (a
// head review, a SHA-matched clean summary, or a completed check run) — the
// feedback layer surfaces it as per-bot status.
func CoReviewedHead(obs Observation, login string) bool { return coReviewedHead(obs, login) }

// requiredBot reports whether login is in RequiredBots (normalized).
func requiredBot(p Policy, login string) bool {
	norm := dialect.NormalizeBotName(login)
	for _, bot := range p.RequiredBots {
		if dialect.NormalizeBotName(strings.TrimSpace(bot)) == norm {
			return true
		}
	}
	return false
}

// selfHealAnchor is what a selfheal trigger measures its grace against: the
// round's fire.
//
// neverFires supplies the fallback for a round that will not get one — a
// summary-only or skipped head resolves without crq ever posting the primary
// command, so a fire-only anchor left those rounds permanently ineligible for
// the trigger that could rescue them. It is deliberately NOT used on a round
// that is about to fire: there the head commit is typically old, and treating
// that as elapsed grace would post the trigger in the same breath as the fire,
// before the bot has had any chance to review this round at all.
func selfHealAnchor(r state.Round, obs Observation, neverFires bool) time.Time {
	if at := roundCutoff(r); !at.IsZero() {
		return at
	}
	if !neverFires {
		return time.Time{}
	}
	// The grace period asks how long the bot has had to show up on its own, so
	// the anchor must be when the head APPEARED — and the head commit's own date
	// is only a lower bound on that. A force-push or branch reset can point a PR
	// at a commit authored months earlier, which reads as a grace that elapsed
	// long ago and posts a trigger over an auto-review that has barely started.
	// The round is the better witness: crq enqueued it when it first saw this
	// head, so take the later of the two.
	//
	// Deliberately NOT applied to obs.HeadAt itself, which is an evidence FLOOR
	// elsewhere (fireCoReviewWait, SHA-less notices) and must stay a lower bound:
	// raising it there discards a co-reviewer answer that landed in the seconds
	// between the push and crq noticing it.
	anchor := obs.HeadAt
	if r.Head != "" && r.Head == obs.Head && r.EnqueuedAt.After(anchor) {
		anchor = r.EnqueuedAt.UTC()
	}
	return anchor
}

// decideCoPosts collects the co-reviewer logins whose trigger crq should post
// while firing this round. Fire-time posting is the always-mode path; a
// selfheal trigger anchors on the fire and so never posts before it.
func decideCoPosts(r state.Round, obs Observation, p Policy, now time.Time) []string {
	var out []string
	for _, cp := range p.coReviewers() {
		if DecideCoPost(r, obs, cp, len(obs.co(cp.Login).Commands) > 0, time.Time{}, now) {
			out = append(out, cp.Login)
		}
	}
	return out
}

// DecideFire consolidates v2's scattered fire guards, in order: PR open →
// head readable → head current → round eligible (phase + RetryAt cooldown) →
// slot free → account quota → global pacing → not already reviewed → adopt
// or post.
func DecideFire(g Global, r state.Round, obs Observation, now time.Time, p Policy) FireDecision {
	if !obs.Open {
		return FireDecision{Verdict: FireDrop, Reason: "pr closed"}
	}
	if obs.Head == "" {
		return FireDecision{Verdict: FireNo, Reason: "could not read head"}
	}
	if r.Head != obs.Head {
		return FireDecision{Verdict: FireSupersede, Reason: "head moved to " + obs.Head}
	}
	if !r.FireEligible(now) {
		reason := "round is " + string(r.Phase)
		switch {
		case r.DispatchHeld(now):
			reason = "a fix session is rewriting this head"
		case r.Phase == state.PhaseAwaitingRetry && r.RetryAt != nil:
			reason = "cooling down until " + r.RetryAt.UTC().Format(time.RFC3339)
		}
		return FireDecision{Verdict: FireNo, Reason: reason}
	}
	// No review of this head is coming from the configured bot, ever — its plan
	// produces a walkthrough only, or it skipped this head outright. Resolve the
	// round on the co-reviewers alone, before the slot / account-quota / pacing
	// gates: none of them should delay (or be spent on) a review that cannot
	// happen. coAwareDedupe posts the triggers crq may, waits bounded for a
	// co-bot that answers on its own, and dedupes when there is none to ask.
	if PrimaryReviewUnavailable(obs, p, obs.Head) {
		return coAwareDedupe(r, obs, p, now, true)
	}
	reviewedHead := false
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, p.Bot) && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			reviewedHead = true
			break
		}
	}
	// A submitted Review is not the only way the primary finishes. It also
	// answers a command with a completion reply and no Review object, and
	// reading only obs.Reviews there means crq posts the command again and buys
	// a second review of code it has already been told about. The reply is
	// paired to THIS round's command, so it only counts while the round still
	// tracks the head, and it must carry the evidence Completion asks for — a
	// reply nothing backs would mark an unreviewed head reviewed for good.
	if !reviewedHead && r.Head == obs.Head {
		reviewedHead = PrimaryCompletedRound(r, obs, p)
	}
	// Belt-and-braces live check: even with a fresh round, never fire at a
	// head the bot has already reviewed (e.g. state was reinitialized). But a
	// CodeRabbit review does not finish a round that a gating co-reviewer still
	// must speak on — command (or wait for) it instead of deduping it away.
	//
	// This resolution runs BEFORE the slot, account-block and pacing gates:
	// none of its verdicts spend CodeRabbit quota (dedupe completes, FireCoOnly
	// posts only co-reviewer triggers, a co-review wait posts nothing), so
	// neither another PR's in-flight review nor an account block may delay a
	// round whose primary work is already done.
	if reviewedHead {
		return coAwareDedupe(r, obs, p, now, false)
	}
	if !g.SlotFree {
		// Co-reviewers need no fire slot: a round parked behind another PR's
		// in-flight review can start its co-reviewer rounds immediately. The
		// round stays queued and CodeRabbit fires once the slot frees, with the
		// recorded command ids preventing duplicate posts.
		if d, ok := decideCoDeferred(r, obs, p, now, "fire slot busy", false); ok {
			return d
		}
		return FireDecision{Verdict: FireNo, Reason: "fire slot busy"}
	}
	if g.BlockedUntil != nil && g.BlockedUntil.After(now) {
		// Degrade instead of stalling: the block only gates CodeRabbit quota,
		// so ask the co-reviewers now and leave the round queued — CodeRabbit
		// still fires the moment the window opens. DecideCoPost's guards
		// (command configured, no already-posted or live command) make this
		// idempotent per round.
		if d, ok := decideCoDeferred(r, obs, p, now, "account blocked", true); ok {
			return d
		}
		return FireDecision{Verdict: FireNo, Reason: "account blocked until " + g.BlockedUntil.UTC().Format(time.RFC3339)}
	}
	if g.LastFired != nil && now.Sub(*g.LastFired) < p.MinInterval {
		return FireDecision{Verdict: FireNo, Reason: "min interval"}
	}
	// crq posts always-mode co-reviewer triggers in the same fire step when the
	// bot does not auto-review and no command exists for this head.
	postCo := decideCoPosts(r, obs, p, now)
	// Adopt the newest already-posted command instead of posting a duplicate.
	// observe() has already applied the adoption cutoffs (LastAttemptAt,
	// force-push, already-answered).
	if newest := newestCommand(obs.Commands); newest != nil {
		at := newest.CreatedAt
		if at.IsZero() {
			at = newest.UpdatedAt
		}
		return FireDecision{Verdict: FireAdopt, Reason: "review command already posted", AdoptCommandID: newest.ID, AdoptAt: at, PostCo: postCo}
	}
	return FireDecision{Verdict: FirePost, PostCo: postCo}
}

// decideCoDeferred starts or adopts the co-reviewer half of a round while
// CodeRabbit cannot fire. Each co-bot's Commands are cutoff-filtered by
// observe(), so an existing command here is safe to bind to this head and must
// be recorded as the round anchor rather than merely suppressing a duplicate
// post. The legacy Adopt fields mirror the Codex entry for pre-migration
// consumers.
func decideCoDeferred(r state.Round, obs Observation, p Policy, now time.Time, reason string, accountBlocked bool) (FireDecision, bool) {
	// RateLimitCoDegrade is documented as governing behaviour during an
	// account-limit window. Applying it to the busy-slot path too would park
	// quota-free co-review work behind an unrelated PR's CodeRabbit review —
	// adding a whole in-flight timeout to bots that never touch the quota.
	if accountBlocked && !p.RateLimitCoDegrade {
		return FireDecision{}, false
	}
	var post []string
	adopt := map[string]CommandSeen{}
	for _, cp := range p.coReviewers() {
		if roundCoCommandID(r, cp.Login) != 0 {
			continue
		}
		commands := obs.co(cp.Login).Commands
		if DecideCoPost(r, obs, cp, len(commands) > 0, time.Time{}, now) {
			post = append(post, cp.Login)
			continue
		}
		// Only a trigger-capable bot may anchor the deferred round on an
		// adopted command; for the rest a live human command is just ambient.
		if cp.Trigger != TriggerAlways {
			continue
		}
		if newest := newestCommand(commands); newest != nil {
			adopt[cp.Login] = *newest
		}
	}
	if len(post) == 0 && len(adopt) == 0 {
		return FireDecision{}, false
	}
	d := FireDecision{Verdict: FireCoDeferred, PostCo: post}
	if len(adopt) > 0 {
		d.AdoptCo = adopt
	}
	switch {
	case len(post) > 0 && len(adopt) > 0:
		d.Reason = reason + "; requesting/adopting co-reviews now, coderabbit deferred"
	case len(post) > 0:
		d.Reason = reason + "; requesting co-review now, coderabbit deferred"
	default:
		d.Reason = reason + "; adopting existing co-review command, coderabbit deferred"
	}
	return d, true
}

func newestCommand(commands []CommandSeen) *CommandSeen {
	var newest *CommandSeen
	for i := range commands {
		cmd := &commands[i]
		if newest == nil || cmd.CreatedAt.After(newest.CreatedAt) {
			newest = cmd
		}
	}
	return newest
}

// coAwareDedupe resolves what to do when the primary bot has delivered
// everything it is ever going to for this head — either it already reviewed the
// head, or (primaryUnavailable) no review is coming: its plan only summarizes,
// or it skipped this head outright.
// If no gating co-reviewer is still outstanding, the round is genuinely done
// (FireDedupe). If a required-or-auto-active co-bot has no review of this head
// yet, the round is not done: post the triggers crq may (FireCoOnly). When crq
// may not post but the bot will still produce evidence on its own — it
// auto-reviews, or a command is already on the PR awaiting its answer — wait
// for it, bounded, without posting or holding the slot (FireCoReviewWait);
// leaving the round queued with no deadline is the bug that hangs the loop
// forever. Only when a co-bot gates purely by configuration with no way to
// obtain its review (no command configured/on the PR and no auto-review) fall
// back to completing on CodeRabbit's review; the feedback gate then surfaces
// it as still pending rather than the round wedging in an un-timed fire loop.
// Completion counts the existing CodeRabbit review, so a FireCoOnly round
// waits on the co-reviewers alone.
//
// Under primaryUnavailable every ENABLED co-reviewer gates, not just the
// required or auto-active ones: they are the round's only reviewers, so a
// co-bot crq would otherwise treat as optional is the difference between a
// review and none.
func coAwareDedupe(r state.Round, obs Observation, p Policy, now time.Time, primaryUnavailable bool) FireDecision {
	var post []string
	wait := false
	anchor := selfHealAnchor(r, obs, primaryUnavailable)
	for _, cp := range p.coReviewers() {
		co := obs.co(cp.Login)
		gates := requiredBot(p, cp.Login) || co.AutoActive || primaryUnavailable || r.ForceCoReviewer(cp.Login)
		if !gates || coReviewedHead(obs, cp.Login) {
			continue
		}
		if DecideCoPost(r, obs, cp, len(co.Commands) > 0, anchor, now) {
			post = append(post, cp.Login)
			continue
		}
		// ActiveThisRound covers the case crq cannot post for: a check already
		// running on this head. Without it the round dedupes to completed while
		// the co-review is still in flight, discarding the findings it is about
		// to publish.
		if co.AutoActive || co.ActiveThisRound || len(co.Commands) > 0 || roundCoCommandID(r, cp.Login) != 0 {
			wait = true
			continue
		}
		// A REQUIRED co-reviewer with no activity yet still gates. Deduping here
		// wrote a completed marker that Completion immediately contradicts —
		// it reports the bot, and so the primary, unfinished — leaving Loop to
		// time out and requeue into the same cycle. A bounded wait is the
		// honest state: the bot has not spoken, and the deadline ends it.
		if primaryUnavailable && requiredBot(p, cp.Login) {
			wait = true
		}
	}
	delivered := "primary reviewed head"
	if primaryUnavailable {
		if p.PrimaryOff {
			delivered = "primary disabled for this repository"
		} else {
			delivered = "primary review unavailable for this head"
		}
	}
	if len(post) > 0 {
		return FireDecision{Verdict: FireCoOnly, Reason: delivered + "; co-review still required", PostCo: post}
	}
	if wait {
		return FireDecision{Verdict: FireCoReviewWait, Reason: "awaiting co-review"}
	}
	if primaryUnavailable {
		return FireDecision{Verdict: FireDedupe, Reason: delivered + "; no co-review outstanding"}
	}
	return FireDecision{Verdict: FireDedupe, Reason: "bot already reviewed head"}
}
