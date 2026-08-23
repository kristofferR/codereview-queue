package crq

import (
	"context"
	"errors"
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

func TestOnePassRetiresQueuedRoundWhenAnOlderReviewConsumesTheCap(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 45, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 88, "acacacac12345678"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	enableOnePass(t, f, repo, "squash")
	f.botReview(repo, pr, 1, "edededed12345678", base.Add(-time.Hour))
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		_, err := st.NewRound(repo, pr, head[:9], base)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	report, err := f.svc.nextAutomated(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != string(engine.ActionFix) {
		t.Fatalf("report = %+v, want the one-pass finalizer", report)
	}
	if round := f.round(repo, pr); round == nil || round.Phase != PhaseCompleted {
		t.Fatalf("round = %+v, want a non-fire-eligible completed marker", round)
	}
}

func TestOnePassPreservesTheReviewSettleWait(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 50, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 87, "adadadad1"
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, head, base)
		if err != nil {
			return err
		}
		round.Phase = PhaseCompleted
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recheck := base.Add(time.Minute)
	report := NextReport{Action: string(engine.ActionWait), Repo: repo, PR: pr, Head: head, RecheckAfter: &recheck}
	action := engine.Action{Kind: engine.ActionWait, Reason: "review is settling"}

	got, handled, err := f.svc.onePassNext(f.ctx, report, action)
	if err != nil || !handled {
		t.Fatalf("handled = %t, err = %v", handled, err)
	}
	if got.Action != string(engine.ActionWait) || got.RecheckAfter == nil || len(got.Findings) != 0 {
		t.Fatalf("report = %+v, want the settle wait unchanged", got)
	}
}

func TestOnePassCollectsEveryRequiredReviewBeforeItsOnlyFixer(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 52, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 871, "adadadad2"
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, head, base)
		if err != nil {
			return err
		}
		round.Phase = PhaseReviewing
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finding := dialect.Finding{ID: "first", Commit: head, Severity: "major"}
	report := NextReport{Action: string(engine.ActionFix), Repo: repo, PR: pr, Head: head, Findings: []dialect.Finding{finding}}
	action := engine.Action{Kind: engine.ActionFix, Findings: []dialect.Finding{finding}, Pending: []string{"later-reviewer[bot]"}}

	got, handled, err := f.svc.onePassNext(f.ctx, report, action)
	if err != nil || !handled {
		t.Fatalf("handled = %t, err = %v", handled, err)
	}
	if got.Action != string(engine.ActionWait) || got.RecheckAfter == nil || len(got.Findings) != 0 {
		t.Fatalf("report = %+v, want a wait until the complete round is collected", got)
	}
}

func TestOnePassAddsFinalizerToTheReviewFindings(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 54, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 872, "adadadad3"
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, head, base)
		if err != nil {
			return err
		}
		round.Phase = PhaseCompleted
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finding := dialect.Finding{ID: "review", Commit: head, Severity: "major"}
	report := NextReport{Action: string(engine.ActionFix), Repo: repo, PR: pr, Head: head, Findings: []dialect.Finding{finding}}
	action := engine.Action{Kind: engine.ActionFix, Findings: []dialect.Finding{finding}}

	got, handled, err := f.svc.onePassNext(f.ctx, report, action)
	if err != nil || !handled {
		t.Fatalf("handled = %t, err = %v", handled, err)
	}
	if len(got.Findings) != 2 || got.Findings[0].ID != finding.ID || got.Findings[1].Source != onePassFinalizeSource {
		t.Fatalf("findings = %+v, want the review finding plus finalizer", got.Findings)
	}
}

func TestOnePassHoldBlocksFinalizerAndMerge(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 56, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 873, "adadadad4"
	f.openPull(repo, pr, head)
	enableOnePass(t, f, repo, "squash")
	mergeable := true
	f.gh.mu.Lock()
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Mergeable = &mergeable
	pull.MergeableState = "clean"
	f.gh.pulls[fakeKey(repo, pr)] = pull
	f.gh.mu.Unlock()
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.Hold(repo, pr, "operator pause", "test", base)
		st.MarkOnePassReady(repo, pr, head, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	report, handled, err := f.svc.onePassNext(f.ctx, NextReport{Repo: repo, PR: pr, Head: head}, engine.Action{Kind: engine.ActionDone})
	if err != nil || !handled || report.Action != string(engine.ActionBlocked) || !strings.Contains(report.Reason, "operator pause") {
		t.Fatalf("held next = handled %t report %+v err %v", handled, report, err)
	}
	eligible, merged, reason, err := f.svc.mergeOnePassReady(f.ctx, repo, pr)
	if err != nil || !eligible || merged || !strings.Contains(reason, "operator pause") {
		t.Fatalf("held merge = eligible %t merged %t reason %q err %v", eligible, merged, reason, err)
	}
	if len(f.gh.merged) != 0 {
		t.Fatalf("held PR reached merge endpoint: %v", f.gh.merged)
	}
}

func TestOnePassFinalizerBypassesSeverityFiltering(t *testing.T) {
	findings := []dialect.Finding{
		{ID: "final", Source: onePassFinalizeSource, Severity: "major"},
		{ID: "ordinary", Severity: "major"},
	}
	got := autofixFindings(findings, map[string]bool{"critical": true})
	if len(got) != 1 || got[0].Source != onePassFinalizeSource {
		t.Fatalf("filtered findings = %+v, want only the workflow finalizer", got)
	}
}

func TestEnablingOnePassDoesNotAdoptAnOrdinaryLiveSession(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 55, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head, token := "owner/security", 85, "aeaeaeae12345678", "ordinary-session"
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, head[:9], base)
		if err != nil {
			return err
		}
		if ok, why := round.ClaimDispatchModels("test", token, base, 3, []string{"gpt-5.6-sol"}); !ok {
			return errors.New(why)
		}
		st.RememberDispatch(repo, pr, *round.Dispatch)
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enableOnePass(t, f, repo, "squash")
	report := NextReport{Repo: repo, PR: pr, Head: head}
	if onePass, err := f.svc.completeSuccessfulDispatch(f.ctx, report, token, head); err != nil {
		t.Fatal(err)
	} else if onePass {
		t.Fatal("ordinary successful dispatch was identified as one-pass")
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if progress, ok := st.OnePassProgressFor(repo, pr); ok {
		t.Fatalf("ordinary session became one-pass handoff: %+v", progress)
	}
}

func TestSuccessfulOnePassDispatchIsReadyForImmediateMerge(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 57, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head, token := "owner/security", 851, "afafafaf12345678", "one-pass-session"
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, head[:9], base)
		if err != nil {
			return err
		}
		if ok, why := round.ClaimDispatchModels("test", token, base, 1, []string{"gpt-5.6-sol"}); !ok {
			return errors.New(why)
		}
		round.Dispatch.OnePass = true
		round.Dispatch.OnePassCampaign = st.EffectiveSolver(repo).OnePassCampaign
		st.RememberDispatch(repo, pr, *round.Dispatch)
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report := NextReport{Repo: repo, PR: pr, Head: head}
	onePass, err := f.svc.completeSuccessfulDispatch(f.ctx, report, token, head)
	if err != nil {
		t.Fatal(err)
	}
	if !onePass {
		t.Fatal("successful one-pass dispatch was not identified as one-pass")
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if progress, ok := st.OnePassProgressFor(repo, pr); !ok || progress.ReadyHead != head {
		t.Fatalf("ready handoff = %+v, ok=%t", progress, ok)
	}
}

func TestOnePassCompletionFromReplacedCampaignIsDiscarded(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 58, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, head := "owner/security", "bfbfbfbf12345678"
	enableOnePass(t, f, repo, "squash")
	seedClaim := func(pr int, token string) {
		t.Helper()
		if _, err := f.store.Update(f.ctx, func(st *State) error {
			round, err := st.NewRound(repo, pr, head[:9], base)
			if err != nil {
				return err
			}
			if ok, why := round.ClaimDispatchModels("test", token, base, 1, []string{"gpt-5.6-sol"}); !ok {
				return errors.New(why)
			}
			round.Dispatch.OnePass = true
			round.Dispatch.OnePassCampaign = st.EffectiveSolver(repo).OnePassCampaign
			st.RememberDispatch(repo, pr, *round.Dispatch)
			st.PutRound(*round)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedClaim(852, "successful-old-campaign")
	seedClaim(853, "failed-old-campaign")

	if _, err := f.svc.SetSolver(f.ctx, repo, SolverChange{UnsetOnePass: true, UnsetMerge: true}); err != nil {
		t.Fatal(err)
	}
	on, one, method := true, 1, "squash"
	if _, err := f.svc.SetSolver(f.ctx, repo, SolverChange{OnePass: &on, MergeMethod: &method, MaxAttempts: &one}); err != nil {
		t.Fatal(err)
	}

	marked, err := f.svc.completeSuccessfulDispatch(f.ctx,
		NextReport{Repo: repo, PR: 852, Head: head}, "successful-old-campaign", head)
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Fatal("a successful fixer from the previous campaign recreated a ready hand-off")
	}
	if err := f.svc.completeUnsuccessfulDispatch(f.ctx,
		NextReport{Repo: repo, PR: 853, Head: head}, "failed-old-campaign"); err != nil {
		t.Fatal(err)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int{852, 853} {
		if progress, ok := st.OnePassProgressFor(repo, pr); ok {
			t.Fatalf("PR %d recreated stale one-pass progress: %+v", pr, progress)
		}
	}
}

func TestOnePassReadyHeadMergesOnceWithExactSHAWhenChecksAreUnstable(t *testing.T) {
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
	// The fixer push invalidates the PR's one allowed review, so GitHub reports
	// the head as unstable until another review arrives. One-pass mode must not
	// recreate that review loop through its merge gate.
	pull.MergeableState = "unstable"
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

func TestRetireMergedNormalizesEveryArchivedAttempt(t *testing.T) {
	base := time.Date(2026, 8, 23, 11, 15, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/security", 915
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.Archive = append(st.Archive,
			Round{Repo: repo, PR: pr, Head: "aaaaaaaa1", Phase: PhaseAbandoned, Note: "merged"},
			Round{Repo: repo, PR: pr, Head: "bbbbbbbb2", Phase: PhaseAbandoned, Note: "pr closed"},
			Round{Repo: repo, PR: pr + 1, Head: "cccccccc3", Phase: PhaseAbandoned, Note: "pr closed"},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RetireMergedVerified(f.ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, round := range st.Archive {
		if round.Repo == repo && round.PR == pr && (round.Note != "merged" || round.Phase != PhaseAbandoned) {
			t.Fatalf("archived attempt = %+v, want merged abandonment", round)
		}
	}
	if got := st.Archive[len(st.Archive)-1].Note; got != "pr closed" {
		t.Fatalf("unrelated archive note = %q, want pr closed", got)
	}
}

func TestAlreadyMergedOnePassPropagatesRetirementFailure(t *testing.T) {
	base := time.Date(2026, 8, 23, 11, 20, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 916, "cbcbcbcb12345678"
	f.openPull(repo, pr, head)
	enableOnePass(t, f, repo, "squash")
	f.gh.mu.Lock()
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Merged = true
	pull.State = "closed"
	f.gh.pulls[fakeKey(repo, pr)] = pull
	f.gh.mu.Unlock()
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassReady(repo, pr, head, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("retirement write failed")
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 1, err: writeErr}

	eligible, merged, reason, err := f.svc.mergeOnePassReady(f.ctx, repo, pr)
	if !eligible || !merged || reason != "already merged" || !errors.Is(err, writeErr) {
		t.Fatalf("already-merged result = eligible %t merged %t reason %q err %v", eligible, merged, reason, err)
	}
}

func TestOnePassDryRunNeverMergesOrClearsTheReadyHead(t *testing.T) {
	base := time.Date(2026, 8, 23, 11, 30, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/security", 86, "cdcdcdcd12345678"
	f.openPull(repo, pr, head)
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassReady(repo, pr, head, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.cfg.DryRun = true

	eligible, merged, _, err := f.svc.mergeOnePassReady(f.ctx, repo, pr)
	if err != nil || !eligible || merged {
		t.Fatalf("merge = eligible %t merged %t err %v", eligible, merged, err)
	}
	if len(f.gh.merged) != 0 {
		t.Fatalf("dry run called merge: %v", f.gh.merged)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.OnePassProgressFor(repo, pr); !ok {
		t.Fatal("dry run cleared the ready hand-off")
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

func TestWatchKeepsMovedOnePassReadyHeadBlocked(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 15, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/security", 921
	fixed, moved := "dadadada12345678", "ebebebeb12345678"
	f.openPull(repo, pr, moved)
	f.gh.mu.Lock()
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Number = pr
	f.gh.pulls[fakeKey(repo, pr)] = pull
	f.gh.mu.Unlock()
	enableOnePass(t, f, repo, "squash")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.MarkOnePassReady(repo, pr, fixed, "test", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := f.svc.watchPass(f.ctx, WatchOptions{Repos: []string{repo}, Dispatch: dispatchOn()}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != string(engine.ActionBlocked) {
		t.Fatalf("events = %+v, want the moved ready head to remain blocked", events)
	}
	if len(f.gh.merged) != 0 {
		t.Fatalf("moved ready head reached merge endpoint: %v", f.gh.merged)
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
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, true, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("Codex-only review was ignored by the one-review predicate")
	}
}

func TestOrdinaryFirstReviewModeStillRequiresThePrimary(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	codex := dialect.CodexBotLogin
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: codex, Required: false, Budget: dialect.BudgetNone},
	}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/ordinary", 2, "eeeeeeee12345678"
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
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, false, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("a co-reviewer suppressed the ordinary primary-only first review")
	}
}

func TestOnePassReviewCapRecognizesCompletedBugbotCheck(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: dialect.BugbotLogin, Name: "Bugbot", Budget: dialect.BudgetNone},
	}
	cfg.CoBots = []CoBotConfig{{Name: "bugbot", Login: dialect.BugbotLogin}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/bugbot", 3, "dddddddd12345678"
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.setCheckRuns(head, corpusCheckRun(t, "bugbot/check-clean.json"))
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, true, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("a completed Bugbot check did not consume the one-pass review cap")
	}
}

func TestOnePassReviewCapRejectsPriorHeadBugbotActivityWithoutAnswer(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: dialect.BugbotLogin, Name: "Bugbot", Budget: dialect.BudgetNone},
	}
	cfg.CoBots = []CoBotConfig{{Name: "bugbot", Login: dialect.BugbotLogin}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/bugbot", 4, "cccccccc12345678"
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	seen := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if _, err := store.Update(ctx, func(st *State) error {
		st.CoActivity = map[string]map[string]time.Time{
			QueueKey(repo, pr): {dialect.NormalizeBotName(dialect.BugbotLogin): seen},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, true, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("prior-head Bugbot activity consumed the one-pass review cap without a completed answer")
	}
}

func TestOnePassReviewCapRecognizesPriorHeadBugbotAnswer(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: dialect.BugbotLogin, Name: "Bugbot", Budget: dialect.BudgetNone},
	}
	cfg.CoBots = []CoBotConfig{{Name: "bugbot", Login: dialect.BugbotLogin}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/bugbot", 41, "cccccccc12345678"
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	answered := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if _, err := store.Update(ctx, func(st *State) error {
		st.CoAnswers = map[string]map[string]time.Time{
			QueueKey(repo, pr): {dialect.NormalizeBotName(dialect.BugbotLogin): answered},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, true, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("prior-head Bugbot answer did not consume the one-pass review cap")
	}
}

func TestOnePassReviewCapRecognizesCodexCleanComment(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: dialect.CodexBotLogin, Name: "Codex", Budget: dialect.BudgetNone},
	}
	cfg.CoBots = []CoBotConfig{{Name: "codex", Login: dialect.CodexBotLogin}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	repo, pr, head := "owner/codex", 5, "bbbbbbbb12345678"
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	comment := ghapi.IssueComment{ID: 7, Body: corpusMessage(t, "codex/clean-summary-legacy.md"), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	comment.User.Login = dialect.CodexBotLogin
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{comment}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, _, err := svc.reviewNeeded(ctx, st, repo, pr, false, true, noAnnounce)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("Codex's clean issue comment did not consume the one-pass review cap")
	}
}
