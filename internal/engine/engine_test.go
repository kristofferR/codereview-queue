package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

var (
	t0     = time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	policy = Policy{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		MinInterval:       90 * time.Second,
		InflightTimeout:   15 * time.Minute,
		RateLimitFallback: 15 * time.Minute,
		RetryBackoff:      5 * time.Minute,
	}
)

func firedRound(t *testing.T, head string) state.Round {
	t.Helper()
	s := state.New()
	r, err := s.NewRound("owner/repo", 448, head, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(1001, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return *r
}

func rateLimitEvent(id int64, at time.Time, window *time.Time) dialect.BotEvent {
	return dialect.BotEvent{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: id, CreatedAt: at, UpdatedAt: at, AutoReply: true, Window: window,
	}
}

// TestRateLimitedRoundParksAndHoldsWindow is the #448 scenario at engine
// level: a fired head that comes back rate limited must park with a real
// window, and re-observing the SAME edited comment must not extend it.
func TestRateLimitedRoundParksAndHoldsWindow(t *testing.T) {
	r := firedRound(t, "a21da4aeb")
	window := t0.Add(40 * time.Minute)
	obs := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, t0.Add(10*time.Second), &window)}}

	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(window) {
		t.Fatalf("want retry at the parsed window, got %+v", tr)
	}
	if tr.Blocked == nil || tr.Blocked.CommentID != 555 {
		t.Fatalf("must record the rate-limit comment identity, got %+v", tr.Blocked)
	}

	// Apply: the round parks. It is not fire-eligible before the window.
	if err := r.AwaitRetry(tr.RetryAt, tr.Reason, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r.FireEligible(window.Add(-time.Second)) {
		t.Fatal("round must stay parked inside the block window")
	}

	// The daemon re-observes the SAME comment (edited in place, later
	// UpdatedAt, later parse base → later window). The standing block wins.
	quota := state.AccountQuota{RLCommentID: 555, BlockedUntil: &window}
	later := t0.Add(5 * time.Minute)
	laterWindow := later.Add(40 * time.Minute)
	obs2 := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, later, &laterWindow)}}
	r2 := firedRound(t, "a21da4aeb")
	tr2 := Progress(r2, quota, obs2, later, policy)
	if tr2.Outcome != OutRetry || !tr2.RetryAt.Equal(window) {
		t.Fatalf("re-observation must reuse the standing window %v, got %+v", window, tr2)
	}
}

func TestUnparseableRateLimitFallsBackConservatively(t *testing.T) {
	r := firedRound(t, "a21da4aeb")
	now := t0.Add(time.Minute)
	obs := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, t0.Add(10*time.Second), nil)}}
	tr := Progress(r, state.AccountQuota{}, obs, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(t0.Add(15*time.Minute+10*time.Second)) {
		t.Fatalf("want the 15m fallback window, got %+v", tr)
	}
}

// TestInstantCompletionReplyDoesNotConverge encodes the 865ef40 fix: a
// "Review finished" ack on the FIRST-ever command (no prior submitted
// review) must not complete the round.
func TestInstantCompletionReplyDoesNotConverge(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{
		{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001, CreatedAt: t0.Add(2 * time.Second), UpdatedAt: t0.Add(2 * time.Second)},
		{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002, AutoReply: true, CreatedAt: t0.Add(7 * time.Second), UpdatedAt: t0.Add(7 * time.Second)},
	}}
	if got := Completion(r, obs, policy); got.Done {
		t.Fatalf("instant ack with no prior review must not converge: %+v", got)
	}
	// With a prior review on an older commit, the same ack DOES stand in for a
	// no-findings re-review.
	obs.Reviews = []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}}
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("re-review completion reply must converge: %+v", got)
	}
}

// TestProcessingSummaryBlocksCompletion encodes the c22eb4b fix, now applied
// on the daemon path too: while the in-place-edited top summary says the
// review is processing, a completion reply must not converge or complete.
func TestProcessingSummaryBlocksCompletion(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}},
		Events: []dialect.BotEvent{
			{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001, CreatedAt: t0.Add(2 * time.Second), UpdatedAt: t0.Add(2 * time.Second)},
			{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002, AutoReply: true, CreatedAt: t0.Add(7 * time.Second), UpdatedAt: t0.Add(7 * time.Second)},
			{Kind: dialect.EvInProgress, Bot: "coderabbitai[bot]", CommentID: 900, CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(8 * time.Second)},
		}}
	if got := Completion(r, obs, policy); got.Done {
		t.Fatalf("processing summary must block convergence: %+v", got)
	}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutReviewing {
		t.Fatalf("daemon path should release the slot but keep the round open, got %+v", tr)
	}
}

// TestFailedSummaryParksTheRound encodes the e2aa2f0 fix on the daemon path:
// a failed review must not complete the round, and retries after a cooldown.
func TestFailedSummaryParksTheRound(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(time.Minute)
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}},
		Events: []dialect.BotEvent{
			{Kind: dialect.EvFailed, Bot: "coderabbitai[bot]", CommentID: 900, CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(9 * time.Second)},
		}}
	tr := Progress(r, state.AccountQuota{}, obs, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("failed review must park with backoff, got %+v", tr)
	}
}

func TestFailedOptionalPrimaryCompletesConvergedRound(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(time.Minute)
	p := withCodex(policy, "@codex review")
	p.RequiredBots = []string{dialect.CodexBotLogin}
	obs := Observation{
		Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{
			Bot: dialect.CodexBotLogin, ReviewID: 4,
			Commit: "abcdef1234567890", SubmittedAt: t0.Add(30 * time.Second),
		}},
		Events: []dialect.BotEvent{{
			Kind: dialect.EvFailed, Bot: "coderabbitai[bot]", CommentID: 900,
			CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(9 * time.Second),
		}},
	}
	if got := Completion(r, obs, p); !got.Done {
		t.Fatalf("the only required reviewer answered; want convergence, got %+v", got)
	}
	if tr := Progress(r, state.AccountQuota{}, obs, now, p); tr.Outcome != OutComplete {
		t.Fatalf("an optional primary failure must finish a converged round, got %+v", tr)
	}
}

func TestReviewAtHeadCompletesRound(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "abcdef1234567890", SubmittedAt: t0.Add(3 * time.Minute)}}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(4*time.Minute), policy)
	if tr.Outcome != OutComplete {
		t.Fatalf("review at head must complete, got %+v", tr)
	}
	// A review of a DIFFERENT commit must not.
	obs.Reviews[0].Commit = "999888777"
	tr = Progress(r, state.AccountQuota{}, obs, t0.Add(4*time.Minute), policy)
	if tr.Outcome == OutComplete {
		t.Fatalf("review of another head must not complete, got %+v", tr)
	}
}

// TestFireSlotHeldUntilPrimaryAcknowledges separates convergence from slot
// release. A repository may leave the primary out of its required set (required
// Codex only), and then the round converges the moment Codex answers — while the
// account-metered command it posted is still unacknowledged. Completing there
// would hand the slot to the next PR mid-review, which is the serialization the
// whole queue exists for.
func TestFireSlotHeldUntilPrimaryAcknowledges(t *testing.T) {
	p := withCodex(policy, "@codex review")
	p.RequiredBots = []string{dialect.CodexBotLogin}
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: dialect.CodexBotLogin, ReviewID: 4, Commit: "abcdef1234567890", SubmittedAt: t0.Add(time.Minute)}}}

	r := firedRound(t, "abcdef123")
	if got := Completion(r, obs, p); !got.Done {
		t.Fatalf("the only required reviewer answered; want a converged round, got %+v", got)
	}
	if tr := Progress(r, state.AccountQuota{}, obs, t0.Add(2*time.Minute), p); tr.Outcome != KeepWaiting {
		t.Fatalf("converged is not acknowledged — the slot must stay held, got %+v", tr)
	}
	// The primary reacts to the command: the slot has done its job, so the
	// converged round completes.
	acked := obs
	acked.Reacted = true
	if tr := Progress(r, state.AccountQuota{}, acked, t0.Add(2*time.Minute), p); tr.Outcome != OutComplete {
		t.Fatalf("an acknowledged converged round must complete, got %+v", tr)
	}
	// It never reacts: the in-flight window ends the wait rather than buying a
	// second metered review no configured reviewer asked for.
	if tr := Progress(r, state.AccountQuota{}, obs, t0.Add(16*time.Minute), p); tr.Outcome != OutComplete {
		t.Fatalf("the in-flight timeout must end a converged round, not re-fire it, got %+v", tr)
	}
	// A co-only round spent no quota and holds no slot, so it completes at once.
	co := firedRound(t, "abcdef123")
	co.CoOnly = true
	if tr := Progress(co, state.AccountQuota{}, obs, t0.Add(2*time.Minute), p); tr.Outcome != OutComplete {
		t.Fatalf("a co-only round has no primary command to wait on, got %+v", tr)
	}
}

func TestSubmittedPrimaryReviewAcknowledgesTheRound(t *testing.T) {
	firedAt := t0.Add(-time.Minute)
	r := state.Round{Phase: state.PhaseFired, Head: "abcdef123", FiredAt: &firedAt}
	obs := Observation{Reviews: []ReviewSeen{{
		Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: t0,
	}}}
	if PrimaryAckPending(r, obs, policy) {
		t.Fatal("a submitted primary review must acknowledge the command that produced it")
	}
}

func TestRegistryPrimaryCompletedCheckAcknowledgesTheRound(t *testing.T) {
	p := policy
	p.Bot = dialect.CodexBotLogin
	p.RequiredBots = []string{"some-other-bot[bot]"}
	cases := []struct {
		name    string
		check   CheckSeen
		outcome Outcome
		reason  string
	}{
		{
			name: "current terminal primary check",
			check: CheckSeen{
				Bot: dialect.CodexBotLogin, Verdict: dialect.CheckDoneClean,
				CompletedAt: t0.Add(time.Minute),
			},
			outcome: OutReviewing, reason: "check completed",
		},
		{
			name: "stale terminal check",
			check: CheckSeen{
				Bot: dialect.CodexBotLogin, Verdict: dialect.CheckDone,
				CompletedAt: t0,
			},
			outcome: KeepWaiting, reason: "review in flight",
		},
		{
			name: "wrong bot",
			check: CheckSeen{
				Bot: "other[bot]", Verdict: dialect.CheckDoneClean,
				CompletedAt: t0.Add(time.Minute),
			},
			outcome: KeepWaiting, reason: "review in flight",
		},
		{
			name: "non-terminal primary check",
			check: CheckSeen{
				Bot: dialect.CodexBotLogin, Verdict: dialect.CheckInProgress,
				CompletedAt: t0.Add(time.Minute),
			},
			outcome: KeepWaiting, reason: "review in flight",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := firedRound(t, "abcdef123")
			obs := Observation{Head: "abcdef123", Open: true, Checks: []CheckSeen{tc.check}}
			tr := Progress(r, state.AccountQuota{}, obs, t0.Add(2*time.Minute), p)
			if tr.Outcome != tc.outcome || tr.Reason != tc.reason {
				t.Fatalf("Progress = %+v, want outcome %v reason %q", tr, tc.outcome, tc.reason)
			}
		})
	}
}

func TestInflightTimeoutCarriesCooldown(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(16 * time.Minute)
	tr := Progress(r, state.AccountQuota{}, Observation{Head: "abcdef123", Open: true}, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("timeout must park with a cooldown (v2 had none — re-fire vector), got %+v", tr)
	}
}

// TestReviewingRoundDeadlineBoundsCoReviewWait covers the daemon-side co-review
// bound: a reviewing round past its WaitDeadline completes when the primary bot
// reviewed the head (its review stands; give up on the silent co-bot). Without a
// primary review it keeps waiting — the loop bounds and times out its own wait,
// so the daemon never resets or re-fires an expired head. Before the deadline it
// keeps waiting on the co-bot too.
func TestReviewingRoundDeadlineBoundsCoReviewWait(t *testing.T) {
	codexReq := policy
	codexReq.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	codexReq = withCodex(codexReq, "@codex review")

	reviewing := func() state.Round {
		r := firedRound(t, "abcdef123")
		if err := r.Acknowledge(); err != nil {
			t.Fatal(err)
		}
		dl := t0.Add(time.Hour)
		r.WaitDeadline = &dl
		return r
	}
	crAtHead := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: t0}}}

	// At the deadline with the primary review standing → complete (co-bot gave up).
	past := t0.Add(time.Hour).Add(time.Second)
	if tr := Progress(reviewing(), state.AccountQuota{}, crAtHead, past, codexReq); tr.Outcome != OutComplete {
		t.Fatalf("primary review at head past the deadline must complete, got %+v", tr)
	}
	// At the deadline with NO primary review → keep waiting (the loop times out its
	// own wait; the daemon must not reset the deadline or re-fire the head).
	noReview := Observation{Head: "abcdef123", Open: true}
	if tr := Progress(reviewing(), state.AccountQuota{}, noReview, past, codexReq); tr.Outcome != KeepWaiting {
		t.Fatalf("no primary review past the deadline must keep waiting, not re-fire, got %+v", tr)
	}
	// Before the deadline the bound must not fire: keep waiting on the co-bot —
	// KeepWaiting, not a re-emitted OutReviewing, so the sweep doesn't write the
	// same state and re-sync the dashboard on every pump.
	if tr := Progress(reviewing(), state.AccountQuota{}, crAtHead, t0.Add(30*time.Minute), codexReq); tr.Outcome != KeepWaiting {
		t.Fatalf("before the deadline a co-review wait must keep waiting, got %+v", tr)
	}

	// The exception to "no primary review → keep waiting": a round whose primary
	// review is never coming (a summary-only plan, a skipped review). Its
	// deadline is the ONLY thing that can end it — `autoreview` runs no Loop, and
	// the primary-reviewed branch above can never become true — so leaving it to
	// the fall-through wedges it in `reviewing` forever, and with it every
	// reviewing round behind it (the daemon sweeps only the oldest per pump).
	summaryOnly := Observation{Head: "abcdef123", Open: true,
		Events: []dialect.BotEvent{{Kind: dialect.EvOther, Bot: "coderabbitai[bot]", SummaryOnly: true, CommentID: 9}}}
	if tr := Progress(reviewing(), state.AccountQuota{}, summaryOnly, past, codexReq); tr.Outcome != OutComplete {
		t.Fatalf("a wait no primary review can ever satisfy must end at its deadline, got %+v", tr)
	}
	if tr := Progress(reviewing(), state.AccountQuota{}, summaryOnly, t0.Add(30*time.Minute), codexReq); tr.Outcome == OutComplete {
		t.Fatalf("before the deadline it must still wait for the co-reviewer, got %+v", tr)
	}
}

func TestDecideFireGuards(t *testing.T) {
	free := Global{SlotFree: true}
	now := t0.Add(10 * time.Minute)

	queued := state.Round{Repo: "owner/repo", PR: 448, Head: "abcdef123", Phase: state.PhaseQueued, Seq: 1}
	open := Observation{Head: "abcdef123", Open: true}

	if d := DecideFire(free, queued, Observation{Head: "abcdef123", Open: false}, now, policy); d.Verdict != FireDrop {
		t.Fatalf("closed PR must drop, got %+v", d)
	}
	if d := DecideFire(free, queued, Observation{Head: "999888777", Open: true}, now, policy); d.Verdict != FireSupersede {
		t.Fatalf("moved head must supersede, got %+v", d)
	}
	fired := firedRound(t, "abcdef123")
	if d := DecideFire(free, fired, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("a fired round must never fire again, got %+v", d)
	}
	if d := DecideFire(Global{SlotFree: false}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("busy slot must block, got %+v", d)
	}
	blocked := now.Add(10 * time.Minute)
	if d := DecideFire(Global{SlotFree: true, BlockedUntil: &blocked}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("account block must block, got %+v", d)
	}
	last := now.Add(-time.Second)
	if d := DecideFire(Global{SlotFree: true, LastFired: &last}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("min interval must block, got %+v", d)
	}
	reviewed := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai", Commit: "abcdef1234567890", SubmittedAt: now}}}
	if d := DecideFire(free, queued, reviewed, now, policy); d.Verdict != FireDedupe {
		t.Fatalf("already-reviewed head must dedupe, got %+v", d)
	}
	withCommand := Observation{Head: "abcdef123", Open: true,
		Commands: []CommandSeen{{ID: 77, CreatedAt: now.Add(-time.Minute)}}}
	if d := DecideFire(free, queued, withCommand, now, policy); d.Verdict != FireAdopt || d.AdoptCommandID != 77 {
		t.Fatalf("existing command must be adopted, got %+v", d)
	}
	if d := DecideFire(free, queued, open, now, policy); d.Verdict != FirePost {
		t.Fatalf("clean queued round must post, got %+v", d)
	}

	// A parked round becomes fire-eligible only after RetryAt.
	parked := firedRound(t, "abcdef123")
	retryAt := now.Add(15 * time.Minute)
	if err := parked.AwaitRetry(retryAt, "rate limited", now); err != nil {
		t.Fatal(err)
	}
	if d := DecideFire(free, parked, open, retryAt.Add(-time.Second), policy); d.Verdict != FireNo {
		t.Fatalf("parked round must not fire before RetryAt, got %+v", d)
	}
	if d := DecideFire(free, parked, open, retryAt, policy); d.Verdict != FirePost {
		t.Fatalf("parked round must fire once RetryAt passes, got %+v", d)
	}
}

// TestBareReactionReleasesSlotButKeepsRoundOpen ports v2's doneBotReacted: a
// reaction on the fired command acknowledges it, releasing the slot while the
// review keeps running.
func TestBareReactionReleasesSlotButKeepsRoundOpen(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Reacted: true}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutReviewing {
		t.Fatalf("a bare reaction must release the slot and keep the round open, got %+v", tr)
	}
}

// TestReviewsPausedNoteIsNotAck ports v2: the auto-pause note is a bot comment
// but not an acknowledgement of the fired command, so the round keeps waiting.
func TestReviewsPausedNoteIsNotAck(t *testing.T) {
	r := firedRound(t, "abcdef123")
	paused := dialect.BotEvent{Kind: dialect.EvPaused, Bot: "coderabbitai[bot]", CommentID: 900,
		CreatedAt: t0.Add(10 * time.Second), UpdatedAt: t0.Add(10 * time.Second)}
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{paused}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != KeepWaiting {
		t.Fatalf("a reviews-paused note must not acknowledge or complete the round, got %+v", tr)
	}
}

// TestRateLimitBeatsAlreadyReviewedAck encodes the carrier#82 incident: a
// rate-limit notice plus an "already reviewed" claim, with no review object,
// must park the round (retry later), never complete it.
func TestRateLimitBeatsAlreadyReviewedAck(t *testing.T) {
	r := firedRound(t, "a0646f010")
	window := t0.Add(40 * time.Minute)
	obs := Observation{Head: "a0646f010", Open: true, Events: []dialect.BotEvent{
		rateLimitEvent(501, t0.Add(10*time.Second), &window),
		{Kind: dialect.EvAlreadyReviewed, Bot: "coderabbitai[bot]", CommentID: 502, CreatedAt: t0.Add(10 * time.Second), UpdatedAt: t0.Add(10 * time.Second)},
	}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutRetry || tr.Blocked == nil {
		t.Fatalf("an unproven already-reviewed ack must yield to the rate limit, got %+v", tr)
	}
}

// TestPreFireReviewOfHeadCompletes ports botsReviewedHead: a required bot's
// review of the head counts even when it landed before the round was fired.
func TestPreFireReviewOfHeadCompletes(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Reviews: []ReviewSeen{
		{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "abcdef1234567890", SubmittedAt: t0.Add(-10 * time.Minute)},
	}}
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("a required bot's pre-fire review of the head must complete the round: %+v", got)
	}
}

// TestCompletionFlipsRequiredBotAcrossSuffix ports crq's markReviewed suffix
// test: a review whose login differs from the configured required bot only by
// the "[bot]" suffix (REST "coderabbitai[bot]" vs GraphQL "coderabbitai") must
// still flip the required key, or convergence (which ANDs every key) stays
// permanently false.
func TestCompletionFlipsRequiredBotAcrossSuffix(t *testing.T) {
	r := firedRound(t, "abcdef123")
	// Required key carries the suffix; the review login does not.
	obs := Observation{Head: "abcdef123", Open: true, Reviews: []ReviewSeen{
		{Bot: "coderabbitai", ReviewID: 9, Commit: "abcdef1234567890", SubmittedAt: t0.Add(time.Minute)},
	}}
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("a suffix-less review login must flip the suffixed required key: %+v", got)
	}
	// Inverse: required key without suffix, review login with it.
	noSuffix := policy
	noSuffix.RequiredBots = []string{"coderabbitai"}
	obs.Reviews[0].Bot = "coderabbitai[bot]"
	if got := Completion(r, obs, noSuffix); !got.Done {
		t.Fatalf("a suffixed review login must flip the suffix-less required key: %+v", got)
	}
}

// TestCommandHasCompletionReply covers the adoption guard: a command already
// answered by a completion reply is spoken for and must not be re-adopted,
// unless an in-progress/rate-limited/paused summary since the reply reopens it.
func TestCommandHasCompletionReply(t *testing.T) {
	base := []dialect.BotEvent{
		{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001, CreatedAt: t0, UpdatedAt: t0},
		{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002, AutoReply: true, CreatedAt: t0.Add(5 * time.Second), UpdatedAt: t0.Add(5 * time.Second)},
	}
	if !CommandHasCompletionReply(Observation{Events: base}, policy, 1001) {
		t.Fatal("a command answered by a completion reply must read as spoken for")
	}
	if CommandHasCompletionReply(Observation{Events: base}, policy, 999) {
		t.Fatal("an unrelated command id must not match")
	}
	// A processing summary edited in place after the reply reopens the round.
	withProcessing := append(append([]dialect.BotEvent(nil), base...),
		dialect.BotEvent{Kind: dialect.EvInProgress, Bot: "coderabbitai[bot]", CommentID: 900, CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(9 * time.Second)})
	if CommandHasCompletionReply(Observation{Events: withProcessing}, policy, 1001) {
		t.Fatal("an in-progress summary after the reply must reopen the command")
	}
}

// TestDecideCodexPost is the Codex trigger-post decision matrix. Posting is now
// governed by CoReviewerPolicy.Trigger, not by RequiredBots membership — these
// cases run under the always-mode policy withCodex builds, and assert the
// mode-independent guards: no auto-review, no live command, no recorded id, and
// no existing head evidence.
func TestDecideCodexPost(t *testing.T) {
	codexReq := Policy{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", dialect.CodexBotLogin},
		CoReviewers:  []CoReviewerPolicy{{Login: dialect.CodexBotLogin, Command: "@codex review", Trigger: TriggerAlways}},
	}
	head := "abcdef123"
	base := Observation{Head: head, Open: true}
	codexReviewHead := ReviewSeen{Bot: dialect.CodexBotLogin, Commit: "abcdef1234567890", SubmittedAt: t0}

	cases := []struct {
		name           string
		round          state.Round
		obs            Observation
		policy         Policy
		commandPresent bool
		want           bool
	}{
		{name: "required, no auto, first fire", round: state.Round{Head: head}, obs: base, policy: codexReq, want: true},
		{name: "always mode overrides historical auto activity", round: state.Round{Head: head}, obs: Observation{Head: head, Open: true, Co: codexSeen(CoSeen{AutoActive: true})}, policy: codexReq, want: true},
		{name: "already reviewed head", round: state.Round{Head: head}, obs: Observation{Head: head, Open: true, Reviews: []ReviewSeen{codexReviewHead}}, policy: codexReq, want: false},
		{name: "command already present", round: state.Round{Head: head}, obs: base, policy: codexReq, commandPresent: true, want: false},
		{name: "not required", round: state.Round{Head: head}, obs: base, policy: policy, want: false},
		{name: "codex command empty", round: state.Round{Head: head}, obs: base, policy: Policy{RequiredBots: codexReq.RequiredBots}, want: false},
		{name: "already asked this round", round: state.Round{Head: head, CodexCommandID: 42}, obs: base, policy: codexReq, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideCoPost(tc.round, tc.obs, codexCP(tc.policy), tc.commandPresent, time.Time{}, time.Time{}); got != tc.want {
				t.Fatalf("DecideCodexPost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCodexAutoActive covers the "latest evidence decides" rule: only the most
// recent Codex review/clean-summary determines auto-review, so an old unprompted
// review no longer suppresses posting once a later commanded review lands.
func TestCodexAutoActive(t *testing.T) {
	codexReview := func(at time.Time) ReviewSeen {
		return ReviewSeen{Bot: dialect.CodexBotLogin, Commit: "abcdef1234567890", SubmittedAt: at}
	}
	codexCommand := func(at time.Time) dialect.BotEvent {
		return dialect.BotEvent{Kind: dialect.EvCoCommand, Bot: "kristofferR", For: dialect.CodexBotLogin, CommentID: 1, CreatedAt: at, UpdatedAt: at}
	}
	codexClean := func(at time.Time) dialect.BotEvent {
		return dialect.BotEvent{Kind: dialect.EvCoClean, Bot: dialect.CodexBotLogin, SHA: "abcdef1234", CommentID: 2, CreatedAt: at, UpdatedAt: at}
	}
	t1 := t0.Add(time.Hour)

	cases := []struct {
		name string
		obs  Observation
		want bool
	}{
		{name: "no evidence", obs: Observation{}, want: false},
		{name: "unprompted review", obs: Observation{Reviews: []ReviewSeen{codexReview(t0)}}, want: true},
		{name: "unprompted clean summary", obs: Observation{Events: []dialect.BotEvent{codexClean(t0)}}, want: true},
		{name: "commanded review", obs: Observation{
			Reviews: []ReviewSeen{codexReview(t0.Add(time.Minute))},
			Events:  []dialect.BotEvent{codexCommand(t0)},
		}, want: false},
		// Old unprompted review, then a command, then a later commanded review: the
		// latest evidence was commanded, so the old epoch stops suppressing posting.
		{name: "old unprompted then commanded", obs: Observation{
			Reviews: []ReviewSeen{codexReview(t0), codexReview(t1.Add(time.Minute))},
			Events:  []dialect.BotEvent{codexCommand(t1)},
		}, want: false},
		// An old command, a commanded review, then a LATER unprompted review: the
		// stale command is before the previous evidence, so it must not mask the
		// latest review as commanded — auto-review is active again.
		{name: "stale command does not mask later auto review", obs: Observation{
			Reviews: []ReviewSeen{codexReview(t0.Add(time.Minute)), codexReview(t1)},
			Events:  []dialect.BotEvent{codexCommand(t0)},
		}, want: true},
		// A review and its clean summary in the SAME second must not collapse the
		// command window to a point: the command at that instant still explains
		// the evidence, so this is commanded, not automatic.
		{name: "co-timestamped evidence keeps the command window open", obs: Observation{
			Reviews: []ReviewSeen{codexReview(t1)},
			Events:  []dialect.BotEvent{codexCommand(t1), codexClean(t1)},
		}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoAutoActive(tc.obs, dialect.CodexBotLogin); got != tc.want {
				t.Fatalf("CoAutoActive(codex) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideFireCodexDedupe covers the dedupe/Codex interaction: a head
// CodeRabbit already reviewed must still command (or wait for) a gating Codex
// rather than completing the round Codex-less.
func TestDecideFireCodexDedupe(t *testing.T) {
	free := Global{SlotFree: true}
	now := t0.Add(10 * time.Minute)
	head := "abcdef123"
	queued := state.Round{Repo: "owner/repo", PR: 448, Head: head, Phase: state.PhaseQueued, Seq: 1}
	codexReq := policy
	codexReq.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	codexReq = withCodex(codexReq, "@codex review")

	crReviewed := ReviewSeen{Bot: "coderabbitai", Commit: "abcdef1234567890", SubmittedAt: now}
	codexReviewed := ReviewSeen{Bot: dialect.CodexBotLogin, Commit: "abcdef1234567890", SubmittedAt: now}

	// CodeRabbit reviewed the head; Codex required with no evidence and crq may
	// post → command Codex alone.
	obs := Observation{Head: head, Open: true, Reviews: []ReviewSeen{crReviewed}}
	if d := DecideFire(free, queued, obs, now, codexReq); d.Verdict != FireCoOnly {
		t.Fatalf("coderabbit-reviewed head with a gating codex must command codex, got %+v", d)
	}
	// Same, but Codex auto-reviews: crq must not post; wait for its own review,
	// bounded (FireCoReviewWait) rather than left queued with no deadline.
	autoObs := Observation{Head: head, Open: true, Co: codexSeen(CoSeen{AutoActive: true}), Reviews: []ReviewSeen{crReviewed}}
	if d := DecideFire(free, queued, autoObs, now, codexReq); d.Verdict != FireCoOnly || !codexPosted(d) {
		t.Fatalf("always-mode codex must be commanded, not deduped, got %+v", d)
	}
	// A live `@codex review` command already on the PR: crq must not repost it;
	// wait for its answer, bounded.
	cmdObs := Observation{Head: head, Open: true, Reviews: []ReviewSeen{crReviewed}, Co: codexSeen(CoSeen{Commands: []CommandSeen{{ID: 55, CreatedAt: now}}})}
	if d := DecideFire(free, queued, cmdObs, now, codexReq); d.Verdict != FireCoReviewWait {
		t.Fatalf("an outstanding codex command must wait (bounded), got %+v", d)
	}
	// Codex already reviewed the head → the round is genuinely done.
	doneObs := Observation{Head: head, Open: true, Reviews: []ReviewSeen{crReviewed, codexReviewed}}
	if d := DecideFire(free, queued, doneObs, now, codexReq); d.Verdict != FireDedupe {
		t.Fatalf("both bots reviewed the head must dedupe, got %+v", d)
	}
	// No Codex configured or active → plain dedupe as before.
	if d := DecideFire(free, queued, obs, now, policy); d.Verdict != FireDedupe {
		t.Fatalf("without a gating codex a reviewed head must dedupe, got %+v", d)
	}
	// Codex required but no command configured and not auto-active: crq cannot
	// obtain a Codex review, so it must dedupe rather than wedge the round waiting
	// forever — the feedback gate surfaces Codex as still pending.
	noCmd := codexReq
	noCmd = withCodex(noCmd, "")
	if d := DecideFire(free, queued, obs, now, noCmd); d.Verdict != FireDedupe {
		t.Fatalf("a required-but-uncommandable codex must dedupe, not wedge, got %+v", d)
	}
}

func TestDecideFireForcedCoReviewerGate(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	head := "abcdef123"
	round := state.Round{Repo: "owner/repo", PR: 448, Head: head, Phase: state.PhaseQueued, Seq: 1}
	obs := Observation{Head: head, Open: true, Reviews: []ReviewSeen{{
		Bot: policy.Bot, Commit: "abcdef1234567890", SubmittedAt: now,
	}}}
	selfHeal := policy
	selfHeal.CoReviewers = []CoReviewerPolicy{{
		Login: dialect.CodexBotLogin, Command: "@codex review", Trigger: TriggerSelfHeal,
	}}

	for _, tc := range []struct {
		name   string
		forced bool
		want   FireVerdict
	}{
		{name: "optional self-heal reviewer is not a gate", want: FireDedupe},
		{name: "forced optional self-heal reviewer gates", forced: true, want: FireCoOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := round
			if tc.forced {
				candidate.ForceCoReviewers = []string{dialect.CodexBotLogin}
			}
			got := DecideFire(Global{SlotFree: true}, candidate, obs, now, selfHeal)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v (%+v)", got.Verdict, tc.want, got)
			}
			if tc.forced && (len(got.PostCo) != 1 || !dialect.IsCodexBot(got.PostCo[0])) {
				t.Fatalf("PostCo = %v, want the forced reviewer", got.PostCo)
			}
		})
	}
}

// TestDynamicCodexGate covers the dynamic completion gate: an observed-active
// Codex gates a round it isn't configured-required for, a usage-limit notice
// disengages that dynamic gate, and a configured-required Codex is left gating
// regardless of the usage limit.
func TestDynamicCodexGate(t *testing.T) {
	// Codex is enabled (wanted) but NOT required: the gate must come from
	// observed participation, not configuration.
	codexEnabled := withCodex(policy, "@codex review")
	r := firedRound(t, "abcdef123")
	cutoff := r.FiredAt.UTC()
	crReview := ReviewSeen{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: cutoff.Add(time.Minute)}
	codexReview := ReviewSeen{Bot: dialect.CodexBotLogin, Commit: "abcdef1234567890", SubmittedAt: cutoff.Add(time.Minute)}
	usageLimit := dialect.BotEvent{Kind: dialect.EvCoUnable, Bot: dialect.CodexBotLogin, CommentID: 700,
		CreatedAt: cutoff.Add(30 * time.Second), UpdatedAt: cutoff.Add(30 * time.Second)}

	// Codex auto-reviews the PR but hasn't reviewed the head yet: the dynamic gate
	// holds even though only CodeRabbit is configured-required.
	held := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{AutoActive: true}), Reviews: []ReviewSeen{crReview}}
	if got := Completion(r, held, codexEnabled); got.Done {
		t.Fatalf("an active Codex must gate the round until it reviews the head: %+v", got)
	}
	// Once Codex reviews the head, it converges.
	held.Reviews = append(held.Reviews, codexReview)
	if got := Completion(r, held, codexEnabled); !got.Done {
		t.Fatalf("the dynamic gate must converge once Codex reviews the head: %+v", got)
	}
	// A usage-limit notice disengages the DYNAMIC gate: CodeRabbit alone converges.
	limited := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{AutoActive: true}), Reviews: []ReviewSeen{crReview}, Events: []dialect.BotEvent{usageLimit}}
	if got := Completion(r, limited, codexEnabled); !got.Done {
		t.Fatalf("a Codex usage limit must disengage the dynamic gate: %+v", got)
	}
	// The configured-required gate is unchanged by a usage limit: it still waits.
	gated := policy
	gated.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	if got := Completion(r, limited, gated); got.Done {
		t.Fatalf("a usage limit must NOT disengage the configured-required Codex gate: %+v", got)
	}
}

// TestCodexGatesCleanSummary ports the codexInactiveOrThumbed rules.
func TestCodexGatesCleanSummary(t *testing.T) {
	// Codex is enabled (wanted) but NOT required: the gate must come from
	// observed participation, not configuration.
	codexEnabled := withCodex(policy, "@codex review")
	r := firedRound(t, "abcdef123")
	noAction := dialect.BotEvent{Kind: dialect.EvNoAction, Bot: "coderabbitai[bot]", CommentID: 2000,
		CreatedAt: t0.Add(30 * time.Second), UpdatedAt: t0.Add(30 * time.Second)}

	// Codex inactive: the clean summary converges alone.
	if got := Completion(r, Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction}}, codexEnabled); !got.Done {
		t.Fatalf("codex-inactive clean summary must converge: %+v", got)
	}

	// Codex active in the round (a real Codex comment) without its review or
	// thumbs-up: the summary must NOT converge.
	codexComment := dialect.BotEvent{Kind: dialect.EvOther, Bot: dialect.CodexBotLogin, CommentID: 2001,
		CreatedAt: t0.Add(20 * time.Second), UpdatedAt: t0.Add(20 * time.Second)}
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction, codexComment}}
	if got := Completion(r, obs, codexEnabled); got.Done {
		t.Fatalf("active codex without review must block: %+v", got)
	}

	// A thumbs-up unblocks it.
	obs.CodexThumbsUp = true
	if got := Completion(r, obs, codexEnabled); !got.Done {
		t.Fatalf("codex thumbs-up must unblock: %+v", got)
	}

	// A Codex clean summary naming the head counts as Codex's review — and if
	// Codex gates the round, flips its ReviewedBy too.
	gated := policy
	gated.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	codexClean := dialect.BotEvent{Kind: dialect.EvCoClean, Bot: dialect.CodexBotLogin, SHA: "abcdef1234",
		CommentID: 2002, CreatedAt: t0.Add(40 * time.Second), UpdatedAt: t0.Add(40 * time.Second)}
	got := Completion(r, Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction, codexClean}}, gated)
	if !got.Done {
		t.Fatalf("codex clean summary at head must complete the gated round: %+v", got)
	}
}

// TestCodexResolutionBypassesAccountBlock pins the quota-bypass reorder: a head
// CodeRabbit already reviewed resolves through codexAwareDedupe even during an
// account block or inside MinInterval — none of those verdicts spend CodeRabbit
// quota, so a block from another PR must not delay them.
func TestCodexResolutionBypassesAccountBlock(t *testing.T) {
	gated := policy
	gated.RequiredBots = []string{policy.Bot, dialect.CodexBotLogin}
	gated = withCodex(gated, "@codex review")
	now := t0.Add(10 * time.Minute)
	blocked := now.Add(30 * time.Minute)
	last := now.Add(-time.Second)
	g := Global{SlotFree: true, BlockedUntil: &blocked, LastFired: &last}

	queued := state.Round{Repo: "owner/repo", PR: 448, Head: "abcdef123", Phase: state.PhaseQueued, Seq: 1}
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: policy.Bot, ReviewID: 1, Commit: "abcdef1234567890", SubmittedAt: now}}}

	if d := DecideFire(g, queued, obs, now, gated); d.Verdict != FireCoOnly {
		t.Fatalf("blocked account must not delay a codex-only fire, got %+v", d)
	}
	// Codex satisfied → plain dedupe, also unblocked.
	obs.Reviews = append(obs.Reviews, ReviewSeen{Bot: dialect.CodexBotLogin, ReviewID: 2, Commit: "abcdef1234567890", SubmittedAt: now})
	if d := DecideFire(g, queued, obs, now, gated); d.Verdict != FireDedupe {
		t.Fatalf("blocked account must not delay a dedupe, got %+v", d)
	}
	// A round needing a REAL CodeRabbit fire still respects the block.
	if d := DecideFire(g, queued, Observation{Head: "abcdef123", Open: true}, now, gated); d.Verdict != FireNo {
		t.Fatalf("a real fire must still respect the account block, got %+v", d)
	}
}

func TestFindingsForActiveRoundIncludesOnlyCurrentOrNewlyArrivedFeedback(t *testing.T) {
	findings := []dialect.Finding{
		{Title: "current", Commit: "abcdef123456", CreatedAt: t0.Add(-time.Hour)},
		{Title: "delayed", Commit: "fedcba987654", CreatedAt: t0.Add(time.Second)},
		{Title: "carried", Commit: "fedcba987654", CreatedAt: t0.Add(-time.Second)},
		{Title: "unbound", CreatedAt: t0.Add(-time.Hour)},
	}

	got := FindingsForActiveRound(findings, "abcdef123", t0)
	if len(got) != 3 || got[0].Title != "current" || got[1].Title != "delayed" || got[2].Title != "unbound" {
		t.Fatalf("unexpected active-round findings: %#v", got)
	}
}

// TestCodexOnlyEligible covers the degrade predicate: a live block plus
// OBSERVED Codex responsiveness — configuration or a live command alone never
// qualifies, and a usage-limit notice disengages it.
func TestCodexOnlyEligible(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(5 * time.Minute)
	blocked := now.Add(25 * time.Minute)
	expired := now.Add(-time.Minute)
	usageLimit := dialect.BotEvent{Kind: dialect.EvCoUnable, Bot: dialect.CodexBotLogin, CommentID: 700,
		CreatedAt: r.FiredAt.Add(30 * time.Second), UpdatedAt: r.FiredAt.Add(30 * time.Second)}

	active := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{ActiveThisRound: true})}
	if CodexOnlyEligible(r, active, nil, now) {
		t.Fatal("no block must not degrade")
	}
	if CodexOnlyEligible(r, active, &expired, now) {
		t.Fatal("an expired block must not degrade")
	}
	if !CodexOnlyEligible(r, active, &blocked, now) {
		t.Fatal("a live block with round-bound codex activity must degrade")
	}
	// Auto-activity alone predicts evidence; it does not qualify until Codex
	// actually responds to this head/round.
	auto := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{AutoActive: true})}
	if CodexOnlyEligible(r, auto, &blocked, now) {
		t.Fatal("auto-activity without current evidence must not degrade")
	}
	configOnly := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{Commands: []CommandSeen{{ID: 55, CreatedAt: now}}})}
	if CodexOnlyEligible(r, configOnly, &blocked, now) {
		t.Fatal("a live command without observed codex evidence must not degrade")
	}
	limited := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{ActiveThisRound: true}), Events: []dialect.BotEvent{usageLimit}}
	if CodexOnlyEligible(r, limited, &blocked, now) {
		t.Fatal("a codex usage limit since the fire must disengage the degrade")
	}
}

// TestDoneExcept covers the Codex-side completeness check for degraded rounds,
// including the vacuous guard: an excluded bot alone never reads as done.
func TestDoneExcept(t *testing.T) {
	cr := "coderabbitai[bot]"
	if !DoneExcept(map[string]bool{cr: false, dialect.CodexBotLogin: true}, cr) {
		t.Fatal("codex reviewed, coderabbit excluded: must be done-except")
	}
	if DoneExcept(map[string]bool{cr: false}, cr) {
		t.Fatal("only the excluded bot gates: must NOT be vacuously done")
	}
	if DoneExcept(map[string]bool{cr: false, dialect.CodexBotLogin: false}, cr) {
		t.Fatal("codex still pending: must not be done-except")
	}
	if !DoneExcept(map[string]bool{cr: true, dialect.CodexBotLogin: true}, cr) {
		t.Fatal("everything reviewed must be done-except too")
	}
	// Bot-name normalization: the suffixless form excludes the bracketed key.
	if DoneExcept(map[string]bool{"coderabbitai": false}, cr) {
		t.Fatal("normalized excluded bot alone must not be vacuously done")
	}
}

func TestDoneExceptWithEvidence(t *testing.T) {
	cr := "coderabbitai[bot]"
	tests := []struct {
		name       string
		reviewedBy map[string]bool
		want       bool
	}{
		{name: "feedback-only evidence supplies non-vacuous gate", reviewedBy: map[string]bool{cr: false}, want: true},
		{name: "configured evidence bot is marked reviewed", reviewedBy: map[string]bool{cr: false, dialect.CodexBotLogin: false}, want: true},
		{name: "suffixless evidence bot is normalized", reviewedBy: map[string]bool{cr: false, "chatgpt-codex-connector": false}, want: true},
		{name: "another pending reviewer still gates", reviewedBy: map[string]bool{cr: false, dialect.CodexBotLogin: false, "other[bot]": false}, want: false},
		{name: "another completed reviewer permits", reviewedBy: map[string]bool{cr: false, "other[bot]": true}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DoneExceptWithEvidence(tc.reviewedBy, cr, dialect.CodexBotLogin); got != tc.want {
				t.Fatalf("DoneExceptWithEvidence() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideFireBlockedCodexDeferred: an account block with the degrade flag
// posts only the Codex command and keeps the round queued for CodeRabbit.
func TestDecideFireBlockedCodexDeferred(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	blocked := now.Add(25 * time.Minute)
	g := Global{SlotFree: true, BlockedUntil: &blocked}
	head := "abcdef123"
	queued := state.Round{Repo: "owner/repo", PR: 448, Head: head, Phase: state.PhaseQueued, Seq: 1}
	open := Observation{Head: head, Open: true}

	degrade := policy
	degrade.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	degrade = withCodex(degrade, "@codex review")
	degrade.RateLimitCoDegrade = true

	if d := DecideFire(g, queued, open, now, degrade); d.Verdict != FireCoDeferred {
		t.Fatalf("blocked + degrade + postable codex must defer to codex, got %+v", d)
	}
	// Flag off → today's behavior.
	off := degrade
	off.RateLimitCoDegrade = false
	if d := DecideFire(g, queued, open, now, off); d.Verdict != FireNo {
		t.Fatalf("degrade off must stay FireNo blocked, got %+v", d)
	}
	// Codex command already posted for this round → plain blocked FireNo.
	posted := queued
	posted.CodexCommandID = 99
	if d := DecideFire(g, posted, open, now, degrade); d.Verdict != FireNo {
		t.Fatalf("an already-posted codex command must not re-defer, got %+v", d)
	}
	// A live command on the PR is adopted as this round's Codex anchor rather
	// than re-posted or left unrecorded.
	cmdObs := Observation{Head: head, Open: true, Co: codexSeen(CoSeen{Commands: []CommandSeen{{ID: 55, CreatedAt: now}}})}
	if d := DecideFire(g, queued, cmdObs, now, degrade); d.Verdict != FireCoDeferred ||
		d.AdoptCo[dialect.CodexBotLogin].ID != 55 ||
		!d.AdoptCo[dialect.CodexBotLogin].CreatedAt.Equal(now) || codexPosted(d) {
		t.Fatalf("a live codex command must be adopted without re-posting, got %+v", d)
	}
	// Auto-active Codex reviews unprompted → nothing to post; blocked FireNo.
	autoObs := Observation{Head: head, Open: true, Co: codexSeen(CoSeen{AutoActive: true})}
	if d := DecideFire(g, queued, autoObs, now, degrade); d.Verdict != FireCoDeferred || !codexPosted(d) {
		t.Fatalf("always-mode codex must be commanded while the primary is blocked, got %+v", d)
	}
	// Unblocked → the normal fire path is untouched.
	if d := DecideFire(Global{SlotFree: true}, queued, open, now, degrade); d.Verdict != FirePost || !codexPosted(d) {
		t.Fatalf("unblocked fire must stay FirePost with codex, got %+v", d)
	}
	// A busy fire slot defers to Codex the same way — Codex needs no slot.
	if d := DecideFire(Global{SlotFree: false}, queued, open, now, degrade); d.Verdict != FireCoDeferred {
		t.Fatalf("slot-busy + degrade + postable codex must defer to codex, got %+v", d)
	}
	// CRQ_RL_CO_DEGRADE governs the ACCOUNT-LIMIT window only, which is what it
	// is named and documented for. Turning it off must not park quota-free
	// co-review work behind an unrelated PR's CodeRabbit slot — that would add a
	// whole in-flight review timeout to bots that never touch the quota.
	if d := DecideFire(Global{SlotFree: false}, queued, open, now, off); d.Verdict != FireCoDeferred {
		t.Fatalf("degrade off must still let a busy slot defer to codex, got %+v", d)
	}
	// It does still suppress the deferral during an account block.
	if d := DecideFire(g, queued, open, now, off); d.Verdict != FireNo {
		t.Fatalf("degrade off must suppress the account-blocked deferral, got %+v", d)
	}
}

// TestCodexOnlyEligibleUnfiredRound: stale PR-level Codex evidence must not
// defer an unfired round — only a head-bound review (or auto-activity) may.
func TestCodexOnlyEligibleUnfiredRound(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	blocked := now.Add(25 * time.Minute)
	queued := state.Round{Repo: "owner/repo", PR: 448, Head: "abcdef123", Phase: state.PhaseQueued, Seq: 1}
	// A round-window flag computed from a zero cutoff (an old SHA-less review)
	// must not qualify an unfired, uncommanded round.
	stale := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{ActiveThisRound: true})}
	if CodexOnlyEligible(queued, stale, &blocked, now) {
		t.Fatal("stale round-window evidence must not defer an unfired round")
	}
	headReviewed := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: dialect.CodexBotLogin, Commit: "abcdef1234567890", SubmittedAt: now}}}
	if !CodexOnlyEligible(queued, headReviewed, &blocked, now) {
		t.Fatal("a codex review of the current head must defer an unfired round")
	}
	auto := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{AutoActive: true})}
	if CodexOnlyEligible(queued, auto, &blocked, now) {
		t.Fatal("auto-activity without current evidence must not defer an unfired round")
	}
	cleanAt := now.Add(-time.Minute)
	cleanHead := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{AutoActive: true}),
		Events: []dialect.BotEvent{{Kind: dialect.EvCoClean, Bot: dialect.CodexBotLogin,
			SHA: "abcdef1234567890", CommentID: 601, CreatedAt: cleanAt, UpdatedAt: cleanAt}}}
	if !CodexOnlyEligible(queued, cleanHead, &blocked, now) {
		t.Fatal("a clean codex summary naming the current head must defer an unfired round")
	}
	// A deferred command anchors the window: command-bound activity (e.g. the
	// usual SHA-less clean comment) qualifies, and an old usage-limit notice
	// from before the command no longer disqualifies.
	commandedAt := now.Add(-5 * time.Minute)
	commanded := queued
	commanded.CodexCommandID = 77
	commanded.CodexCommandedAt = &commandedAt
	answered := Observation{Head: "abcdef123", Open: true, Co: codexSeen(CoSeen{ActiveThisRound: true}),
		Events: []dialect.BotEvent{{Kind: dialect.EvCoUnable, Bot: dialect.CodexBotLogin,
			CommentID: 600, CreatedAt: commandedAt.Add(-time.Hour), UpdatedAt: commandedAt.Add(-time.Hour)}}}
	if !CodexOnlyEligible(commanded, answered, &blocked, now) {
		t.Fatal("command-bound activity must defer a commanded unfired round despite an old usage-limit notice")
	}
}

// TestCompletionBindsPreFireCodexAnswer: a SHA-less Codex clean summary
// delivered after the deferred command but before the delayed CodeRabbit fire
// must still count for the round — the command time is the codex cutoff.
func TestCompletionBindsPreFireCodexAnswer(t *testing.T) {
	r := firedRound(t, "abcdef123")
	commandedAt := r.FiredAt.Add(-10 * time.Minute)
	r.CodexCommandedAt = &commandedAt
	r.CodexCommandID = 77
	gated := policy
	gated.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	crReview := ReviewSeen{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: r.FiredAt.Add(time.Minute)}
	cleanBeforeFire := dialect.BotEvent{Kind: dialect.EvCoClean, Bot: dialect.CodexBotLogin, CommentID: 900,
		CreatedAt: commandedAt.Add(2 * time.Minute), UpdatedAt: commandedAt.Add(2 * time.Minute)}

	obs := Observation{Head: "abcdef123", Open: true, Reviews: []ReviewSeen{crReview},
		Events: []dialect.BotEvent{cleanBeforeFire}}
	if got := Completion(r, obs, gated); !got.Done {
		t.Fatalf("a codex answer after its command but before the deferred fire must count: %+v", got)
	}
	// Evidence from before the command stays excluded.
	early := cleanBeforeFire
	early.CreatedAt = commandedAt.Add(-time.Minute)
	early.UpdatedAt = early.CreatedAt
	obs.Events = []dialect.BotEvent{early}
	if got := Completion(r, obs, gated); got.Done {
		t.Fatalf("evidence older than the codex command must not count: %+v", got)
	}
}

// --- Codex migration helpers ------------------------------------------------
// The pre-registry Codex tests below were written against Policy.CodexCommand
// and the Observation.Codex* fields. Those shims are gone: Codex is now one
// registry entry like any other co-reviewer, so these helpers express the same
// setup through CoReviewers / Observation.Co and the rules under test are
// unchanged.

// withCodex gives p the single Codex co-reviewer entry the legacy tests
// assumed: crq posts its trigger at fire time (the historical
// "configured-required Codex that does not auto-review" behavior).
func withCodex(p Policy, command string) Policy {
	p.CoReviewers = []CoReviewerPolicy{{Login: dialect.CodexBotLogin, Command: command, Trigger: TriggerAlways}}
	return p
}

// codexSeen builds an Observation.Co map holding just Codex's slice.
func codexSeen(c CoSeen) map[string]CoSeen {
	return map[string]CoSeen{dialect.NormalizeBotName(dialect.CodexBotLogin): c}
}

// codexPosted reports whether the decision asks the apply layer to post the
// Codex trigger (the old FireDecision.PostCodex mirror).
func codexPosted(d FireDecision) bool {
	for _, login := range d.PostCo {
		if dialect.IsCodexBot(login) {
			return true
		}
	}
	return false
}

// codexCP extracts p's Codex co-reviewer policy (zero-command entry if absent).
func codexCP(p Policy) CoReviewerPolicy {
	for _, cp := range p.CoReviewers {
		if dialect.IsCodexBot(cp.Login) {
			return cp
		}
	}
	return CoReviewerPolicy{Login: dialect.CodexBotLogin}
}

// A block only ever extends. Evidence comes from more than one place — a rate-limit
// comment and the local CLI — and they can describe different limits, so a narrower
// local reading must never shorten a standing account-wide window.
func TestAcceptAccountBlock(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	standing := now.Add(time.Hour)
	for _, tc := range []struct {
		name     string
		standing *time.Time
		observed time.Time
		want     bool
	}{
		{"first block is recorded", nil, now.Add(time.Minute), true},
		{"a longer window extends", &standing, now.Add(2 * time.Hour), true},
		{"a shorter window is refused", &standing, now.Add(time.Minute), false},
		{"an equal window changes nothing", &standing, standing, false},
		{"a zero observation is not evidence", &standing, time.Time{}, false},
		{"a zero observation with no standing block is still nothing", nil, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptAccountBlock(tc.standing, tc.observed); got != tc.want {
				t.Errorf("AcceptAccountBlock = %v, want %v", got, tc.want)
			}
		})
	}
}

// A reopened round asks whether the primary already answered THIS round's
// command, and answering yes writes a completed marker every later same-head
// check skips — so the gate must hold to Completion's evidence, not to the
// weaker adoption question CommandHasCompletionReply answers.
func TestReopenedRoundDedupesOnlyOnCompletionEvidence(t *testing.T) {
	free := Global{SlotFree: true}
	now := t0.Add(10 * time.Minute)
	head := "abcdef123"

	reopened := func(t *testing.T) state.Round {
		t.Helper()
		r := firedRound(t, head)
		if err := r.Complete(); err != nil {
			t.Fatal(err)
		}
		if err := r.Reopen(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	command := dialect.BotEvent{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001,
		CreatedAt: t0.Add(2 * time.Second), UpdatedAt: t0.Add(2 * time.Second)}
	completion := dialect.BotEvent{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002,
		AutoReply: true, CreatedAt: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute)}
	priorReview := ReviewSeen{Bot: "coderabbitai[bot]", Commit: "0000000099", SubmittedAt: t0.Add(-time.Hour)}

	cases := []struct {
		name string
		obs  Observation
		want FireVerdict
	}{
		{
			// The reply stands in for a re-review that found nothing new.
			name: "completion reply with a prior review",
			obs: Observation{Head: head, Open: true, Reviews: []ReviewSeen{priorReview},
				Events: []dialect.BotEvent{command, completion}},
			want: FireDedupe,
		},
		{
			// A clean-summary verdict is Completion's other no-Review evidence.
			// Reopening the marker for a reviewer change must not buy the same
			// primary review again.
			name: "clean summary for this round",
			obs: Observation{Head: head, Open: true, Events: []dialect.BotEvent{{
				Kind: dialect.EvNoAction, Bot: "coderabbitai[bot]", CommentID: 1003,
				CreatedAt: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute),
			}}},
			want: FireDedupe,
		},
		{
			// Nothing to stand in for: the bot has never submitted a review, so
			// the head has not been reviewed and the round must still fire.
			name: "completion reply with no review anywhere",
			obs: Observation{Head: head, Open: true,
				Events: []dialect.BotEvent{command, completion}},
			want: FirePost,
		},
		{
			// The review failed after answering, so the reply describes a review
			// that did not land.
			name: "failed summary after the completion reply",
			obs: Observation{Head: head, Open: true, Reviews: []ReviewSeen{priorReview},
				Events: []dialect.BotEvent{command, completion,
					{Kind: dialect.EvFailed, Bot: "coderabbitai[bot]", CommentID: 900,
						CreatedAt: t0, UpdatedAt: t0.Add(2 * time.Minute)}}},
			want: FirePost,
		},
		{
			// Each half used to find its own reply: the round's failed command
			// satisfied the command-id check, while a later successful command
			// satisfied the completion-evidence check.
			name: "later command cannot repair this round's failed reply",
			obs: Observation{Head: head, Open: true, Reviews: []ReviewSeen{priorReview},
				Events: []dialect.BotEvent{
					command,
					completion,
					{Kind: dialect.EvFailed, Bot: "coderabbitai[bot]", CommentID: 900,
						CreatedAt: t0, UpdatedAt: t0.Add(2 * time.Minute)},
					{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 2001,
						CreatedAt: t0.Add(3 * time.Minute), UpdatedAt: t0.Add(3 * time.Minute)},
					{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 2002,
						AutoReply: true, CreatedAt: t0.Add(4 * time.Minute), UpdatedAt: t0.Add(4 * time.Minute)},
				}},
			want: FirePost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := DecideFire(free, reopened(t), tc.obs, now, policy); d.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v (%+v)", d.Verdict, tc.want, d)
			}
		})
	}
}

// TestPrimaryOffNeverFiresAndNeverWaitsOnTheQuota pins the repository-level
// primary switch. A private repo on a free plan gets nothing from the metered
// reviewer, so crq must neither spend the account on it nor let that account's
// state — or another PR's fire slot — delay the co-reviewers that DO run there.
func TestPrimaryOffNeverFiresAndNeverWaitsOnTheQuota(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	head := "abcdef123"
	queued := state.Round{Repo: "owner/private", PR: 7, Head: head, Phase: state.PhaseQueued, Seq: 1}
	obs := Observation{Head: head, Open: true}

	off := withCodex(policy, "@codex review")
	off.PrimaryOff = true
	off.RequiredBots = []string{dialect.CodexBotLogin} // ForRepo drops the primary

	blocked := now.Add(time.Hour)
	for _, g := range []Global{
		{SlotFree: true},
		{SlotFree: false},                        // another PR holds the slot
		{SlotFree: true, BlockedUntil: &blocked}, // account quota blocked
		{SlotFree: true, LastFired: &now},        // inside the pacing window
	} {
		d := DecideFire(g, queued, obs, now, off)
		if d.Verdict != FireCoOnly {
			t.Fatalf("primary off must resolve on the co-reviewers alone whatever the account is doing, got %+v (global %+v)", d, g)
		}
		if len(d.PostCo) != 1 || d.PostCo[0] != dialect.CodexBotLogin {
			t.Fatalf("primary off must still command the co-reviewers, got %+v", d.PostCo)
		}
		if d.Reason != "primary disabled for this repository; co-review still required" {
			t.Fatalf("primary-off reason must not imply a failed or completed review, got %q", d.Reason)
		}
	}

	// With the primary on, the same round is an ordinary metered fire — the
	// switch is what changed the decision, not the observation.
	on := off
	on.PrimaryOff = false
	on.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	if d := DecideFire(Global{SlotFree: true}, queued, obs, now, on); d.Verdict != FirePost {
		t.Fatalf("primary on must still fire, got %+v", d)
	}

	// And the reason is reportable, so an agent is never left reasoning about
	// quota for a reviewer that does not run here.
	if why := PrimaryUnavailableReason(obs, off, head); why == "" {
		t.Fatal("primary off must name itself as the reason no review is coming")
	}
}
