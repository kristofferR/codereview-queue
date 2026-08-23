package crq

import (
	"context"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// maxStaleFactor bounds how long the waiter will trust the state ref alone.
//
// The ref is a necessary but not sufficient wake signal: a bot posting a review
// does not move it — only a daemon observing that review and transitioning the
// round does. So the waiter re-evaluates on a ceiling too, set from the leader
// lease it already has in hand. Two lease periods means a daemon has had two
// full chances to notice before the waiter looks for itself.
const maxStaleFactor = 2

// minWaitTick floors the idle cadence.
//
// Every interval the waiter derives comes from config, and config can be zero:
// an unset CRQ_LEADER_TTL puts the staleness ceiling in the past, and an unset
// CRQ_POLL makes the tick zero. Either turns this loop into a hot spin against
// the shared API budget — the exact failure the command exists to prevent — so
// the floor is arithmetic here rather than a documented expectation of config.
const minWaitTick = time.Second

// waitTick is the idle poll cadence, never zero.
func (s *Service) waitTick() time.Duration {
	if s.cfg.PollInterval > minWaitTick {
		return s.cfg.PollInterval
	}
	return minWaitTick
}

// WaitForAction blocks until there is something for the caller to DO, then
// prints that instruction and exits.
//
// It exists because the wait, not the decision, was the part agents could not
// hold. `crq loop` blocked AND owned the round, so a harness killing it between
// turns either re-fired a review on restart (spending account quota) or hit the
// dedupe and reported a converged round it never collected findings for. Both
// branches are wrong, and every observed session wrapped the command in
// `set +e … ; echo "CRQ_EXIT:$?"` to smuggle the exit code back out.
//
// This owns no review round and has no exit-code vocabulary of its own. It only
// renews the interactive work claim, whose finite TTL makes interruption safe.
// Its job is to notice, so that its exit can be the wake event for an agent that
// ended its turn. If it dies, the caller re-runs it (or calls `crq next`) and
// gets the same answer from persisted state.
//
// It is read-only in the steady state, but NOT unconditionally: when nothing is
// advancing this PR — no round for the head, or no live leader — it drives the
// queue itself through Next, which enqueues and can post the review command.
// The alternative is idling forever on a queue nobody will put the PR in, so the
// honest contract is "writes only when it must, to avoid waiting for nobody".
//
// Cost matters as much as correctness here: the account shares one REST budget
// across the daemon and every agent, and seven concurrent waiters once
// out-spent it on their own. So the loop watches the state ref with a
// conditional GET. An authenticated unchanged-ref response answers 304 and
// does not count against the primary REST rate limit, so the waiter only pays
// for a full evaluation when the ref moves or the staleness ceiling elapses.
func (s *Service) WaitForAction(ctx context.Context, repo string, pr int) (NextReport, error) {
	repo = NormalizeRepo(repo)
	for {
		claim, err := s.claimInteractiveWork(ctx, repo, pr)
		if err != nil {
			return NextReport{}, err
		}
		// Sample after claiming but before deciding. The claim itself moves the
		// state ref, so sampling first would make the waiter observe its own write
		// as external progress and immediately renew in a hot loop.
		lastRef, refErr := s.stateRef(ctx)
		if !claim.acquired {
			// The owner that won may finish long before its lease expires. Watch the
			// shared ref so its release (or an autofix heartbeat/completion) wakes us,
			// with a bounded recheck in case the ref cannot be observed.
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
			if _, _, werr := s.watchStateRef(ctx, lastRef, deadline); werr != nil {
				return workClaimConflictReport(repo, pr, claim, now, s.waitTick()), werr
			}
			continue
		}

		report, action, _, err := s.nextFromState(ctx, repo, pr)
		if err != nil {
			if wait, throttled := ghapi.ThrottleWait(err); throttled {
				if wait <= 0 {
					wait = s.waitTick()
				}
				if wait > workClaimRenewalInterval {
					wait = workClaimRenewalInterval
				}
				if serr := s.sleep(ctx, wait); serr != nil {
					return report, serr
				}
				continue
			}
			return report, err
		}
		if actionable(action.Kind) {
			// A decision can spend longer than WorkClaimTTL in GitHub transport
			// retries. Renew and verify ownership before handing actionable work to
			// the caller; if another host took over, resume waiting instead.
			if action.Kind == engine.ActionFix || action.Kind == engine.ActionPush {
				claim, err = s.refreshInteractiveWork(ctx, repo, pr)
			} else {
				claim, err = s.claimInteractiveWork(ctx, repo, pr)
			}
			if err != nil {
				return report, err
			}
			if !claim.acquired {
				continue
			}
			if terminalInteractiveAction(report.Action) {
				if releaseErr := s.releaseInteractiveWork(ctx, repo, pr); releaseErr != nil {
					return report, releaseErr
				}
			}
			return report, nil
		}

		// Nothing to do yet. Someone has to be advancing the queue, or this waits
		// forever: the daemon normally does, and a live leader lease is how the
		// waiter knows. Without one it drives the queue itself through Next,
		// which enqueues and pumps — correct, just costlier.
		//
		// A live leader is not enough on its own. The daemon only advances PRs in
		// its own scope, so a PR outside the fleet with no round for this head
		// would wait forever on a queue nobody was ever going to put it in. If
		// this head is untracked, drive it regardless of who holds the lease.
		st, _, err := s.store.Load(ctx)
		if err != nil {
			// The same shared-quota pressure this command exists to survive: the
			// first load sleeps through throttling, so this one must too, or the
			// waiter exits precisely when the fleet is busiest.
			if wait, throttled := ghapi.ThrottleWait(err); throttled {
				if wait <= 0 {
					wait = s.waitTick()
				}
				if wait > workClaimRenewalInterval {
					wait = workClaimRenewalInterval
				}
				if serr := s.sleep(ctx, wait); serr != nil {
					return report, serr
				}
				continue
			}
			return report, err
		}
		now := s.clock()
		round := st.Round(repo, pr)
		untracked := round == nil || (report.Head != "" && round.Head != report.Head)
		_, held := st.HeldPR(repo, pr)
		if !held && (untracked || !leaderLive(st, now)) {
			nested, nerr := s.Next(ctx, repo, pr)
			if nerr != nil {
				return report, nerr
			}
			if actionable(engine.ActionKind(nested.Action)) {
				return nested, nil
			}
			delay := s.waitTick()
			if delay > workClaimRenewalInterval {
				delay = workClaimRenewalInterval
			}
			if serr := s.sleep(ctx, delay); serr != nil {
				return report, serr
			}
			continue
		}

		deadline := now.Add(maxStaleFactor * s.leaderTTL(st))
		if report.RecheckAfter != nil && report.RecheckAfter.Before(deadline) {
			deadline = *report.RecheckAfter
		}
		// Never a deadline that has already passed: that would return from the
		// watch without idling and spin this loop.
		if floor := now.Add(s.waitTick()); deadline.Before(floor) {
			deadline = floor
		}
		// Without a baseline the watch cannot recognise a change — it would adopt
		// whatever it reads first — so a failed sample must not buy a long sleep.
		// Come back after one tick and take the baseline again instead.
		if refErr != nil {
			deadline = now.Add(s.waitTick())
		}
		if renewal := now.Add(workClaimRenewalInterval); renewal.Before(deadline) {
			deadline = renewal
		}
		if _, _, err := s.watchStateRef(ctx, lastRef, deadline); err != nil {
			return report, err
		}
	}
}

// sleep idles for d, honouring cancellation. Tests replace it so a replay costs
// no wall-clock time.
func (s *Service) sleep(ctx context.Context, d time.Duration) error {
	if s.sleepFn != nil {
		return s.sleepFn(ctx, d)
	}
	return ghapi.SleepCtx(ctx, d)
}

// actionable reports whether an action is something the caller can act on now.
// wait and hold are the two the caller cannot: both mean "come back later",
// which is precisely what this command does on their behalf.
func actionable(kind engine.ActionKind) bool {
	switch kind {
	case engine.ActionWait, engine.ActionHold:
		return false
	default:
		return true
	}
}

// leaderLive reports whether an autoreview daemon currently holds the lease and
// is therefore advancing rounds (and moving the state ref) on its own.
func leaderLive(st State, now time.Time) bool {
	return st.Leader != nil && st.Leader.ExpiresAt.After(now)
}

// watchStateRef polls the state ref until its SHA changes or deadline passes,
// returning the SHA last seen.
//
// This is the cheap half of the waiter: one authenticated conditional GET per
// tick. An unchanged ref answers 304 without counting against the primary REST
// rate limit. Every meaningful queue transition — fired, reviewing, completed,
// superseded — is a state write, so the ref moving is the signal that something
// worth re-deciding happened.
//
// A read failure is not fatal. Losing a tick only delays a re-evaluation the
// deadline would force anyway, and a waiter that exits on a transient GitHub
// error is exactly the fragility this command replaces.
func (s *Service) watchStateRef(ctx context.Context, lastRef string, deadline time.Time) (bool, string, error) {
	for {
		ref, err := s.stateRef(ctx)
		if err == nil {
			if lastRef != "" && ref != lastRef {
				return true, ref, nil
			}
			lastRef = ref
		}
		tick := s.waitTick()
		if remaining := deadline.Sub(s.clock()); remaining <= 0 {
			return false, lastRef, nil
		} else if remaining < tick {
			tick = remaining
		}
		if serr := s.sleep(ctx, tick); serr != nil {
			return false, lastRef, serr
		}
	}
}

type stateRefReader interface {
	StateRef(context.Context) (string, error)
}

func (s *Service) stateRef(ctx context.Context) (string, error) {
	if reader, ok := s.store.(stateRefReader); ok {
		return reader.StateRef(ctx)
	}
	return s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
}
