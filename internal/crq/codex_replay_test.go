package crq

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// End-to-end scenarios for Codex firing, auto-review detection, and the
// dynamic completion gate. They reuse the replay fixture (injected clock, fake
// GitHub, MemoryStore) so every claim is driven through the real
// Pump/Feedback/observe pipeline.

// codexLogin is the canonical Codex app login, owned by internal/dialect.
const codexLogin = dialect.CodexBotLogin

// codexReviewCommand is the trigger these replays configure for Codex.
const codexReviewCommand = "@codex review"

// codexClean renders the tada clean-summary corpus with a chosen reviewed SHA,
// so the replay shares one source of truth with the classifier golden corpus. A
// rewording that breaks classification breaks this too.
func codexClean(t *testing.T, sha string) string {
	msg := corpusMessage(t, "codex/clean-summary-tada.md")
	out := strings.Replace(msg, "4d9e8bca82", sha, 1)
	if out == msg {
		t.Fatal("clean-summary corpus no longer carries the 4d9e8bca82 anchor SHA")
	}
	return out
}

// codexUsageLimit loads the Codex usage-limit exhaustion notice from the corpus.
func codexUsageLimit(t *testing.T) string {
	return corpusMessage(t, "codex/usage-limit.md")
}

func newCodexReplayFixture(t *testing.T, base time.Time, mutate func(*Config)) *replayFixture {
	t.Helper()
	clk := newReplayClock(base)
	cfg := replayConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	// Codex keeps its historical default: crq posts the trigger at fire time
	// exactly when Codex is configured-required (parseCoBots' codex rule).
	trigger := engine.TriggerNever
	if dialect.HasCodexBot(cfg.RequiredBots) {
		trigger = engine.TriggerAlways
	}
	cfg.CoBots = []CoBotConfig{{
		Login: codexLogin, Name: "codex", Command: codexReviewCommand,
		Trigger: trigger, Required: dialect.HasCodexBot(cfg.RequiredBots),
		SelfHealGrace: 10 * time.Minute,
	}}
	gh := newFakeGitHub()
	gh.now = clk.now
	store := NewMemoryStore(cfg)
	store.SetClock(clk.now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = clk.now
	return &replayFixture{t: t, ctx: t.Context(), clk: clk, gh: gh, store: store, svc: svc, cfg: cfg, bot: cfg.Bot}
}

// codexComment appends an issue comment authored by the Codex app.
func (f *replayFixture) codexComment(repo string, pr int, id int64, body string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	c.User.Login = codexLogin
	key := fakeKey(repo, pr)
	f.gh.comments[key] = append(f.gh.comments[key], c)
}

// codexReview appends a submitted Codex review.
func (f *replayFixture) codexReview(repo string, pr int, id int64, commitSHA string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	r := ghapi.Review{ID: id, CommitID: commitSHA, State: "COMMENTED", SubmittedAt: at.UTC(),
		Body: "### \U0001F4A1 Codex Review\n\nReviewed the changes."}
	r.User.Login = codexLogin
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
}

// codexPosted counts how many `@codex review` commands crq actually posted.
func (f *replayFixture) codexPosted(repo string, pr int) int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	want := QueueKey(repo, pr) + ":" + codexReviewCommand
	n := 0
	for _, p := range f.gh.posted {
		if p == want {
			n++
		}
	}
	return n
}

// (i) Codex configured-required with no auto-review: crq posts the Codex
// command exactly once alongside the CodeRabbit command, does NOT repost it on
// the post-rate-limit retry of the same head, and completes the round only
// when BOTH reviews are in.
func TestCodexReplayRequiredFiresOnceAndGatesCompletion(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 7, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("coderabbit commands = %d, want 1", got)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex commands = %d, want 1", got)
	}
	if r := f.round(repo, pr); r == nil || r.CodexCommandID == 0 {
		t.Fatalf("round must record the codex command, got %+v", r)
	}

	// CodeRabbit answers rate-limited; the round parks. After the window one
	// retry fires — but the Codex command is NOT reposted (CodexCommandID set).
	f.botComment(repo, pr, 9001, replayFairUsage(t, 10), f.clk.now().Add(5*time.Second))
	if res := f.pump(); res.Action != "requeued" {
		t.Fatalf("expected requeue, got %+v", res)
	}
	f.clk.advance(11 * time.Minute)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected retry fire, got %+v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 2 {
		t.Fatalf("coderabbit commands after retry = %d, want 2", got)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex must not be reposted on retry, got %d", got)
	}

	// CodeRabbit's review lands: with Codex still outstanding the round must
	// stay open (reviewing), not complete.
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must wait for codex, got %+v", r)
	}

	// Codex's review lands too — now the round completes.
	f.codexReview(repo, pr, 502, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete after both reviews, got %+v", r)
	}
}

// (ii) Codex configured-required in always mode: historical auto-review
// activity must not override the explicit trigger policy for the new head.
func TestCodexReplayAlwaysModeOverridesHistoricalAutoActivity(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr := "o/r", 8
	oldHead, head := "1111222233334444", "5555666677778888"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// History: Codex reviewed the previous head unprompted. That may predict
	// another automatic review, but "always" still commands this head.
	f.codexReview(repo, pr, 400, oldHead, base.Add(-2*time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("always-mode codex must be commanded once, got %d posts", got)
	}

	// CodeRabbit finishes; the round still waits for Codex's auto review.
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must wait for codex auto review, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("waiting must not duplicate the codex post, got %d", got)
	}

	// Codex auto-reviews the head (clean) — round completes.
	f.codexComment(repo, pr, 700, codexClean(t, head[:10]), f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete on codex's auto review, got %+v", r)
	}
}

// (iii) Codex NOT required: when it joins a round on its own (actionable
// comment mid-round), the dynamic gate holds completion until its review — and
// a usage-limit notice disengages the dynamic gate so the round can complete
// on CodeRabbit alone.
func TestCodexReplayDynamicGateAndUsageLimitEscape(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, nil) // RequiredBots = coderabbit only
	repo, pr, head := "o/r", 9, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// Not required + not active → no codex command.
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("unrequired codex must not be commanded, got %d", got)
	}

	// Codex joins the round with an actionable comment; CodeRabbit finishes.
	// The dynamic gate must keep the round open for Codex.
	f.codexComment(repo, pr, 700, "There is a bug in `foo.go` line 3: the cutoff is inverted.", f.clk.now().Add(30*time.Second))
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("dynamic gate must hold for codex, got %+v", r)
	}
	report, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed, tracked := report.ReviewedBy[codexLogin]; !tracked || reviewed {
		t.Fatalf("dynamically-gated codex must appear pending in ReviewedBy: %+v", report.ReviewedBy)
	}

	// Codex runs out of quota: the dynamic gate disengages and the round
	// completes on CodeRabbit's review alone.
	f.codexComment(repo, pr, 701, codexUsageLimit(t), f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("usage limit must release the dynamic gate, got %+v", r)
	}
}

// (iv) CodeRabbit already reviewed the head before crq fires, with Codex
// configured-required: the dedupe must NOT complete the round Codex-less. crq
// posts exactly one @codex review (and zero @coderabbitai review), and the round
// completes only once Codex answers.
func TestCodexReplayDedupeStillCommandsCodex(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 11, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// CodeRabbit already reviewed this head before crq gets to fire.
	f.botReview(repo, pr, 500, head, base.Add(-time.Minute))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected a codex-only fire, got %+v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("coderabbit must not be commanded when it already reviewed, got %d", got)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex must be commanded exactly once, got %d", got)
	}
	if r := f.round(repo, pr); r == nil || r.CodexCommandID == 0 {
		t.Fatalf("round must record the codex command, got %+v", r)
	}

	// CodeRabbit is already satisfied; the round waits on Codex alone.
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must wait for codex, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("waiting must not repost the codex command, got %d", got)
	}

	// Codex answers → the round completes; still no coderabbit command posted.
	f.codexReview(repo, pr, 502, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete after codex answers, got %+v", r)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("no coderabbit command may post across the scenario, got %d", got)
	}
}

// TestCodexReplayCoReviewWaitBoundsSilentCodex reproduces the hang a CodeRabbit
// review of our own PR surfaced: CodeRabbit posts a clean review at head with
// Codex configured-required and a `@codex review` command already on the PR (so
// crq must not repost — the FireCoReviewWait branch). Before the fix the round
// stayed queued with no WaitDeadline and Wait looped forever. Now a pump parks it
// in reviewing WITH a deadline; past the deadline a pump completes it on
// CodeRabbit's standing review rather than spinning, and crq never posts a second
// review command of either kind.
func TestCodexReplayCoReviewWaitBoundsSilentCodex(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	f.gh.graphQL = noForcePush // let the head-commit/force-push cutoff resolve so the codex command is adoptable
	repo, pr, head := "o/r", 13, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// CodeRabbit already reviewed this head (clean), and a `@codex review` command
	// is already on the PR awaiting Codex's answer.
	f.botReview(repo, pr, 500, head, base.Add(-time.Minute))
	f.humanComment(repo, pr, 600, codexReviewCommand, base.Add(-30*time.Second))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "waiting" {
		t.Fatalf("expected a bounded co-review wait, got %+v", res)
	}
	r := f.round(repo, pr)
	if r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must park in reviewing, not stay queued, got %+v", r)
	}
	if r.WaitDeadline == nil {
		t.Fatalf("the co-review wait must be bounded by a WaitDeadline, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("the wait must not post a codex command, got %d", got)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("the wait must not fire @coderabbitai review, got %d", got)
	}

	// Codex stays silent past the deadline: the next pump completes the round on
	// CodeRabbit's standing review (Progress OutComplete) rather than looping.
	f.clk.advance(f.cfg.FeedbackWaitTimeout + time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("past the deadline the round must complete, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("no codex command may post across the scenario, got %d", got)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("no coderabbit command may post across the scenario, got %d", got)
	}
}

// TestObserveScopesShellFilterToCodeRabbit pins fix #1: the empty-COMMENTED
// review filter (which drops CodeRabbit's inline-comment carrier shells) must
// not drop another bot's empty review — a Codex-gated round could be waiting on
// exactly that evidence.
func TestObserveScopesShellFilterToCodeRabbit(t *testing.T) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 11, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	// A CodeRabbit shell (empty COMMENTED) and a Codex empty COMMENTED review,
	// both at head.
	f.gh.mu.Lock()
	key := fakeKey(repo, pr)
	crShell := ghapi.Review{ID: 800, CommitID: head, State: "COMMENTED", SubmittedAt: base}
	crShell.User.Login = f.bot
	codexReview := ghapi.Review{ID: 801, CommitID: head, State: "COMMENTED", SubmittedAt: base}
	codexReview.User.Login = codexLogin
	f.gh.reviews[key] = []ghapi.Review{crShell, codexReview}
	f.gh.mu.Unlock()

	obs, err := f.svc.observe(f.ctx, f.svc.cfg, repo, pr, nil, nil, f.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	var sawCR, sawCodex bool
	for _, r := range obs.eng.Reviews {
		if r.ReviewID == 800 {
			sawCR = true
		}
		if r.ReviewID == 801 {
			sawCodex = true
		}
	}
	if sawCR {
		t.Fatal("CodeRabbit's empty COMMENTED shell must be filtered out")
	}
	if !sawCodex {
		t.Fatal("a non-CodeRabbit empty COMMENTED review must be kept as evidence")
	}
}

// TestFireCoOnlyPostFailureParks pins fix #3: when the Codex-only post fails,
// the round parks in awaiting_retry with a cooldown instead of re-posting on the
// very next pump.
func TestFireCoOnlyPostFailureParks(t *testing.T) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 12, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// CodeRabbit already reviewed the head → DecideFire returns FireCoOnly.
	f.botReview(repo, pr, 500, head, base.Add(-time.Minute))
	// The Codex command post fails.
	f.gh.mu.Lock()
	if f.gh.postErrs == nil {
		f.gh.postErrs = map[string]error{}
	}
	f.gh.postErrs[fakeKey(repo, pr)] = errors.New("boom")
	f.gh.mu.Unlock()

	f.enqueue(repo, pr)
	// fireCodexOnly returns the post error alongside the post_failed result, so
	// call Pump directly rather than through the fatal-on-error helper.
	res, err := f.svc.Pump(f.ctx)
	if res.Action != "post_failed" {
		t.Fatalf("expected post_failed, got %+v (err=%v)", res, err)
	}
	r := f.round(repo, pr)
	if r == nil || r.Phase != PhaseAwaitingRetry {
		t.Fatalf("failed post must park the round, got %+v", r)
	}
	if r.RetryAt == nil || !r.RetryAt.Equal(f.clk.now().Add(postFailureBackoff)) {
		t.Fatalf("park must carry the post-failure cooldown, got RetryAt=%v", r.RetryAt)
	}
	if r.FireEligible(f.clk.now()) {
		t.Fatal("a just-parked round must not be immediately fire-eligible")
	}
	if r.FireEligible(f.clk.now().Add(postFailureBackoff)) == false {
		t.Fatal("the round must become eligible once the cooldown passes")
	}
}

// A co-only round's CommandID is the co-reviewer's trigger comment — one
// comment under two names. Recording it as the primary's own would let
// unrelated CodeRabbit activity pass for the answer that trigger is still
// waiting on, and would put the same id on the cleanup list twice.
func TestFireCoOnlyRecordsItsAnchorAsTheCoReviewersCommand(t *testing.T) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 13, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// CodeRabbit already reviewed the head → DecideFire returns FireCoOnly.
	f.botReview(repo, pr, 500, head, base.Add(-time.Minute))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected a co-only fire, got %+v", res)
	}
	r := f.round(repo, pr)
	if r == nil || !r.CoOnly {
		t.Fatalf("expected a co-only round, got %+v", r)
	}
	if len(r.PostedCommands) != 1 {
		t.Fatalf("posted = %v, want the one comment crq wrote", r.PostedCommands)
	}
	posted := r.PostedCommands[0]
	if posted.ID != r.CommandID {
		t.Errorf("posted id = %d, want the round's anchor %d", posted.ID, r.CommandID)
	}
	if dialect.NormalizeBotName(posted.Bot) != dialect.NormalizeBotName(codexLogin) {
		t.Errorf("posted bot = %q, want the co-reviewer it was addressed to", posted.Bot)
	}
}

// TestSelfHealCodexClaimPreventsDoublePost pins claim-before-post: two sweepers
// observing the same round with CodexCommandID==0 must produce exactly one
// `@codex review` — the second claim fails under CAS.
func TestSelfHealCodexClaimPreventsDoublePost(t *testing.T) {
	base := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 21, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	// A fired round whose initial Codex post never happened (CodexCommandID 0).
	f.enqueue(repo, pr)
	f.gh.mu.Lock()
	if f.gh.postErrs == nil {
		f.gh.postErrs = map[string]error{}
	}
	f.gh.postErrs[fakeKey(repo, pr)] = errors.New("boom")
	f.gh.mu.Unlock()
	res, _ := f.svc.Pump(f.ctx)
	if res.Action != "post_failed" {
		t.Fatalf("setup expected post_failed, got %+v", res)
	}
	f.gh.mu.Lock()
	delete(f.gh.postErrs, fakeKey(repo, pr))
	f.gh.mu.Unlock()
	f.clk.advance(3 * time.Minute) // past the post-failure cooldown
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected retry fire, got %+v", res)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("fire should post codex once, got %d", got)
	}

	// Simulate the race: strip the recorded command id and claim, as if the
	// sweeper's observation predates the record, then run two sweeps in a row
	// within one claim TTL. Only the claimed one may post.
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		r := st.Round(repo, pr)
		r.CodexCommandID = 0
		r.CodexClaimedAt = nil
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, _ := f.store.Load(f.ctx)
	round := st.Round(repo, pr)
	obs, err := f.svc.observe(f.ctx, f.svc.cfg, repo, pr, round, nil, f.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	// Erase the live command from the observation so the trigger decision wants
	// to post (models the failed-initial-post world the finding describes).
	if co, ok := obs.eng.Co[dialect.NormalizeBotName(codexLogin)]; ok {
		co.Commands = nil
		obs.eng.Co[dialect.NormalizeBotName(codexLogin)] = co
	}
	events := obs.eng.Events[:0]
	for _, ev := range obs.eng.Events {
		if ev.Kind != dialect.EvCoCommand {
			events = append(events, ev)
		}
	}
	obs.eng.Events = events
	before := f.codexPosted(repo, pr)
	f.svc.selfHealCoReviewers(f.ctx, f.svc.cfg, *round, obs.eng, f.clk.now())
	// Second sweeper with the SAME stale observation (still CodexCommandID==0).
	f.svc.selfHealCoReviewers(f.ctx, f.svc.cfg, *round, obs.eng, f.clk.now())
	if got := f.codexPosted(repo, pr) - before; got != 1 {
		t.Fatalf("concurrent self-heals must post exactly once, got %d extra posts", got)
	}
}

// TestPumpAbandonsParkedClosedPR pins the parked-closed sweep: a PR closed
// while its round cools down in awaiting_retry is abandoned on the next pump,
// not after the retry window.
func TestPumpAbandonsParkedClosedPR(t *testing.T) {
	base := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, nil)
	repo, pr, head := "o/r", 22, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// Account block parks the round for 40 minutes.
	f.botComment(repo, pr, 9001, replayFairUsage(t, 40), f.clk.now().Add(5*time.Second))
	if res := f.pump(); res.Action != "requeued" {
		t.Fatalf("expected requeue, got %+v", res)
	}
	// The PR is closed mid-cooldown.
	f.gh.mu.Lock()
	p := f.gh.pulls[fakeKey(repo, pr)]
	p.State = "closed"
	f.gh.pulls[fakeKey(repo, pr)] = p
	f.gh.mu.Unlock()
	f.clk.advance(2 * time.Minute) // well inside the 40m window
	if res := f.pump(); res.Action != "skipped" || res.Reason != "pr closed" {
		t.Fatalf("a parked closed PR must be abandoned promptly, got %+v", res)
	}
	if r := f.round(repo, pr); r != nil {
		t.Fatalf("round must leave Rounds, got %+v", r)
	}
	// Never-delete invariant: the round must land in the archive as abandoned,
	// not vanish.
	if a := f.archived(repo, pr, head[:9]); a == nil || a.Phase != PhaseAbandoned {
		t.Fatalf("closed parked round must be archived abandoned, got %+v", a)
	}
}

// TestCoReviewWaitCountsPrePumpLegacySummary pins the wait anchor: with an
// adopted `@codex review` command already on the PR, a SHA-less legacy clean
// summary posted BEFORE the pump observes the round must still count — the wait
// anchors at the command time, not the observation time.
func TestCoReviewWaitCountsPrePumpLegacySummary(t *testing.T) {
	base := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 23, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	f.gh.graphQL = noForcePush // the force-push guard must resolve for the command to be adoptable
	// CodeRabbit reviewed the head; a human posted @codex review 5 minutes ago;
	// Codex answered with the LEGACY (SHA-less) clean summary 2 minutes ago.
	f.botReview(repo, pr, 500, head, base.Add(-10*time.Minute))
	f.humanComment(repo, pr, 600, codexReviewCommand, base.Add(-5*time.Minute))
	f.codexComment(repo, pr, 601, corpusMessage(t, "codex/clean-summary-legacy.md"), base.Add(-2*time.Minute))

	f.enqueue(repo, pr)
	f.pump() // FireCoReviewWait: anchors at the command time (-5m), not now
	f.clk.advance(time.Minute)
	f.pump() // sweep: Completion must count the -2m summary (≥ the -5m anchor)
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("a pre-pump legacy clean summary after the adopted command must complete the round, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("adopting the human's command must not post another, got %d", got)
	}
}

// TestLoopSettleWindowCatchesTrailingWave pins the settle window: a loop that
// observes convergence must NOT exit 0 immediately — a trailing review wave
// (Codex auto-reviewing the pushed head) arriving inside the window flips the
// verdict to findings, and a quiet window exits 0.
func TestLoopSettleWindowCatchesTrailingWave(t *testing.T) {
	base := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	run := func(inject bool) (int, int) {
		f := newCodexReplayFixture(t, base, func(cfg *Config) {
			cfg.SettleWindow = 10 * time.Minute
		})
		repo, pr, head := "o/r", 31, "aaaabbbbccccdddd"
		f.openPull(repo, pr, head)
		f.setCommitDate(head, base.Add(-time.Hour))
		f.botReview(repo, pr, 500, head, base.Add(-10*time.Minute)) // converged state
		// An ACTIVE wait is what the settle protects: without one, Wait's dedupe
		// path legitimately short-circuits before the feedback poll.
		seedRound(t, f.store, f.cfg, repo, pr, head[:9], PhaseReviewing, base.Add(-11*time.Minute), 0)
		type out struct {
			code int
			n    int
		}
		done := make(chan out, 1)
		go func() {
			rep, code, _ := f.svc.Loop(f.ctx, repo, pr)
			done <- out{code, len(rep.Findings)}
		}()
		// Wait for the loop to actually be polling before injecting, rather than
		// sleeping and hoping. A fixed 50ms lost under parallel load, which made
		// this the suite's one flaky test.
		deadline := time.Now().Add(5 * time.Second)
		for f.gh.reviewPolls() < 2 {
			if time.Now().After(deadline) {
				t.Fatal("the loop never reached its polling cycle")
			}
			time.Sleep(time.Millisecond)
		}
		if inject {
			// The trailing wave: a fresh CodeRabbit review whose body carries an
			// outside-diff finding (corpus shape), as after a push.
			f.gh.mu.Lock()
			r := ghapi.Review{ID: 501, CommitID: head, State: "COMMENTED", SubmittedAt: f.clk.now(),
				Body: corpusMessage(t, "coderabbit/findings-outside-diff.md")}
			r.User.Login = f.bot
			key := fakeKey(repo, pr)
			f.gh.reviews[key] = append(f.gh.reviews[key], r)
			before := f.gh.reviewReads // read under the same lock as the append
			f.gh.mu.Unlock()
			// A poll count of 2 only proves the loop is running — those reads may
			// both predate the injection (waitToFire polls too). Advancing the
			// clock then closes the settle window on a snapshot taken before the
			// wave, and the loop converges over a finding it never saw. Wait for a
			// poll whose snapshot is provably after the append.
			for f.gh.reviewPolls() <= before {
				if time.Now().After(deadline) {
					t.Fatal("no poll observed the injected review")
				}
				time.Sleep(time.Millisecond)
			}
		}
		f.clk.advance(11 * time.Minute) // past the settle window either way
		select {
		case o := <-done:
			return o.code, o.n
		case <-time.After(5 * time.Second):
			t.Fatal("loop did not return")
			return -1, -1
		}
	}
	if code, n := run(true); code != 10 || n == 0 {
		t.Fatalf("a wave inside the settle window must surface findings, got code=%d n=%d", code, n)
	}
	if code, _ := run(false); code != 0 {
		t.Fatalf("a quiet settle window must converge, got code=%d", code)
	}
}

// TestCodexReplayIgnoresAnsweredCommandForNewHead pins the observe fix: an old
// `@codex review` command Codex already answered with a review must not read as
// live for a later head. On a regular push whose commit date predates that
// consumed command, the command survives the adoption cutoff — but treating it as
// present would make DecideCodexPost see the head as already-asked and suppress
// the Codex command the new head still needs. crq must post a fresh @codex review.
func TestCodexReplayIgnoresAnsweredCommandForNewHead(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	f.gh.graphQL = noForcePush // resolve the force-push cutoff so a surviving command would be adoptable
	repo, pr := "o/r", 21
	oldHead, head := "1111222233334444", "5555666677778888"
	f.openPull(repo, pr, head)
	// The new head is an ordinary push whose commit date predates the old command,
	// so that command clears the cutoff and only the answered-review guard rejects it.
	f.setCommitDate(head, base.Add(-2*time.Hour))
	// Previous head's history: a `@codex review` command Codex already answered.
	f.humanComment(repo, pr, 600, codexReviewCommand, base.Add(-time.Hour))
	f.codexReview(repo, pr, 400, oldHead, base.Add(-30*time.Minute))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// The consumed command is ignored, so crq commands Codex for the new head.
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex must be commanded for the new head, got %d posts", got)
	}
	if r := f.round(repo, pr); r == nil || r.CodexCommandID == 0 {
		t.Fatalf("round must record the fresh codex command, got %+v", r)
	}
}

// TestCodexReplayAdoptRecordsExistingCodexCommand pins the adopt-path fix: when a
// round adopts an already-posted `@coderabbitai review` command and a live `@codex
// review` command already answers the head, crq is not posting Codex — but it must
// still record that command's id. Otherwise the self-heal scan (which anchors on
// FiredAt) misses a Codex command posted before the adopted CodeRabbit one and
// posts a duplicate.
func TestCodexReplayAdoptRecordsExistingCodexCommand(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	f.gh.graphQL = noForcePush // resolve the cutoff so both commands are adoptable
	repo, pr, head := "o/r", 22, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// The Codex command is posted BEFORE the CodeRabbit command crq will adopt, so a
	// self-heal scan anchored on the fire time would miss it and repost.
	const codexCmdID = 601
	f.humanComment(repo, pr, codexCmdID, codexReviewCommand, base.Add(-2*time.Minute))
	f.humanComment(repo, pr, 602, f.cfg.ReviewCommand, base.Add(-time.Minute))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected an adopt fire, got %+v", res)
	}
	// The adopt path posts neither command; it records the existing Codex command.
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("adopt must not post a codex command, got %d", got)
	}
	if r := f.round(repo, pr); r == nil || r.CodexCommandID != codexCmdID {
		t.Fatalf("adopt must record the existing codex command id %d, got %+v", codexCmdID, r)
	}

	// A later pump's self-heal must not repost the Codex command now that it is
	// recorded as the round's CodexCommandID.
	f.clk.advance(2 * time.Minute)
	f.pump()
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("self-heal must not repost the recorded codex command, got %d", got)
	}
}

// A round can go straight from fired to completed on one observation, and a
// completed round is never looked at again. The co-reviewer bookkeeping was
// only done in the reviewing sweep, so the evidence that Codex answered was
// lost with the round that carried it — and the bot guide showed a bot that had
// just reviewed as one nobody had heard from.
func TestCodexReplayRecordsAnswerWhenTheSlotRoundCompletes(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr := "o/r", 12
	head := "1111222233334444"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// Both reviews land before the next observation, so the slot round is
	// progressed once, all the way to completed.
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.codexReview(repo, pr, 502, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()

	r := f.round(repo, pr)
	if r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete once both reviewers answered, got %+v", r)
	}
	if r.Co(codexLogin).AnsweredAt == nil {
		t.Error("the round completed without recording that codex answered — the only field that tells a working bot from a silent one")
	}
}

// The primary's answer is recorded the same way a co-reviewer's is, from the
// same observation — and the round's PHASE is not allowed to stand in for it.
//
// A required set that omits the primary completes as soon as the co-reviewers
// answer and the primary merely acknowledges the metered command, so reading a
// completed round as review evidence labelled a reviewer as working on the
// strength of its own acknowledgement.
func TestReplayRecordsThePrimaryAnswerFromEvidenceNotThePhase(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		// Codex alone gates convergence, so the round can complete without the
		// primary having reviewed anything.
		cfg.RequiredBots = []string{codexLogin}
	})
	repo, pr := "o/r", 21
	head := "1111222233334444"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// Only Codex answers. The primary has been asked and has done nothing.
	f.codexReview(repo, pr, 502, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()

	r := f.round(repo, pr)
	if r == nil {
		t.Fatal("the round vanished")
	}
	if r.FiredAt == nil {
		t.Error("the round must still record that the primary was asked")
	}
	if r.PrimaryAnsweredAt != nil {
		t.Errorf("the primary was recorded as having answered at %s, but it only ever acknowledged", r.PrimaryAnsweredAt)
	}

	// Once it actually reviews the head, that IS the evidence.
	f.botReview(repo, pr, 503, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r = f.round(repo, pr); r == nil || r.PrimaryAnsweredAt == nil {
		t.Errorf("a review at the head must record the primary's answer, got %+v", r)
	}
}

func TestReplayRecordsCleanPrimarySummaryAsAnAnswer(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot}
	})
	repo, pr, head := "o/r", 22, "5555666677778888"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	f.botComment(repo, pr, 504,
		corpusMessage(t, "coderabbit/no-actionable-comments.md"),
		f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()

	r := f.round(repo, pr)
	if r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("clean primary summary must complete the round, got %+v", r)
	}
	if r.PrimaryAnsweredAt == nil {
		t.Fatalf("clean primary summary completed the round without recording an answer: %+v", r)
	}
}
