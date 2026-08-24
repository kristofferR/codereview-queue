package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

const (
	nextHead    = "a21da4aeb"
	nextPrimary = "coderabbitai[bot]"
	nextCoBot   = "bugbot[bot]"
)

func completionOf(reviewed map[string]bool) CompletionStatus {
	done := true
	for _, ok := range reviewed {
		if !ok {
			done = false
		}
	}
	return CompletionStatus{ReviewedBy: reviewed, Done: done}
}

func openObs() Observation { return Observation{Head: nextHead, Open: true} }

func finding(commit string) dialect.Finding {
	return dialect.Finding{Bot: "coderabbitai[bot]", Title: "nil deref", Commit: commit, ThreadID: "T1"}
}

func TestRoundPhaseRules(t *testing.T) {
	for _, tc := range []struct {
		name            string
		phase           state.Phase
		reviewRequested bool
		canStillFire    bool
	}{
		{"missing", "", false, false},
		{"queued", state.PhaseQueued, false, true},
		{"reserved", state.PhaseReserved, true, true},
		{"fired", state.PhaseFired, true, false},
		{"reviewing", state.PhaseReviewing, true, false},
		{"awaiting retry", state.PhaseAwaitingRetry, true, true},
		{"completed", state.PhaseCompleted, true, false},
		{"abandoned", state.PhaseAbandoned, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			round := state.Round{Phase: tc.phase}
			if got := reviewRequested(round); got != tc.reviewRequested {
				t.Errorf("reviewRequested(%q) = %t, want %t", tc.phase, got, tc.reviewRequested)
			}
			if got := CanStillFire(round); got != tc.canStillFire {
				t.Errorf("CanStillFire(%q) = %t, want %t", tc.phase, got, tc.canStillFire)
			}
		})
	}
}

// The action table. Each row is a claim about what a caller must do next, and
// together they are the whole contract `crq next` exposes.
func TestNextAction(t *testing.T) {
	both := func(cr, codex bool) CompletionStatus {
		return completionOf(map[string]bool{nextPrimary: cr, "chatgpt-codex-connector[bot]": codex})
	}
	deferredUntil := t0.Add(30 * time.Minute)
	coPendingRound := state.Round{
		Phase:    state.PhaseCompleted,
		Dispatch: &state.DispatchClaim{Heartbeat: t0},
	}
	coPendingRound.SetCoCommand(nextCoBot, 42, t0.Add(-time.Minute))

	cases := []struct {
		name    string
		in      NextInput
		want    ActionKind
		wantAt  time.Time // zero = don't check
		pending []string
	}{
		{
			name: "closed pr needs a human",
			in:   NextInput{Obs: Observation{Head: nextHead, Open: false}},
			want: ActionBlocked,
		},
		{
			name: "unreadable head is transient, not terminal",
			in:   NextInput{Obs: Observation{Open: true}, MinDelay: time.Minute},
			want: ActionWait, wantAt: t0.Add(time.Minute),
		},
		{
			name: "findings come before everything else",
			in: NextInput{
				Obs: openObs(), Completion: both(true, true),
				Findings: []dialect.Finding{finding(nextHead)}, LocalWork: true,
			},
			want: ActionFix,
		},
		{
			name: "live fix session holds findings while a reviewer is reading",
			in: NextInput{
				Round: state.Round{
					Phase:    state.PhaseReviewing,
					Dispatch: &state.DispatchClaim{Heartbeat: t0},
				},
				Obs: openObs(), Completion: both(false, true), LocalWork: true,
				Findings: []dialect.Finding{finding(nextHead)}, MinDelay: time.Minute,
				Primary: nextPrimary,
			},
			want: ActionHold, wantAt: t0.Add(time.Minute),
			pending: []string{nextPrimary},
		},
		{
			name: "live fix session holds findings while a co-reviewer is reading",
			in: NextInput{
				Round: coPendingRound,
				Obs:   openObs(),
				Completion: completionOf(map[string]bool{
					nextPrimary: true, "chatgpt-codex-connector[bot]": true, nextCoBot: false,
				}),
				LocalWork: true, Findings: []dialect.Finding{finding(nextHead)},
				Deferred: true, DeferredUntil: &deferredUntil,
				MinDelay: time.Minute, Primary: nextPrimary,
			},
			want: ActionHold, wantAt: t0.Add(time.Minute),
			pending: []string{nextCoBot},
		},
		{
			name: "live fix session may replace a queued review that nobody is reading",
			in: NextInput{
				Round: state.Round{
					Phase:             state.PhaseAwaitingRetry,
					DispatchHoldPhase: state.PhaseQueued,
					Dispatch:          &state.DispatchClaim{Heartbeat: t0},
				},
				Obs: openObs(), Completion: both(false, true), LocalWork: true,
				Findings: []dialect.Finding{finding(nextHead)}, MinDelay: time.Minute,
				Primary: nextPrimary,
			},
			want: ActionFix,
		},
		{
			// The rule agents break most: a finding is in hand but a required bot
			// has not spoken, so the head must not move.
			name: "pending reviewer holds the head when work is staged",
			in: NextInput{
				Round: state.Round{Phase: state.PhaseReviewing},
				Obs:   openObs(), Completion: both(false, true), LocalWork: true,
				MinDelay: time.Minute,
			},
			want: ActionHold, wantAt: t0.Add(time.Minute),
			pending: []string{"coderabbitai[bot]"},
		},
		{
			name: "pending reviewer with nothing staged is just a wait",
			in: NextInput{
				Obs: openObs(), Completion: both(false, true), MinDelay: time.Minute,
			},
			want: ActionWait, wantAt: t0.Add(time.Minute),
			pending: []string{"coderabbitai[bot]"},
		},
		{
			// A rate-limit degrade RELEASES the head: the queued CodeRabbit review
			// fires against whatever head exists when the window opens, so holding
			// costs a whole window and buys nothing.
			name: "rate-limit degrade releases the head instead of holding it",
			in: NextInput{
				Obs: openObs(), Completion: both(false, true), LocalWork: true,
				Deferred: true, DeferredUntil: &deferredUntil, MinDelay: time.Minute,
			},
			want: ActionPush,
		},
		{
			name: "rate-limit degrade with nothing staged waits out the window",
			in: NextInput{
				Round: state.Round{Phase: state.PhaseQueued},
				Obs:   openObs(), Completion: both(false, true),
				Deferred: true, DeferredUntil: &deferredUntil, MinDelay: time.Minute,
			},
			want: ActionWait, wantAt: deferredUntil,
			pending: []string{"coderabbitai[bot]"},
		},
		{
			// The degrade releases the head from the PRIMARY only. Another
			// required reviewer is still mid-review, and pushing would restart it.
			name: "rate-limit degrade still holds for a pending co-reviewer",
			in: NextInput{
				Obs: openObs(),
				Completion: completionOf(map[string]bool{
					nextPrimary: false, "chatgpt-codex-connector[bot]": true, nextCoBot: false,
				}),
				LocalWork: true,
				Deferred:  true, DeferredUntil: &deferredUntil, MinDelay: time.Minute,
			},
			want: ActionHold, wantAt: t0.Add(time.Minute),
			pending: []string{nextCoBot, nextPrimary},
		},
		{
			// ...and it must keep the short cadence while doing so. The account
			// window has no hold over a co-reviewer, so sleeping until it opens
			// would miss the very findings the degrade exists to deliver.
			name: "rate-limit degrade rechecks soon while a co-reviewer is owed",
			in: NextInput{
				Round: state.Round{Phase: state.PhaseQueued},
				Obs:   openObs(),
				Completion: completionOf(map[string]bool{
					nextPrimary: false, "chatgpt-codex-connector[bot]": true, nextCoBot: false,
				}),
				Global:   Global{BlockedUntil: &deferredUntil},
				Deferred: true, DeferredUntil: &deferredUntil, MinDelay: time.Minute,
			},
			want: ActionWait, wantAt: t0.Add(time.Minute),
			pending: []string{nextCoBot, nextPrimary},
		},
		{
			// A threadless finding is repeated rather than suppressed. Hiding it
			// once the tree is dirty was tried and is worse: an unrelated dirty
			// file would then bury a finding the caller never saw, and a
			// threadless review_skipped has no resolution state to recover from.
			name: "threadless finding keeps being reported, not swallowed",
			in: NextInput{
				Obs: openObs(), Completion: both(true, true), LocalWork: true,
				Findings: []dialect.Finding{{Bot: nextPrimary, Title: "body finding", Commit: nextHead}},
			},
			want: ActionFix,
		},
		{
			// Holding protects a review that is actually running. With no round,
			// nothing was ever requested for this head, so holding would stall the
			// caller and spend a window on code it is about to replace.
			name: "no round means land the work rather than hold for nobody",
			in: NextInput{
				Obs: openObs(), Completion: both(false, false), LocalWork: true,
			},
			want: ActionPush,
		},
		{
			// A queued round has asked for nothing either — and `crq dismiss`
			// creates one to record its decision, so the documented fix flow
			// reaches here holding the very fixes the review would be spent on.
			name: "a queued round is not a review to hold for",
			in: NextInput{
				Round: state.Round{Phase: state.PhaseQueued},
				Obs:   openObs(), Completion: both(false, false), LocalWork: true,
			},
			want: ActionPush,
		},
		{
			// The other side of that boundary: reserving the slot IS the request,
			// so the command is imminent and the head must stop moving.
			name: "a reserved round is a review to hold for",
			in: NextInput{
				Round: state.Round{Phase: state.PhaseReserved},
				Obs:   openObs(), Completion: both(false, false), LocalWork: true,
				MinDelay: time.Minute,
			},
			want: ActionHold, wantAt: t0.Add(time.Minute),
		},
		{
			name: "all answered with staged work means push",
			in: NextInput{
				Obs: openObs(), Completion: both(true, true), LocalWork: true,
			},
			want: ActionPush,
		},
		{
			name: "all answered with nothing staged is convergence",
			in: NextInput{
				Obs: openObs(), Completion: both(true, true),
			},
			want: ActionDone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Primary is configuration, not scenario: default it so each row
			// stays a statement about the review state alone.
			if tc.in.Primary == "" {
				tc.in.Primary = nextPrimary
			}
			got := NextAction(tc.in, t0)
			if got.Kind != tc.want {
				t.Fatalf("NextAction = %q (%s), want %q", got.Kind, got.Reason, tc.want)
			}
			if !tc.wantAt.IsZero() && !got.At.Equal(tc.wantAt) {
				t.Errorf("At = %s, want %s", got.At, tc.wantAt)
			}
			if len(tc.pending) > 0 {
				if len(got.Pending) != len(tc.pending) {
					t.Fatalf("Pending = %v, want %v", got.Pending, tc.pending)
				}
				for i := range tc.pending {
					if got.Pending[i] != tc.pending[i] {
						t.Fatalf("Pending = %v, want %v", got.Pending, tc.pending)
					}
				}
			}
			// A caller must never be told to act in the past.
			if got.Kind == ActionWait || got.Kind == ActionHold {
				if !got.At.After(t0) {
					t.Errorf("At = %s is not in the future", got.At)
				}
			}
		})
	}
}

// Every wait respects MinDelay, so no caller can hot-loop crq next — the floor
// is arithmetic, not a documented request.
func TestNextActionNeverWaitsLessThanMinDelay(t *testing.T) {
	passed := t0.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		in   NextInput
	}{
		{"plain wait", NextInput{Obs: openObs(), Completion: completionOf(map[string]bool{"coderabbitai[bot]": false})}},
		{"elapsed account block", NextInput{
			Obs:        openObs(),
			Round:      state.Round{Phase: state.PhaseQueued},
			Global:     Global{BlockedUntil: &passed},
			Completion: completionOf(map[string]bool{"coderabbitai[bot]": false}),
		}},
		{"elapsed retry window", NextInput{
			Obs:        openObs(),
			Round:      state.Round{Phase: state.PhaseAwaitingRetry, RetryAt: &passed},
			Completion: completionOf(map[string]bool{"coderabbitai[bot]": false}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.MinDelay = 30 * time.Second
			got := NextAction(tc.in, t0)
			if want := t0.Add(30 * time.Second); got.At.Before(want) {
				t.Errorf("At = %s, want >= %s", got.At, want)
			}
		})
	}
}

// A waiting round sleeps past the gates that actually hold it; a round already
// under review does not, because its answer can land at any moment.
func TestNextActionGatesOnlyApplyToWaitingRounds(t *testing.T) {
	blocked := t0.Add(40 * time.Minute)
	retry := t0.Add(20 * time.Minute)
	pending := completionOf(map[string]bool{nextPrimary: false})

	waiting := NextAction(NextInput{
		Round:      state.Round{Phase: state.PhaseAwaitingRetry, RetryAt: &retry},
		Obs:        openObs(),
		Global:     Global{BlockedUntil: &blocked},
		Completion: pending,
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0)
	if !waiting.At.Equal(blocked) {
		t.Errorf("waiting round: At = %s, want the later gate %s", waiting.At, blocked)
	}

	reviewing := NextAction(NextInput{
		Round:      state.Round{Phase: state.PhaseReviewing},
		Obs:        openObs(),
		Global:     Global{BlockedUntil: &blocked},
		Completion: pending,
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0)
	if !reviewing.At.Equal(t0.Add(time.Minute)) {
		t.Errorf("reviewing round: At = %s, want a poll-interval recheck, not the account block", reviewing.At)
	}
}

// Those same gates belong to the primary alone. A queued round that is also
// waiting on a co-reviewer must keep the short cadence: neither the account
// window nor this round's fire cooldown has any hold over that bot, so sleeping
// until they clear would sleep through its answer.
func TestNextActionKeepsShortCadenceForPendingCoReviewers(t *testing.T) {
	blocked := t0.Add(40 * time.Minute)
	retry := t0.Add(20 * time.Minute)

	got := NextAction(NextInput{
		Round:      state.Round{Phase: state.PhaseAwaitingRetry, RetryAt: &retry},
		Obs:        openObs(),
		Global:     Global{BlockedUntil: &blocked},
		Completion: completionOf(map[string]bool{nextPrimary: false, nextCoBot: false}),
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0)
	if got.Kind != ActionWait {
		t.Fatalf("NextAction = %q (%s), want %q", got.Kind, got.Reason, ActionWait)
	}
	if !got.At.Equal(t0.Add(time.Minute)) {
		t.Errorf("At = %s, want a poll-interval recheck while %s is still owed", got.At, nextCoBot)
	}
}

// Findings carried from an older commit that have no thread to resolve must not
// pin the loop on "fix" forever — BlockingFindings already drops them, and
// NextAction must move on to the review state.
func TestNextActionIgnoresUnresolvableStaleFindings(t *testing.T) {
	stale := dialect.Finding{Bot: "coderabbitai[bot]", Title: "old", Commit: "deadbeef1"}
	got := NextAction(NextInput{
		Obs:        openObs(),
		Completion: completionOf(map[string]bool{"coderabbitai[bot]": true}),
		Findings:   []dialect.Finding{stale},
	}, t0)
	if got.Kind != ActionDone {
		t.Fatalf("NextAction = %q (%s), want %q", got.Kind, got.Reason, ActionDone)
	}
}

// The account window gates the PRIMARY's review and nothing else. In the default
// configuration only the primary is required, so a co-reviewer commanded for
// this round never appears in ReviewedBy — and the caller would be told to sleep
// until the window opened, hours later, straight through the co-review findings
// the degrade exists to deliver.
func TestNextActionKeepsShortCadenceForACommandedCoReviewer(t *testing.T) {
	blocked := t0.Add(90 * time.Minute)
	commanded := t0.Add(-time.Minute)
	round := state.Round{Phase: state.PhaseQueued}
	round.CoBots = map[string]state.CoBotRound{
		"chatgpt-codex-connector": {CommandID: 42, CommandedAt: &commanded},
	}

	got := NextAction(NextInput{
		Round:      round,
		Obs:        openObs(),
		Global:     Global{BlockedUntil: &blocked},
		Completion: completionOf(map[string]bool{nextPrimary: false}),
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0)
	if !got.At.Equal(t0.Add(time.Minute)) {
		t.Errorf("At = %s, want a poll-interval recheck while a co-review is in flight", got.At)
	}
}

// A primary that acknowledges the command and then never submits a review leaves
// the round reviewing forever — Progress deliberately does not time that out,
// because the legacy Loop owned the deadline. Without an actionable verdict here
// the newly recommended flow idles indefinitely on a bot that crashed.
func TestNextActionReportsAnExpiredReviewWait(t *testing.T) {
	expired := t0.Add(-time.Minute)
	got := NextAction(NextInput{
		Round:      state.Round{Phase: state.PhaseReviewing, WaitDeadline: &expired},
		Obs:        openObs(),
		Completion: completionOf(map[string]bool{nextPrimary: false}),
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0)
	if got.Kind != ActionBlocked {
		t.Fatalf("NextAction = %q (%s), want %q", got.Kind, got.Reason, ActionBlocked)
	}

	// An unexpired deadline is still just a wait.
	live := t0.Add(time.Hour)
	if got := NextAction(NextInput{
		Round:      state.Round{Phase: state.PhaseReviewing, WaitDeadline: &live},
		Obs:        openObs(),
		Completion: completionOf(map[string]bool{nextPrimary: false}),
		Primary:    nextPrimary,
		MinDelay:   time.Minute,
	}, t0); got.Kind != ActionWait {
		t.Fatalf("NextAction = %q (%s), want %q while the deadline is live", got.Kind, got.Reason, ActionWait)
	}
}

// Bots deliver in waves, so the first clean observation is not an answer yet.
// The legacy loop held its verdict for a settle window; a stateless caller needs
// the same guarantee or it stops moments before the findings land.
func TestNextActionHoldsConvergenceThroughTheSettleWindow(t *testing.T) {
	settleUntil := t0.Add(90 * time.Second)
	got := NextAction(NextInput{
		Obs:         openObs(),
		Completion:  completionOf(map[string]bool{nextPrimary: true}),
		Primary:     nextPrimary,
		SettleUntil: &settleUntil,
		MinDelay:    time.Minute,
	}, t0)
	if got.Kind != ActionWait {
		t.Fatalf("NextAction = %q (%s), want %q inside the settle window", got.Kind, got.Reason, ActionWait)
	}
	if !got.At.Equal(settleUntil) {
		t.Errorf("At = %s, want the settle boundary %s", got.At, settleUntil)
	}

	// Once quiet, it converges.
	passed := t0.Add(-time.Second)
	if got := NextAction(NextInput{
		Obs:         openObs(),
		Completion:  completionOf(map[string]bool{nextPrimary: true}),
		Primary:     nextPrimary,
		SettleUntil: &passed,
	}, t0); got.Kind != ActionDone {
		t.Fatalf("NextAction = %q (%s), want %q once settled", got.Kind, got.Reason, ActionDone)
	}
}

// A session that is holding a round must not be told to hold for a reviewer.
//
// ClaimDispatch mirrors a queued round into awaiting_retry so older binaries
// honour the exclusion, and reviewRequested read that as "a review was
// requested for this head". The session was then told `hold`, waited for a
// review nobody could ask for — the claim makes the round ineligible to fire —
// and its own heartbeat extended the window it was waiting on. It ended by
// exiting with a commit it never pushed.
func TestADispatchHoldIsNotAReviewRequest(t *testing.T) {
	now := time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC)
	retry := now.Add(10 * time.Minute)
	for _, heldPhase := range []state.Phase{state.PhaseQueued, state.PhaseAwaitingRetry} {
		round := state.Round{
			Repo: "o/r", PR: 1, Head: "aaaaaaaa1",
			Phase:             state.PhaseAwaitingRetry,
			DispatchHoldPhase: heldPhase,
			RetryAt:           &retry,
			EnqueuedAt:        now.Add(-time.Minute),
		}

		got := NextAction(NextInput{
			Round:      round,
			Obs:        Observation{Head: "aaaaaaaa1", Open: true},
			Completion: CompletionStatus{ReviewedBy: map[string]bool{"coderabbitai[bot]": false}},
			Primary:    "coderabbitai[bot]",
			LocalWork:  true,
		}, now)
		if got.Kind != ActionPush {
			t.Errorf("held phase %s: action = %s (%s), want push: no review is active for this head", heldPhase, got.Kind, got.Reason)
		}
	}
}
