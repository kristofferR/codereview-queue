package crq

import (
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Greptile end to end, with its wording taken from the dialect corpus: a clean
// review leaves only an edited summary comment and a check run, findings
// arrive as inline threads that the summary repeats, and every trigger spends
// a credit.

const greptileLogin = dialect.GreptileLogin

// greptileOnly configures Greptile as the sole co-reviewer, the way an
// operator enables it, optionally required, with extra environment on top.
func greptileOnly(t *testing.T, required bool, extra ...string) func(*Config) {
	t.Helper()
	env := map[string]string{"CRQ_COBOTS": "greptile"}
	if required {
		env["CRQ_COBOT_GREPTILE_REQUIRED"] = "1"
	}
	for i := 0; i+1 < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	co, err := parseCoBots(env, nil)
	if err != nil {
		t.Fatalf("parseCoBots: %v", err)
	}
	return func(cfg *Config) {
		cfg.CoBots = co
		cfg.OnceBots = parseReviewOnce(env, cfg.Bot)
		if required {
			cfg.RequiredBots = append(cfg.RequiredBots, greptileLogin)
		}
		cfg.FeedbackBots = unionBots(cfg.RequiredBots, []string{greptileLogin})
	}
}

func (f *replayFixture) greptileComment(repo string, pr int, id int64, body string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	c.User.Login = greptileLogin
	key := fakeKey(repo, pr)
	f.gh.comments[key] = append(f.gh.comments[key], c)
}

// A required Greptile is asked with the fire, and a clean answer — no review
// object, just the summary naming the head — converges the round.
func TestGreptileReplayRequiredCleanSummaryConverges(t *testing.T) {
	base := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, greptileOnly(t, true))
	// The commit the captured clean summary says it reviewed.
	repo, pr, sha := "o/r", 41, "605a68e7ff137a4adc5b446074aede196d4db158"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}
	if got := f.coPostedBody(repo, pr, "@greptile"); got != 1 {
		t.Fatalf("a required Greptile must be asked with the fire, got %d posts", got)
	}

	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 500, sha, f.clk.now())
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converged {
		t.Fatalf("a required Greptile that has not answered must hold the round: %+v", rep)
	}

	f.clk.advance(3 * time.Minute)
	f.greptileComment(repo, pr, 600, corpusMessage(t, "greptile/summary-clean.md"), f.clk.now())
	rep, err = f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Converged || len(rep.Findings) != 0 {
		t.Fatalf("the clean summary must converge the round without becoming a finding: %+v", rep)
	}
	if st := rep.CoReviewers[dialect.NormalizeBotName(greptileLogin)]; !st.Reviewed {
		t.Fatalf("co_reviewers must report Greptile reviewed, got %+v", st)
	}
	if got := f.coPostedBody(repo, pr, "@greptile"); got != 1 {
		t.Fatalf("an answered command must not be repeated, got %d posts", got)
	}
}

// greptileFindingsOnFirstHead seeds a reviewed first head: CodeRabbit's
// review, Greptile's findings review, its summary and its check run, and one
// open Greptile thread.
func greptileFindingsOnFirstHead(t *testing.T, f *replayFixture, repo string, pr int, sha string, base time.Time) string {
	t.Helper()
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, sha[:9], PhaseReviewing, base.Add(-10*time.Minute), 400)
	f.botReview(repo, pr, 500, sha, base.Add(-5*time.Minute))
	f.gh.mu.Lock()
	carrier := ghapi.Review{ID: 501, CommitID: sha, State: "COMMENTED", SubmittedAt: base.Add(-4 * time.Minute)}
	carrier.User.Login = greptileLogin
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], carrier)
	f.gh.mu.Unlock()
	f.greptileComment(repo, pr, 600, corpusMessage(t, "greptile/summary-findings.md"), base.Add(-4*time.Minute))
	check := corpusCheckRun(t, "greptile/check-findings.json")
	check.CompletedAt = base.Add(-4 * time.Minute)
	f.gh.setCheckRuns(sha, check)
	return corpusMessage(t, "greptile/inline-finding-p1.md")
}

// An optional Greptile's findings surface once, from the thread rather than
// the summary that lists them. By default it reviews each pull request once,
// so a later head neither spends a credit asking it nor waits for it.
func TestGreptileReplayFindingsSurfaceOnceAndNoCreditIsSpent(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, greptileOnly(t, false))
	// The commit the captured findings summary says it reviewed.
	repo, pr, sha := "o/r", 42, "3ee8653629153f67c8d1a3abf8de7d7a1c914635"
	finding := greptileFindingsOnFirstHead(t, f, repo, pr, sha, base)
	path := "custom_components/adjustable_bed/beds/richmat/smart_base.py"
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", false, "greptile-apps", path, 308, 901, finding, base.Add(-4*time.Minute)),
	})

	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	var greptile []dialect.Finding
	for _, fd := range rep.Findings {
		if dialect.NormalizeBotName(fd.Bot) == dialect.NormalizeBotName(greptileLogin) {
			greptile = append(greptile, fd)
		}
	}
	if len(greptile) != 1 || greptile[0].CommentID != 901 {
		t.Fatalf("want exactly the inline finding, not the summary repeating it: %+v", rep.Findings)
	}
	if got := greptile[0]; got.Title != "Light stays off after turn-on" || got.Severity != dialect.SeverityMajor {
		t.Fatalf("finding = %q/%q, want the badge's title and P1 severity", got.Title, got.Severity)
	}
	if rep.Converged {
		t.Fatal("an open finding must block convergence")
	}

	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", true, "greptile-apps", path, 308, 901, finding, base.Add(-4*time.Minute)),
	})
	if rep, err = f.svc.Feedback(f.ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	if !rep.Converged || len(rep.Findings) != 0 {
		t.Fatalf("a resolved thread and completed check must converge: %+v", rep)
	}

	// Greptile reviewed the previous head on its own and stays silent on this
	// one, as it does unless the repository asks for reviews on every push.
	next := "4444444444444444444444444444444444444444"
	f.setHead(repo, pr, next)
	f.setCommitDate(next, base)
	f.gh.setCheckRuns(next)
	f.enqueue(repo, pr)
	f.pump()
	f.clk.advance(30 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "@greptile"); got != 0 {
		t.Fatalf("Greptile reviews once by default and must not be asked again, got %d posts", got)
	}
	// Nor waited for: CodeRabbit's review alone converges the later head.
	f.botReview(repo, pr, 700, next, f.clk.now())
	if rep, err = f.svc.Feedback(f.ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	if !rep.Converged {
		t.Fatalf("a Greptile that reviewed once must not hold a later head: %+v", rep)
	}
}

// Set to review every push, Greptile is asked for a head it skipped once the
// grace has passed, and that head waits for it.
func TestGreptileReplayEveryPushAsksForMissedHeads(t *testing.T) {
	base := time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, greptileOnly(t, false, "CRQ_REVIEW_ONCE", ""))
	repo, pr, sha := "o/r", 43, "3ee8653629153f67c8d1a3abf8de7d7a1c914635"
	finding := greptileFindingsOnFirstHead(t, f, repo, pr, sha, base)
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", true, "greptile-apps", "a.py", 1, 901, finding, base.Add(-4*time.Minute)),
	})

	next := "5555555555555555555555555555555555555555"
	f.setHead(repo, pr, next)
	f.setCommitDate(next, base)
	f.gh.setCheckRuns(next)
	f.enqueue(repo, pr)
	f.pump()
	f.botReview(repo, pr, 700, next, f.clk.now())
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converged {
		t.Fatalf("an every-push Greptile must hold the head it has not reviewed: %+v", rep)
	}
	f.clk.advance(15 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "@greptile"); got != 1 {
		t.Fatalf("an every-push Greptile must be asked once for the missed head, got %d posts", got)
	}
}

// A Codex thumbs-up names no commit. Seen after a push it cannot answer the
// previous head's round or spend that head's review budget, but it is still
// the reviewer's answer to the pull request, which a once reviewer needs.
func TestThumbsUpAfterPushAnswersThePullRequestOnly(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, func(cfg *Config) { cfg.MaxReviewRounds = 10 })
	repo, pr, old := "o/r", 44, "aaaaaaaa1"
	seedRound(t, f.store, f.cfg, repo, pr, old, PhaseFired, base.Add(-10*time.Minute), 400)
	obs := engine.Observation{Head: "bbbbbbbb2", Open: true, CodexThumbsUp: true}

	if _, err := f.svc.noteCoAnswers(f.ctx, f.cfg, *f.round(repo, pr), obs, base); err != nil {
		t.Fatal(err)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.CoReviewerAnswered(repo, pr, dialect.CodexBotLogin) {
		t.Fatal("the thumbs-up must be recorded as Codex's answer to the pull request")
	}
	if got := st.ReviewRoundCount(repo, pr); got != 0 {
		t.Fatalf("a thumbs-up seen after a push charged %d head(s) to the review budget", got)
	}
}

// Recording that answer must not disturb a replacement round another worker
// has already completed: nothing about that round changed.
func TestThumbsUpAfterPushKeepsACompletedReplacement(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr := "o/r", 45
	seedRound(t, f.store, f.cfg, repo, pr, "aaaaaaaa1", PhaseFired, base.Add(-10*time.Minute), 400)
	stale := *f.round(repo, pr)
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		fresh, err := st.Supersede(repo, pr, "bbbbbbbb2", base)
		if err != nil {
			return err
		}
		fresh.Phase = PhaseCompleted
		st.PutRound(*fresh)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	obs := engine.Observation{Head: "bbbbbbbb2", Open: true, CodexThumbsUp: true}
	if _, err := f.svc.noteCoAnswers(f.ctx, f.cfg, stale, obs, base); err != nil {
		t.Fatal(err)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("a PR-wide answer reopened the completed replacement round: %+v", r)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.CoReviewerAnswered(repo, pr, dialect.CodexBotLogin) {
		t.Fatal("the answer must still be recorded")
	}
}

// Read only through CRQ_FEEDBACK_BOTS, Greptile is not an enabled co-reviewer,
// but its summary still lists findings its threads carry and is not one itself.
func TestGreptileFeedbackOnlySummaryIsNotAFinding(t *testing.T) {
	base := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, func(cfg *Config) {
		cfg.CoBots = nil
		cfg.FeedbackBots = unionBots(cfg.RequiredBots, []string{greptileLogin})
	})
	repo, pr, sha := "o/r", 46, "3ee8653629153f67c8d1a3abf8de7d7a1c914635"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.greptileComment(repo, pr, 600, corpusMessage(t, "greptile/summary-findings.md"), base.Add(-time.Minute))
	f.greptileComment(repo, pr, 601, corpusMessage(t, "greptile/summary-clean.md"), base.Add(-time.Minute))
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range rep.Findings {
		if fd.CommentID == 600 || fd.CommentID == 601 {
			t.Fatalf("a Greptile summary surfaced as a finding: %+v", fd)
		}
	}
}

// Greptile's out-of-credits comment, seen on any pull request crq drives, is
// recorded fleet-wide for the dashboard.
func TestGreptileCreditPauseIsRecordedForTheDashboard(t *testing.T) {
	base := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, greptileOnly(t, false))
	repo, pr, sha := "o/r", 47, "3ee8653629153f67c8d1a3abf8de7d7a1c914635"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, sha[:9], PhaseReviewing, base.Add(-10*time.Minute), 400)
	f.greptileComment(repo, pr, 600, corpusMessage(t, "greptile/credits-exhausted.md"), base.Add(-time.Minute))
	if _, err := f.svc.Feedback(f.ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	note, ok := st.CoUnable[dialect.NormalizeBotName(greptileLogin)]
	if !ok || note.Repo != repo || note.PR != pr || !note.At.Equal(base.Add(-time.Minute)) {
		t.Fatalf("CoUnable = %+v, want the credit pause on %s#%d", st.CoUnable, repo, pr)
	}
}
