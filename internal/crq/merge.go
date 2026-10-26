package crq

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// readMergeSeen reads what the merge gate needs beyond Feedback's observation:
// the head's commit statuses, the base's required checks and, when observe
// skipped them, the head's check runs. Every read is an ETag'd GET, so an
// unchanged PR costs no REST quota. Person threads are added by the caller,
// which owns the GraphQL thread read.
func (s *Service) readMergeSeen(ctx context.Context, cfg Config, repo string, obs observation) engine.MergeSeen {
	pull := obs.pull
	m := engine.MergeSeen{State: pull.MergeableState, Base: pull.Base.Ref}
	if pull.Head.SHA == "" {
		m.CIUnknown = true
		return m
	}
	var (
		wg                sync.WaitGroup
		runs              = obs.runs
		runsErr           = obs.runsErr
		statuses          []ghapi.CommitStatus
		statusErr, reqErr error
		required          []string
	)
	if !obs.runsRead {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runs, runsErr = s.gh.ListCheckRuns(ctx, repo, pull.Head.SHA)
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		statuses, statusErr = s.gh.ListCommitStatuses(ctx, repo, pull.Head.SHA)
	}()
	go func() {
		defer wg.Done()
		if pull.Base.Ref != "" {
			required, reqErr = s.gh.RequiredChecks(ctx, repo, pull.Base.Ref)
		}
	}()
	wg.Wait()

	// A 404 is "nothing reported", not a failed read.
	if (runsErr != nil && !errors.Is(runsErr, ghapi.ErrNotFound)) ||
		(statusErr != nil && !errors.Is(statusErr, ghapi.ErrNotFound)) {
		m.CIUnknown = true
	}
	// Unreadable protection degrades to "nothing required": GitHub's own
	// mergeable_state still reports a required check that never ran as blocked.
	if reqErr != nil && s.log != nil {
		s.log.Printf("warning: required checks unavailable for %s@%s: %v", repo, pull.Base.Ref, reqErr)
	}
	// A reviewer's own checks are left to the reviewer, including when branch
	// protection requires one: excluded from CI, it would otherwise read as a
	// required check that never reports.
	reviewerOwned := map[string]bool{}
	for _, run := range runs {
		if dialect.IsReviewerCheck(run.App.Slug, run.Name) {
			reviewerOwned[run.Name] = true
			continue
		}
		m.CI = append(m.CI, engine.CISeen{Name: run.Name, State: checkRunState(run), URL: run.HTMLURL})
	}
	for _, status := range statuses {
		if dialect.IsReviewerCheck("", status.Context) {
			reviewerOwned[status.Context] = true
			continue
		}
		m.CI = append(m.CI, engine.CISeen{Name: status.Context, State: commitStatusState(status.State), URL: status.TargetURL})
	}
	for _, name := range required {
		if !reviewerOwned[name] && !dialect.IsReviewerCheck("", name) {
			m.Required = append(m.Required, name)
		}
	}
	m.ChangesRequested = changesRequested(cfg, obs.allReviews)
	return m
}

func checkRunState(run ghapi.CheckRun) engine.CIState {
	if run.Status != "completed" {
		return engine.CIPending
	}
	switch run.Conclusion {
	case "success", "neutral", "skipped":
		return engine.CIPass
	}
	// failure, cancelled, timed_out, action_required, startup_failure, stale:
	// none of them lets the run count as green.
	return engine.CIFail
}

func commitStatusState(state string) engine.CIState {
	switch state {
	case "success":
		return engine.CIPass
	case "pending":
		return engine.CIPending
	}
	return engine.CIFail
}

// changesRequested lists the people whose latest verdict on the PR requests
// changes. A comment is not a verdict; an approval or a dismissal replaces one.
func changesRequested(cfg Config, reviews []ghapi.Review) []string {
	sorted := append([]ghapi.Review(nil), reviews...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SubmittedAt.Before(sorted[j].SubmittedAt) })
	latest := map[string]string{}
	for _, review := range sorted {
		if !isPerson(cfg, review.User.Login, review.User.Type) {
			continue
		}
		switch state := strings.ToUpper(review.State); state {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[review.User.Login] = state
		}
	}
	var out []string
	for login, state := range latest {
		if state == "CHANGES_REQUESTED" {
			out = append(out, login)
		}
	}
	sort.Strings(out)
	return out
}

// personThreads returns the URLs of unresolved review threads a person
// started. Outdated ones count: the conversation is still open.
func personThreads(cfg Config, threads []reviewThread) []string {
	var out []string
	for _, thread := range threads {
		if thread.IsResolved || len(thread.Comments.Nodes) == 0 {
			continue
		}
		first := thread.Comments.Nodes[0]
		if isPerson(cfg, first.Author.Login, first.Author.Typename) {
			out = append(out, first.URL)
		}
	}
	return out
}

// isPerson reports whether a login is a person rather than an app: GitHub's
// account type when the payload carries it, the "[bot]" suffix REST uses, and
// the reviewers crq is configured to know.
func isPerson(cfg Config, login, accountType string) bool {
	if login == "" || accountType == "Bot" || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	return !isReviewerBot(cfg, login)
}

// mergeDone merges a pull request `crq next` called done, when its repository
// opted in with `crq solver set --merge` outside a one-pass campaign. done
// already means reviewers, CI, mergeability and people are settled; this only
// re-reads shared policy at the irreversible step and has GitHub refuse the
// merge if the head moved since that decision. eligible is false when the
// repository did not opt in, so the caller leaves the event alone.
func (s *Service) mergeDone(ctx context.Context, report NextReport, pull ghapi.Pull) (eligible, merged bool, reason string, err error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return false, false, "", err
	}
	cfg := s.cfgFor(st, report.Repo)
	if cfg.MergeMethod == "" || cfg.OnePass {
		return false, false, "", nil
	}
	switch {
	case forkPull(report.Repo, pull):
		return true, false, "done; not merging a pull request from another repository", nil
	case report.HeadOID == "" || pull.Head.SHA != report.HeadOID:
		return true, false, "done; the head moved since the listing, so merging waits for the next pass", nil
	case !st.AutofixEnabled(report.Repo):
		return true, false, "done; merging is paused because autofix is off for this repository", nil
	}
	if hold, held := st.HeldPR(report.Repo, report.PR); held {
		return true, false, "held: " + hold.Reason, nil
	}
	// done describes the remote head. Someone holding the PR may have unpushed
	// work this watcher cannot see, so their claim outranks the merge.
	now := s.clock()
	if claim, owned := st.WorkClaim(report.Repo, report.PR, now); owned {
		return true, false, "done; " + claim.By + " is working on this pull request, so merging waits", nil
	}
	if round := st.Round(report.Repo, report.PR); (round != nil && round.DispatchHeld(now)) ||
		st.ArchivedDispatchHeld(report.Repo, report.PR, now) {
		return true, false, "done; a fix session owns this pull request, so merging waits", nil
	}
	if s.cfg.DryRun {
		return true, false, "dry run: would merge with " + cfg.MergeMethod, nil
	}
	result, err := s.gh.MergePull(ctx, report.Repo, report.PR, report.HeadOID, cfg.MergeMethod)
	if err != nil {
		return true, false, "", err
	}
	if !result.Merged {
		reason := strings.TrimSpace(result.Message)
		if reason == "" {
			reason = "GitHub refused the merge"
		}
		return true, false, reason, nil
	}
	return true, true, "merged with " + cfg.MergeMethod, s.retireMerged(ctx, report.Repo, report.PR)
}
