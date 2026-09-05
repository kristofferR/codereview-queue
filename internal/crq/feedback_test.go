package crq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

func TestLooksLikePathAcceptsRootFiles(t *testing.T) {
	for _, p := range []string{"Dockerfile", "Makefile", "LICENSE", "go.mod", "src/app.go", "a/b/c"} {
		if !dialect.LooksLikePath(p) {
			t.Fatalf("expected %q to be treated as a path", p)
		}
	}
	for _, p := range []string{"", "Additional comments", "🧹 Nitpick comments", "two words"} {
		if dialect.LooksLikePath(p) {
			t.Fatalf("expected %q NOT to be a path", p)
		}
	}
}

func TestDedupeSuppressesResolvedThreadPromptDuplicate(t *testing.T) {
	findings := []dialect.Finding{
		{Bot: "coderabbitai", Path: "internal/x.go", Line: 10, Title: "dup", Body: "do x", Source: "review_prompt"},
	}
	suppress := map[string]bool{"coderabbitai|internal/x.go|10": true}
	if got := dedupeFindings(findings, suppress, nil); len(got) != 0 {
		t.Fatalf("expected the prompt duplicate at a resolved-thread location to be suppressed, got %#v", got)
	}
	if got := dedupeFindings(findings, nil, nil); len(got) != 1 {
		t.Fatalf("expected the prompt finding to survive when not suppressed, got %#v", got)
	}
}

func TestFeedbackBoundsIssueCommentsToHead(t *testing.T) {
	cfg := Config{
		Bot:             "coderabbitai[bot]",
		RequiredBots:    []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		RateLimitMarker: "rate limited by coderabbit.ai",
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC()
	sha := "abcdef1234567890"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	gc := ghapi.Commit{SHA: sha}
	gc.Committer.Date = headTime
	gh.commits[sha] = gc
	mkc := func(id int64, body string, at time.Time) ghapi.IssueComment {
		ic := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = "chatgpt-codex-connector[bot]"
		return ic
	}
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{
		mkc(1, "Stale finding from the previous head", headTime.Add(-time.Hour)),
		mkc(2, "Current finding for this head", headTime.Add(time.Minute)),
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	rep, err := svc.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Body, "Current finding") {
		t.Fatalf("expected only the post-head issue comment as a finding, got %#v", rep.Findings)
	}
}

func TestFeedbackTreatsCodexRunningSummaryAsActivityNotFinding(t *testing.T) {
	started := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", dialect.CodexBotLogin},
	}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "15d9ec9abcdef123"
	gh.pulls[fakeKey("o/repo", 45)] = pull
	comment := ghapi.IssueComment{
		ID:        5462573246,
		Body:      corpusMessage(t, "codex/review-summary-running.md"),
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(2 * time.Minute),
	}
	comment.User.Login = dialect.CodexBotLogin
	gh.comments[fakeKey("o/repo", 45)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 45, "15d9ec9a", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(context.Background(), "o/repo", 45)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy[dialect.CodexBotLogin] {
		t.Fatalf("a running summary must not count as a completed review: %#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("a running summary must not become a finding: %#v", report.Findings)
	}
}

func TestFeedbackReturnsObservedAccountBlockPersistenceFailure(t *testing.T) {
	cfg := firingConfig()
	now := time.Now().UTC()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 3)] = pull
	notice := ghapi.IssueComment{
		ID: 17, Body: "You are rate limited by coderabbit.ai. Reviews available in 3 minutes.",
		CreatedAt: now, UpdatedAt: now,
	}
	notice.User.Login = cfg.Bot
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{notice}

	writeErr := errors.New("transient state write failure")
	inner := NewMemoryStore(cfg)
	store := &failNthUpdateStore{StateStore: inner, n: 1, err: writeErr}
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.Feedback(context.Background(), "o/repo", 3); !errors.Is(err, writeErr) {
		t.Fatalf("Feedback error = %v, want the account-block persistence failure", err)
	}
}

func TestReadOnlyFeedbackDoesNotPersistObservedAccountBlocks(t *testing.T) {
	cfg := firingConfig()
	now := time.Now().UTC()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 3)] = pull
	notice := ghapi.IssueComment{
		ID: 17, Body: "You are rate limited by coderabbit.ai. Reviews available in 3 minutes.",
		CreatedAt: now, UpdatedAt: now,
	}
	notice.User.Login = cfg.Bot
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{notice}

	writeErr := errors.New("read-only feedback attempted a state write")
	store := &failNthUpdateStore{StateStore: NewMemoryStore(cfg), n: 1, err: writeErr}
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.FeedbackReadOnly(context.Background(), "o/repo", 3); err != nil {
		t.Fatalf("read-only feedback failed: %v", err)
	}
}

func TestFeedbackReadOnlyInReusesLoadedStateAndPullFacts(t *testing.T) {
	cfg := firingConfig()
	github := newFakeGitHub()
	sha := "abcdef1234567890"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	pull.Additions = 12
	pull.Deletions = 4
	pull.ChangedFiles = 3
	github.pulls[fakeKey("o/repo", 3)] = pull
	github.commits[sha] = ghapi.Commit{SHA: sha}

	// Any accidental state reload fails this call. The supplied state must be
	// the only persisted snapshot the dashboard observation consumes.
	service := NewService(cfg, github, unreadableStore{NewMemoryStore(cfg)}, nil)
	report, err := service.FeedbackReadOnlyIn(t.Context(), DefaultState(cfg), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.HeadOID != sha {
		t.Fatalf("full head = %q, want %q", report.HeadOID, sha)
	}
	if report.Diff != (dialect.DiffStat{Additions: 12, Deletions: 4, ChangedFiles: 3}) {
		t.Fatalf("diff = %+v", report.Diff)
	}
}

func TestFeedbackCountsCompletionReplyForFiredHead(t *testing.T) {
	// A re-review with nothing new to say produces no review object: CodeRabbit
	// only replies "Review finished" to the command. That reply must satisfy
	// ReviewedBy when crq state proves the command was fired for this head —
	// otherwise the loop times out on a complete round. It only counts when the
	// bot has actually reviewed the PR before: on a never-reviewed PR the same
	// ack can arrive seconds after the trigger while the real review is still
	// queued on CodeRabbit's side.
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	setup := func(replyAt time.Time, replyBody string, seedHistory, priorReview bool) *Service {
		gh := newFakeGitHub()
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = sha
		gh.pulls[fakeKey("o/repo", 3)] = pull
		trigger := ghapi.IssueComment{ID: 1, Body: "@coderabbitai review", CreatedAt: firedAt, UpdatedAt: firedAt}
		trigger.User.Login = "kristofferR"
		reply := ghapi.IssueComment{ID: 2, Body: replyBody, CreatedAt: replyAt, UpdatedAt: replyAt}
		reply.User.Login = "coderabbitai[bot]"
		gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{trigger, reply}
		if priorReview {
			// A no-findings re-review presupposes an earlier review of the PR
			// (on some older commit); without one the completion reply must
			// not stand in for a review (covered by the dedicated case below).
			prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
			prior.User.Login = "coderabbitai[bot]"
			gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{prior}
		}
		store := NewMemoryStore(cfg)
		if seedHistory {
			seedRound(t, store, cfg, "o/repo", 3, head, PhaseReviewing, firedAt, 1)
		}
		return NewService(cfg, gh, store, nil)
	}

	rep, err := setup(firedAt.Add(5*time.Second), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("a completion reply after the fired command must satisfy the configured bot, got %#v", rep)
	}

	// A reply older than the fire belongs to an earlier round.
	rep, err = setup(firedAt.Add(-time.Minute), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a pre-fire reply must not satisfy the configured bot, got %#v", rep)
	}

	// Without a state anchor tying a fire to this head, issue comments never
	// satisfy ReviewedBy (the pre-existing rule).
	rep, err = setup(firedAt.Add(5*time.Second), completion, false, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("without a fired-head anchor the reply must not count, got %#v", rep)
	}

	// A rate-limit reply is not a completion.
	rep, err = setup(firedAt.Add(5*time.Second), "⚠️ rate limited by coderabbit.ai — please wait", true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a rate-limit reply must not satisfy the configured bot, got %#v", rep)
	}

	// An acknowledgement/progress reply without the completion marker is not a
	// completion either — the review is still running.
	rep, err = setup(firedAt.Add(5*time.Second), "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nFull review triggered.", true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("an ack reply without the completion marker must not satisfy the configured bot, got %#v", rep)
	}

	// A completion reply on a PR the bot has never reviewed is CodeRabbit
	// racing its own scheduler (instant "Review finished" ack, real review
	// still to come) — it must not converge the round.
	rep, err = setup(firedAt.Add(5*time.Second), completion, true, false).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a completion reply without any prior review must not satisfy the configured bot, got %#v", rep)
	}

	cfg.CompletionMarker = ""
	rep, err = setup(firedAt.Add(5*time.Second), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("an empty completion marker must disable the completion fallback, got %#v", rep)
	}
}

func TestFeedbackRejectsCompletionReplyWhileTopSummaryIsProcessing(t *testing.T) {
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	mk := func(id int64, login, body string, createdAt, updatedAt time.Time) ghapi.IssueComment {
		comment := ghapi.IssueComment{ID: id, Body: body, CreatedAt: createdAt, UpdatedAt: updatedAt}
		comment.User.Login = login
		return comment
	}

	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	command := mk(1, "kristofferR", cfg.ReviewCommand, firedAt, firedAt)
	completion := mk(2, cfg.Bot, "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", firedAt.Add(time.Minute), firedAt.Add(time.Minute))
	summary := mk(3, cfg.Bot, "<!-- review in progress by coderabbit.ai -->\nCurrently processing new changes in this PR. This may take a few minutes, please wait...", firedAt.Add(-time.Hour), firedAt.Add(2*time.Minute))
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{summary, command, completion}
	prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{prior}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 3, head, PhaseReviewing, firedAt, 1)
	service := NewService(cfg, gh, store, nil)

	report, err := service.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy[cfg.Bot] || report.Converged {
		t.Fatalf("a completion reply must not converge while the current top summary is processing, got %#v", report)
	}

	summary.Body = "No actionable comments were generated in the recent review."
	summary.UpdatedAt = firedAt.Add(3 * time.Minute)
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{summary, command, completion}
	report, err = service.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReviewedBy[cfg.Bot] || !report.Converged {
		t.Fatalf("the same completion may converge after the top summary becomes terminal, got %#v", report)
	}
}

func TestFeedbackRejectsCompletionReplyWhenTopSummaryFailed(t *testing.T) {
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	mk := func(id int64, login, body string, createdAt, updatedAt time.Time) ghapi.IssueComment {
		comment := ghapi.IssueComment{ID: id, Body: body, CreatedAt: createdAt, UpdatedAt: updatedAt}
		comment.User.Login = login
		return comment
	}

	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	command := mk(1, "kristofferR", cfg.ReviewCommand, firedAt, firedAt)
	completion := mk(2, cfg.Bot, "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", firedAt.Add(time.Minute), firedAt.Add(3*time.Minute))
	failure := mk(3, cfg.Bot, "<!-- This is an auto-generated comment: failure by coderabbit.ai -->\n## Review failed\n\nAn error occurred during the review process.", firedAt.Add(-time.Hour), firedAt.Add(2*time.Minute))
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{failure, command, completion}
	prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{prior}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 3, head, PhaseReviewing, firedAt, 1)
	service := NewService(cfg, gh, store, nil)

	report, err := service.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy[cfg.Bot] || report.Converged {
		t.Fatalf("a completion reply must not converge after the current top summary reports review failure, got %#v", report)
	}
	if report.Status != "waiting" {
		t.Fatalf("a failed review with no findings must remain waiting for successful evidence, got %#v", report)
	}
}

func TestFeedbackRejectsCompletionReplyFromEarlierRound(t *testing.T) {
	// Overlapping rounds: an old command's review is still finishing when the
	// PR is pushed and a new command fires. The old round's "Review finished"
	// lands after the new firedAt, but pairs with the old command — it must
	// not converge the new round. The completion answering the new command
	// must.
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	mk := func(id int64, login, body string, at time.Time) ghapi.IssueComment {
		ic := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = login
		return ic
	}
	setup := func(comments []ghapi.IssueComment) *Service {
		gh := newFakeGitHub()
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = sha
		gh.pulls[fakeKey("o/repo", 3)] = pull
		gh.comments[fakeKey("o/repo", 3)] = comments
		// The completion fallback requires a prior review by the bot (the old
		// round's review of the previous head).
		prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
		prior.User.Login = "coderabbitai[bot]"
		gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{prior}
		store := NewMemoryStore(cfg)
		seedRound(t, store, cfg, "o/repo", 3, head, PhaseReviewing, firedAt, 1)
		return NewService(cfg, gh, store, nil)
	}

	oldCmd := mk(1, "kristofferR", "@coderabbitai review", firedAt.Add(-5*time.Minute))
	newCmd := mk(2, "kristofferR", "@coderabbitai review", firedAt)
	oldDone := mk(3, "coderabbitai[bot]", completion, firedAt.Add(30*time.Second))

	rep, err := setup([]ghapi.IssueComment{oldCmd, newCmd, oldDone}).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("the old round's completion must not converge the new round, got %#v", rep)
	}

	newDone := mk(4, "coderabbitai[bot]", completion, firedAt.Add(2*time.Minute))
	rep, err = setup([]ghapi.IssueComment{oldCmd, newCmd, oldDone, newDone}).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("the completion answering the new command must converge the round, got %#v", rep)
	}
}

func TestFeedbackPairsRepliesWithTidiedCommandHistory(t *testing.T) {
	cfg := firingConfig()
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	oldCommandAt := firedAt.Add(-5 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	mk := func(id int64, login, body string, at time.Time) ghapi.IssueComment {
		ic := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = login
		return ic
	}

	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	// Tidy removed command 1. Its delayed completion lands after command 2,
	// and must still pair with the remembered command 1.
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{
		mk(2, "kristofferR", cfg.ReviewCommand, firedAt),
		mk(3, cfg.Bot, completion, firedAt.Add(30*time.Second)),
	}
	prior := ghapi.Review{
		ID:          9,
		CommitID:    "0123456fedcba",
		State:       "COMMENTED",
		SubmittedAt: oldCommandAt.Add(-time.Hour),
		Body:        "**Actionable comments posted: 2**",
	}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{prior}

	store := NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *State) error {
		old, err := st.NewRound("o/repo", 3, "0123456fe", oldCommandAt)
		if err != nil {
			return err
		}
		if err := old.Reserve("old", "host", oldCommandAt); err != nil {
			return err
		}
		if err := old.Fire(1, oldCommandAt); err != nil {
			return err
		}
		old.RecordPosted(cfg.Bot, 1, oldCommandAt)
		st.PutRound(*old)

		current, err := st.Supersede("o/repo", 3, head, firedAt)
		if err != nil {
			return err
		}
		if err := current.Reserve("current", "host", firedAt); err != nil {
			return err
		}
		if err := current.Fire(2, firedAt); err != nil {
			return err
		}
		current.RecordPosted(cfg.Bot, 2, firedAt)
		if err := current.Acknowledge(); err != nil {
			return err
		}
		st.PutRound(*current)
		// Tidy persisted the old command outside Archive before removing it.
		// Enough unrelated rounds then finish to evict this PR's old round from
		// the fleet-wide ring before its delayed completion arrives.
		st.RecordTidied("o/repo", 3, PostedCommand{ID: 1, Bot: cfg.Bot, At: oldCommandAt})
		for i := 0; i < ArchiveMax; i++ {
			st.Archive = append(st.Archive, Round{
				Repo: "o/other", PR: 100 + i, Head: "999999999",
				Phase: PhaseCompleted, EnqueuedAt: oldCommandAt,
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := NewService(cfg, gh, store, nil).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy[cfg.Bot] || report.Converged {
		t.Fatalf("the old command's delayed completion converged the current round: %#v", report)
	}
}

func TestFeedbackSkipsReviewAnsweredCommandsWhenPairingCompletionReplies(t *testing.T) {
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	mk := func(id int64, login, body string, at time.Time) ghapi.IssueComment {
		ic := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = login
		return ic
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	gh.comments[fakeKey("o/repo", 3)] = []ghapi.IssueComment{
		mk(1, "kristofferR", "@coderabbitai review", firedAt.Add(-5*time.Minute)),
		mk(2, "kristofferR", "@coderabbitai review", firedAt),
		mk(3, "coderabbitai[bot]", completion, firedAt.Add(30*time.Second)),
	}
	oldReview := ghapi.Review{ID: 44, CommitID: "1111111111111111", SubmittedAt: firedAt.Add(-4 * time.Minute)}
	oldReview.User.Login = cfg.Bot
	gh.reviews[fakeKey("o/repo", 3)] = []ghapi.Review{oldReview}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 3, head, PhaseReviewing, firedAt, 1)
	svc := NewService(cfg, gh, store, nil)

	rep, err := svc.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("the old review object should consume the old command so the completion pairs with the fired command, got %#v", rep)
	}
}

func TestFeedbackSurfacesCodexEvenWhenNotRequired(t *testing.T) {
	// Regression: Codex (chatgpt-codex-connector) reviews a PR and posts inline
	// findings, but it isn't in RequiredBots (which is CodeRabbit-only by default).
	// crq must still surface Codex's findings — and must NOT falsely converge just
	// because CodeRabbit reviewed clean — while not waiting on Codex to converge.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, []string{dialect.CodexBotLogin}),
	}
	gh := newFakeGitHub()
	sha := "abcdef1234567890"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 7)] = pull

	// CodeRabbit reviewed this head and found nothing (empty body → no findings).
	crReview := ghapi.Review{ID: 1, Body: "", CommitID: sha, SubmittedAt: time.Now().UTC()}
	crReview.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 7)] = []ghapi.Review{crReview}

	// Codex left an inline finding on the same head (REST review-comment path, since
	// the fake GraphQL is unavailable and Feedback falls back to it).
	cx := ghapi.ReviewComment{ID: 22, Body: "**Fix the off-by-one.** This clips the last row.", Path: "app/x.go", Line: 10, CommitID: sha}
	cx.User.Login = "chatgpt-codex-connector[bot]"
	gh.reviewComments[fakeKey("o/repo", 7)] = []ghapi.ReviewComment{cx}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Body, "off-by-one") {
		t.Fatalf("expected the Codex finding to be surfaced, got %#v", rep.Findings)
	}
	if dialect.NormalizeBotName(rep.Findings[0].Bot) != "chatgpt-codex-connector" {
		t.Fatalf("expected the finding attributed to Codex, got %q", rep.Findings[0].Bot)
	}
	if rep.Converged {
		t.Fatal("must not converge while a Codex finding is open")
	}
	// Convergence gate stays CodeRabbit-only: Codex must not be tracked in ReviewedBy
	// (otherwise crq would hang on repos where Codex never reviews).
	if _, tracked := rep.ReviewedBy["chatgpt-codex-connector[bot]"]; tracked {
		t.Fatalf("Codex must not gate convergence, ReviewedBy=%#v", rep.ReviewedBy)
	}
	if reviewed, ok := rep.ReviewedBy["coderabbitai[bot]"]; !ok || !reviewed {
		t.Fatalf("CodeRabbit should be marked reviewed, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestParseReviewBodyFindingsExtractsOutsideDiffItems(t *testing.T) {
	review := ghapi.Review{
		ID: 99,
		Body: `> [!CAUTION]
> Some comments are outside the diff and can't be posted inline.
>
> <details>
> <summary>Outside diff range comments (1)</summary><blockquote>
>
> <details>
> <summary>internal/foo.go (1)</summary><blockquote>
>
> ` + "`42-43`: _Functional Correctness_ | _🟠 Major_ | _⚡ Quick win_" + `
>
> **Fix the cancellation path.**
>
> The operation can continue after cancellation.
>
> <!-- cr-comment:v1:abcdef -->
>
> </blockquote></details>
> </blockquote></details>`,
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "internal/foo.go" || findings[0].Line != 42 || findings[0].Severity != "major" {
		t.Fatalf("first finding mismatch: %#v", findings[0])
	}
	if findings[0].Title != "Fix the cancellation path." {
		t.Fatalf("title mismatch: %#v", findings[0])
	}
}

func TestParseReviewBodyFindingsExtractsNestedQuoteSections(t *testing.T) {
	// CodeRabbit nests "Outside diff range" / "Duplicate" / nitpick sections
	// two-plus blockquote levels deep. A single-level quote strip leaves
	// "> " prefixes that break the anchored line-range header match, so
	// every finding in those sections used to be silently dropped.
	review := ghapi.Review{
		ID: 100,
		Body: "> [!WARNING]\n" +
			"> Review had issues posting inline.\n" +
			">\n" +
			"> <details>\n" +
			"> <summary>Outside diff range comments (2)</summary><blockquote>\n" +
			">\n" +
			"> > <details>\n" +
			"> > <summary>internal/deep.go (1)</summary><blockquote>\n" +
			"> >\n" +
			"> > `10-12`: _Functional Correctness_ | _Major_\n" +
			"> >\n" +
			"> > **Nested finding one.**\n" +
			"> >\n" +
			"> > Body of the first nested finding.\n" +
			"> >\n" +
			"> > </blockquote></details>\n" +
			"> > <details>\n" +
			"> > <summary>internal/deeper.go (1)</summary><blockquote>\n" +
			"> >\n" +
			"> > > `20-21`: _Maintainability_ | _Minor_\n" +
			"> > >\n" +
			"> > > **Nested finding two.**\n" +
			"> > >\n" +
			"> > > Body of the second, even deeper finding.\n" +
			"> >\n" +
			"> > </blockquote></details>\n" +
			"> </blockquote></details>",
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "internal/deep.go" || findings[0].Line != 10 || findings[0].Title != "Nested finding one." {
		t.Fatalf("first finding mismatch: %#v", findings[0])
	}
	if findings[1].Path != "internal/deeper.go" || findings[1].Line != 20 || findings[1].Title != "Nested finding two." {
		t.Fatalf("second finding mismatch: %#v", findings[1])
	}
}

func TestParseReviewBodyFindingsExtractsCommentsFailedToPost(t *testing.T) {
	// CodeRabbit's "Comments failed to post" section uses un-backticked line
	// headers (561-573:) unlike the backticked "Outside diff range" form.
	review := ghapi.Review{
		ID: 7,
		Body: "<details>\n<summary>🛑 Comments failed to post (1)</summary><blockquote>\n\n" +
			"<details>\n<summary>src-tauri/inject/messenger.js (1)</summary><blockquote>\n\n" +
			"561-573: _📐 Maintainability & Code Quality_ | _🟠 Major_ | _⚡ Quick win_\n\n" +
			"**Move the hide-names toggle out of `messenger.js` or update the allowlist first.**\n\n" +
			"This adds a new injection-layer responsibility outside the documented scope.\n\n" +
			"</blockquote></details>\n\n</blockquote></details>",
		CommitID:    "165f71e41",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %#v", len(findings), findings)
	}
	f := findings[0]
	if f.Path != "src-tauri/inject/messenger.js" || f.Line != 561 || f.Severity != "major" {
		t.Fatalf("finding mismatch: %#v", f)
	}
	if f.Title != "Move the hide-names toggle out of `messenger.js` or update the allowlist first." {
		t.Fatalf("title mismatch: %q", f.Title)
	}
}

func TestParseReviewBodyFindingsExtractsPromptBlock(t *testing.T) {
	review := ghapi.Review{
		ID: 100,
		Body: `<details>
<summary>🤖 Prompt for all review comments with AI agents</summary>

` + "```" + `
Verify each finding against current code.

Inline comments:
In ` + "`@src/app.ts`" + `:
- Around line 12-14: The parser accepts stale state. Re-read the latest state
  before writing so concurrent updates are not lost.

Outside diff comments:
In @README.md:
- Line 7: Add the missing install warning.
` + "```" + `
</details>`,
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "src/app.ts" || findings[0].Line != 12 || findings[0].Source != "review_prompt" {
		t.Fatalf("first prompt finding mismatch: %#v", findings[0])
	}
	if findings[1].Path != "README.md" || findings[1].Line != 7 {
		t.Fatalf("second prompt finding mismatch: %#v", findings[1])
	}
}

func TestParseReviewBodyFindingsExtractsCodexOutsideDiffItem(t *testing.T) {
	review := ghapi.Review{
		ID: 101,
		Body: `
### 💡 Codex Review

https://github.com/kristofferR/krisHQ/blob/347388ffda8ae3eb7060a6b960ea437a78780045/convex/sections/aiCommands.ts#L2170-L2174
**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Query learning history by topic before taking**

Taking the newest sessions for the whole user before filtering can hide older sessions for the requested topic.

<details><summary>ℹ️ About Codex in GitHub</summary>
This boilerplate must not become part of the finding.
</details>`,
		CommitID:    "850772b68de27efabc7ec5eeda30bb5ea138eb29",
		SubmittedAt: time.Date(2026, 7, 14, 13, 46, 14, 0, time.UTC),
		HTMLURL:     "https://github.com/kristofferR/krisHQ/pull/947#pullrequestreview-1",
	}
	findings := parseReviewBodyFindings(review, "chatgpt-codex-connector[bot]")
	if len(findings) != 1 {
		t.Fatalf("expected 1 Codex review-body finding, got %d: %#v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Path != "convex/sections/aiCommands.ts" || finding.Line != 2170 {
		t.Fatalf("location mismatch: %#v", finding)
	}
	if finding.Title != "Query learning history by topic before taking" ||
		finding.Severity != "potential" || finding.Scale != "P2" {
		t.Fatalf("metadata mismatch: %#v", finding)
	}
	if finding.Commit != "347388ffd" || finding.Source != "review_body" {
		t.Fatalf("source mismatch: %#v", finding)
	}
	if strings.Contains(finding.Body, "boilerplate") || !strings.Contains(finding.Body, "newest sessions") {
		t.Fatalf("unexpected finding body: %q", finding.Body)
	}
}

func TestFeedbackSurfacesBodyFindingsFromSupersededCommit(t *testing.T) {
	// Regression: CodeRabbit's inline comments failed to post (GitHub 5xx / code
	// review limits), so 2 findings exist ONLY in the review body's prompt block,
	// on a commit that is no longer the head (the branch was rebased / squash-
	// merged after the review). Gating body extraction to the head silently drops
	// the whole review; crq must still surface those findings since no newer
	// CodeRabbit review supersedes them.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, []string{dialect.CodexBotLogin}),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	review := ghapi.Review{
		ID: 7,
		Body: `**Actionable comments posted: 2**
<details>
<summary>🤖 Prompt for all review comments with AI agents</summary>

` + "```" + `
Verify each finding against current code.

Inline comments:
In ` + "`@src/app.ts`" + `:
- Around line 12-14: The parser accepts stale state. Re-read the latest state.
- Around line 40-42: Validate the HTTP status before decoding the body.
` + "```" + `
</details>`,
		// Reviewed an earlier commit; the head has since moved on.
		CommitID:    "1111111111111111",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{review}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 body findings from the superseded-commit review, got %d: %#v", len(rep.Findings), rep.Findings)
	}
	// The review never landed on the head, so convergence must still wait.
	if rep.Converged {
		t.Fatal("must not converge: CodeRabbit has not reviewed the current head")
	}
	if reviewed := rep.ReviewedBy["coderabbitai[bot]"]; reviewed {
		t.Fatalf("CodeRabbit must not be marked reviewed for a non-head commit, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestFeedbackNewerHeadReviewSupersedesOldBodyFindings(t *testing.T) {
	// The companion to the above: once CodeRabbit re-reviews the current head and
	// finds nothing (empty body), its newer review supersedes the older body
	// findings so the loop can converge instead of resurfacing addressed items.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, []string{dialect.CodexBotLogin}),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	old := ghapi.Review{
		ID: 7,
		Body: "**Actionable comments posted: 1**\n<details>\n<summary>🤖 Prompt for all review comments with AI agents</summary>\n\n" +
			"```\nIn `@src/app.ts`:\n- Around line 12-14: Stale state.\n```\n</details>",
		CommitID:    "1111111111111111",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	old.User.Login = "coderabbitai[bot]"
	fresh := ghapi.Review{ID: 9, Body: "", CommitID: head, SubmittedAt: time.Now().UTC()}
	fresh.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{old, fresh}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("newer head review should supersede the old body findings, got %#v", rep.Findings)
	}
	if !rep.Converged {
		t.Fatalf("should converge: fresh head review is clean, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestFeedbackCurrentRoundDoesNotResurfacePreRoundBodyFindings(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, []string{dialect.CodexBotLogin}),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	old := ghapi.Review{
		ID: 7,
		Body: "**Actionable comments posted: 1**\n<details>\n<summary>🤖 Prompt for all review comments with AI agents</summary>\n\n" +
			"```\nIn `@src/app.ts`:\n- Around line 12-14: Stale state.\n```\n</details>",
		CommitID:    "1111111111111111",
		SubmittedAt: started.Add(-time.Hour),
	}
	old.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{old}
	completion := ghapi.IssueComment{
		ID:        10,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	completion.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 5)] = []ghapi.IssueComment{completion}

	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 5, head[:9], PhaseReviewing, started, 0)

	rep, err := NewService(cfg, gh, store, nil).Feedback(ctx, "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("pre-round body findings must not be re-emitted: %#v", rep.Findings)
	}
	if !rep.Converged || !rep.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("current clean completion should converge: %#v", rep)
	}
}

func TestFeedbackCurrentCodeRabbitRoundKeepsLatestCodexBodyFinding(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 14, 14, 15, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, []string{dialect.CodexBotLogin}),
	}
	gh := newFakeGitHub()
	head := "850772b68de27efabc7ec5eeda30bb5ea138eb29"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	codeRabbit := ghapi.Review{ID: 8, CommitID: head, SubmittedAt: started.Add(time.Minute)}
	codeRabbit.User.Login = "coderabbitai[bot]"
	codex := ghapi.Review{
		ID: 7,
		Body: `https://github.com/o/repo/blob/347388ffda8ae3eb7060a6b960ea437a78780045/convex/sections/aiCommands.ts#L2170-L2174
**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub> Query learning history by topic before taking**

Fetch by topic before applying the result limit.`,
		CommitID:    "347388ffda8ae3eb7060a6b960ea437a78780045",
		SubmittedAt: started.Add(-30 * time.Minute),
	}
	codex.User.Login = "chatgpt-codex-connector[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{codex, codeRabbit}

	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 5, head[:9], PhaseReviewing, started, 0)

	rep, err := NewService(cfg, gh, store, nil).Feedback(ctx, "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Source != "review_body" {
		t.Fatalf("the current CodeRabbit round must not suppress the latest Codex body finding: %#v", rep.Findings)
	}
}

func TestThreadFindingsSurfacesUnresolvedAcrossCommits(t *testing.T) {
	bots := dialect.BotSet([]string{"coderabbitai[bot]"})
	mk := func(resolved, outdated bool, oid string) reviewThread {
		var th reviewThread
		th.ID = "PRRT_x"
		th.IsResolved = resolved
		th.IsOutdated = outdated
		th.Path = "internal/foo.go"
		th.Line = 42
		c := struct {
			DatabaseID   int64     `json:"databaseId"`
			Body         string    `json:"body"`
			URL          string    `json:"url"`
			Path         string    `json:"path"`
			Line         int       `json:"line"`
			OriginalLine int       `json:"originalLine"`
			CreatedAt    time.Time `json:"createdAt"`
			Author       struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
			OriginalCommit struct {
				OID string `json:"oid"`
			} `json:"originalCommit"`
		}{Body: "**Potential issue** still unfixed.", Line: 42}
		c.Author.Login = "coderabbitai[bot]"
		c.Commit.OID = oid
		th.Comments.Nodes = append(th.Comments.Nodes, c)
		return th
	}

	// Unresolved + not outdated, filed on an OLD commit (!= current HEAD): still surfaced.
	got := threadFindings(mk(false, false, "0000oldcommit"), bots)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for unresolved old-commit thread, got %d", len(got))
	}
	if got[0].ThreadID != "PRRT_x" || got[0].Line != 42 {
		t.Fatalf("finding mismatch: %#v", got[0])
	}
	if got[0].Title != "Potential issue still unfixed." {
		t.Fatalf("finding title = %q, want reviewer markup removed", got[0].Title)
	}

	// Resolved and outdated threads are skipped regardless of commit.
	if got := threadFindings(mk(true, false, "0000oldcommit"), bots); len(got) != 0 {
		t.Fatalf("expected resolved thread to be skipped, got %d", len(got))
	}
	if got := threadFindings(mk(false, true, "0000oldcommit"), bots); len(got) != 0 {
		t.Fatalf("expected outdated thread to be skipped, got %d", len(got))
	}
}

func TestInBotsToleratesBotSuffix(t *testing.T) {
	bots := dialect.BotSet([]string{"coderabbitai[bot]", "chatgpt-codex"})
	// REST reports "coderabbitai[bot]"; GraphQL review threads report "coderabbitai".
	for _, login := range []string{"coderabbitai[bot]", "coderabbitai", "chatgpt-codex", "chatgpt-codex[bot]"} {
		if !dialect.InBots(bots, login) {
			t.Fatalf("expected %q to match a configured bot", login)
		}
	}
	if dialect.InBots(bots, "some-human") {
		t.Fatal("unexpected match for a non-bot login")
	}
}

func TestFeedbackSkipsConfiguredBotIssueCommentsAcrossSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", RequiredBots: []string{"coderabbitai"}}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := ghapi.IssueComment{Body: "CodeRabbit summary text", UpdatedAt: time.Now().UTC()}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, err := svc.Feedback(context.Background(), "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("configured bot summary issue comments should be skipped across suffix forms: %#v", report.Findings)
	}
}

func TestFeedbackMarksCurrentNoActionCompletionCommentReviewed(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := ghapi.IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("expected completion comment to converge the round, got %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("expected CodeRabbit to be marked reviewed, ReviewedBy=%#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("completion comment must not become a finding: %#v", report.Findings)
	}
}

func TestFeedbackIgnoresStaleNoActionCompletionComment(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := ghapi.IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(-time.Minute),
		UpdatedAt: started.Add(-time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" || report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("stale completion comment must not satisfy the current round, got %#v", report)
	}
}

func TestFeedbackDoesNotUseNoActionCompletionWhileCodexRequiredWithoutThumbsUp(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	cfg.CoBots = codexCoBots(cfg.RequiredBots) // Codex enabled as a co-reviewer
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := ghapi.IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" {
		t.Fatalf("expected to keep waiting for Codex without a thumbs-up, got %#v", report)
	}
	if report.ReviewedBy["coderabbitai[bot]"] || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("no-action comment must not satisfy the round while Codex is active without +1: %#v", report.ReviewedBy)
	}
}

func TestFeedbackUsesNoActionCompletionAfterCodexThumbsUp(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	command := ghapi.IssueComment{
		ID:        99,
		Body:      "@coderabbitai review",
		CreatedAt: started,
		UpdatedAt: started,
	}
	command.User.Login = "kristofferR"
	comment := ghapi.IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{command, comment}
	thumb := ghapi.Reaction{Content: "+1", CreatedAt: started.Add(2 * time.Minute)}
	thumb.User.Login = "chatgpt-codex-connector[bot]"
	gh.reactions[99] = []ghapi.Reaction{thumb}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 99)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("expected CodeRabbit no-action plus Codex +1 to converge, got %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || !report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected both bots reviewed, ReviewedBy=%#v", report.ReviewedBy)
	}
}

func TestFeedbackIgnoresOptionalCodexCleanReviewSummary(t *testing.T) {
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := ghapi.Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []ghapi.Review{review}
	comment := ghapi.IssueComment{
		ID:        10,
		Body:      "## Codex Review\n\nDidn't find any major issues. Keep them coming!",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, err := svc.Feedback(context.Background(), "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("optional Codex clean summary must not block a clean required review: %#v", report)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Codex clean summary must not become a finding: %#v", report.Findings)
	}
	if _, tracked := report.ReviewedBy["chatgpt-codex-connector[bot]"]; tracked {
		t.Fatalf("optional Codex must remain outside the convergence gate: %#v", report.ReviewedBy)
	}
}

func TestFeedbackMarksRequiredCodexCleanReviewSummaryReviewed(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	cfg.CoBots = codexCoBots(cfg.RequiredBots) // Codex enabled as a co-reviewer
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := ghapi.Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: started.Add(time.Minute)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []ghapi.Review{review}
	comment := ghapi.IssueComment{
		ID:        10,
		Body:      "## Codex Review\n\nDidn't find any major issues. Keep them coming!",
		CreatedAt: started.Add(2 * time.Minute),
		UpdatedAt: started.Add(2 * time.Minute),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("current Codex clean summary should converge both required bots: %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || !report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected both required bots reviewed: %#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Codex clean summary must not become a finding: %#v", report.Findings)
	}
}

func TestFeedbackDoesNotUseStaleCodexCleanReviewSummary(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := ghapi.Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: started.Add(time.Minute)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []ghapi.Review{review}
	comment := ghapi.IssueComment{
		ID:        10,
		Body:      "Codex Review: Didn’t find any major issues. Keep them coming!",
		CreatedAt: started.Add(-time.Minute),
		UpdatedAt: started.Add(-time.Minute),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/repo", 1, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" {
		t.Fatalf("stale Codex clean summary must not satisfy the current round: %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("only CodeRabbit should be current: %#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("stale Codex clean summary must still be non-actionable: %#v", report.Findings)
	}
}

func TestLoopConvergesOnCurrentNoActionCompletionComment(t *testing.T) {
	ctx := context.Background()
	started := time.Now().UTC().Add(-2 * time.Millisecond)
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		PollInterval:        time.Nanosecond,
		FeedbackWaitTimeout: time.Millisecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	comment := ghapi.IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(500 * time.Microsecond),
		UpdatedAt: started.Add(500 * time.Microsecond),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Converged {
		t.Fatalf("expected loop to stop converged on completion comment, code=%d report=%#v", code, report)
	}
}

func TestDedupeFindingsDropsNonActionableBotArtifacts(t *testing.T) {
	findings := []dialect.Finding{
		{Bot: "coderabbitai", Title: "> Skipped: comment is from another GitHub bot.", Body: "> Skipped: comment is from another GitHub bot.", Source: "review_thread"},
		{Bot: "chatgpt-codex-connector[bot]", Title: "You have reached your Codex usage limits for code reviews.", Body: "You have reached your Codex usage limits for code reviews.", Source: "issue_comment"},
		{Bot: "coderabbitai", Title: "<!-- cr-comment:v1:abcdef -->", Body: "---\n<!-- cr-indicator-types:nitpick -->", Source: "review_body"},
		{Bot: "coderabbitai", Title: "Past review finding", Body: "The previously flagged issue is now fixed. No further action is needed.", Source: "review_body"},
		{Bot: "coderabbitai", Title: "Biometric flow", Body: "Worth confirming this is the intended UX.", Source: "review_body"},
	}
	if got := dedupeFindings(findings, nil, nil); len(got) != 0 {
		t.Fatalf("expected non-actionable bot artifacts to be dropped, got %#v", got)
	}
}

func TestThreadFindingsMatchesGraphQLBotLogin(t *testing.T) {
	bots := dialect.BotSet([]string{"coderabbitai[bot]"})
	var th reviewThread
	th.ID = "PRRT_z"
	th.Path = "internal/crq/foo.go"
	th.Line = 7
	c := th.Comments.Nodes[:0:0]
	_ = c
	node := struct {
		DatabaseID   int64     `json:"databaseId"`
		Body         string    `json:"body"`
		URL          string    `json:"url"`
		Path         string    `json:"path"`
		Line         int       `json:"line"`
		OriginalLine int       `json:"originalLine"`
		CreatedAt    time.Time `json:"createdAt"`
		Author       struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			OID string `json:"oid"`
		} `json:"commit"`
		OriginalCommit struct {
			OID string `json:"oid"`
		} `json:"originalCommit"`
	}{Body: "**Potential issue** fix this", Line: 7}
	node.Author.Login = "coderabbitai" // GraphQL form, no [bot]
	th.Comments.Nodes = append(th.Comments.Nodes, node)

	got := threadFindings(th, bots)
	if len(got) != 1 || got[0].ThreadID != "PRRT_z" {
		t.Fatalf("expected the GraphQL-login thread to surface with its thread_id, got %#v", got)
	}
}

func TestRateLimitDetectionCoversFairUsageFormat(t *testing.T) {
	cfg := Config{RateLimitMarker: "rate limited by coderabbit.ai"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	newMsg := "<!-- This is an auto-generated reply by CodeRabbit -->\n" +
		"You're currently rate limited under our Fair Usage Limits Policy. " +
		"Your next review will be available in 48 minutes."
	oldMsg := "You are rate limited by coderabbit.ai. Reviews available in 3 minutes."

	if !svc.cr.IsRateLimited(newMsg) {
		t.Fatal("must detect CodeRabbit's Fair Usage rate-limit message")
	}
	if !svc.cr.IsRateLimited(oldMsg) {
		t.Fatal("must still detect the legacy marker")
	}
	if svc.cr.IsRateLimited("LGTM — nice fix, nothing about limits here") {
		t.Fatal("must not flag a normal review comment")
	}

	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if reset := dialect.ParseAvailableIn(newMsg, base); reset == nil || !reset.Equal(base.Add(48*time.Minute)) {
		t.Fatalf("expected reset base+48m from the new message, got %v", reset)
	}
}

func TestParseAvailableIn(t *testing.T) {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	reset := dialect.ParseAvailableIn("Review limit reached. Reviews available in 1 hour 2 minutes 3 seconds.", base)
	if reset == nil {
		t.Fatal("expected reset")
	}
	want := base.Add(time.Hour + 2*time.Minute + 3*time.Second)
	if !reset.Equal(want) {
		t.Fatalf("reset mismatch: got %s want %s", reset, want)
	}
}

func TestParseQuota(t *testing.T) {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	remaining, reset := dialect.ParseQuota("0 reviews remaining. Reviews available in 3 minutes.", base)
	if remaining == nil || *remaining != 0 {
		t.Fatalf("remaining mismatch: %#v", remaining)
	}
	if reset == nil || !reset.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("reset mismatch: %#v", reset)
	}
}

func TestExtendDeadlineForBlock(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	budget := 20 * time.Minute
	base := now.Add(budget) // original deadline: now + 20m

	// Not blocked → deadline unchanged.
	if got := extendDeadlineForBlock(base, nil, now, budget); !got.Equal(base) {
		t.Fatalf("nil block: want %v, got %v", base, got)
	}
	// Block already elapsed → deadline unchanged.
	past := now.Add(-time.Minute)
	if got := extendDeadlineForBlock(base, &past, now, budget); !got.Equal(base) {
		t.Fatalf("past block: want %v, got %v", base, got)
	}
	// Block extends past the current deadline → push to blockedUntil + budget, so a
	// full review wait remains after the rate-limit window clears.
	until := now.Add(60 * time.Minute)
	if want, got := until.Add(budget), extendDeadlineForBlock(base, &until, now, budget); !got.Equal(want) {
		t.Fatalf("future block: want %v, got %v", want, got)
	}
	// A nearer block must never pull an already-extended deadline earlier.
	later := now.Add(80 * time.Minute)
	near := now.Add(5 * time.Minute)
	if got := extendDeadlineForBlock(later, &near, now, budget); !got.Equal(later) {
		t.Fatalf("must not shrink: want %v, got %v", later, got)
	}
}

// parkedRound builds a State whose repo#pr round is parked awaiting_retry at
// head until retryAt — the v3 per-head cooldown.
func parkedRound(t *testing.T, repo string, pr int, head string, retryAt, now time.Time) State {
	t.Helper()
	st := DefaultState(Config{})
	r, err := st.NewRound(repo, pr, head, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("t", "h", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.AwaitRetry(retryAt, "rate limited", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.PutRound(*r)
	return st
}

func TestAccountBlockedUntilHonorsPerHeadCooldownAfterGlobalBlockClears(t *testing.T) {
	now := time.Date(2026, 7, 13, 14, 4, 0, 0, time.UTC)
	cooldownUntil := now.Add(15 * time.Minute)
	st := parkedRound(t, "owner/repo", 947, "168df6ae6", cooldownUntil, now)

	got, ok := st.AccountBlockedUntil("owner/repo", 947, "168df6ae6", now)
	if !ok || !got.Equal(cooldownUntil) {
		t.Fatalf("matching head retry window must keep the feedback wait blocked: got %v, ok=%v", got, ok)
	}
	if _, ok := st.AccountBlockedUntil("owner/repo", 947, "different", now); ok {
		t.Fatal("a retry window for an older head must not block the current head")
	}
}

func TestAccountBlockedUntilUsesLatestAccountOrHeadWindow(t *testing.T) {
	now := time.Date(2026, 7, 13, 14, 4, 0, 0, time.UTC)
	accountUntil := now.Add(20 * time.Minute)
	cooldownUntil := now.Add(15 * time.Minute)
	st := parkedRound(t, "owner/repo", 947, "168df6ae6", cooldownUntil, now)
	st.Account.BlockedUntil = &accountUntil

	got, ok := st.AccountBlockedUntil("owner/repo", 947, "168df6ae6", now)
	if !ok || !got.Equal(accountUntil) {
		t.Fatalf("latest blocking window must win: got %v, ok=%v", got, ok)
	}
}

func TestFeedbackWaitElapsedExcludesBlockedTimeAndClampsProgress(t *testing.T) {
	budget := 20 * time.Minute
	blockStarts := time.Date(2026, 7, 13, 14, 4, 0, 0, time.UTC)
	blockEnds := blockStarts.Add(15 * time.Minute)
	deadline := blockEnds.Add(budget)

	if got := feedbackWaitElapsed(deadline, budget, blockStarts); got != 0 {
		t.Fatalf("blocked time must not consume review budget: got %v", got)
	}
	if got := feedbackWaitElapsed(deadline, budget, blockEnds.Add(5*time.Minute)); got != 5*time.Minute {
		t.Fatalf("reviewable time after the block must count normally: got %v", got)
	}
	if got := feedbackWaitElapsed(deadline, budget, deadline.Add(time.Hour)); got != budget {
		t.Fatalf("displayed progress must clamp at the configured budget: got %v", got)
	}
}

func TestBlockedPollInterval(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	base := 15 * time.Second

	// Block far out → capped, so the loop still re-checks periodically (not 15s).
	if got := blockedPollInterval(now.Add(30*time.Minute), now, base); got != 5*time.Minute {
		t.Fatalf("far block: want 5m, got %v", got)
	}
	// Block a couple minutes out → wait until just past it.
	if got := blockedPollInterval(now.Add(2*time.Minute), now, base); got != 2*time.Minute+time.Second {
		t.Fatalf("near block: want 2m1s, got %v", got)
	}
	// Block about to clear (within base) → never poll faster than the base interval.
	if got := blockedPollInterval(now.Add(3*time.Second), now, base); got != base {
		t.Fatalf("imminent block: want %v, got %v", base, got)
	}
}

func TestLoopReportsClosedPRSkip(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Millisecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || report.Status != "skipped" || report.Reason != "pr closed" {
		t.Fatalf("closed PR should be a terminal skipped report, code=%d report=%#v", code, report)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.Feedback(ctx, "owner/repo", 12); err != nil {
			t.Fatalf("repeated merged feedback %d: %v", i+1, err)
		}
	}
}

func TestMergedFeedbackStillFiltersDismissedFindings(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
	}
	gh := newFakeGitHub()
	repo, pr, head := "owner/repo", 13, "abcdef1234567890"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	review := ghapi.Review{
		ID: 901, CommitID: head, State: "COMMENTED", SubmittedAt: time.Now().UTC(),
		Body: corpusMessage(t, "coderabbit/findings-outside-diff.md"),
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head[:9], PhaseCompleted, review.SubmittedAt, 0)
	svc := NewService(cfg, gh, store, nil)

	before, err := svc.Feedback(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Findings) == 0 {
		t.Fatal("fixture did not produce a threadless finding")
	}
	if _, err := store.Update(ctx, func(st *State) error {
		round := st.Round(repo, pr)
		if round == nil {
			t.Fatal("fixture lost its round")
		}
		for _, finding := range before.Findings {
			if finding.ThreadID != "" || !round.Dismiss(finding.ID, "already handled") {
				t.Fatal("fixture did not record a threadless dismissal")
			}
		}
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pull.State = "closed"
	pull.Merged = true
	gh.pulls[fakeKey(repo, pr)] = pull

	after, err := svc.Feedback(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Findings) != 0 || after.Dismissed != len(before.Findings) {
		t.Fatalf("merged feedback resurfaced a dismissed finding: %+v", after)
	}
}

func TestLoopRequiresAllRequiredBotsAfterDedupe(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		PollInterval:        time.Nanosecond,
		FeedbackWaitTimeout: time.Nanosecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseCompleted, time.Now().UTC(), 1)
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatalf("deduped CodeRabbit review must not succeed before every required bot reviews: %#v", report)
	}
	if report.Status != "timeout" || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected timeout waiting for the missing required bot, code=%d report=%#v", code, report)
	}
}

func TestLoopResumesAwaitingFeedbackWithoutRefiring(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	cfg.CoBots = codexCoBots(cfg.RequiredBots) // Codex enabled as a co-reviewer
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	comment := ghapi.IssueComment{ID: 91, Body: "Actionable finding on the current head", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	review := ghapi.Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC().Add(-time.Minute)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) != 1 {
		t.Fatalf("expected resumed loop to return the persisted review feedback, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("resuming an awaiting head must not post another review command, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Codex posted a finding but no review verdict, so it dynamically gates this
	// round and is still pending. Hold-the-head keeps the round active (not
	// completed) so the daemon keeps observing Codex's review/timeout — the
	// co-review wait deadline bounds it. The invariant this test guards is that
	// resuming does not re-fire, which still holds.
	if state.WaitingHead("owner/repo", 12) != "abcdef123" {
		t.Fatalf("round must stay active while a gating reviewer is pending, got %q", state.WaitingHead("owner/repo", 12))
	}
	if state.FiredMarker("owner/repo", 12) != "abcdef123" {
		t.Fatalf("fired marker should remain for dedupe after collection")
	}
}

func TestLoopWaitsForReplacementReviewInsteadOfReturningCarriedPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Second,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	stale := ghapi.Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC()
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)

	go func() {
		time.Sleep(5 * time.Millisecond)
		fresh := ghapi.Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
		fresh.User.Login = "coderabbitai[bot]"
		gh.mu.Lock()
		gh.reviews[fakeKey("owner/repo", 12)] = append(gh.reviews[fakeKey("owner/repo", 12)], fresh)
		gh.mu.Unlock()
	}()

	svc := NewService(cfg, gh, store, nil)
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Converged || len(report.Findings) != 0 {
		t.Fatalf("loop should wait for the clean replacement review, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("an existing feedback wait must not post another command, posted=%d", len(gh.posted))
	}
}

func TestLoopReturnsExistingCodexFeedbackBeforeWaitingForReviewSlot(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		WaitTimeout:         25 * time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{
		ID:        91,
		Body:      "Actionable finding on the current head",
		CreatedAt: headTime.Add(time.Second),
		UpdatedAt: headTime.Add(time.Second),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	blockedUntil := time.Now().UTC().Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || report.Status != "feedback" || len(report.Findings) != 1 {
		t.Fatalf("existing actionable feedback must be cleared before a new review round, code=%d report=%#v", code, report)
	}
	if report.Reason != "unresolved findings must be addressed before a new review round" {
		t.Fatalf("expected an explicit fix-first reason, got %#v", report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("existing feedback must not fire or enqueue a replacement review, posted=%d", len(gh.posted))
	}
}

func TestLoopDoesNotBlockOnThreadlessReviewBodyFromPreviousHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Hour,
		PollInterval:        time.Millisecond,
		WaitTimeout:         time.Second,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.Review{
		ID:          7,
		Body:        "**Actionable comments posted: 1**\n<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Already fixed on the new head.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	go func() {
		time.Sleep(5 * time.Millisecond)
		fresh := ghapi.Review{ID: 8, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
		fresh.User.Login = "coderabbitai[bot]"
		gh.mu.Lock()
		gh.reviews[fakeKey("owner/repo", 12)] = append(gh.reviews[fakeKey("owner/repo", 12)], fresh)
		gh.mu.Unlock()
	}()

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Converged || len(report.Findings) != 0 {
		t.Fatalf("previous-head body summaries must not block a fresh review, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one current-head review command, posted=%d", len(gh.posted))
	}
}

func TestLoopReturnsFindingsBeforeRequiredReviewerTimeout(t *testing.T) {
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Minute)
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Nanosecond,
		FeedbackWaitTimeout: time.Millisecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	comment := ghapi.IssueComment{
		ID:        91,
		Body:      "Actionable finding on the current head",
		CreatedAt: started.Add(time.Second),
		UpdatedAt: started.Add(time.Second),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || report.Status != "feedback" || len(report.Findings) != 1 {
		t.Fatalf("buffered actionable feedback must take precedence over timeout, code=%d report=%#v", code, report)
	}
	if report.Reason != "hold current head: fix locally, but do not commit or push until every required reviewer finishes" {
		t.Fatalf("expected an explicit hold-head reason, got %#v", report)
	}
}

func TestLoopReturnsFasterCodexFeedbackBeforeCodeRabbitReviews(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Second,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	codexFinding := ghapi.IssueComment{
		ID:        91,
		Body:      "Actionable Codex finding on the current head",
		CreatedAt: headTime.Add(time.Second),
		UpdatedAt: headTime.Add(time.Second),
	}
	codexFinding.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{codexFinding}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC()
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)

	svc := NewService(cfg, gh, store, nil)
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) != 1 || report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("loop should return Codex feedback before CodeRabbit completes the round, code=%d report=%#v", code, report)
	}
	if report.Reason != "hold current head: fix locally, but do not commit or push until every required reviewer finishes" {
		t.Fatalf("expected an explicit hold-head reason, got %#v", report)
	}
}

func TestLoopUsesPersistedFeedbackDeadline(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	started := time.Now().UTC().Add(-2 * time.Hour)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, started, 0)
	svc := NewService(cfg, gh, store, nil)

	begin := time.Now()
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
		t.Fatalf("expired persisted deadline should not reset to a fresh hour; elapsed=%s", elapsed)
	}
	if code != 2 || report.Status != "timeout" {
		t.Fatalf("expected timeout from the persisted deadline, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("expired feedback wait still must not refire the same head, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.WaitingHead("owner/repo", 12) != "" {
		t.Fatalf("expired feedback wait should clear after timeout")
	}
	if _, ok := state.WorkClaim("owner/repo", 12, time.Now().UTC()); ok {
		t.Fatal("timed-out loop left the interactive work claim behind")
	}
}

func TestPumpEveryForNeverPumpsMoreThanOncePerMinute(t *testing.T) {
	if got := pumpEveryFor(15 * time.Second); got != time.Minute {
		t.Fatalf("fast polls must clamp pump cadence to a minute, got %v", got)
	}
	if got := pumpEveryFor(5 * time.Minute); got != 5*time.Minute {
		t.Fatalf("slow polls pump once per poll, got %v", got)
	}
}

func TestCodexCleanSummaryFormats(t *testing.T) {
	legacy := "Codex Review: Didn't find any major issues. Keep them coming!"
	tada := "Codex Review: Didn't find any major issues. :tada:\n\n**Reviewed commit:** `4d9e8bca82`"

	if !dialect.IsCodexNoActionReviewCompletion(legacy) {
		t.Fatal("legacy clean summary must count as a completion")
	}
	if !dialect.IsCodexNoActionReviewCompletion(tada) {
		t.Fatal("reviewed-commit clean summary must count as a completion")
	}
	if dialect.IsCodexNoActionReviewCompletion("Codex Review: 2 issues need attention") {
		t.Fatal("a summary with findings must not count as a completion")
	}
	if got := dialect.CodexReviewedCommitSHA(tada); got != "4d9e8bca82" {
		t.Fatalf("expected the reviewed-commit sha, got %q", got)
	}
	if got := dialect.CodexReviewedCommitSHA(legacy); got != "" {
		t.Fatalf("legacy summary has no sha, got %q", got)
	}
	// crq truncates heads to 9 chars while Codex abbreviates to 10 — the
	// prefix match must work in both directions.
	if !dialect.SHAPrefixMatch("4d9e8bca82", "4d9e8bca8") || !dialect.SHAPrefixMatch("4d9e8bca8", "4d9e8bca82") {
		t.Fatal("mutual sha prefixes must match")
	}
	if dialect.SHAPrefixMatch("4d9e8bca82", "deadbeef1") {
		t.Fatal("different shas must not match")
	}
}

// addThreadComment appends a comment to a reviewThread (the node type is
// anonymous, so this hides the verbose literal).
func addThreadComment(th *reviewThread, id int64, login, body string) {
	node := th.Comments.Nodes[:0:0]
	_ = node
	var n struct {
		DatabaseID   int64     `json:"databaseId"`
		Body         string    `json:"body"`
		URL          string    `json:"url"`
		Path         string    `json:"path"`
		Line         int       `json:"line"`
		OriginalLine int       `json:"originalLine"`
		CreatedAt    time.Time `json:"createdAt"`
		Author       struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			OID string `json:"oid"`
		} `json:"commit"`
		OriginalCommit struct {
			OID string `json:"oid"`
		} `json:"originalCommit"`
	}
	n.DatabaseID = id
	n.Body = body
	n.Author.Login = login
	th.Comments.Nodes = append(th.Comments.Nodes, n)
}

func TestThreadRebuttalSurfacesContestedResolvedThreads(t *testing.T) {
	bots := dialect.BotSet([]string{"coderabbitai[bot]"})
	newThread := func(resolved bool) reviewThread {
		return reviewThread{ID: "PRRT_x", Path: "internal/engine/fire.go", Line: 126, IsResolved: resolved}
	}

	// Contested rebuttal on a resolved thread → surfaced.
	th := newThread(true)
	addThreadComment(&th, 1, "coderabbitai", "**Potential issue** the wait is unbounded")
	addThreadComment(&th, 2, "kristofferR", "Declined: the loop deadline bounds it.")
	addThreadComment(&th, 3, "coderabbitai", "I'm retaining the finding: adopt/record the existing command into a timed waiting round.")
	if got := threadRebuttal(th, bots); got == nil {
		t.Fatal("a contested bot reply on a resolved thread must surface")
	} else if got.Source != "review_reply" || got.ThreadID != "PRRT_x" || got.CommentID != 3 {
		t.Fatalf("rebuttal finding mismatch: %#v", got)
	} else if !strings.Contains(got.Title, "contests your reply") {
		t.Errorf("a reply that really retains the finding must be reported as a contest, got %q", got.Title)
	}

	// Withdrawn rebuttal → not surfaced.
	th = newThread(true)
	addThreadComment(&th, 1, "coderabbitai", "**Potential issue** duplicate declaration")
	addThreadComment(&th, 2, "kristofferR", "Declined: it compiles, single declaration.")
	addThreadComment(&th, 3, "coderabbitai", "You're right—my finding was incorrect. I'm withdrawing this comment.")
	if got := threadRebuttal(th, bots); got != nil {
		t.Fatalf("a withdrawn finding must not surface, got %#v", got)
	}

	// Ambiguous bot reply after the agent → surfaced (never bury a rebuttal).
	th = newThread(true)
	addThreadComment(&th, 1, "coderabbitai", "**Nitpick** rename this")
	addThreadComment(&th, 2, "kristofferR", "Declined: name is intentional.")
	addThreadComment(&th, 3, "coderabbitai", "Here is some additional context on the naming convention.")
	// It surfaces at the same severity — never bury a possible rebuttal — but it
	// must NOT be announced as a contest. Asserting one for every non-withdrawal
	// reply meant a bot AGREEING with the decline read as standing its ground,
	// and an agent told to re-address a rebuttal that did not exist looped on it.
	if got := threadRebuttal(th, bots); got == nil {
		t.Fatal("an ambiguous (non-withdrawal) reply must surface by default")
	} else if got.Severity != "major" {
		t.Fatalf("an unknown-severity rebuttal must floor at major, got %q", got.Severity)
	} else if strings.Contains(got.Title, "contests your reply") {
		t.Errorf("an ambiguous reply must not be asserted as a contest, got %q", got.Title)
	} else if !strings.Contains(got.Title, "read it") {
		t.Errorf("an ambiguous reply should tell the caller to read it, got %q", got.Title)
	}

	// No agent reply (just the bot's finding) → not a rebuttal.
	th = newThread(true)
	addThreadComment(&th, 1, "coderabbitai", "**Potential issue** fix this")
	if got := threadRebuttal(th, bots); got != nil {
		t.Fatalf("a lone bot finding is not a rebuttal, got %#v", got)
	}

	// Unresolved thread → threadFindings already covers it; no double-surface.
	th = newThread(false)
	addThreadComment(&th, 1, "coderabbitai", "**Potential issue** the wait is unbounded")
	addThreadComment(&th, 2, "kristofferR", "Declined.")
	addThreadComment(&th, 3, "coderabbitai", "I'm retaining the finding.")
	if got := threadRebuttal(th, bots); got != nil {
		t.Fatalf("unresolved threads are handled by threadFindings, got %#v", got)
	}
}

// TestFeedbackSurfacesSkippedReviewDespiteRateLimitMarker pins an ordering trap
// that bit the first implementation: CodeRabbit's "Review skipped" notice embeds
// its rate-limit marker, and the "an account-quota notice is never a finding"
// guard therefore swallowed it. The round then converged with the primary
// reviewer marked done and NOTHING telling the agent the review never happened —
// a silently absent reviewer, which is worse than a loud one.
func TestFeedbackSurfacesSkippedReviewDespiteRateLimitMarker(t *testing.T) {
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		FeedbackBots:      []string{"coderabbitai[bot]"},
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
	}
	body := corpusMessage(t, "coderabbit/review-skipped-too-many-files.md")
	probe := dialect.CodeRabbit{RateLimitMarker: cfg.RateLimitMarker, CalibrationMarker: cfg.CalibrationMarker}
	if !probe.IsRateLimited(body) {
		t.Fatal("precondition: the skip notice must still carry the rate-limit marker")
	}

	gh := newFakeGitHub()
	sha := "56150a0423a243224b03f355c3a3ba6941011b5b"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 214)] = pull
	skip := ghapi.IssueComment{ID: 5074197614, Body: body,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC().Add(-time.Minute)}
	skip.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 214)] = []ghapi.IssueComment{skip}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	rep, err := svc.Feedback(context.Background(), "o/repo", 214)
	if err != nil {
		t.Fatal(err)
	}
	var skipFinding *dialect.Finding
	for i := range rep.Findings {
		if rep.Findings[i].CommentID == 5074197614 {
			skipFinding = &rep.Findings[i]
		}
	}
	if skipFinding == nil {
		t.Fatalf("the skipped-review notice must surface as actionable work, got %#v", rep.Findings)
	}
	if skipFinding.Severity != "major" {
		t.Errorf("severity = %q, want major", skipFinding.Severity)
	}
	if !strings.Contains(skipFinding.Title, "Too many files") {
		t.Errorf("the finding must carry CodeRabbit's own reason, got %q", skipFinding.Title)
	}
	// Findings block convergence, so the loop reports work rather than a false pass.
	if rep.Converged {
		t.Error("a skipped review with an open finding must not report converged")
	}
}

func TestFeedbackUsesTheFleetEditedPrimary(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "o/gate",
		"CRQ_HOST":          "test",
		"CRQ_COBOTS":        "",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	if err != nil {
		t.Fatal(err)
	}
	const primary = "replacement-reviewer[bot]"
	body := corpusMessage(t, "coderabbit/review-skipped-too-many-files.md")
	gh := newFakeGitHub()
	sha := "56150a0423a243224b03f355c3a3ba6941011b5b"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 214)] = pull
	skip := ghapi.IssueComment{ID: 5074197614, Body: body,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC().Add(-time.Minute)}
	skip.User.Login = primary
	gh.comments[fakeKey("o/repo", 214)] = []ghapi.IssueComment{skip}

	store := NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Fleet = fleetEnvSet(st.Fleet, "CRQ_BOT", primary, false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := NewService(cfg, gh, store, nil).Feedback(context.Background(), "o/repo", 214)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range rep.Findings {
		if finding.CommentID == skip.ID {
			return
		}
	}
	t.Fatalf("the skip notice from the state-resolved primary was dropped: %#v", rep.Findings)
}

// TestAccountBlockIgnoredWhenPrimaryWillNotReview pins the rule that stopped
// agents reasoning about CodeRabbit on repos it never reviews. On a summary-only
// round the account block is meaningless: it must not extend the wait deadline,
// must not slow the poll to the block window, and must not be narrated. Agents
// were reading that narration and concluding "hold the head until CodeRabbit
// lands" — on a repo where CodeRabbit never lands, i.e. never push, never
// converge.
func TestAccountBlockIgnoredWhenPrimaryWillNotReview(t *testing.T) {
	now := time.Now().UTC()
	blocked := now.Add(42 * time.Minute)
	deadline := now.Add(5 * time.Minute)

	// The block must not buy a summary-only round any extra deadline...
	if got := extendDeadlineForBlock(deadline, nil, now, 20*time.Minute); !got.Equal(deadline) {
		t.Fatalf("a nil block must leave the deadline alone, got %v", got)
	}
	// ...whereas a round that genuinely awaits CodeRabbit still gets it.
	if got := extendDeadlineForBlock(deadline, &blocked, now, 20*time.Minute); !got.After(deadline) {
		t.Fatalf("a real block must still extend a round that waits on it, got %v", got)
	}
	// And the poll must stay fast rather than sliding to the block window.
	if got := blockedPollInterval(blocked, now, time.Second); got < time.Minute {
		t.Fatalf("precondition: a real block slows the poll, got %v", got)
	}

	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		FeedbackBots:      []string{"coderabbitai[bot]"},
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
	}
	gh := newFakeGitHub()
	sha := "abcdef1234567890"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/private", 1022)] = pull
	walkthrough := ghapi.IssueComment{ID: 900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/private", 1022)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.BlockedUntil = &blocked
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	rep, err := svc.Feedback(context.Background(), "o/private", 1022)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.PrimaryUnavailable {
		t.Fatal("a summary-only head must report primary_review_unavailable so agents stop waiting on it")
	}
	if !strings.Contains(rep.PrimaryUnavailableReason, "summary") {
		t.Errorf("the reason must say why, got %q", rep.PrimaryUnavailableReason)
	}
	// The primary counts as delivered, so the agent is never told to hold the
	// head for it while an unrelated account block ticks down.
	if !rep.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("the primary must read as delivered, got %#v", rep.ReviewedBy)
	}
}

// TestSkippedFindingBindsToTheSkippedHead: the skip is a per-head refusal whose
// fix is narrowing the PR. Loop treats findings as blocking BEFORE it enqueues,
// so a skip of an earlier head that keeps surfacing would stop the narrowed
// replacement head from ever being submitted — the repair would permanently
// disable the reviewer it was meant to restore.
func TestSkippedFindingBindsToTheSkippedHead(t *testing.T) {
	skipBody := corpusMessage(t, "coderabbit/review-skipped-too-many-files.md")
	skippedSHA := "56150a0423a243224b03f355c3a3ba6941011b5b"

	if !skipAppliesToHead(skipBody, skippedSHA[:9]) {
		t.Error("the skipped head itself must still surface the notice")
	}
	if skipAppliesToHead(skipBody, "abcdef123") {
		t.Error("a narrowed replacement head must not inherit the old skip")
	}
	// A notice naming no commit is read conservatively as current.
	if !skipAppliesToHead("> ## Review skipped\n>\n> No commits to review.", "abcdef123") {
		t.Error("a SHA-less skip must bind to the observed head")
	}
}

// TestCoReviewerVerdictScopedToRound: co_reviewers[].verdict is documented as
// current-head state. Between a push and Macroscope's new Approvability comment
// the newest verdict on the PR describes the PREVIOUS head, and reporting it
// would misstate the current one.
func TestCoReviewerVerdictScopedToRound(t *testing.T) {
	cfg := Config{Bot: "coderabbitai[bot]", CoBots: []CoBotConfig{
		{Login: dialect.MacroscopeLogin, Name: "macroscope"},
	}}
	firedAt := time.Now().UTC()
	approved := true
	stale := dialect.BotEvent{Kind: dialect.EvCoVerdict, Bot: dialect.MacroscopeLogin,
		For: dialect.MacroscopeLogin, Approved: &approved, CommentID: 1,
		CreatedAt: firedAt.Add(-time.Hour), UpdatedAt: firedAt.Add(-time.Hour)}
	obs := engine.Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{stale}}

	key := dialect.NormalizeBotName(dialect.MacroscopeLogin)
	if got := coReviewerStatuses(cfg, obs, firedAt)[key].Verdict; got != "" {
		t.Fatalf("a verdict from the previous head must not be reported, got %q", got)
	}
	// The same verdict inside the round is reported normally.
	if got := coReviewerStatuses(cfg, obs, firedAt.Add(-2*time.Hour))[key].Verdict; got != "approved" {
		t.Fatalf("an in-round verdict must be reported, got %q", got)
	}
}

// TestBugbotStableIDSettledInAnySiblingThread: Bugbot re-reports one
// BUGBOT_BUG_ID across several threads after each push. Deduplication alone
// collapses only the findings emitted together, so resolving the emitted thread
// promotes a sibling on the next poll and the "single" finding resurfaces —
// blocking convergence until every duplicate is resolved by hand.
func TestBugbotStableIDSettledInAnySiblingThread(t *testing.T) {
	body := corpusMessage(t, "bugbot/inline-finding-high.md")
	stable, ok := dialect.BugbotFindingDedupeKey(body)
	if !ok {
		t.Fatal("precondition: the corpus finding must carry a BUGBOT_BUG_ID")
	}
	findings := []dialect.Finding{
		{Bot: dialect.BugbotLogin, Path: "a.ts", Line: 1, Title: "dup", Body: body, ThreadID: "PRRT_2", Source: "review_thread"},
	}
	// Nothing settled yet: the surviving sibling is still actionable.
	if got := dedupeFindings(findings, nil, nil); len(got) != 1 {
		t.Fatalf("an unsettled bug must surface, got %d", len(got))
	}
	// The same bug resolved in ANOTHER thread settles the whole family.
	settled := map[string]bool{dialect.NormalizeBotName(dialect.BugbotLogin) + "|" + stable: true}
	if got := dedupeFindings(findings, nil, settled); len(got) != 0 {
		t.Fatalf("a bug settled in a sibling thread must not resurface, got %#v", got)
	}
}

// TestExcludeSkipNoticeIsTargeted: the pre-review fix-first gate exempts exactly the
// thread-less skip notice, which cannot be resolved and would otherwise stop
// the co-review round it is meant to inform. Real unresolved findings on the
// same head must still hold it, as on any other PR — an earlier version
// bypassed every blocking finding whenever the primary was unavailable.
func TestExcludeSkipNoticeIsTargeted(t *testing.T) {
	skip := dialect.Finding{Severity: "major", Source: skipNoticeSource,
		Title: "some-reviewer skipped this review — narrow the PR to get one: Too many files!"}
	blocking := dialect.Finding{Severity: "major", Source: "review_thread",
		Title: "Nil deref in the retry path", ThreadID: "PRRT_x"}

	if got := excludeSkipNotice([]dialect.Finding{skip}); len(got) != 0 {
		t.Fatalf("the skip notice must not hold the round, got %+v", got)
	}
	got := excludeSkipNotice([]dialect.Finding{skip, blocking})
	if len(got) != 1 || got[0].ThreadID != "PRRT_x" {
		t.Fatalf("a genuine finding must still hold the head, got %+v", got)
	}
}

// Which occurrences of a stable id count as settled is decided by the head each
// one names, not by this filter — see
// TestCoReplayBugbotSiblingSettlementUsesHead, which drives the real threads.

// The loop completes the round it waited on, which releases the fire slot. With
// the primary outside the required set the round converges without it, so that
// completion would hand the next pull request a second concurrent metered
// command while this one's is still unanswered. Holding leaves the round fired
// for a Pump to finish — on the acknowledgement, or on the in-flight timeout.
func TestCompleteWaitRoundHoldsAnUnacknowledgedFireSlot(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	repo, pr, head := "o/r", 4, "aaaaaaaa1"
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 12)

	svc.completeWaitRound(ctx, repo, pr, head, true, &cfg)
	if got := roundPhase(t, store, repo, pr); got != PhaseFired {
		t.Fatalf("phase = %s, want the round left fired while the primary is silent", got)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FireSlot == nil {
		t.Fatal("the fire slot must stay held until the primary acknowledges")
	}

	svc.completeWaitRound(ctx, repo, pr, head, false, &cfg)
	if got := roundPhase(t, store, repo, pr); got != PhaseCompleted {
		t.Fatalf("phase = %s, want an acknowledged round completed as before", got)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FireSlot != nil {
		t.Fatalf("completing the round must release the slot, got %+v", st.FireSlot)
	}
}

// Converging is the loop's signal to push, and the push supersedes the round the
// held slot points at. Leaving the round fired is therefore not enough on its
// own: the replacement round archives its predecessor, Normalize drops a slot no
// round holds, and the next Pump spends the account allowance on a second review
// command while the first is still unanswered.
func TestAHeldFireSlotSurvivesTheHeadAdvanceItInvites(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	repo, pr, head := "o/r", 5, "aaaaaaaa1"
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 12)
	svc.completeWaitRound(ctx, repo, pr, head, true, &cfg)

	// The push: the agent acted on the success the loop just reported.
	if _, err := store.Update(ctx, func(st *State) error {
		_, err := st.Supersede(repo, pr, "bbbbbbbb2", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.SlotHeld(now) {
		t.Fatalf("fire slot = %+v, want it still held for the unanswered command", st.FireSlot)
	}
	// Bounded by the in-flight window, so an unanswered command cannot wedge the
	// queue for longer than crq would have waited for it anyway.
	if st.SlotHeld(now.Add(cfg.InflightTimeout + time.Minute)) {
		t.Error("the hold must expire with the in-flight window it was taken for")
	}
}
