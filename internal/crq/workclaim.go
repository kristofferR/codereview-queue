package crq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// WorkClaimTTL is long enough for a substantial fix pass, but finite so an
// interrupted agent cannot permanently keep a PR away from autofix. Interactive
// next/wait/loop calls renew it as expiry approaches.
const (
	WorkClaimTTL             = 2 * time.Hour
	workClaimRenewalInterval = WorkClaimTTL / 3
	workClaimReleaseLimit    = 30 * time.Second
)

type workClaimOutcome struct {
	acquired bool
	reason   string
	until    time.Time
}

// WorkClaimResult is printed by `crq unclaim`.
type WorkClaimResult struct {
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	Released bool   `json:"released"`
}

// claimInteractiveWork atomically excludes unattended fix sessions before an
// interactive loop reads feedback. The inverse check lives in claimDispatch:
// whichever claimant wins the state CAS works, and the other waits.
func (s *Service) claimInteractiveWork(ctx context.Context, repo string, pr int) (workClaimOutcome, error) {
	repo = NormalizeRepo(repo)
	owner, by := s.workClaimOwner()
	dispatchToken := s.dispatchToken()
	if s.cfg.DryRun {
		st, _, err := s.store.Load(ctx)
		if err != nil {
			return workClaimOutcome{}, err
		}
		outcome, _, err := s.interactiveWorkClaim(&st, repo, pr, owner, by, dispatchToken, s.clock().UTC(), false)
		return outcome, err
	}

	for {
		outcome := workClaimOutcome{}
		st, err := s.store.Update(ctx, func(st *State) error {
			var changed bool
			var err error
			outcome, changed, err = s.interactiveWorkClaim(
				st, repo, pr, owner, by, dispatchToken, s.clock().UTC(), true,
			)
			if err != nil {
				return err
			}
			if !changed {
				return ErrNoChange
			}
			return nil
		})
		if err != nil || !outcome.acquired {
			return outcome, err
		}

		// A store update can spend an unbounded time retrying transport or CAS
		// operations after the mutation callback. Verify that the lease which
		// actually committed still has a renewal interval left before proceeding.
		now := s.clock().UTC()
		if dispatchToken != "" && st.OwnsLiveDispatch(repo, pr, dispatchToken, now) {
			return outcome, nil
		}
		if claim, ok := st.WorkClaim(repo, pr, now); ok && claim.Owner == owner &&
			claim.ExpiresAt.After(now.Add(workClaimRenewalInterval)) {
			outcome.until = claim.ExpiresAt
			return outcome, nil
		}
	}
}

func (s *Service) interactiveWorkClaim(
	st *State,
	repo string,
	pr int,
	owner string,
	by string,
	dispatchToken string,
	now time.Time,
	persist bool,
) (workClaimOutcome, bool, error) {
	// The unattended fix session uses `crq next` as its pre-push oracle. It
	// already won exclusion through this exact dispatch token, so treating it
	// as a competing interactive caller would deadlock it on its own claim.
	if dispatchToken != "" && st.OwnsLiveDispatch(repo, pr, dispatchToken, now) {
		return workClaimOutcome{acquired: true, until: now.Add(DispatchTTL)}, false, nil
	}
	existing, ownsExisting := st.WorkClaim(repo, pr, now)
	if ownsExisting && existing.Owner != owner {
		return workClaimOutcome{
			reason: "interactive work is already claimed by " + existing.By,
			until:  existing.ExpiresAt,
		}, false, nil
	}
	round := st.Round(repo, pr)
	if (round != nil && round.DispatchHeld(now)) || st.ArchivedDispatchHeld(repo, pr, now) {
		return workClaimOutcome{
			reason: "unattended autofix is already working on this pull request",
			until:  now.Add(DispatchTTL),
		}, false, nil
	}
	if lagging := laggingAutofixHosts(*st, now); len(lagging) > 0 {
		return workClaimOutcome{}, false, fmt.Errorf("cannot safely claim interactive work while autofix host(s) %s run a version that ignores work claims; upgrade those daemons first",
			strings.Join(lagging, ", "))
	}
	if !persist {
		until := now.Add(WorkClaimTTL)
		if ownsExisting {
			until = existing.ExpiresAt
		}
		return workClaimOutcome{acquired: true, until: until}, false, nil
	}
	if ownsExisting && existing.ExpiresAt.After(now.Add(workClaimRenewalInterval)) {
		return workClaimOutcome{acquired: true, until: existing.ExpiresAt}, false, nil
	}
	claimedAt := now
	if ownsExisting {
		claimedAt = existing.ClaimedAt
	}
	claim := WorkClaim{
		Owner: owner, By: by, ClaimedAt: claimedAt,
		ExpiresAt: now.Add(WorkClaimTTL),
	}
	st.SetWorkClaim(repo, pr, claim)
	return workClaimOutcome{acquired: true, until: claim.ExpiresAt}, true, nil
}

func (s *Service) dispatchToken() string {
	if s.dispatchTokenFn != nil {
		return strings.TrimSpace(s.dispatchTokenFn())
	}
	return strings.TrimSpace(os.Getenv("CRQ_DISPATCH_TOKEN"))
}

func laggingAutofixHosts(st State, now time.Time) []string {
	var hosts []string
	for host, report := range st.HostReports {
		if report.RolesFresh([]string{"autofix"}, now, HostReportTTL) &&
			report.CapsFor("autofix") < CapsWorkClaims {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return hosts
}

func (s *Service) workClaimOwner() (string, string) {
	if s.workOwnerFn != nil {
		return s.workOwnerFn()
	}
	dir := s.cfg.WorkDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if root, err := gitDir(context.Background(), dir, "rev-parse", "--show-toplevel"); err == nil {
		dir = root
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	session := firstWorkOwnerValue(
		s.cfg.WorkOwner,
		os.Getenv("CODEX_THREAD_ID"),
		os.Getenv("CLAUDE_SESSION_ID"),
	)
	identity := session
	if identity == "" {
		identity = dir
	}
	sum := sha256.Sum256([]byte(s.cfg.Host + "\x00" + identity))
	owner := hex.EncodeToString(sum[:])
	by := s.cfg.Host
	if base := filepath.Base(dir); base != "" && base != "." {
		by += ":" + base
	}
	return owner, by
}

func firstWorkOwnerValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *Service) releaseInteractiveWork(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	if s.cfg.DryRun {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workClaimReleaseLimit)
	defer cancel()
	owner, _ := s.workClaimOwner()
	_, err := s.store.Update(releaseCtx, func(st *State) error {
		if !st.ReleaseWorkClaim(repo, pr, owner, false) {
			return ErrNoChange
		}
		return nil
	})
	return err
}

// UnclaimWork is the explicit escape hatch when an interactive loop is being
// abandoned before it converges. It intentionally releases any owner: invoking
// it is an operator decision, unlike automatic cleanup which is owner-scoped.
func (s *Service) UnclaimWork(ctx context.Context, repo string, pr int) (WorkClaimResult, error) {
	repo = NormalizeRepo(repo)
	result := WorkClaimResult{Repo: repo, PR: pr}
	if s.cfg.DryRun {
		return result, nil
	}
	_, err := s.store.Update(ctx, func(st *State) error {
		if !st.ReleaseWorkClaim(repo, pr, "", true) {
			return ErrNoChange
		}
		result.Released = true
		return nil
	})
	return result, err
}

func workClaimConflictReport(repo string, pr int, outcome workClaimOutcome, now time.Time, poll time.Duration) NextReport {
	at := outcome.until.UTC()
	if at.IsZero() || !at.After(now) {
		at = now.Add(poll).UTC()
	}
	return NextReport{
		Action: string(engine.ActionWait), Reason: outcome.reason,
		Repo: NormalizeRepo(repo), PR: pr, Findings: []dialect.Finding{},
		RecheckAfter: &at, CheckedAt: now.UTC(),
	}
}

func terminalInteractiveAction(action string) bool {
	switch engine.ActionKind(action) {
	case engine.ActionDone, engine.ActionBlocked:
		return true
	default:
		return false
	}
}

// Loop keeps the same interactive lease as next/wait. Unlike those ephemeral
// calls it may block for hours, so it renews the lease in the background while
// the review round is active and leaves it in place when returning findings.
func (s *Service) Loop(ctx context.Context, repo string, pr int) (FeedbackReport, int, error) {
	repo = NormalizeRepo(repo)
	for {
		lastRef, refErr := s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
		claim, err := s.claimInteractiveWork(ctx, repo, pr)
		if err != nil {
			return FeedbackReport{}, 1, err
		}
		if claim.acquired {
			break
		}
		now := s.clock()
		deadline := now.Add(time.Minute)
		if !claim.until.IsZero() && claim.until.Before(deadline) {
			deadline = claim.until
		}
		if floor := now.Add(s.waitTick()); deadline.Before(floor) {
			deadline = floor
		}
		if refErr != nil {
			deadline = now.Add(s.waitTick())
		}
		if _, _, err := s.watchStateRef(ctx, lastRef, deadline); err != nil {
			return FeedbackReport{}, 1, err
		}
	}

	loopCtx, cancel := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(workClaimRenewalInterval)
		defer ticker.Stop()
		s.heartbeatWorkClaim(loopCtx, repo, pr, ticker.C, heartbeatErr, cancel)
	}()

	report, code, loopErr := s.loopClaimed(loopCtx, repo, pr)
	cancel()
	<-heartbeatDone
	select {
	case err := <-heartbeatErr:
		if loopErr == nil || errors.Is(loopErr, context.Canceled) {
			loopErr = err
			code = 1
		}
	default:
	}
	if loopErr == nil && code != 10 {
		if releaseErr := s.releaseInteractiveWork(ctx, repo, pr); releaseErr != nil {
			loopErr = releaseErr
			code = 1
		}
	}
	return report, code, loopErr
}

func (s *Service) heartbeatWorkClaim(
	ctx context.Context,
	repo string,
	pr int,
	ticks <-chan time.Time,
	heartbeatErr chan<- error,
	cancel context.CancelFunc,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			renewed, err := s.claimInteractiveWork(ctx, repo, pr)
			if err == nil && !renewed.acquired {
				err = fmt.Errorf("lost interactive work claim: %s", renewed.reason)
			}
			if err == nil {
				continue
			}
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return
			}
			select {
			case heartbeatErr <- err:
			default:
			}
			cancel()
			return
		}
	}
}
