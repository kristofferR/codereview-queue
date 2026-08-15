package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

const (
	bugbotLogin = dialect.BugbotLogin
	macroLogin  = dialect.MacroscopeLogin
)

func TestCoReviewerActiveIgnoresHeadScope(t *testing.T) {
	cases := []struct {
		name string
		obs  Observation
		want bool
	}{
		{
			name: "review on an earlier head",
			obs:  Observation{Head: "new", Reviews: []ReviewSeen{{Bot: bugbotLogin, Commit: "old"}}},
			want: true,
		},
		{
			name: "classified comment",
			obs:  Observation{Head: "new", Events: []dialect.BotEvent{{Bot: bugbotLogin}}},
			want: true,
		},
		{
			name: "comment attributed to reviewer",
			obs:  Observation{Head: "new", Events: []dialect.BotEvent{{Bot: macroLogin, For: bugbotLogin}}},
			want: true,
		},
		{
			name: "trigger command attributed to reviewer",
			obs: Observation{Head: "new", Events: []dialect.BotEvent{{
				Kind: dialect.EvCoCommand, Bot: "human", For: bugbotLogin,
			}}},
		},
		{
			name: "explicit attribution takes precedence over author",
			obs:  Observation{Head: "new", Events: []dialect.BotEvent{{Bot: bugbotLogin, For: macroLogin}}},
		},
		{
			name: "check activity",
			obs:  Observation{Head: "new", Checks: []CheckSeen{{Bot: bugbotLogin}}},
			want: true,
		},
		{
			name: "different reviewer",
			obs:  Observation{Head: "new", Reviews: []ReviewSeen{{Bot: macroLogin, Commit: "old"}}},
		},
		{name: "no activity", obs: Observation{Head: "new"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoReviewerActive(tc.obs, bugbotLogin); got != tc.want {
				t.Fatalf("CoReviewerActive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideCoPostTriggerMatrix is the trigger-mode decision matrix:
// never/selfheal/always crossed with the bot's observed activity, plus the
// mode-independent guards (already commanded, live command, head evidence,
// check run present including in-progress).
func TestDecideCoPostTriggerMatrix(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	grace := 10 * time.Minute
	freshAnchor := now.Add(-time.Minute)
	staleAnchor := now.Add(-time.Hour)

	policy := func(mode TriggerMode) CoReviewerPolicy {
		return CoReviewerPolicy{Login: bugbotLogin, Command: "bugbot run", Trigger: mode, SelfHealGrace: grace}
	}
	obsWith := func(co CoSeen, checks ...CheckSeen) Observation {
		return Observation{Head: head, Open: true, Checks: checks,
			Co: map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): co}}
	}
	commanded := state.Round{Head: head}
	commanded.SetCoCommand(bugbotLogin, 42, now.Add(-time.Hour))
	seenAt := now.Add(-2 * time.Hour)
	seen := state.Round{Head: head, CoBots: map[string]state.CoBotRound{
		dialect.NormalizeBotName(bugbotLogin): {SeenActiveAt: &seenAt},
	}}
	queuedAt := now.Add(-2 * time.Hour)
	firedAt := now.Add(-30 * time.Minute)
	firedAfterUnable := seen
	firedAfterUnable.EnqueuedAt = queuedAt
	firedAfterUnable.FiredAt = &firedAt

	cases := []struct {
		name           string
		cp             CoReviewerPolicy
		round          state.Round
		obs            Observation
		commandPresent bool
		anchor         time.Time
		want           bool
	}{
		{name: "never posts nothing even when silent", cp: policy(TriggerNever), obs: obsWith(CoSeen{}), anchor: staleAnchor, want: false},
		{name: "always posts for a silent bot", cp: policy(TriggerAlways), obs: obsWith(CoSeen{}), want: true},
		{name: "always overrides historical auto-review activity", cp: policy(TriggerAlways), obs: obsWith(CoSeen{AutoActive: true}), want: true},
		{name: "always respects a live command", cp: policy(TriggerAlways), obs: obsWith(CoSeen{}), commandPresent: true, want: false},
		{name: "always respects the recorded round command", cp: policy(TriggerAlways), round: commanded, obs: obsWith(CoSeen{}), want: false},
		{
			name: "always never double-reviews a completed check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: now}),
			want: false,
		},
		{
			name: "always waits out an in-progress check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}),
			want: false,
		},
		{
			name: "always ignores another bot's check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: macroLogin, Verdict: dialect.CheckDone, CompletedAt: now}),
			want: true,
		},
		{
			name: "always posts when checks are unreadable",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{ChecksUnknown: true}),
			want: true,
		},
		{name: "selfheal stays quiet for an inactive bot", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{}), anchor: staleAnchor, want: false},
		{name: "selfheal stays quiet when checks are unreadable", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true, ChecksUnknown: true}), anchor: staleAnchor, want: false},
		{name: "selfheal posts for an active bot that missed the head past grace", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), anchor: staleAnchor, want: true},
		{name: "selfheal counts round activity as active", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{ActiveThisRound: true}), anchor: staleAnchor, want: true},
		{name: "selfheal counts activity carried from a previous head", cp: policy(TriggerSelfHeal), round: seen, obs: obsWith(CoSeen{}), anchor: staleAnchor, want: true},
		{name: "selfheal respects an unable notice after carried activity", cp: policy(TriggerSelfHeal), round: seen,
			obs: Observation{Head: head, Open: true, Events: []dialect.BotEvent{{Kind: dialect.EvCoUnable, Bot: bugbotLogin,
				CreatedAt: staleAnchor.Add(time.Minute), UpdatedAt: staleAnchor.Add(time.Minute)}}}, anchor: staleAnchor, want: false},
		{name: "selfheal preserves an unable notice received before the primary fired", cp: policy(TriggerSelfHeal), round: firedAfterUnable,
			obs: Observation{Head: head, Open: true, Events: []dialect.BotEvent{{Kind: dialect.EvCoUnable, Bot: bugbotLogin,
				CreatedAt: queuedAt.Add(time.Minute), UpdatedAt: queuedAt.Add(time.Minute)}}}, anchor: firedAt, want: false},
		{name: "selfheal preserves an unable notice received before enqueue for this head", cp: policy(TriggerSelfHeal), round: firedAfterUnable,
			obs: Observation{Head: head, Open: true, HeadAt: queuedAt.Add(-time.Hour), Events: []dialect.BotEvent{{Kind: dialect.EvCoUnable, Bot: bugbotLogin,
				CreatedAt: queuedAt.Add(-time.Minute), UpdatedAt: queuedAt.Add(-time.Minute)}}}, anchor: firedAt, want: false},
		{name: "selfheal waits out the grace period", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), anchor: freshAnchor, want: false},
		{name: "selfheal needs an anchor", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), want: false},
		{
			name: "selfheal never re-triggers a bot with head evidence",
			cp:   policy(TriggerSelfHeal),
			obs: Observation{Head: head, Open: true,
				Reviews: []ReviewSeen{{Bot: bugbotLogin, Commit: "abcdef1234567890", SubmittedAt: now.Add(-time.Minute)}},
				Co:      map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): {AutoActive: true}}},
			anchor: staleAnchor, want: false,
		},
		{
			name:   "selfheal suppressed by an in-progress check",
			cp:     policy(TriggerSelfHeal),
			obs:    obsWith(CoSeen{AutoActive: true}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}),
			anchor: staleAnchor, want: false,
		},
		{name: "no command configured never posts", cp: CoReviewerPolicy{Login: bugbotLogin, Trigger: TriggerAlways}, obs: obsWith(CoSeen{}), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.round.Head == "" {
				tc.round = state.Round{Head: head}
			}
			if got := DecideCoPost(tc.round, tc.obs, tc.cp, tc.commandPresent, tc.anchor, now); got != tc.want {
				t.Fatalf("DecideCoPost = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideCoPostHonorsReviewerChangeForce(t *testing.T) {
	cp := CoReviewerPolicy{
		Login:   "cursor[bot]",
		Command: "@cursor review",
		Trigger: TriggerSelfHeal,
	}
	round := state.Round{ForceCoReviewers: []string{"cursor[bot]"}}
	if !DecideCoPost(round, Observation{}, cp, false, time.Now().Add(-time.Minute), time.Now()) {
		t.Fatal("a newly required self-heal reviewer must be triggered without prior activity")
	}
}

// TestCompletionCheckEvidence pins step 2b and the generic dynamic gate: a
// required check-bearing bot converges on its completed check run alone (the
// silent-clean Bugbot round), an in-progress check does not, and a
// wanted-only bot gates exactly when observed active.
func TestCompletionCheckEvidence(t *testing.T) {
	fired := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	round := state.Round{Head: head, FiredAt: &fired}
	crReview := ReviewSeen{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: fired.Add(time.Minute)}
	bugbotKey := dialect.NormalizeBotName(bugbotLogin)

	required := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", bugbotLogin},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}

	// Required Bugbot, no evidence: not done.
	silent := Observation{Head: head, Open: true, Reviews: []ReviewSeen{crReview}}
	if got := Completion(round, silent, required); got.Done {
		t.Fatalf("required bugbot with no evidence must gate: %+v", got)
	}
	// Its clean check run alone converges the round.
	clean := silent
	clean.Checks = []CheckSeen{{Bot: bugbotLogin, Name: "Cursor Bugbot", Verdict: dialect.CheckDoneClean, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, clean, required); !got.Done {
		t.Fatalf("a completed clean check must converge the silent-clean round: %+v", got)
	}
	// A completed check with findings converges too — the findings gate via
	// threads in the feedback layer, not via Completion.
	issues := silent
	issues.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDone, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, issues, required); !got.Done {
		t.Fatalf("a completed check with findings must still mark the bot reviewed: %+v", got)
	}
	// An in-progress check is not evidence.
	running := silent
	running.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}}
	if got := Completion(round, running, required); got.Done {
		t.Fatalf("an in-progress check must not converge the round: %+v", got)
	}

	// Wanted-only (not required): an inactive bot never gates.
	wanted := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}
	if got := Completion(round, silent, wanted); !got.Done {
		t.Fatalf("a wanted-only inactive bot must not gate: %+v", got)
	}
	// Observed active this round (in-progress check), it gates dynamically…
	activeRunning := running
	activeRunning.Co = map[string]CoSeen{bugbotKey: {ActiveThisRound: true}}
	if got := Completion(round, activeRunning, wanted); got.Done {
		t.Fatalf("an active wanted bot must gate until its evidence lands: %+v", got)
	}
	// …and its completed check satisfies the dynamic gate.
	activeDone := activeRunning
	activeDone.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, activeDone, wanted); !got.Done {
		t.Fatalf("the active bot's completed check must satisfy the gate: %+v", got)
	}
}

// TestCompletionVerdictIsParticipationOnly: a Macroscope approvability verdict
// engages round participation (CoActiveThisRound) but never counts as review
// evidence.
func TestCompletionVerdictIsParticipationOnly(t *testing.T) {
	fired := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	round := state.Round{Head: head, FiredAt: &fired}
	approved := true
	verdict := dialect.BotEvent{Kind: dialect.EvCoVerdict, Bot: macroLogin, For: macroLogin,
		Approved: &approved, CommentID: 9, CreatedAt: fired.Add(time.Minute), UpdatedAt: fired.Add(time.Minute)}
	obs := Observation{Head: head, Open: true, Events: []dialect.BotEvent{verdict}}

	if !CoActiveThisRound(round, obs, macroLogin) {
		t.Fatal("a round-window verdict must count as participation")
	}
	if reviewed := coReviewedHead(obs, macroLogin); reviewed {
		t.Fatal("a verdict must never count as head review evidence")
	}
}

// TestDecideFireMultiBotPostCo: multiple always-mode co-reviewers post in one
// fire step, ride the same dedupe resolution, and the deferred degrade carries
// all of them without touching the fire slot.
func TestDecideFireMultiBotPostCo(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	queued := state.Round{Repo: "o/r", PR: 1, Head: head, Phase: state.PhaseQueued}
	p := Policy{Bot: "coderabbitai[bot]",
		RequiredBots:       []string{"coderabbitai[bot]", dialect.CodexBotLogin, bugbotLogin},
		RateLimitCoDegrade: true,
		CoReviewers: []CoReviewerPolicy{
			{Login: dialect.CodexBotLogin, Command: "@codex review", Trigger: TriggerAlways},
			{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerAlways},
		}}
	free := Global{SlotFree: true}

	hasLogin := func(logins []string, want string) bool {
		for _, l := range logins {
			if dialect.NormalizeBotName(l) == dialect.NormalizeBotName(want) {
				return true
			}
		}
		return false
	}
	wantBoth := func(t *testing.T, d FireDecision) {
		t.Helper()
		if len(d.PostCo) != 2 || !hasLogin(d.PostCo, dialect.CodexBotLogin) || !hasLogin(d.PostCo, bugbotLogin) {
			t.Fatalf("PostCo = %v, want codex+bugbot", d.PostCo)
		}
	}

	d := DecideFire(free, queued, Observation{Head: head, Open: true}, now, p)
	if d.Verdict != FirePost {
		t.Fatalf("verdict = %v, want FirePost", d.Verdict)
	}
	wantBoth(t, d)

	// CodeRabbit already reviewed the head → co-only trigger for both.
	crReviewed := Observation{Head: head, Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: now}}}
	d = DecideFire(free, queued, crReviewed, now, p)
	if d.Verdict != FireCoOnly {
		t.Fatalf("verdict = %v, want FireCoOnly", d.Verdict)
	}
	wantBoth(t, d)

	// One bot already has head evidence → only the other posts.
	partial := crReviewed
	partial.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: now}}
	d = DecideFire(free, queued, partial, now, p)
	if d.Verdict != FireCoOnly || len(d.PostCo) != 1 || !dialect.IsCodexBot(d.PostCo[0]) {
		t.Fatalf("expected codex-only PostCo, got %+v", d)
	}

	// Account blocked → deferred degrade posts both, slot untouched.
	blocked := now.Add(time.Hour)
	d = DecideFire(Global{SlotFree: true, BlockedUntil: &blocked}, queued, Observation{Head: head, Open: true}, now, p)
	if d.Verdict != FireCoDeferred {
		t.Fatalf("verdict = %v, want FireCoDeferred", d.Verdict)
	}
	wantBoth(t, d)

	// A wanted-only silent bot never forces a wait: with every co-reviewer
	// satisfied or inert, the reviewed head dedupes.
	inert := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers: []CoReviewerPolicy{{Login: macroLogin, Command: "", Trigger: TriggerSelfHeal}}}
	d = DecideFire(free, queued, crReviewed, now, inert)
	if d.Verdict != FireDedupe {
		t.Fatalf("verdict = %v, want FireDedupe for a wanted-only silent bot", d.Verdict)
	}
}

// TestSummaryOnlyPlanRunsCoReviewersAlone pins the CodeRabbit-Free-on-a-private-repo
// contract: the bot ships a walkthrough and NO review object, ever. crq must never
// spend a fire (or a slot, or account quota, or pacing) on that review, must fall
// back to the co-reviewers as the round's only reviewers, and must let the round
// converge on them instead of hanging on a review that cannot arrive.
func TestSummaryOnlyPlanRunsCoReviewersAlone(t *testing.T) {
	now := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	head := "abcdef123"
	codex := dialect.CodexBotLogin
	queued := state.Round{Repo: "o/r", PR: 1021, Head: head, Phase: state.PhaseQueued}
	free := Global{SlotFree: true}

	// Codex enabled and triggerable, but NOT required and not auto-active: the
	// case that only summary-only promotes into a gating reviewer.
	p := Policy{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		MinInterval:  90 * time.Second,
		CoReviewers:  []CoReviewerPolicy{{Login: codex, Command: "@codex review", Trigger: TriggerAlways}},
	}
	// The walkthrough carries the plan notice alongside its own kind.
	notice := dialect.BotEvent{
		Kind: dialect.EvOther, Bot: "coderabbitai[bot]", CommentID: 7,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), SummaryOnly: true,
	}
	obs := func(events []dialect.BotEvent, reviews []ReviewSeen, co CoSeen) Observation {
		return Observation{Head: head, Open: true, Events: events, Reviews: reviews,
			Co: map[string]CoSeen{dialect.NormalizeBotName(codex): co}}
	}
	codexAtHead := []ReviewSeen{{Bot: codex, Commit: "abcdef1234567890", SubmittedAt: now.Add(-30 * time.Minute)}}

	t.Run("fires codex instead of coderabbit", func(t *testing.T) {
		d := DecideFire(free, queued, obs([]dialect.BotEvent{notice}, nil, CoSeen{}), now, p)
		if d.Verdict != FireCoOnly {
			t.Fatalf("verdict = %v (%s), want FireCoOnly", d.Verdict, d.Reason)
		}
		if len(d.PostCo) != 1 || !dialect.IsCodexBot(d.PostCo[0]) {
			t.Fatalf("PostCo = %v, want codex only", d.PostCo)
		}
	})

	t.Run("without the notice the same round fires coderabbit", func(t *testing.T) {
		d := DecideFire(free, queued, obs(nil, nil, CoSeen{}), now, p)
		if d.Verdict != FirePost {
			t.Fatalf("verdict = %v, want FirePost", d.Verdict)
		}
	})

	// The resolution costs no CodeRabbit quota, so neither a busy slot nor an
	// account block from another PR may delay or divert it.
	t.Run("bypasses the slot and account-block gates", func(t *testing.T) {
		blocked := now.Add(time.Hour)
		for name, g := range map[string]Global{
			"slot busy":       {SlotFree: false},
			"account blocked": {SlotFree: true, BlockedUntil: &blocked},
			"pacing":          {SlotFree: true, LastFired: &now},
		} {
			d := DecideFire(g, queued, obs([]dialect.BotEvent{notice}, nil, CoSeen{}), now, p)
			if d.Verdict != FireCoOnly {
				t.Errorf("%s: verdict = %v (%s), want FireCoOnly", name, d.Verdict, d.Reason)
			}
		}
	})

	t.Run("always mode commands an historically auto-reviewing codex", func(t *testing.T) {
		d := DecideFire(free, queued, obs([]dialect.BotEvent{notice}, nil, CoSeen{AutoActive: true}), now, p)
		if d.Verdict != FireCoOnly {
			t.Fatalf("verdict = %v (%s), want FireCoOnly", d.Verdict, d.Reason)
		}
		if len(d.PostCo) != 1 {
			t.Fatalf("PostCo = %v, want one explicit always-mode post", d.PostCo)
		}
	})

	t.Run("dedupes when no co-reviewer can be asked", func(t *testing.T) {
		inert := p
		inert.CoReviewers = []CoReviewerPolicy{{Login: codex, Trigger: TriggerNever}}
		d := DecideFire(free, queued, obs([]dialect.BotEvent{notice}, nil, CoSeen{}), now, inert)
		if d.Verdict != FireDedupe {
			t.Fatalf("verdict = %v (%s), want FireDedupe", d.Verdict, d.Reason)
		}
	})

	// Completion: the round may resolve without ever firing, so the never-fired
	// round is the one that has to converge.
	t.Run("unfired round converges on the codex review", func(t *testing.T) {
		st := Completion(queued, obs([]dialect.BotEvent{notice}, codexAtHead, CoSeen{AutoActive: true}), p)
		if !st.Done {
			t.Fatalf("Done = false, want true (reviewedBy = %v)", st.ReviewedBy)
		}
	})

	t.Run("does not converge while codex is still silent", func(t *testing.T) {
		st := Completion(queued, obs([]dialect.BotEvent{notice}, nil, CoSeen{AutoActive: true}), p)
		if st.Done {
			t.Fatalf("Done = true, want false while codex has not answered (reviewedBy = %v)", st.ReviewedBy)
		}
	})

	t.Run("without the notice coderabbit still gates", func(t *testing.T) {
		st := Completion(queued, obs(nil, codexAtHead, CoSeen{AutoActive: true}), p)
		if st.Done {
			t.Fatalf("Done = true, want false: coderabbit has not reviewed (reviewedBy = %v)", st.ReviewedBy)
		}
	})
}

// skippedEvent builds the classified "Review skipped" notice for a head.
func skippedEvent(sha string, at time.Time) dialect.BotEvent {
	return dialect.BotEvent{
		Kind: dialect.EvSkipped, Bot: "coderabbitai[bot]", SHA: sha,
		CommentID: 7000, CreatedAt: at, UpdatedAt: at,
	}
}

// TestReviewSkippedNeverBlocksTheAccount is the load-bearing rule behind the
// "Review skipped" support: the notice ships WITH CodeRabbit's rate-limit
// marker, but it is a refusal of one head, not a timed account block. If
// Progress ever treats it as a rate limit it fabricates a window that never
// clears — crq re-fires the same oversized PR forever and, far worse, parks the
// ACCOUNT-wide quota so every other PR in the fleet stalls behind one bad PR.
func TestReviewSkippedNeverBlocksTheAccount(t *testing.T) {
	fired := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	head := "56150a042"
	round := state.Round{Repo: "o/r", PR: 214, Head: head, Phase: state.PhaseFired, FiredAt: &fired}
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"}}
	obs := Observation{Head: head, Open: true,
		Events: []dialect.BotEvent{skippedEvent("56150a0423a243224b03f355c3a3ba6941011b5b", fired.Add(time.Minute))}}

	tr := Progress(round, state.AccountQuota{}, obs, fired.Add(2*time.Minute), p)
	if tr.Blocked != nil {
		t.Fatalf("a skipped review must never record an account block, got %+v", tr.Blocked)
	}
	if tr.Outcome == OutRetry {
		t.Fatalf("a skipped review must not park the round for retry — waiting changes nothing: %+v", tr)
	}
	// With no co-reviewer left outstanding the round is finished, not wedged.
	if tr.Outcome != OutComplete {
		t.Fatalf("outcome = %v, want OutComplete", tr.Outcome)
	}
}

// TestReviewSkippedBindsToItsHead: the refusal is per-head. Splitting the PR
// produces a new head CodeRabbit may well review, so a skip naming an older
// commit must not suppress the new one — otherwise fixing the problem would
// permanently disable the primary reviewer for that PR.
func TestReviewSkippedBindsToItsHead(t *testing.T) {
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"}}
	at := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	skipped := skippedEvent("56150a0423a243224b03f355c3a3ba6941011b5b", at)

	sameHead := Observation{Head: "56150a042", Open: true, Events: []dialect.BotEvent{skipped}}
	if !PrimaryReviewUnavailable(sameHead, p, sameHead.Head) {
		t.Fatal("the skipped head must read as unreviewable")
	}
	newHead := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{skipped}}
	if PrimaryReviewUnavailable(newHead, p, newHead.Head) {
		t.Fatal("a skip of an older head must not suppress CodeRabbit on the reworked head")
	}
	// A SHA-less skip is read conservatively: it binds to whatever head is observed.
	noSHA := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{skippedEvent("", at)}}
	if !PrimaryReviewUnavailable(noSHA, p, noSHA.Head) {
		t.Fatal("a SHA-less skip must bind to the observed head")
	}
	// Another bot's skip is not the configured reviewer's problem.
	other := skippedEvent("56150a0423a243224b03f355c3a3ba6941011b5b", at)
	other.Bot = "someone-else[bot]"
	foreign := Observation{Head: "56150a042", Open: true, Events: []dialect.BotEvent{other}}
	if PrimaryReviewUnavailable(foreign, p, foreign.Head) {
		t.Fatal("only the configured bot's skip counts")
	}
}

// TestReviewSkippedRunsCoReviewersInsteadOfFiring: the round must resolve on the
// co-reviewers rather than spending a fire on a review that cannot happen.
func TestReviewSkippedRunsCoReviewersInsteadOfFiring(t *testing.T) {
	now := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	head := "56150a042"
	queued := state.Round{Repo: "o/r", PR: 214, Head: head, Phase: state.PhaseQueued}
	p := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", dialect.CodexBotLogin},
		CoReviewers:  []CoReviewerPolicy{{Login: dialect.CodexBotLogin, Command: "@codex review", Trigger: TriggerAlways}}}
	obs := Observation{Head: head, Open: true,
		Events: []dialect.BotEvent{skippedEvent("56150a0423a243224b03f355c3a3ba6941011b5b", now)},
		Co: map[string]CoSeen{
			dialect.NormalizeBotName(dialect.CodexBotLogin): {ChecksUnknown: true},
		}}

	// Even with a free slot and no block, crq must not fire CodeRabbit.
	d := DecideFire(Global{SlotFree: true}, queued, obs, now, p)
	if d.Verdict == FirePost || d.Verdict == FireAdopt {
		t.Fatalf("crq must never fire CodeRabbit at a head it already skipped: %+v", d)
	}
	if d.Verdict != FireCoOnly || len(d.PostCo) != 1 {
		t.Fatalf("verdict = %v PostCo = %v, want FireCoOnly asking codex", d.Verdict, d.PostCo)
	}
	// And it must still resolve while the account is blocked — the block has no
	// authority over a round that will never spend the quota.
	blocked := now.Add(time.Hour)
	if d := DecideFire(Global{SlotFree: true, BlockedUntil: &blocked}, queued, obs, now, p); d.Verdict != FireCoOnly {
		t.Fatalf("a blocked account must not delay a skipped round: %+v", d)
	}
}

// TestSkippedNoticeExpiresWithTheHead: a real "Review skipped" that names no
// commit must not suppress CodeRabbit forever. Accepting it indefinitely means
// a narrowed PR is never reviewed again — and silently, because Feedback
// filters the same notice by commit time and stops surfacing it.
func TestSkippedNoticeExpiresWithTheHead(t *testing.T) {
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"}}
	postedAt := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	skip := dialect.BotEvent{Kind: dialect.EvSkipped, Bot: "coderabbitai[bot]",
		CommentID: 1, CreatedAt: postedAt, UpdatedAt: postedAt}

	// Head predates the notice: it describes this head, so it still applies.
	current := Observation{Head: "abcdef123", Open: true, HeadAt: postedAt.Add(-time.Hour),
		Events: []dialect.BotEvent{skip}}
	if !PrimaryReviewUnavailable(current, p, current.Head) {
		t.Fatal("a skip observed after the head appeared must apply to it")
	}
	// The PR was narrowed: the new head is NEWER than the notice, so the old
	// refusal must not suppress it.
	narrowed := Observation{Head: "9999abcde", Open: true, HeadAt: postedAt.Add(time.Hour),
		Events: []dialect.BotEvent{skip}}
	if PrimaryReviewUnavailable(narrowed, p, narrowed.Head) {
		t.Fatal("a narrowed head must be reviewable again, not suppressed by the old skip")
	}
	// Unknown head time falls back to accepting it (conservative).
	unknown := Observation{Head: "9999abcde", Open: true, Events: []dialect.BotEvent{skip}}
	if !PrimaryReviewUnavailable(unknown, p, unknown.Head) {
		t.Fatal("with no head time the skip is read conservatively as applying")
	}
}

// TestUnavailablePrimaryWaitsForTheBotItJustCommanded: when no primary review
// is coming, coAwareDedupe may post an optional co-reviewer's trigger. That bot
// is not yet AutoActive or ActiveThisRound, so without counting the recorded
// command the round marks the primary delivered and reports done immediately
// after asking — before its only real reviewer has said anything.
func TestUnavailablePrimaryWaitsForTheBotItJustCommanded(t *testing.T) {
	head := "abcdef123"
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers: []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerAlways}}}
	summaryOnly := dialect.BotEvent{Kind: dialect.EvOther, Bot: "coderabbitai[bot]",
		SummaryOnly: true, CommentID: 1}
	obs := Observation{Head: head, Open: true, Events: []dialect.BotEvent{summaryOnly}}

	// Nothing commanded yet and the bot is silent: nothing to wait for.
	bare := state.Round{Head: head}
	if got := Completion(bare, obs, p); !got.Done {
		t.Fatalf("with no co-reviewer engaged the round resolves: %+v", got)
	}
	// crq has now posted its trigger — the round must wait for the answer.
	commanded := state.Round{Head: head}
	commanded.SetCoCommand(bugbotLogin, 55, time.Now().UTC())
	if got := Completion(commanded, obs, p); got.Done {
		t.Fatalf("a round must not complete right after commanding its only reviewer: %+v", got)
	}
}

// TestRequiredCoReviewerHoldsAnUnavailablePrimaryRound: with no primary review
// coming and a required co-reviewer that has not spoken yet, deduping wrote a
// completed marker that Completion immediately contradicts — it still reports
// that bot, and so the primary, unfinished — leaving Loop to time out and
// requeue into the same cycle. A bounded wait is the honest state.
func TestRequiredCoReviewerHoldsAnUnavailablePrimaryRound(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	head := "abcdef123"
	queued := state.Round{Repo: "o/r", PR: 1, Head: head, Phase: state.PhaseQueued}
	summaryOnly := dialect.BotEvent{Kind: dialect.EvOther, Bot: "coderabbitai[bot]", SummaryOnly: true, CommentID: 1}
	obs := Observation{Head: head, Open: true, Events: []dialect.BotEvent{summaryOnly}}

	required := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", bugbotLogin},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}
	if d := DecideFire(Global{SlotFree: true}, queued, obs, now, required); d.Verdict != FireCoReviewWait {
		t.Fatalf("a silent REQUIRED co-reviewer must hold the round open, got %+v", d)
	}
	// A merely wanted bot in the same state still dedupes — "wanted" must not
	// wedge a round on a bot that may never appear.
	wanted := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}
	if d := DecideFire(Global{SlotFree: true}, queued, obs, now, wanted); d.Verdict != FireDedupe {
		t.Fatalf("a wanted-only silent bot must not wedge the round, got %+v", d)
	}
}

// TestFailedCheckStaysActivityForSelfHeal: a crashed run must not suppress the
// recovery trigger, but it IS the bot working on this PR. If it were not
// activity, a bot whose FIRST check failed would have no signal at all and
// self-heal could never fire.
func TestFailedCheckStaysActivityForSelfHeal(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	head := "abcdef123"
	fired := now.Add(-time.Hour)
	round := state.Round{Head: head, FiredAt: &fired}
	obs := Observation{Head: head, Open: true,
		Checks: []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckFailed, CompletedAt: now.Add(-30 * time.Minute)}}}

	if !CoActiveThisRound(round, obs, bugbotLogin) {
		t.Fatal("a failed check is still activity — otherwise self-heal has no signal to act on")
	}
	cp := CoReviewerPolicy{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal, SelfHealGrace: 10 * time.Minute}
	obs.Co = map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): {ActiveThisRound: true}}
	if !DecideCoPost(round, obs, cp, false, fired, now) {
		t.Fatal("self-heal must be able to retrigger a bot whose check crashed")
	}
	// But an unreadable check-run fetch must fail closed: absence proves nothing.
	obs.Co = map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): {ActiveThisRound: true, ChecksUnknown: true}}
	if DecideCoPost(round, obs, cp, false, fired, now) {
		t.Fatal("an unreadable check must suppress the trigger rather than double-ask a run in flight")
	}
}

// TestUnableCheckStopsWaitingWithoutNudging: Macroscope's billing-issue skip is
// the check-run twin of Codex's usage-limit notice. The bot ANSWERED — it cannot
// review this commit — so the round must neither count it as reviewed nor keep
// asking. Reading it as a completed review (the pre-fix behaviour of any
// unrecognised skip) converged the round having been reviewed by nobody;
// reading it as a crash posted a trigger every round at a workspace whose
// billing no trigger can fix.
func TestUnableCheckStopsWaitingWithoutNudging(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	head := "abcdef123"
	fired := now.Add(-time.Hour)
	round := state.Round{Repo: "o/r", PR: 1, Head: head, Phase: state.PhaseReviewing, FiredAt: &fired}
	unable := CheckSeen{Bot: macroLogin, Name: "Macroscope - Correctness Check",
		Verdict: dialect.CheckUnable, CompletedAt: now.Add(-30 * time.Minute)}
	obs := Observation{Head: head, Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", Commit: head, SubmittedAt: now.Add(-40 * time.Minute)}},
		Checks:  []CheckSeen{unable},
		Co:      map[string]CoSeen{dialect.NormalizeBotName(macroLogin): {ActiveThisRound: true}}}
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers: []CoReviewerPolicy{{Login: macroLogin, Command: "@macroscope-app review",
			Trigger: TriggerSelfHeal, SelfHealGrace: 10 * time.Minute}}}

	// Never review evidence.
	if c := Completion(round, obs, p); c.ReviewedBy[macroLogin] {
		t.Fatal("a bot that says it could not review must never count as having reviewed")
	}
	// ...but it disengages the dynamic gate, so the primary review completes the
	// round instead of waiting out the deadline for a review that cannot come.
	if c := Completion(round, obs, p); !c.Done {
		t.Fatalf("an unable co-reviewer must not hold the round open: %+v", c.ReviewedBy)
	}
	// And no nudge: the run IS the answer.
	cp := p.CoReviewers[0]
	if DecideCoPost(round, obs, cp, false, fired, now) {
		t.Fatal("re-triggering a bot that reported it cannot review is comment spam")
	}
}

// TestSelfHealGraceStartsWhenCrqSawTheHead: a force-push can point a PR at a
// commit authored long ago. Anchoring the grace on that commit's date reads as
// "elapsed hours ago" and posts a trigger over an auto-review that has barely
// started, so the round's own enqueue time — when crq first saw this head —
// wins when it is later.
func TestSelfHealGraceStartsWhenCrqSawTheHead(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	head := "abcdef123"
	// The head commit was authored a month ago; crq saw it one minute ago.
	obs := Observation{Head: head, Open: true, HeadAt: now.AddDate(0, -1, 0),
		Events: []dialect.BotEvent{{Kind: dialect.EvOther, Bot: "coderabbitai[bot]", SummaryOnly: true, CommentID: 1}},
		Co:     map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): {AutoActive: true}}}
	round := state.Round{Repo: "o/r", PR: 1, Head: head, Phase: state.PhaseQueued, EnqueuedAt: now.Add(-time.Minute)}
	p := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers: []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run",
			Trigger: TriggerSelfHeal, SelfHealGrace: 10 * time.Minute}}}

	if d := DecideFire(Global{SlotFree: true}, round, obs, now, p); d.Verdict == FireCoOnly {
		t.Fatalf("grace must run from when crq saw the head, not the commit's date: %+v", d)
	}
	// Once the grace really has elapsed the trigger goes out as before.
	if d := DecideFire(Global{SlotFree: true}, round, obs, now.Add(11*time.Minute), p); d.Verdict != FireCoOnly {
		t.Fatalf("self-heal must still fire once the grace elapses: %+v", d)
	}
}
