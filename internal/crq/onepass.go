package crq

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

const onePassFinalizeSource = "one_pass_finalize"

// onePassNext applies the automated campaign boundary after Feedback has read
// the current PR. false means no review has happened anywhere on this PR yet,
// so the ordinary enqueue path should create its one allowed round.
func (s *Service) onePassNext(
	ctx context.Context,
	report NextReport,
	action engine.Action,
) (NextReport, bool, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return report, false, err
	}
	cfg := s.cfgFor(st, report.Repo)
	if !cfg.OnePass {
		return report, false, nil
	}
	if hold, held := st.HeldPR(report.Repo, report.PR); held {
		report.Action = string(engine.ActionBlocked)
		report.Reason = "held: " + hold.Reason
		report.Findings = []dialect.Finding{}
		report.Pending = nil
		report.RecheckAfter = nil
		return report, true, nil
	}

	// The owning session must keep receiving the ordinary fix/push decision.
	// In particular, its documented `crq next` pre-push check cannot be made to
	// wait on the dispatch claim that same process owns.
	if round := st.Round(report.Repo, report.PR); (round != nil && round.DispatchHeld(s.clock())) ||
		st.ArchivedDispatchHeld(report.Repo, report.PR, s.clock()) {
		return report, true, nil
	}

	if progress, ok := st.OnePassProgressFor(report.Repo, report.PR); ok {
		if st.OnePassReady(report.Repo, report.PR, report.Head) {
			report.Action = string(engine.ActionDone)
			report.Reason = "one-pass fixer completed for this head"
			report.Findings = []dialect.Finding{}
			report.Pending = nil
			report.RecheckAfter = nil
			return report, true, nil
		}
		if progress.ReadyHead == "" {
			report.Action = string(engine.ActionBlocked)
			report.Reason = fmt.Sprintf("the one-pass fixer already ran for %s but did not release a mergeable head", progress.AttemptHead)
			report.Findings = []dialect.Finding{}
			report.Pending = nil
			report.RecheckAfter = nil
			return report, true, nil
		}
		// Exactly one unattended fixer is part of the contract. A later push is
		// not silently trusted and does not buy a second agent session.
		report.Action = string(engine.ActionBlocked)
		report.Reason = fmt.Sprintf("head moved after the one-pass fixer (%s); refusing another fixer or an unverified merge", progress.ReadyHead)
		report.Findings = []dialect.Finding{}
		report.Pending = nil
		report.RecheckAfter = nil
		return report, true, nil
	}

	reviewed := false
	collectingRound := false
	if round := st.Round(report.Repo, report.PR); round != nil && round.Head == report.Head {
		switch round.Phase {
		case PhaseCompleted:
			reviewed = true
		case PhaseQueued:
			// Ignore this not-yet-fired marker while asking whether an older
			// review already consumed the PR's one round. If not, the ordinary
			// path must advance this queued round.
			probe := cloneState(st)
			delete(probe.Rounds, QueueKey(report.Repo, report.PR))
			need, _, err := s.reviewNeeded(ctx, probe, report.Repo, report.PR, false, true, noAnnounce)
			if err != nil {
				return report, true, err
			}
			if need {
				return report, false, nil
			}
			reviewed = true
			// Persist the one-pass decision before handing work to the fixer. A
			// queued marker left fire-eligible can otherwise be restored after
			// the session and spend a second review round.
			result, err := s.dedupeRound(ctx, cfg, *round, s.clock(), "one-pass review already consumed")
			if err != nil {
				return report, true, err
			}
			if result.Action != "deduped" {
				return report, true, nil
			}
		case PhaseReserved, PhaseFired, PhaseReviewing, PhaseAwaitingRetry:
			collectingRound = true
			// Findings bound to this head are the review answer. Without them,
			// this is still the one existing round and must finish or block; it
			// must never be replaced by the finalizer early.
			if len(engine.FindingsOnHead(action.Findings, report.Head)) == 0 {
				return report, true, nil
			}
			reviewed = true
		}
	}
	if !reviewed {
		probe := cloneState(st)
		delete(probe.Rounds, QueueKey(report.Repo, report.PR))
		need, _, err := s.reviewNeeded(ctx, probe, report.Repo, report.PR, false, true, noAnnounce)
		if err != nil {
			return report, true, err
		}
		reviewed = !need
	}
	if !reviewed {
		return report, false, nil
	}
	if action.Kind == engine.ActionPush {
		return report, true, nil
	}
	// A completed review can still be inside its settle window while trailing
	// inline findings arrive. Preserve that wait; launching the finalizer now
	// would let it release and merge before those findings are observable.
	if action.Kind == engine.ActionBlocked {
		return report, true, nil
	}
	if action.Kind == engine.ActionWait && (collectingRound || len(action.Pending) == 0) {
		return report, true, nil
	}
	// NextAction deliberately surfaces findings first, even while another
	// required reviewer is pending. A one-pass campaign has only one fixer, so
	// hold all feedback until the complete round is collected instead of giving
	// that sole session an incomplete findings file.
	if collectingRound && len(action.Pending) > 0 {
		report.Action = string(engine.ActionWait)
		report.Reason = "collecting the complete one-pass review round"
		report.Findings = []dialect.Finding{}
		report.Pending = action.Pending
		at := s.clock().Add(s.waitTick()).UTC()
		report.RecheckAfter = &at
		return report, true, nil
	}

	// Keep real feedback and add the final integration/security pass to the same
	// sole session. A non-clean first review needs that finalizer just as much as
	// a clean one does.
	if action.Kind == engine.ActionFix && len(report.Findings) > 0 {
		report.Findings = append(report.Findings, onePassFinalizer(report.Head))
		return report, true, nil
	}

	// A clean first review still receives one Codex finalizer. Its job is to
	// integrate the base, inspect the security PR as a whole, and run checks;
	// the synthetic item travels through the existing isolated dispatch path.
	report.Action = string(engine.ActionFix)
	report.Reason = "first review round complete; run the one-pass fixer/finalizer"
	report.Findings = []dialect.Finding{onePassFinalizer(report.Head)}
	report.Pending = nil
	report.RecheckAfter = nil
	return report, true, nil
}

func onePassFinalizer(head string) dialect.Finding {
	return dialect.Finding{
		ID:       onePassFinalizeSource,
		Bot:      "crq",
		Severity: "major",
		Title:    "Finalize this PR after its one allowed review round",
		Body: "Fetch and merge the latest base branch without rewriting history, resolve conflicts, " +
			"inspect the complete PR diff for security and correctness, and run the repository's documented checks. " +
			"Commit and push only if the branch changes.",
		Commit: head,
		Source: onePassFinalizeSource,
	}
}

// completeUnsuccessfulDispatch records a real, available code-fix session as
// the campaign's sole attempt while releasing its claim atomically. Provider
// outages and watcher shutdowns use the ordinary refund path instead.
func (s *Service) completeUnsuccessfulDispatch(
	ctx context.Context,
	report NextReport,
	token string,
) error {
	st, err := s.store.Update(ctx, func(st *State) error {
		onePass, campaign := st.OnePassDispatchCampaign(report.Repo, report.PR, token)
		solver := st.EffectiveSolver(report.Repo)
		onePass = onePass && solver.OnePass && solver.OnePassCampaign == campaign
		released := st.ReleaseArchivedDispatch(report.Repo, report.PR, token)
		if round := st.Round(report.Repo, report.PR); round != nil && round.ReleaseDispatch(token) {
			st.PutRound(*round)
			released = true
		}
		if !released {
			return ErrNoChange
		}
		if onePass {
			st.MarkOnePassAttempted(report.Repo, report.PR, report.Head, s.cfg.Host, s.clock())
		}
		return nil
	})
	if err == nil {
		s.sync(ctx, st)
	}
	return err
}

// completeSuccessfulDispatch atomically releases the session claim and hands
// its exact detached HEAD to the one-pass merger. For ordinary repositories it
// is just the existing release operation.
func (s *Service) completeSuccessfulDispatch(
	ctx context.Context,
	report NextReport,
	token string,
	readyHead string,
) (bool, error) {
	var marked bool
	st, err := s.store.Update(ctx, func(st *State) error {
		onePass, campaign := st.OnePassDispatchCampaign(report.Repo, report.PR, token)
		solver := st.EffectiveSolver(report.Repo)
		onePass = onePass && solver.OnePass && solver.OnePassCampaign == campaign
		released := st.ReleaseArchivedDispatch(report.Repo, report.PR, token)
		if round := st.Round(report.Repo, report.PR); round != nil && round.ReleaseDispatch(token) {
			st.PutRound(*round)
			released = true
		}
		if onePass {
			if !released {
				return errors.New("the successful fixer no longer owns its dispatch claim")
			}
			st.MarkOnePassReady(report.Repo, report.PR, readyHead, s.cfg.Host, s.clock())
			marked = true
		}
		if !released && !marked {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	s.sync(ctx, st)
	return marked, nil
}

// mergeOnePassReady performs the post-fix merge gate. eligible is false for an
// ordinary converged PR; callers use it to avoid changing ordinary watch
// events. Every network decision is rechecked against the exact current head.
func (s *Service) mergeOnePassReady(ctx context.Context, repo string, pr int) (eligible, merged bool, reason string, err error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return false, false, "", err
	}
	cfg := s.cfgFor(st, repo)
	progress, ready := st.OnePassProgressFor(repo, pr)
	if !cfg.OnePass || cfg.MergeMethod == "" || !ready || progress.ReadyHead == "" {
		return false, false, "", nil
	}
	if !st.AutofixEnabled(repo) {
		return true, false, "post-fix merge is paused because autofix is off for this repository", nil
	}
	if s.cfg.DryRun {
		return true, false, "dry run: exact-head merge not performed", nil
	}

	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return true, false, "", err
	}
	if pull.Merged {
		if err := s.retireOnePassMerged(ctx, repo, pr); err != nil {
			return true, true, "already merged", err
		}
		return true, true, "already merged", nil
	}
	if !strings.EqualFold(pull.State, "open") {
		return true, false, "pull request is no longer open", nil
	}
	if pull.Draft {
		return true, false, "waiting: pull request is still a draft", nil
	}
	if hold, held := st.HeldPR(repo, pr); held {
		return true, false, "held: " + hold.Reason, nil
	}
	if !st.OnePassReady(repo, pr, pull.Head.SHA) {
		return true, false, fmt.Sprintf("head moved after the one-pass fixer (%s); refusing an unverified merge", progress.ReadyHead), nil
	}
	if pull.Mergeable == nil || strings.EqualFold(pull.MergeableState, "unknown") || pull.MergeableState == "" {
		return true, false, "waiting for GitHub to compute mergeability", nil
	}
	if !*pull.Mergeable || strings.EqualFold(pull.MergeableState, "dirty") {
		return true, false, "merge conflict after the one-pass fixer", nil
	}

	result, err := s.gh.MergePull(ctx, repo, pr, pull.Head.SHA, cfg.MergeMethod)
	if err != nil {
		return true, false, "", err
	}
	if !result.Merged {
		reason := strings.TrimSpace(result.Message)
		if reason == "" {
			reason = "GitHub refused the exact-head merge"
		}
		return true, false, reason, nil
	}
	if err := s.retireOnePassMerged(ctx, repo, pr); err != nil {
		return true, true, "merged the fixed head with " + cfg.MergeMethod, err
	}
	return true, true, "merged the fixed head with " + cfg.MergeMethod, nil
}

// MergeOnePassReady exposes the exact-head post-fix merge gate for operational
// recovery and rolling upgrades. It performs the same policy and head checks as
// the watcher, so callers cannot turn a merely open PR into a campaign merge.
func (s *Service) MergeOnePassReady(ctx context.Context, repo string, pr int) (eligible, merged bool, reason string, err error) {
	return s.mergeOnePassReady(ctx, repo, pr)
}

// RetireMerged verifies the pull request is merged before recording that
// outcome in recently-finished history. It is also useful during a rolling
// binary upgrade, when a newer exact-head merger may finish work owned by an
// older long-lived watcher.
func (s *Service) RetireMerged(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return err
	}
	if !pull.Merged {
		return errors.New("pull request is not merged")
	}
	return s.retireOnePassMerged(ctx, repo, pr)
}

// RetireMergedVerified records a merge already verified by the caller. The
// rolling-upgrade exact-head merger uses the merge API response itself as that
// proof, avoiding a second GitHub read that may be rate-limited.
func (s *Service) RetireMergedVerified(ctx context.Context, repo string, pr int) error {
	return s.retireOnePassMerged(ctx, repo, pr)
}

func (s *Service) retireOnePassMerged(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	st, err := s.store.Update(ctx, func(st *State) error {
		changed := false
		if st.Round(repo, pr) != nil {
			st.EndRound(repo, pr, "merged")
			changed = true
		} else {
			for i := len(st.Archive) - 1; i >= 0; i-- {
				round := &st.Archive[i]
				if NormalizeRepo(round.Repo) != repo || round.PR != pr {
					continue
				}
				if round.Note != "merged" || round.Phase != PhaseAbandoned {
					round.Abandon("merged")
					changed = true
				}
			}
		}
		if st.ClearOnePassProgress(repo, pr) {
			changed = true
		}
		if !changed {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, st)
	return nil
}
