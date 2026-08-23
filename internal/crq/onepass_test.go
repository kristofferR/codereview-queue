package crq

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

func enableOnePass(t *testing.T, f *replayFixture, repo string, merge string) {
	t.Helper()
	on := true
	one := 1
	if _, err := f.svc.SetSolver(f.ctx, repo, SolverChange{
		OnePass: &on, MergeMethod: &merge, MaxAttempts: &one,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOnePassUsesTheOnlyReviewThenCreatesOneFinalizer(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/security", 90
	oldHead, head := "aaaaaaaa12345678", "bbbbbbbb12345678"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")

	// The PR was reviewed before the campaign setting landed, on its previous
	// head. That still consumes the PR's one review round.
	f.botReview(repo, pr, 1, oldHead, base.Add(-time.Hour))
	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != string(engine.ActionFix) || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one synthetic finalizer", report)
	}
	if got := report.Findings[0].Source; got != onePassFinalizeSource {
		t.Fatalf("source = %q, want %q", got, onePassFinalizeSource)
	}
	if round := f.round(repo, pr); round != nil {
		t.Fatalf("one-pass finalizer enqueued a second review round: %+v", round)
	}
}

func TestOnePassStillFiresTheFirstQueuedReview(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 30, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 89, "abababab12345678"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")

	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action == string(engine.ActionFix) {
		t.Fatalf("first pass fabricated a finalizer before review: %+v", report)
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("first review posts = %d, want 1", got)
	}
}

func TestOnePassReadyHeadMergesOnceWithExactSHA(t *testing.T) {
	base := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 91, "cccccccc12345678"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")

	mergeable := true
	f.gh.mu.Lock()
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Mergeable = &mergeable
	pull.MergeableState = "clean"
	f.gh.pulls[fakeKey(repo, pr)] = pull
	f.gh.mu.Unlock()
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassReady(repo, pr, head, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != string(engine.ActionDone) {
		t.Fatalf("ready report = %+v, want done before watcher merge", report)
	}
	eligible, merged, reason, err := f.svc.mergeOnePassReady(f.ctx, repo, pr)
	if err != nil || !eligible || !merged {
		t.Fatalf("merge = eligible %t merged %t reason %q err %v", eligible, merged, reason, err)
	}
	f.gh.mu.Lock()
	merges := append([]string(nil), f.gh.merged...)
	f.gh.mu.Unlock()
	if len(merges) != 1 || !strings.Contains(merges[0], "@"+head+":squash") {
		t.Fatalf("merge calls = %v, want one exact-head squash", merges)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.OnePassProgressFor(repo, pr); ok {
		t.Fatal("successful merge left a reusable one-pass hand-off")
	}
}

func TestOnePassNeverRefixesOrMergesAHeadThatMovedAfterFixing(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/security", 92
	fixed, moved := "dddddddd12345678", "eeeeeeee12345678"
	f.openPull(repo, pr, moved)
	f.setCommitDate(moved, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassReady(repo, pr, fixed, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != string(engine.ActionBlocked) || len(report.Findings) != 0 {
		t.Fatalf("moved report = %+v, want a terminal block without another fixer", report)
	}
	eligible, merged, _, err := f.svc.mergeOnePassReady(f.ctx, repo, pr)
	if err != nil || !eligible || merged {
		t.Fatalf("moved merge = eligible %t merged %t err %v", eligible, merged, err)
	}
	if len(f.gh.merged) != 0 {
		t.Fatalf("moved head reached merge endpoint: %v", f.gh.merged)
	}
}

func TestOnePassDoesNotRetryARealFailedFixer(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 30, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 93, "dededede12345678"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassAttempted(repo, pr, head, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != string(engine.ActionBlocked) || !strings.Contains(report.Reason, "already ran") {
		t.Fatalf("failed fixer report = %+v, want no retry", report)
	}
}

func TestFirstReviewModeRecognizesCodexOnlyReview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	codex := "chatgpt-codex-connector[bot]"
	cfg.PrimaryOff = true
	cfg.Reviewers = []Reviewer{{Login: codex, Required: true, Budget: dialect.BudgetNone}}
	cfg.RequiredBots = []string{codex}
	cfg.FeedbackBots = []string{codex}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/codex-only", 1, "ffffffff12345678"
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	review := ghapi.Review{CommitID: head, State: "COMMENTED", Body: "reviewed"}
	review.User.Login = codex
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("Codex-only review was ignored by the one-review predicate")
	}
}
