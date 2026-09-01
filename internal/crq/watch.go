package crq

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
	"github.com/kristofferR/codereview-queue/internal/workspace"
)

// WatchOptions configures `crq watch`.
type WatchOptions struct {
	// Repos to watch. Empty means every repository in CRQ_REPOS.
	Repos []string
	// Interval between passes.
	Interval time.Duration
	// Once runs a single pass and returns.
	Once bool
	// Dispatch turns "a PR needs fixing" into a session that fixes it. nil means
	// the default, which is ON: watching a pull request nobody fixes is a queue
	// that reports work and does none. A pointer because "unset" and "explicitly
	// off" are different instructions, and only the first may be overridden by
	// the configured default.
	Dispatch *bool
	// Command is the fix session to run, argv-style. Empty means CRQ_DISPATCH_CMD.
	Command []string
	// MaxAttempts bounds dispatches per head. 0 means the configured default.
	MaxAttempts int
	// Concurrency caps how many fix sessions run at once. nil takes the
	// configured cap; 0 means no cap, which is the default: fixing findings
	// spends no CodeRabbit quota, so it has no reason to queue. It exists only as
	// a resource valve for a machine that cannot take the load.
	//
	// A pointer because 0 is a real instruction here, not an unset int: passing
	// `--concurrency 0` is how one run overrides a cap set in
	// CRQ_DISPATCH_CONCURRENCY, and an int cannot tell that from no flag at all.
	Concurrency *int
}

// dispatching reports whether this run starts fix sessions. Watch resolves the
// default before a pass ever reads it, so nil here means the caller reached a
// pass without going through Watch — observe rather than guess.
func (o WatchOptions) dispatching() bool {
	return o.Dispatch != nil && *o.Dispatch
}

// WatchEvent is one PR's state at a pass, and what the watcher did about it.
type WatchEvent struct {
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
	Findings int    `json:"findings"`
	// Dispatched says a fix session was claimed and handed to the dispatch pool;
	// Skipped says why one was not, when dispatch was enabled and the action
	// asked for it. A one-shot watch waits and reports the session's final result.
	Dispatched bool      `json:"dispatched,omitempty"`
	Skipped    string    `json:"skipped,omitempty"`
	At         time.Time `json:"at"`
}

// Watch drives every open PR in scope through the same `next` oracle an agent
// uses, and — with Dispatch — starts a session to fix the ones that need it.
//
// The queue's stated non-goal is that crq does not write code or decide which
// findings are real. Dispatch does not change that: crq starts a session and
// tells it which PR to look at; the session does the judging. That is why this
// is a separate command over the same oracle rather than something the pump
// does, and why it is off unless asked for.
//
// Every dispatch is claimed under CAS, so two watchers cannot both spawn a
// session for one PR, and bounded in cooldown-backed cycles, so a fix that keeps
// not working neither spins nor permanently dead-letters the head.
func (s *Service) Watch(ctx context.Context, opts WatchOptions, emit func(WatchEvent) error) error {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = s.cfg.DispatchMaxAttempts
	}
	// Dispatch is what watching is FOR, so it is on unless something says
	// otherwise: an observed pull request nobody fixes is a queue that reports
	// work and does none. Off comes from three places — --no-dispatch for one
	// run, a per-repo switch in the state ref, and having no fix command at all.
	if opts.Dispatch == nil {
		on := true
		opts.Dispatch = &on
	}
	if *opts.Dispatch && len(opts.Command) == 0 {
		opts.Command = s.cfg.DispatchCommand
	}
	if *opts.Dispatch && len(opts.Command) == 0 {
		// Not an error: `crq watch` on a machine with no fix agent configured is
		// a perfectly good observer, and refusing to start would make the
		// default setting break the plain command. Say it once, out loud, so it
		// is not the silent nothing dispatch health exists to prevent.
		off := false
		opts.Dispatch = &off
		if s.log != nil {
			s.log.Printf("watch: observing only — no fix command configured (set CRQ_DISPATCH_CMD, or pass one after --)")
		}
	}
	// The watcher's PATH is the one a fix session inherits, so its report is the
	// one that answers "can this host actually run the agent". Observation-only
	// watchers must not claim a capability they cannot exercise.
	if opts.dispatching() {
		s.ReportHost(ctx, "autofix")
	}
	// Fix sessions run OUTSIDE the pass, and by default without a cap.
	//
	// The queue exists for exactly one thing: the account-metered review. Fixing
	// findings spends none of that allowance, so making a PR wait for a dispatch
	// slot queues work that has no reason to wait — the same mistake crq already
	// corrected for co-only rounds, which bypass the slot and the quota gate
	// entirely. A PR whose findings are ready gets a session now.
	//
	// The decisions stay serial on purpose: `Next` is what enqueues and fires, so
	// deciding one PR at a time is what keeps the metered review in one queue.
	// Only the sessions overlap.
	//
	// The configured cap applies unless this run states one: sizing the pool from
	// an unset option ignored the cap an operator set in CRQ_DISPATCH_CONCURRENCY,
	// which is precisely the machine that cannot take the load.
	concurrency := s.cfg.DispatchConcurrency
	if opts.Concurrency != nil {
		concurrency = *opts.Concurrency
	}
	pool := newDispatchPool(concurrency)
	// Sessions run under a context this function can cancel, and the deferred
	// order matters: cancel first, wait second.
	//
	// Returning an error — an emit that failed because the JSON consumer went
	// away, a repository that could not be read — used to fall straight into
	// pool.wait() with the sessions still running. Agents kept writing code with
	// nobody observing the watcher, and the error the caller needed could not
	// surface until the last session exited, possibly hours later.
	sessionCtx, endSessions := context.WithCancel(ctx)
	defer pool.wait()
	defer endSessions()
	for {
		// Resolved per pass, not once at startup: the interval is fleet-settable
		// from the dashboard, and a watcher that read it when the service booted
		// reported the new cadence while still running on the old one until
		// somebody restarted it. This run's own --interval still wins.
		wait := opts.Interval
		if wait <= 0 {
			wait = s.watchInterval(ctx)
		}
		if err := s.watchPass(sessionCtx, opts, pool, emit); err != nil {
			reset, throttled := ghapi.ThrottleWait(err)
			if !throttled {
				// Includes an unreachable control plane: the service manager's
				// restart policy is the retry, and a non-zero exit is what makes a
				// worker with no gateway visible from outside its own log.
				return err
			}
			// A one-shot run is somebody's cron or CI job, and it does not get to
			// sleep out a reset that may be an hour away. Report the throttle:
			// exiting 0 after checking part of the fleet — or none of it — makes an
			// incomplete scan look like a clean one, which is the same lie the
			// per-PR failure path above already refuses to tell.
			if opts.Once {
				return err
			}
			// Retrying on the ordinary interval hammers an exhausted quota and
			// pushes the reset further out, which is the opposite of waiting it
			// out. Sleep for what the API said.
			if reset > wait {
				wait = reset
			}
			if s.log != nil {
				s.log.Printf("watch: %v; waiting %s for the reset", err, wait.Round(time.Second))
			}
		}
		if opts.Once {
			return nil
		}
		if err := ghapi.SleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

// abortsPass reports a failure that belongs to the whole pass, not to the
// repository or pull request it surfaced on: an exhausted GitHub quota, or a
// control plane this host cannot reach. Both are logged-and-skipped at the
// per-target sites otherwise, and skipping a dead gateway target by target
// leaves the daemon retrying "connection refused" forever.
func abortsPass(err error) bool {
	if _, throttled := ghapi.ThrottleWait(err); throttled {
		return true
	}
	return ghapi.IsServerUnreachable(err)
}

// watchInterval is the pass cadence with the fleet's recorded default applied,
// falling back to this host's env when the ref cannot be read — a watcher must
// not stop pacing itself because one load failed.
func (s *Service) watchInterval(ctx context.Context) time.Duration {
	if st, _, err := s.store.Load(ctx); err == nil {
		if d := s.cfg.WithFleet(st.Fleet).WatchInterval; d > 0 {
			return d
		}
	}
	return s.cfg.WatchInterval
}

func (s *Service) watchPass(ctx context.Context, opts WatchOptions, pool *dispatchPool, emit func(WatchEvent) error) error {
	var failures []string
	type pendingEvent struct {
		event  WatchEvent
		result <-chan dispatchResult
	}
	var pending []pendingEvent
	if opts.dispatching() {
		s.ReportHost(ctx, "autofix")
	}
	// One snapshot for the pass. It decides both which repositories are watched
	// at all and which of them may be fixed, so it is read BEFORE the target
	// list is built.
	st, _, stateErr := s.store.Load(ctx)
	if stateErr != nil {
		return fmt.Errorf("loading shared state for watch pass: %w", stateErr)
	}
	fleetCfg := s.cfg.WithFleet(st.Fleet)
	// Copied, never appended to in place. `crq watch -- <cmd>` splits argv at
	// "--", so the flag half keeps CAPACITY reaching into the command half, and
	// fs.Args() is a sub-slice of it: filling an empty list with append wrote the
	// repository names straight over the fix command, and every dispatch in the
	// fleet died with "fork/exec kristofferr/codereview-queue: no such file or
	// directory". A caller's slice is the caller's.
	repos := make([]string, 0, len(opts.Repos)+len(fleetCfg.AllowRepos))
	repos = append(repos, opts.Repos...)
	if len(repos) == 0 {
		// Enrollment is a fleet-wide record, and autoreview already builds its
		// scan list from it. Reading only CRQ_REPOS here meant a repository
		// enrolled from the dashboard was reviewed but never watched — its
		// findings arrived and no fix session ever started — until somebody
		// edited an env file on this host and reinstalled the service.
		seen := map[string]bool{}
		add := func(repo string) {
			if repo = NormalizeRepo(repo); repo == "" || seen[repo] {
				return
			}
			seen[repo] = true
			repos = append(repos, repo)
		}
		for repo := range fleetCfg.AllowRepos {
			add(repo)
		}
		for _, repo := range st.EnrolledRepos() {
			add(repo)
		}
		sort.Strings(repos) // stable order: a pass must not depend on map iteration
	}
	if len(repos) == 0 {
		return errors.New("nothing to watch: pass a repository, or set CRQ_REPOS")
	}
	// Gather every candidate first, then start from a different one each pass.
	//
	// A fixed order starves the tail: with three dispatch slots and four PRs
	// needing fixes, the same three take the slots every pass and the fourth is
	// told "at dispatch capacity" forever. One PR sat five hours that way while
	// its findings grew from 15 to 25. This is the same fix the quota-free
	// rescue scan already needed, for the same reason.
	type candidate struct {
		repo     string
		pull     ghapi.Pull
		priority int64
	}
	var candidates []candidate
	gate := NormalizeRepo(s.cfg.GateRepo)
	// Which repositories may be FIXED is per repository, read once for the pass.
	// Watching is unaffected: a repository with autofix off is still observed
	// and still reviewed, so its feedback arrives for a person to act on.
	autofixOff := map[string]bool{}
	// A repository turned off from the dashboard must stop being watched on the
	// next pass, not on restart.
	notEnrolled := map[string]bool{}
	for _, repo := range repos {
		if !st.AutofixEnabled(repo) {
			autofixOff[NormalizeRepo(repo)] = true
		}
		if !s.reviewsRepo(st, repo) {
			notEnrolled[NormalizeRepo(repo)] = true
		}
	}
	for _, repo := range repos {
		// The calibration PR is deliberately kept open to probe account quota; it
		// is not work, and `Next` would enqueue a real review for it — or dispatch
		// a session against CALIBRATION.md. autoReviewPass excludes the gate
		// repository for the same reason.
		if gate != "" && NormalizeRepo(repo) == gate {
			continue
		}
		// CRQ_EXCLUDE means "crq does not go here", and it has to mean that to
		// every path that acts on a repository. autoReviewPass has always honoured
		// it; this one did not, so the single setting that reads like a fleet-wide
		// opt-out silently covered half of what crq does — reviews stopped and the
		// watcher carried on, which is not a setting anyone can reason about.
		if s.cfg.ExcludeRepos[NormalizeRepo(repo)] {
			continue
		}
		// "crq does not go here" has to mean that to every path, including the
		// one that spends an agent on a fix session.
		if notEnrolled[NormalizeRepo(repo)] {
			continue
		}
		pulls, err := s.gh.ListPulls(ctx, repo, openPullQuery())
		if err != nil {
			// Throttling is the whole fleet's problem and the caller sleeps it
			// out. One repository being renamed, deleted, or unreadable by this
			// token is not: aborting the pass over it means every healthy
			// repository after it gets no events and no fix sessions, on this pass
			// and — since the service restarts into the same list — on every pass
			// after it. Same treatment as an unreadable PR below.
			if abortsPass(err) {
				return err
			}
			if s.log != nil {
				s.log.Printf("watch: %s: %v", repo, err)
			}
			if opts.Once {
				failures = append(failures, fmt.Sprintf("%s: %v", repo, err))
			}
			continue
		}
		open := make(map[int]bool, len(pulls))
		for _, pull := range pulls {
			var priority int64
			if round := st.Round(repo, pull.Number); round != nil && round.Seq < 0 {
				priority = round.Seq
			}
			candidates = append(candidates, candidate{repo: repo, pull: pull, priority: priority})
			open[pull.Number] = true
		}
		// This list is the authoritative set of open pull requests for the
		// repository, and it has already been paid for. Any round still waiting
		// for one that is not in it belongs to a closed or merged PR, so retire
		// them all here rather than leaving them to a sweep that inspects one
		// candidate per pass — a merged PR sat in the rendered queue for an
		// afternoon that way, behind rounds the sweep reached first.
		if err := s.retireClosedRounds(ctx, repo, open); err != nil {
			if abortsPass(err) {
				return err
			}
			if s.log != nil {
				s.log.Printf("watch: %s: retiring closed rounds: %v", repo, err)
			}
		}
	}
	// Operator-prioritized PRs always lead the pass, ordered by their queue
	// sequence. Everyone else keeps the rotating start that prevents a fixed
	// repository/PR order from starving the tail when dispatch capacity is full.
	var prioritized, regular []candidate
	for _, c := range candidates {
		if c.priority < 0 {
			prioritized = append(prioritized, c)
		} else {
			regular = append(regular, c)
		}
	}
	sort.SliceStable(prioritized, func(i, j int) bool {
		return prioritized[i].priority < prioritized[j].priority
	})
	if len(regular) > 0 {
		s.watchOffset = (s.watchOffset + 1) % len(regular)
		rotated := append([]candidate(nil), regular[s.watchOffset:]...)
		rotated = append(rotated, regular[:s.watchOffset]...)
		candidates = append(prioritized, rotated...)
	} else {
		candidates = prioritized
	}
	for i := range candidates {
		c := candidates[i]
		repo, pull := c.repo, c.pull
		{
			if err := ctx.Err(); err != nil {
				return err
			}
			// The skip rules suppress review deliberately — the marker to protect
			// the shared quota, the author list to keep crq off pull requests a
			// repository has said not to touch. Next is a MUTATING oracle: it
			// enqueues, it can fire, and it can start a fix session that runs an
			// agent with approvals bypassed. So both are honoured BEFORE calling
			// it, not after.
			//
			// Read through the pass's state, the way autoReviewPass reads them:
			// both are fleet- and repository-settable, and a watcher answering
			// from its startup env reviewed pull requests the settings it was
			// displaying had just excluded.
			skipCfg := s.cfgFor(st, repo)
			skipped := ""
			switch {
			case skipCfg.SkipsReview(pull.Body):
				skipped = "fleet auto-review skip marker present"
			case skipCfg.SkipAuthors[dialect.NormalizeBotName(strings.ToLower(pull.User.Login))]:
				skipped = "the author is on this repository's skip list"
			}
			if skipped != "" {
				event := WatchEvent{
					Repo: repo, PR: pull.Number,
					Action: "skipped", Reason: skipped,
					At: s.clock().UTC(),
				}
				if emit != nil {
					if err := emit(event); err != nil {
						return fmt.Errorf("emitting %s#%d: %w", repo, pull.Number, err)
					}
				}
				continue
			}
			var report NextReport
			var err error
			if opts.dispatching() {
				// Peek through the non-firing decision path first. A carried
				// finding can make Next enqueue and Pump the current head before
				// returning "fix"; claiming only afterwards spends a metered
				// review on the code this session is about to replace.
				var action engine.Action
				report, action, _, err = s.nextFromState(ctx, repo, pull.Number)
				if err == nil && report.Action == string(engine.ActionFix) {
					if onePassReport, handled, onePassErr := s.onePassNext(ctx, report, action, true); onePassErr != nil {
						err = onePassErr
					} else if handled {
						report = onePassReport
					}
				} else if err == nil {
					report, err = s.nextAutomated(ctx, repo, pull.Number)
				}
			} else {
				report, err = s.nextAutomated(ctx, repo, pull.Number)
			}
			if err != nil {
				if abortsPass(err) {
					return err
				}
				if s.log != nil {
					s.log.Printf("watch: %s#%d: %v", repo, pull.Number, err)
				}
				// A one-shot run is somebody's cron or CI job: reporting success
				// after skipping a PR it could not read makes a broken scan look
				// like a clean one.
				if opts.Once {
					failures = append(failures, fmt.Sprintf("%s#%d: %v", repo, pull.Number, err))
				}
				continue
			}
			if opts.dispatching() && report.Action != string(engine.ActionFix) && report.Action != string(engine.ActionBlocked) {
				eligible, merged, mergeReason, mergeErr := s.mergeOnePassReady(ctx, repo, pull.Number)
				if mergeErr != nil {
					if abortsPass(mergeErr) {
						return mergeErr
					}
					if s.log != nil {
						s.log.Printf("watch: merging %s#%d: %v", repo, pull.Number, mergeErr)
					}
					if opts.Once {
						failures = append(failures, fmt.Sprintf("%s#%d merge: %v", repo, pull.Number, mergeErr))
					}
					report.Action = string(engine.ActionWait)
					report.Reason = "post-fix merge check failed: " + mergeErr.Error()
				} else if eligible {
					report.Reason = mergeReason
					if merged {
						report.Action = "merged"
					} else {
						report.Action = string(engine.ActionWait)
					}
				}
			}
			event := WatchEvent{
				Repo: repo, PR: pull.Number,
				Action: report.Action, Reason: report.Reason,
				Findings: len(report.Findings), At: s.clock().UTC(),
			}
			report.Fork = forkPull(repo, pull)
			var result <-chan dispatchResult
			if opts.dispatching() && report.Action == string(engine.ActionFix) && autofixOff[NormalizeRepo(repo)] {
				event.Skipped = "autofix is off for this repository (crq autofix on " + NormalizeRepo(repo) + ")"
			} else if opts.dispatching() && report.Action == string(engine.ActionFix) && !s.mayDispatch(skipCfg, repo, pull) {
				event.Skipped = "the head branch is a fork; set CRQ_DISPATCH_FORKS=1 to fix contributor pull requests"
			} else if opts.dispatching() && report.Action == string(engine.ActionFix) && !report.dispatchReady {
				event.Skipped = "waiting for active reviewers before starting autofix"
			} else if opts.dispatching() && report.Action == string(engine.ActionFix) {
				// Claim here and hand preparation to the pool. Checkout and
				// cmd.Start may block on a remote; neither belongs in the serial
				// decision pass that still has other PRs to inspect.
				event.Dispatched, event.Skipped, result = s.queueDispatchResult(ctx, opts, pool, report)
			}
			if opts.dispatching() && report.Action == string(engine.ActionFix) && !event.Dispatched {
				// The peek above deliberately avoided firing before a session
				// could claim the findings. If no session actually started,
				// however, a carried finding from an older head must not suppress
				// Next forever: Next knows it may enqueue and review the current
				// head because FindingsOnHead is empty.
				if _, advanceErr := s.nextAutomated(ctx, repo, pull.Number); advanceErr != nil {
					if abortsPass(advanceErr) {
						return advanceErr
					}
					if s.log != nil {
						s.log.Printf("watch: advancing skipped fix for %s#%d: %v", repo, pull.Number, advanceErr)
					}
					if opts.Once {
						failures = append(failures, fmt.Sprintf("%s#%d: %v", repo, pull.Number, advanceErr))
					}
				}
			}
			if opts.Once {
				pending = append(pending, pendingEvent{event: event, result: result})
				continue
			}
			if emit != nil {
				// A consumer that has gone away (a closed pipe, a full
				// destination) means nothing is observing a watcher that is still
				// firing reviews and starting sessions. Stop instead.
				if err := emit(event); err != nil {
					return fmt.Errorf("emitting %s#%d: %w", repo, pull.Number, err)
				}
			}
		}
	}
	if opts.Once {
		// A one-shot invocation is a cron/CI result, not a long-lived observer.
		// Wait for every session it started and make both its events and its exit
		// status describe the outcome, rather than only the successful handoff to
		// a goroutine.
		pool.wait()
		for _, item := range pending {
			event := item.event
			if item.result != nil {
				result := <-item.result
				if !result.ok {
					event.Dispatched = false
					event.Skipped = result.reason
					failures = append(failures,
						fmt.Sprintf("%s#%d: %s", event.Repo, event.PR, result.reason))
				}
			}
			if emit != nil {
				if err := emit(event); err != nil {
					return fmt.Errorf("emitting %s#%d: %w", event.Repo, event.PR, err)
				}
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d target(s) could not be checked: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// mayDispatch reports whether a fix session may run on this pull request's code.
//
// A fix session checks the head out and runs an agent over it with approvals
// bypassed, holding a token that can write to the repository. On a pull request
// from a fork, that code is a stranger's: a build script, a test, or an
// instruction file in the branch executes with the account's credentials on this
// host. Which is fine for a fleet of one's own branches, and is not something to
// turn on for a project accepting contributions without saying so.
//
// So a fork is skipped unless CRQ_DISPATCH_FORKS says otherwise. This is not a
// sandbox and does not claim to be one — it is the line between code the
// operator wrote and code somebody else did. Reviewing a fork is unaffected:
// reading a pull request runs nothing.
func (s *Service) mayDispatch(cfg Config, repo string, pull ghapi.Pull) bool {
	if cfg.DispatchForks {
		return true
	}
	return !forkPull(repo, pull)
}

func forkPull(repo string, pull ghapi.Pull) bool {
	head := NormalizeRepo(pull.Head.Repo.FullName)
	// An unreadable head repository is not evidence that it is ours. A deleted
	// fork answers with an empty name, and defaulting to "same repository" would
	// hand exactly the untrusted case the permission it is missing.
	return head == "" || head != NormalizeRepo(repo)
}

// noteDispatchHealth records whether fix sessions are starting, and says so
// loudly the first time it is clear they are not.
//
// The failure this exists for looked exactly like success from the outside: the
// watcher ran, the queue moved, PRs reported findings — and every dispatch died
// on a wedged git mirror, in a log line nobody was reading.
func (s *Service) noteDispatchHealth(ctx context.Context, started bool, reason string) {
	var flipped, unhealthy bool
	state, err := s.store.Update(ctx, func(st *State) error {
		was := st.Autofix.Unhealthy()
		st.NoteDispatch(s.cfg.Host, started, reason, s.clock())
		unhealthy = st.Autofix.Unhealthy()
		flipped = unhealthy != was
		return nil
	})
	if err != nil {
		return
	}
	// The dashboard is where this alert is meant to be read, and an autofix service that has
	// stopped working may produce no other write for hours. Every unhealthy
	// attempt changes the rendered count, host, or error; recovery removes it.
	if unhealthy || flipped {
		s.sync(ctx, state)
	}
	if flipped && unhealthy && s.log != nil {
		s.log.Printf("ALERT: no fix session has started in %d dispatch attempts — %s", AutofixUnhealthyAfter, reason)
	}
}

// startDispatch claims the round and hands the session to the pool.
//
// The claim is taken HERE, in the pass, rather than inside the session: an event
// saying `dispatched` is read as "somebody is handling this PR", so a round
// another watcher holds, or one that has spent its attempts, has to come back as
// skipped rather than as work in progress.
func (s *Service) startDispatch(ctx context.Context, opts WatchOptions, pool *dispatchPool, report NextReport) (bool, string) {
	ok, why, _ := s.startDispatchResult(ctx, opts, pool, report)
	return ok, why
}

// startDispatchResult is the synchronous test/helper form: it waits through
// checkout and cmd.Start so its boolean means a process actually started.
func (s *Service) startDispatchResult(
	ctx context.Context,
	opts WatchOptions,
	pool *dispatchPool,
	report NextReport,
) (bool, string, <-chan dispatchResult) {
	queued, why, result, started := s.queueDispatch(ctx, opts, pool, report)
	if !queued {
		return false, why, nil
	}
	start := <-started
	return start.ok, start.reason, result
}

// queueDispatchResult returns once the session is claimed and queued. Its
// buffered result lets a continuous watch forget the exit while a one-shot
// watch waits for an honest final event.
func (s *Service) queueDispatchResult(
	ctx context.Context,
	opts WatchOptions,
	pool *dispatchPool,
	report NextReport,
) (bool, string, <-chan dispatchResult) {
	queued, why, result, _ := s.queueDispatch(ctx, opts, pool, report)
	return queued, why, result
}

func (s *Service) queueDispatch(
	ctx context.Context,
	opts WatchOptions,
	pool *dispatchPool,
	report NextReport,
) (bool, string, <-chan dispatchResult, <-chan dispatchResult) {
	// DryRun means crq writes nothing and posts nothing. Claiming shared state,
	// running a code-writing command, and recording dispatch health are all
	// writes, so this is checked before any of them.
	if s.cfg.DryRun {
		return false, "dry run: would dispatch a fix session", nil, nil
	}
	solver := s.repoCfg(ctx, report.Repo)
	if len(report.Findings) > 0 {
		report.Findings = autofixFindings(report.Findings, solver.FixSeverities)
		if len(report.Findings) == 0 {
			return false, "autofix policy excludes every open finding", nil, nil
		}
	}
	var ok bool
	var why string
	if opts.Once {
		ok, why = pool.acquireContext(ctx)
	} else {
		ok, why = pool.acquire()
	}
	if !ok {
		return false, why, nil, nil
	}
	token := randomToken()
	claimed, why, byDesign, model := s.claimDispatchModels(ctx, &report, token, opts.MaxAttempts)
	if !claimed {
		pool.release()
		// A round another watcher already holds, or one that has spent its
		// per-head attempts, is the bound doing its job — not this dispatcher
		// failing. Counting it would raise "fix sessions are not starting" after
		// three passes over a watcher that is obeying its own configuration, and
		// an exhausted head refuses again on every pass forever.
		if !byDesign {
			s.noteDispatchHealth(ctx, false, why)
			if s.log != nil {
				s.log.Printf("watch: %s#%d not fixed: %s", report.Repo, report.PR, why)
			}
		}
		return false, why, nil, nil
	}
	result := make(chan dispatchResult, 1)
	started := make(chan dispatchResult, 1)
	pool.run(func() {
		result <- s.runDispatch(ctx, opts, report, token, model, started)
		close(result)
	})
	return true, "", result, started
}

// recordedMaxAttempts is the fix-session budget for one repository: whatever
// shared state RECORDS, falling back to this run's own value.
//
// Per repository, not per watcher — the budget is a property of the project
// being fixed, and one that keeps needing a fourth try should not have to raise
// the limit for every other repository the watcher handles.
//
// RECORDED, not resolved. The merged configuration always carries a positive
// default, so reading that here outranked this run's own `--max-attempts` on
// every ordinary setup: the flag was accepted and then silently replaced by 3.
//
// Both records are asked. The typed solver record is the per-repository answer;
// the generic fleet map is where the settings editor saves
// CRQ_DISPATCH_MAX_ATTEMPTS, and a claim that consulted only the first went on
// enforcing this host's number while the dashboard reported the fleet's — with
// no later state read able to change it.
func (s *Service) recordedMaxAttempts(ctx context.Context, repo string, fallback int) int {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return fallback
	}
	return recordedMaxAttemptsIn(st, repo, fallback)
}

func recordedMaxAttemptsIn(st State, repo string, fallback int) int {
	if sv := st.EffectiveSolver(repo); sv.MaxAttempts != nil {
		return *sv.MaxAttempts
	}
	// Positive only, the way every consumer of a fleet setting reads one: a
	// recorded 0 or a value that will not parse is not a budget, and honouring
	// it would stop the whole fleet fixing anything.
	if n, err := strconv.Atoi(strings.TrimSpace(st.Fleet.Env["CRQ_DISPATCH_MAX_ATTEMPTS"])); err == nil && n > 0 {
		return n
	}
	return fallback
}

type dispatchResult struct {
	ok     bool
	reason string
}

// runDispatch runs one claimed dispatch and records whether a session started. It
// runs in the pool, off the pass, so a long session delays nothing else.
func (s *Service) runDispatch(
	ctx context.Context,
	opts WatchOptions,
	report NextReport,
	token string,
	model string,
	started chan<- dispatchResult,
) dispatchResult {
	ok, why := s.dispatchWithStart(ctx, opts, report, token, model, started)
	if !ok && s.log != nil {
		s.log.Printf("watch: %s#%d not fixed: %s", report.Repo, report.PR, why)
	}
	return dispatchResult{ok: ok, reason: why}
}

// dispatch checks the claimed round's head out and runs the fix session.
func (s *Service) dispatch(ctx context.Context, opts WatchOptions, report NextReport, token string) (bool, string) {
	model, _ := s.claimedDispatchModel(ctx, report, token)
	return s.dispatchWithStart(ctx, opts, report, token, model, nil)
}

func (s *Service) dispatchWithStart(
	ctx context.Context,
	opts WatchOptions,
	report NextReport,
	token string,
	model string,
	started chan<- dispatchResult,
) (ok bool, reason string) {
	// Health means "can this watcher START a session", not whether the agent later
	// succeeds at the work it was given. Record pre-start failures on return;
	// cmd.Start records recovery immediately, while a healthy long-running
	// session is still running.
	processStarted := false
	defer func() {
		if !processStarted {
			s.noteDispatchHealth(context.WithoutCancel(ctx), false, reason)
			if started != nil {
				started <- dispatchResult{reason: reason}
			}
		}
	}()

	// Losing the claim means another watcher has taken this round and may be
	// running its own session. Two sessions writing one worktree is worse than
	// no session, so the heartbeat cancels this one.
	//
	// It starts BEFORE the checkout, not after: a first clone of a large
	// repository can outlast DispatchTTL, and a claim nobody is refreshing reads
	// as abandoned — so the takeover this exists to prevent would happen while
	// this dispatch was still fetching.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := s.beatDispatch(runCtx, report, token, cancel)

	ws := s.workspace(runCtx)
	co, err := ws.Checkout(runCtx, report.Repo, report.PR, report.Head)
	if err != nil {
		// Nothing ran, so the attempt did not happen: a transient clone failure
		// must not eat the per-head budget and permanently skip the PR.
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		return false, "checkout failed: " + err.Error()
	}

	// The findings go OUTSIDE the worktree. At the repository root they are an
	// untracked file, and a session following the documented `git add -A` push
	// would commit crq's review payload into the PR.
	findingsPath, err := s.writeFindings(report)
	if err != nil {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, err.Error()
	}
	defer os.Remove(findingsPath)

	// A session's output went nowhere, so when one failed there was nothing to
	// read but the fact that nothing happened. Every session gets a file, and
	// the path is logged before it starts so it is findable while it runs.
	logPath, logFile, err := s.sessionLog(ctx, report)
	if err == nil {
		// Record where the output is going and what it is working on, so the
		// dashboard can say more about a running session than "attempt 2".
		s.noteSessionDetail(ctx, report, token, logPath, len(report.Findings))
	}
	if err != nil {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, "could not open a session log: " + err.Error()
	}
	defer logFile.Close()

	cmd := exec.CommandContext(runCtx, opts.Command[0], opts.Command[1:]...)
	configureDispatchProcess(cmd)
	cmd.Dir = co.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// The repository's solver settings travel in the ENVIRONMENT, because argv
	// is fixed when the watcher starts and these differ per repository. The
	// session script reads them; a script from an older install ignores them
	// and runs exactly as it did.
	solver := s.repoCfg(runCtx, report.Repo)
	sessionPrompt := appendAutofixPolicy(solver.FixPrompt, solver.FixSeverities, solver.FixAskMode)
	cmd.Env = append(os.Environ(),
		"CRQ_DISPATCH_REPO="+report.Repo,
		fmt.Sprintf("CRQ_DISPATCH_PR=%d", report.PR),
		"CRQ_DISPATCH_HEAD="+report.Head,
		"CRQ_DISPATCH_FINDINGS="+findingsPath,
		"CRQ_DISPATCH_TOKEN="+token,
		// The agent and prompt come from the unit's environment and are already
		// in os.Environ(); only the per-repository half is added here.
		"CRQ_FIX_MODEL="+model,
		"CRQ_FIX_EFFORT="+solver.FixEffort,
		"CRQ_FIX_PROMPT="+sessionPrompt,
	)
	// The session's push is a plain `git push`, and its crq/gh commands also need
	// the daemon's GitHub identity. Resolve the current token immediately before
	// launch: a long-lived daemon must not retain the credential it started with,
	// and the agent's login shell may put a broken gh shim first on PATH. The
	// secret lives only in the child environment, never in argv or git config.
	sessionToken := strings.TrimSpace(ws.Token)
	if ws.TokenSource != nil {
		sessionToken = strings.TrimSpace(ws.TokenSource(runCtx))
	}
	if sessionToken != "" {
		cmd.Env = setCommandEnv(cmd.Env, workspace.TokenEnv, sessionToken)
		cmd.Env = setCommandEnv(cmd.Env, "GITHUB_TOKEN", sessionToken)
		cmd.Env = setCommandEnv(cmd.Env, "GH_TOKEN", sessionToken)
	}
	if s.log != nil {
		s.log.Printf("watch: dispatching %s for %s#%d@%s (%d findings) — log: %s",
			opts.Command[0], report.Repo, report.PR, report.Head, len(report.Findings), logPath)
	}
	// Checkout preparation can take long enough for another fixer to land and
	// merge the pull request. The pass-level open-PR snapshot is no longer proof
	// that this claimed head still needs an agent, so re-read it at the last safe
	// point before starting a code-writing process.
	pull, err := s.gh.GetPull(runCtx, report.Repo, report.PR)
	if err != nil {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, "could not refresh the pull request before starting its fix session: " + err.Error()
	}
	if pull.State != "open" || pull.Merged {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, "pull request is no longer open; stale fix session skipped"
	}
	liveHead := strings.TrimSpace(pull.Head.SHA)
	claimedHead := strings.TrimSpace(report.Head)
	if liveHead == "" || claimedHead == "" || !strings.HasPrefix(liveHead, claimedHead) {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, fmt.Sprintf("pull request head moved from %s to %s; stale fix session skipped",
			shortSHA(claimedHead), shortSHA(liveHead))
	}
	finalizingOnePass := hasOnePassFinalizer(report.Findings)
	readyBase := strings.TrimSpace(pull.Base.SHA)
	if finalizingOnePass && readyBase == "" {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, "GitHub did not report the campaign's base revision; fix session skipped"
	}
	if err := cmd.Start(); err != nil {
		// A command that never reached a process did not use up the per-head
		// budget. Correcting a missing agent must leave this head retryable.
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		return false, "fix session could not start: " + err.Error()
	}
	processStarted = true
	if started != nil {
		started <- dispatchResult{ok: true}
	}
	s.noteDispatchHealth(context.WithoutCancel(ctx), true, "")
	runErr := cmd.Wait()
	_ = logFile.Sync()
	if question := clarificationFromLog(string(readLogTail(logPath, 256<<10))); question != "" {
		// Asking is not a failed solution attempt. Hold the PR with the exact
		// question so it appears in the dashboard's existing attention surface,
		// then release this claim without consuming the per-head budget.
		reason := "Autofix needs clarification: " + question
		if _, err := s.holdDispatch(context.WithoutCancel(ctx), report, token, reason); err == nil {
			s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
			return false, fmt.Sprintf("%s (log: %s)", reason, logPath)
		}
		// Standalone watch/autofix has no autoreview leader, so it cannot create
		// an administrative hold. Keep a head-scoped terminal dispatch marker
		// instead; otherwise the released claim launches another paid session
		// for the same unanswered question on the next pass.
		if err := s.stopDispatchForClarification(
			context.WithoutCancel(ctx), report, token, question,
		); err != nil {
			return false, fmt.Sprintf("%s (could not record the clarification stop: %v; log: %s)", reason, err, logPath)
		}
		return false, fmt.Sprintf("%s (autofix stopped for this head; log: %s)", reason, logPath)
	}
	// A session the WATCHER stopped did not use up the head's attempt budget.
	//
	// That budget exists to stop a fix which keeps not working from looping for
	// ever, and a session killed because the daemon went down never got to
	// fail — it is the operator's restart, not the fix's quality. Counting it
	// spent two of this head's three attempts on redeploys alone, and a third
	// would have left the pull request unfixable at that commit while `crq next`
	// went on asking for a fix.
	attempted := ctx.Err() == nil
	if runErr != nil && attempted {
		if failure := dialect.ClassifyAgentFailure(readLogTail(logPath, 256<<10), s.clock()); failure.Unavailable {
			s.releaseDispatchUnavailable(context.WithoutCancel(ctx), report, token, failure)
			if lost() {
				return false, "another watcher took this round; the session was stopped"
			}
			return false, fmt.Sprintf("%s; fallback will retry after %s (log: %s)",
				failure.Reason, failure.RetryAt.Format(time.RFC3339), logPath)
		}
	}
	if lost() {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, attempted)
		return false, "another watcher took this round; the session was stopped"
	}
	if runErr != nil {
		if !attempted {
			s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
			// Say which kind of ending this was: "failed" reads as the fix being
			// wrong, and the reader's next move is different when crq stopped it.
			return false, fmt.Sprintf("fix session stopped with the watcher, and keeps its attempt (log: %s)", logPath)
		}
		if err := s.completeUnsuccessfulDispatch(context.WithoutCancel(ctx), report, token); err != nil {
			return false, fmt.Sprintf("fix session failed and its one-pass stop could not be recorded: %v (log: %s)", err, logPath)
		}
		// Keep the worktree AND name the log: a failed session is the one whose
		// state somebody needs to look at.
		return false, fmt.Sprintf("fix session failed: %v (log: %s)", runErr, logPath)
	}

	// Keep a worktree the session left work in. Removing it discards fixes that
	// were made but not pushed, which is the one outcome a fix session must
	// never suffer.
	if kept, why := sessionWork(context.WithoutCancel(ctx), co, report.Head); kept {
		if err := s.completeUnsuccessfulDispatch(context.WithoutCancel(ctx), report, token); err != nil {
			why += "; its one-pass stop could not be recorded: " + err.Error()
		}
		if s.log != nil {
			s.log.Printf("watch: keeping %s — %s", co.Dir, why)
		}
		return false, why
	}
	readyHead, err := co.Git(context.WithoutCancel(ctx), "rev-parse", "HEAD")
	if err != nil {
		_ = s.completeUnsuccessfulDispatch(context.WithoutCancel(ctx), report, token)
		return false, "the successful session's exact HEAD could not be read"
	}
	readyHead = strings.TrimSpace(readyHead)
	verificationPending := false
	if finalizingOnePass {
		// Prefer the base current after the finalizer when the resulting head
		// actually contains it. If GitHub advances again after the agent has
		// finished, retain the pre-launch base this checkout can still prove and
		// let the post-fix base verification gate wait rather than terminalizing the sole
		// successful fixer.
		refreshed, refreshErr := s.gh.GetPull(context.WithoutCancel(ctx), report.Repo, report.PR)
		refreshedBase := strings.TrimSpace(refreshed.Base.SHA)
		if refreshErr == nil && refreshedBase != "" {
			if refreshedBase != readyBase {
				if _, err := co.Git(context.WithoutCancel(ctx), "merge-base", "--is-ancestor", refreshedBase, readyHead); err == nil {
					readyBase = refreshedBase
				} else {
					verificationPending = true
				}
			}
		} else {
			// The fixer succeeded. A transient REST failure must not turn that
			// sole allowed session into a terminal failed attempt or launch a
			// second fixer. Preserve the pre-launch base after proving ancestry;
			// the merge gate retries GitHub and accepts it only if still current.
			verificationPending = true
		}
		if _, err := co.Git(context.WithoutCancel(ctx), "merge-base", "--is-ancestor", readyBase, readyHead); err != nil {
			_ = s.completeUnsuccessfulDispatch(context.WithoutCancel(ctx), report, token)
			return false, "the one-pass finalizer did not integrate the exact base " + shortSHA(readyBase)
		}
	}
	onePass, err := s.completeSuccessfulDispatchWithVerification(
		context.WithoutCancel(ctx), report, token, readyHead, readyBase, verificationPending,
	)
	if err != nil {
		// Keep the checkout as an audit trail. The branch already holds the fix,
		// but without a durable exact-head hand-off crq must not merge it.
		return false, "could not release the fixed head for merge: " + err.Error()
	}
	_ = co.Remove(context.WithoutCancel(ctx))
	if onePass {
		eligible, merged, reason, mergeErr := s.mergeOnePassReady(
			context.WithoutCancel(ctx), report.Repo, report.PR,
		)
		switch {
		case mergeErr != nil:
			if s.log != nil {
				s.log.Printf("watch: immediate merge check for %s#%d failed: %v", report.Repo, report.PR, mergeErr)
			}
			if opts.Once {
				return false, "immediate one-pass merge failed: " + mergeErr.Error()
			}
		case eligible && s.log != nil:
			s.log.Printf("watch: immediate merge check for %s#%d: merged=%t reason=%s", report.Repo, report.PR, merged, reason)
		}
	}
	return true, ""
}

func setCommandEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func autofixFindings(findings []dialect.Finding, allowed map[string]bool) []dialect.Finding {
	if len(allowed) == 0 {
		return findings
	}
	out := make([]dialect.Finding, 0, len(findings))
	for _, finding := range findings {
		// This is workflow control, not reviewer feedback. Filtering it would
		// leave a clean one-pass campaign with no finalizer and no merge.
		if finding.Source == onePassFinalizeSource {
			out = append(out, finding)
			continue
		}
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "unknown"
		}
		if allowed[severity] {
			out = append(out, finding)
		}
	}
	return out
}

const clarificationMarker = "CRQ_NEEDS_CLARIFICATION:"

func appendAutofixPolicy(prompt string, severities map[string]bool, askMode string) string {
	allowed := sortedKeys(severities)
	if len(allowed) == 0 {
		allowed = dialect.KnownSeverities()
	}
	instruction := "Autofix policy:\n- Work only on findings passed to this session (configured severities: " +
		strings.Join(allowed, ", ") + ").\n"
	switch askMode {
	case "ambiguous":
		instruction += "- Stop at the first meaningful ambiguity: if multiple reasonable solutions would change behavior differently, do not guess.\n"
	case "uncertain":
		instruction += "- Stop when confidence is low that the chosen solution is the intended one; do not guess through unclear requirements.\n"
	default:
		instruction += "- Use best judgment and stop for clarification only when missing information makes a safe fix impossible.\n"
	}
	instruction += "- To ask, make no further edits and end your response with exactly `" +
		clarificationMarker + " <one concrete question>`."
	if strings.TrimSpace(prompt) == "" {
		return instruction
	}
	return strings.TrimSpace(prompt) + "\n\n" + instruction
}

func clarificationFromLog(body string) string {
	var response string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	for scanner.Scan() {
		var event struct {
			Type   string `json:"type"`
			Result string `json:"result"`
			Item   struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch {
		case event.Type == "result":
			// Claude's stream-json terminal event.
			response = event.Result
		case event.Type == "item.completed" && event.Item.Type == "agent_message":
			// Codex's --json terminal assistant message.
			response = event.Item.Text
		}
	}

	response = strings.TrimSpace(response)
	if response == "" {
		return ""
	}
	line := response
	if start := strings.LastIndexByte(response, '\n'); start >= 0 {
		line = response[start+1:]
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, clarificationMarker) {
		return ""
	}
	question := strings.TrimSpace(strings.TrimPrefix(line, clarificationMarker))
	if len(question) > 500 {
		question = question[:500]
	}
	return strings.TrimSpace(question)
}

// sessionWork reports whether this checkout holds work that exists nowhere else,
// and says what it is.
//
// A clean working tree is not proof that the session's fixes landed: it also
// commits, and a commit it did not push lives only here. So anything that cannot
// be established — an unreadable tree, an unconfirmable push — counts as work
// worth keeping. The worktree is pruned by age either way; a lost fix is not
// recoverable at all.
func sessionWork(ctx context.Context, co workspace.Checkout, head string) (bool, string) {
	dirty, err := co.Git(ctx, "status", "--porcelain")
	if err != nil {
		return true, "its working tree could not be read"
	}
	if strings.TrimSpace(dirty) != "" {
		return true, "the session left uncommitted work"
	}
	local, err := co.Git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return true, "its HEAD could not be read"
	}
	if head == "" || strings.HasPrefix(local, head) {
		return false, "" // still on the reviewed head: nothing was committed here
	}
	// The session committed. Confirm it reached THIS pull request. A commit on
	// some other remote branch is not a successful push and must not make us
	// discard the only checkout holding the fix.
	if co.PR <= 0 {
		return true, "the session committed, but its pull request ref is unknown"
	}
	pullRef := fmt.Sprintf("refs/remotes/origin/pr/%d", co.PR)
	if _, err := co.Git(ctx, "fetch", "origin",
		fmt.Sprintf("+refs/pull/%d/head:%s", co.PR, pullRef)); err != nil {
		return true, "the session committed, and the pull request push could not be confirmed"
	}
	if _, err := co.Git(ctx, "merge-base", "--is-ancestor", local, pullRef); err != nil {
		return true, "the session committed work that did not reach the pull request"
	}
	return false, ""
}

// writeFindings puts the findings somewhere the fix session can read and the
// repository cannot accidentally commit.
func (s *Service) writeFindings(report NextReport) (string, error) {
	body, err := json.MarshalIndent(report.Findings, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", fmt.Sprintf("crq-findings-%d-*.json", report.PR))
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return "", err
	}
	return file.Name(), nil
}

// claimDispatch claims this PR's round for the session about to run.
//
// The third return says a refusal is the queue working as designed — a session
// already running for this PR, or a head that has spent its attempt budget —
// rather than evidence about whether fix sessions can start at all.
func (s *Service) claimDispatch(ctx context.Context, report NextReport, token string, maxAttempts int) (bool, string, bool) {
	ok, why, byDesign, _ := s.claimDispatchModels(ctx, &report, token, maxAttempts)
	return ok, why, byDesign
}

func (s *Service) claimDispatchModels(
	ctx context.Context,
	report *NextReport,
	token string,
	fallbackMaxAttempts int,
) (bool, string, bool, string) {
	reason, byDesign := "", false
	var seen string
	var selectedModel string
	var selectedFindings []dialect.Finding
	st, err := s.store.Update(ctx, func(st *State) error {
		// The pass-level snapshot is only an optimization. The switch is an
		// operator safety gate, so enforce it in the same CAS that grants the
		// claim; a concurrent off or a failed earlier Load must fail closed.
		if !st.AutofixEnabled(report.Repo) {
			reason, byDesign = "fix sessions are disabled for this repository", true
			return ErrNoChange
		}
		// Enrollment is the same kind of gate and gets the same treatment. The
		// pass reads it once and can be minutes old, and its load is best-effort —
		// so "Stop reviewing" could be clicked, and a coding-agent session still
		// start against the repository, on the strength of a snapshot taken before
		// the click or of no snapshot at all.
		if !s.reviewsRepo(*st, report.Repo) {
			reason, byDesign = "crq is not reviewing this repository", true
			return ErrNoChange
		}
		if report.Fork && !s.cfgFor(*st, report.Repo).DispatchForks {
			reason, byDesign = "the head branch is a fork and current solver policy forbids fixing it", true
			return ErrNoChange
		}
		if claim, ok := st.WorkClaim(report.Repo, report.PR, s.clock()); ok {
			reason, byDesign = "interactive work is claimed by "+claim.By, true
			return ErrNoChange
		}
		round := st.Round(report.Repo, report.PR)
		// What this attempt actually read, recorded before anything acts on it.
		// Three sessions once ran on one PR at one head while the round showed no
		// claim at all, and no test reproduces it: the tests assert the refusal
		// and get it. So the next occurrence has to explain itself from the log —
		// which round each grant saw, and what its claim said at the time.
		seen = describeDispatchClaim(round, st, *report, s.clock())
		// Somebody is fixing an earlier head of this PR right now, and is entitled
		// to finish: a session's own push moves the head, so this is what a
		// successful session looks like from the outside. Superseding its round
		// here would start a second session against the work it is still landing.
		//
		// The claim is looked for in the archive too. Next enqueues before this
		// runs, and enqueueing at a moved head supersedes — which archives the
		// round the session is holding and leaves a fresh, claim-less one in its
		// place. Reading only that one saw no session at all, which is exactly the
		// case this guard exists for.
		if (round != nil && round.Head != report.Head && round.DispatchHeld(s.clock())) ||
			st.ArchivedDispatchHeld(report.Repo, report.PR, s.clock()) {
			reason, byDesign = "another watcher is already fixing this pull request", true
			return ErrNoChange
		}
		if round == nil || round.Head != report.Head {
			// Findings on a head the queue is not tracking: a review somebody
			// triggered by hand, feedback that predates autofix, or a head that
			// moved on while the previous round still stood. `Next` returns `fix`
			// before Enqueue in that case — deliberately, so no second review is
			// bought for a head whose findings are already in hand — so nothing
			// else supersedes the stale round either, and every pass took the same
			// path and refused these dispatches forever.
			var err error
			if round == nil {
				round, err = st.NewRound(report.Repo, report.PR, report.Head, s.clock())
			} else {
				round, err = st.Supersede(report.Repo, report.PR, report.Head, s.clock())
			}
			if err != nil {
				return err
			}
			// Record the head as reviewed only when the METERED primary
			// demonstrably reviewed it. That is the "this head was reviewed"
			// marker, so the round is NOT fire-eligible and no review is bought
			// here either.
			//
			// Findings alone are not that evidence, whoever left them. A Codex or
			// Bugbot finding on the current head says a CO-reviewer looked, and
			// they spend no account quota — dedupe on that and the round is
			// completed for a primary review nobody ever asked for, which Pump
			// can then never fire while the session waits for it. Feedback
			// CARRIED from an older commit proves nothing about this head either.
			// Both cases leave the round adoptable instead, so the queue can
			// still buy the review that is actually missing.
			//
			// Who the primary IS comes from the state this write lands on, not
			// from this watcher's startup env: a fleet that changed CRQ_BOT would
			// otherwise have its new primary's review read as a co-reviewer's, and
			// the head marked unreviewed for a review it already has.
			if reviewedByConfiguredBot(report.ReviewedBy, s.cfgFor(*st, report.Repo).Bot) &&
				len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 {
				if err := round.Dedupe(s.clock()); err != nil {
					return err
				}
				round.Note = "reviewed outside the queue; adopted to fix its findings"
			} else if len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 {
				round.Note = "adopted to fix co-reviewer findings; the primary has not reviewed this head"
			} else {
				round.Note = "adopted to fix feedback carried from an earlier head"
			}
		}
		// The operator can replace the severity policy, model ranking, or attempt
		// limit after this pass's initial read. Resolve all three from the state
		// revision this CAS will write.
		claimCfg := s.cfgFor(*st, report.Repo)
		solver := st.EffectiveSolver(report.Repo)
		finalizer := hasOnePassFinalizer(report.Findings)
		if claimCfg.OnePass && !finalizer {
			reason, byDesign = "a one-pass campaign became active after this ordinary fix report was observed; recompute the pull request", true
			return ErrNoChange
		}
		if finalizer &&
			(!claimCfg.OnePass || report.onePassCampaign == "" ||
				solver.OnePassCampaign != report.onePassCampaign) {
			reason, byDesign = "the campaign that produced this one-pass finalizer is no longer active", true
			return ErrNoChange
		}
		// A one-pass campaign can be enabled while an ordinary fixer is still
		// running. Installing the campaign binary stops that old process, but its
		// stale attempt counter remains on the round. With the campaign limit set
		// to one, counting that pre-campaign attempt would prevent the campaign's
		// one actual finalizer from ever starting. Once the old lease is no
		// longer live, discard only attempts older than the repository setting
		// that enabled this campaign. A real one-pass attempt has progress state
		// and is never reset here.
		_, onePassStarted := st.OnePassProgressFor(report.Repo, report.PR)
		if claimCfg.OnePass && !onePassStarted && round.Dispatch != nil &&
			!round.DispatchHeld(s.clock()) && solver.UpdatedAt != nil &&
			round.Dispatch.At.Before(solver.UpdatedAt.UTC()) {
			round.Dispatch = nil
		}
		selectedFindings = autofixFindings(report.Findings, claimCfg.FixSeverities)
		if len(report.Findings) > 0 && len(selectedFindings) == 0 {
			reason, byDesign = "current autofix policy excludes every open finding", true
			return ErrNoChange
		}
		models := append([]string(nil), claimCfg.FixModels...)
		if len(models) == 0 && claimCfg.FixModel != "" {
			models = []string{claimCfg.FixModel}
		}
		maxAttempts := recordedMaxAttemptsIn(*st, report.Repo, fallbackMaxAttempts)
		if finalizer {
			maxAttempts = 1
		}
		ok, why := round.ClaimDispatchModels(s.cfg.Host, token, s.clock(), maxAttempts, models)
		if !ok {
			reason, byDesign = why, true
			return ErrNoChange
		}
		round.Dispatch.OnePass = finalizer
		if finalizer {
			round.Dispatch.OnePassCampaign = report.onePassCampaign
		}
		selectedModel = round.Dispatch.Model
		report.dispatchUntil = round.Dispatch.Heartbeat.Add(DispatchTTL)
		st.RememberDispatch(report.Repo, report.PR, *round.Dispatch)
		st.PutRound(*round)
		return nil
	})
	if err != nil {
		return false, err.Error(), false, ""
	}
	if reason == "" {
		// Update may spend long enough retrying transport or CAS operations after
		// the mutation commits to leave less than one heartbeat interval on its
		// lease. Renew before handing it to the session, without spending another
		// attempt: otherwise beatDispatch's first tick arrives after local expiry.
		for {
			now := s.clock()
			until, live := st.LiveDispatchUntil(report.Repo, report.PR, now)
			if live && st.OwnsLiveDispatch(report.Repo, report.PR, token, now) &&
				until.After(now.Add(DispatchTTL/3)) {
				report.dispatchUntil = until
				break
			}
			var updated, taken, gone bool
			var refreshedAt time.Time
			st, err = s.store.Update(ctx, func(st *State) error {
				updated, taken, gone = false, false, false
				refreshedAt = s.clock()
				updated, taken, gone = refreshDispatch(st, *report, token, refreshedAt)
				if !updated {
					return ErrNoChange
				}
				return nil
			})
			if err != nil {
				return false, err.Error(), false, ""
			}
			if taken {
				return false, "another watcher is already fixing this pull request", true, ""
			}
			if gone || !updated {
				return false, "the committed dispatch claim is no longer owned by this session", true, ""
			}
			report.dispatchUntil = refreshedAt.Add(DispatchTTL)
		}
	}
	if reason == "" {
		// The session receives exactly the finding set selected by the same
		// state revision that granted its claim.
		report.Findings = selectedFindings
	}
	if s.log != nil {
		outcome := "granted"
		if reason != "" {
			outcome = "refused (" + reason + ")"
		}
		s.log.Printf("dispatch claim %s for %s#%d@%s token=%s: %s",
			outcome, report.Repo, report.PR, report.Head, token, seen)
	}
	return reason == "", reason, byDesign, selectedModel
}

// describeDispatchClaim renders what a claim attempt read, for the log line
// above. Seq identifies the round object itself, so two grants naming one Seq
// mean a claim was lost rather than a round replaced underneath them.
func describeDispatchClaim(round *Round, st *State, report NextReport, now time.Time) string {
	if round == nil {
		return fmt.Sprintf("no round; archived-claim=%t", st.ArchivedDispatchHeld(report.Repo, report.PR, now))
	}
	claim := "none"
	if d := round.Dispatch; d != nil {
		claim = fmt.Sprintf("token=%s host=%s attempts=%d beat=%s",
			d.Token, d.Host, d.Attempts, d.Heartbeat.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("round seq=%d head=%s phase=%s held=%t claim=%s; archived-claim=%t",
		round.Seq, round.Head, round.Phase, round.DispatchHeld(now), claim,
		st.ArchivedDispatchHeld(report.Repo, report.PR, now))
}

// releaseDispatch frees the round. attempted=false also gives the attempt back,
// for a claim that never reached a running session — a clone that failed or a
// command that could not start did not use up the per-head budget.
func (s *Service) releaseDispatch(ctx context.Context, report NextReport, token string, attempted bool) {
	_, _ = s.store.Update(ctx, func(st *State) error {
		// This session's own push may have superseded the round it was holding,
		// archiving the claim with it. Released here too, or a finished session
		// keeps the next dispatch for this PR out until the claim's TTL expires.
		archived := st.ReleaseArchivedDispatch(report.Repo, report.PR, token)
		round := st.Round(report.Repo, report.PR)
		if round == nil || !round.ReleaseDispatch(token) {
			if archived {
				return nil
			}
			return ErrNoChange
		}
		if !attempted && round.Dispatch != nil && round.Dispatch.Attempts > 0 {
			round.Dispatch.Attempts--
			round.Dispatch.AttemptResetAt = nil
		}
		st.PutRound(*round)
		return nil
	})
}

func (s *Service) stopDispatchForClarification(
	ctx context.Context,
	report NextReport,
	token string,
	question string,
) error {
	_, err := s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		if round == nil || !round.MarkDispatchClarification(token, question) {
			return ErrNoChange
		}
		if claim, ok := st.Dispatches[QueueKey(report.Repo, report.PR)]; ok && claim.Token == token {
			delete(st.Dispatches, QueueKey(report.Repo, report.PR))
		}
		st.PutRound(*round)
		return nil
	})
	return err
}

// releaseDispatchUnavailable refunds an attempt that never reached the code
// problem: the provider/model could not serve it. The selected model is parked
// until its retry time while the already-advanced cursor makes the next claim
// choose a fallback.
func (s *Service) releaseDispatchUnavailable(
	ctx context.Context,
	report NextReport,
	token string,
	failure dialect.AgentFailure,
) {
	_, _ = s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		current := round != nil && round.MarkDispatchUnavailable(token, failure.RetryAt, failure.Reason)
		archived := false
		if !current {
			archived = st.MarkArchivedDispatchUnavailable(
				report.Repo, report.PR, token, failure.RetryAt, failure.Reason,
			)
		}
		if !current && !archived {
			return ErrNoChange
		}
		if current {
			if !round.ReleaseDispatch(token) {
				return ErrNoChange
			}
			st.PutRound(*round)
		}
		st.ReleaseArchivedDispatch(report.Repo, report.PR, token)
		return nil
	})
}

func (s *Service) claimedDispatchModel(
	ctx context.Context,
	report NextReport,
	token string,
) (string, bool) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return "", false
	}
	if round := st.Round(report.Repo, report.PR); round != nil &&
		round.Dispatch != nil && round.Dispatch.Token == token {
		return round.Dispatch.Model, true
	}
	if claim, ok := st.Dispatches[QueueKey(report.Repo, report.PR)]; ok && claim.Token == token {
		return claim.Model, true
	}
	return "", false
}

func readLogTail(path string, limit int64) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	body := make([]byte, info.Size()-start)
	n, _ := file.ReadAt(body, start)
	return body[:n]
}

// beatDispatch refreshes the claim while the session runs, so a session that
// outlives the TTL keeps its round and a crashed watcher's does not. It reports
// whether the claim was lost: losing it means another watcher took the round
// over, so stop() ends this session rather than let two write one worktree.
func (s *Service) beatDispatch(ctx context.Context, report NextReport, token string, stop func()) func() bool {
	var lost atomic.Bool
	markLost := func() {
		if lost.CompareAndSwap(false, true) {
			stop()
		}
	}
	leaseUntil := report.dispatchUntil
	if leaseUntil.IsZero() {
		leaseUntil = s.clock().Add(DispatchTTL)
	}
	expiry := time.AfterFunc(max(leaseUntil.Sub(s.clock()), 0), markLost)
	go func() {
		defer expiry.Stop()
		ticker := time.NewTicker(DispatchTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var updated, taken, gone bool
				var refreshedAt time.Time
				if _, err := s.store.Update(ctx, func(st *State) error {
					// Reset per ATTEMPT, not per tick: Update re-runs this
					// closure on a CAS conflict, and a verdict left over from
					// an attempt that lost would be read as this one's.
					updated, taken, gone = false, false, false
					refreshedAt = s.clock()
					updated, taken, gone = refreshDispatch(st, report, token, refreshedAt)
					if !updated {
						return ErrNoChange
					}
					return nil
				}); err != nil && !errors.Is(err, ErrNoChange) {
					// The local expiry timer stops the session if writes remain
					// unavailable for the rest of this lease.
					continue
				}
				if taken {
					// Somebody else is running a session for this round. Two in
					// one worktree is worse than none.
					markLost()
					return
				}
				// The token is nowhere in current or archived state. There is
				// nothing left to refresh; let the session finish.
				if gone {
					return
				}
				if updated {
					leaseUntil = refreshedAt.Add(DispatchTTL)
					expiry.Reset(max(leaseUntil.Sub(s.clock()), 0))
				}
			}
		}
	}()
	return lost.Load
}

func refreshDispatch(st *State, report NextReport, token string, now time.Time) (updated, taken, gone bool) {
	// An expired unattended claim may have been replaced by an interactive
	// work claim while this watcher could not write. The work claim won that
	// CAS race, so the old token must not restore itself when connectivity
	// returns.
	if _, ok := st.WorkClaim(report.Repo, report.PR, now); ok {
		return false, true, false
	}

	round := st.Round(report.Repo, report.PR)
	// A round for ANOTHER head is not this round: superseding is what this
	// session's own push does, and the fresh round's claim belongs to whoever
	// takes the new head. Reading its absence as theft would kill this session
	// between pushing and resolving every time it succeeded.
	if round == nil || round.Head != report.Head {
		ok, byOther := st.HeartbeatArchivedDispatch(report.Repo, report.PR, token, now)
		taken = byOther
		if !ok {
			// A current live claim for the replacement head is also proof that
			// this session no longer owns the PR.
			if round != nil && round.DispatchHeld(now) {
				taken = true
			}
			gone = !taken
		}
		return ok, taken, gone
	}
	ok, byOther := round.HeartbeatDispatch(token, now)
	taken = byOther
	if !ok {
		return false, taken, false
	}
	st.RememberDispatch(report.Repo, report.PR, *round.Dispatch)
	st.PutRound(*round)
	return true, false, false
}

func openPullQuery() url.Values {
	q := url.Values{}
	q.Set("state", "open")
	return q
}

// dispatchPool bounds how many fix sessions run at once.
//
// Non-blocking on purpose: when every slot is busy the PR is left for the next
// pass rather than stalling the decision loop behind a session. Queuing here
// would recreate the problem it exists to solve.
type dispatchPool struct {
	slots chan struct{}
	wg    sync.WaitGroup
}

// newDispatchPool bounds concurrent sessions. size <= 0 means no bound, which is
// the default: this is a resource valve, not a queue.
func newDispatchPool(size int) *dispatchPool {
	if size <= 0 {
		return &dispatchPool{}
	}
	return &dispatchPool{slots: make(chan struct{}, size)}
}

// acquire takes a slot, reporting why not when every one is busy. It is separate
// from run so the caller can claim the round — and give the slot back if the
// claim fails — before anything is said to have been dispatched.
func (p *dispatchPool) acquire() (bool, string) {
	if p.slots == nil {
		return true, ""
	}
	select {
	case p.slots <- struct{}{}:
		return true, ""
	default:
		// Only reachable when an operator has set a cap. Unfixed findings
		// waiting on a slot is the shape of problem this whole command
		// exists to remove, so say so rather than logging it as routine.
		return false, "at the configured dispatch cap (CRQ_DISPATCH_CONCURRENCY); this PR waits"
	}
}

// acquireContext waits for capacity in a one-shot run. Unlike a long-lived
// watcher, --once has no later pass in which to revisit a skipped PR.
func (p *dispatchPool) acquireContext(ctx context.Context) (bool, string) {
	if p.slots == nil {
		return true, ""
	}
	if err := ctx.Err(); err != nil {
		return false, err.Error()
	}
	select {
	case p.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			p.release()
			return false, err.Error()
		}
		return true, ""
	case <-ctx.Done():
		return false, ctx.Err().Error()
	}
}

// release gives an acquired slot back without running anything.
func (p *dispatchPool) release() {
	if p.slots != nil {
		<-p.slots
	}
}

// run runs fn in an already-acquired slot.
func (p *dispatchPool) run(fn func()) {
	p.wg.Add(1)
	go func() {
		defer func() {
			p.release()
			p.wg.Done()
		}()
		fn()
	}()
}

// start acquires a slot and runs fn in it, reporting why not when every slot is
// busy.
func (p *dispatchPool) start(fn func()) (bool, string) {
	if ok, why := p.acquire(); !ok {
		return false, why
	}
	p.run(fn)
	return true, ""
}

// wait blocks until every running session has finished, so a --once run does not
// return while its sessions are still writing.
func (p *dispatchPool) wait() { p.wg.Wait() }

// sessionLog opens the file a fix session's output goes to, and prunes the ones
// nobody is going to read.
func (s *Service) sessionLog(ctx context.Context, report NextReport) (string, *os.File, error) {
	dir, err := s.workspace(ctx).LogDir(report.Repo)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	pruneSessionLogs(dir, report.PR)
	path := filepath.Join(dir, fmt.Sprintf("%d-%s-%s.log",
		report.PR, shortSHA(report.Head), s.clock().UTC().Format("20060102T150405")))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, err
	}
	return path, file, nil
}

// pruneSessionLogs keeps the most recent few logs per PR. Older ones describe a
// head nobody is working on any more.
//
// It runs BEFORE the new log is created, so it leaves room for it: keeping the
// full bound here would settle at one more file per PR than the bound says.
func pruneSessionLogs(dir string, pr int) {
	const keep = 5
	room := keep - 1
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("%d-", pr)
	var mine []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".log") {
			mine = append(mine, e.Name())
		}
	}
	if len(mine) <= room {
		return
	}
	sort.Slice(mine, func(i, j int) bool {
		// Names are <pr>-<head>-<timestamp>.log. Comparing the whole name orders
		// primarily by head SHA and can delete a recent failure while retaining
		// an old log. The fixed-width UTC timestamp itself sorts chronologically.
		left, right := sessionLogTimestamp(mine[i]), sessionLogTimestamp(mine[j])
		if left == right {
			return mine[i] < mine[j]
		}
		return left < right
	})
	for _, name := range mine[:len(mine)-room] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func sessionLogTimestamp(name string) string {
	name = strings.TrimSuffix(name, ".log")
	if at := strings.LastIndexByte(name, '-'); at >= 0 {
		return name[at+1:]
	}
	return ""
}

// retireClosedRounds abandons every waiting round of repo whose pull request is
// not in the open set the pass just listed.
//
// It costs nothing extra: the list is the one watchPass already fetched, and it
// is the same evidence a per-round `pullHead` would gather one PR at a time.
// Rounds that are fired or reviewing are left alone — those are answered by
// Progress, which has the round's own observation to reason from.
func (s *Service) retireClosedRounds(ctx context.Context, repo string, open map[int]bool) error {
	key := NormalizeRepo(repo)
	var stale []Round
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	for _, r := range st.Rounds {
		if NormalizeRepo(r.Repo) != key || open[r.PR] {
			continue
		}
		if r.Phase == PhaseQueued || r.Phase == PhaseAwaitingRetry {
			stale = append(stale, r)
		}
	}
	// Ordered, so a pass that is interrupted has done a prefix of the work
	// rather than an arbitrary subset.
	sort.Slice(stale, func(i, j int) bool { return stale[i].PR < stale[j].PR })
	for _, r := range stale {
		if _, err := s.abandonRound(ctx, r, "pr closed", "skipped"); err != nil {
			return err
		}
		if s.log != nil {
			s.log.Printf("watch: %s#%d left the queue: pr closed", r.Repo, r.PR)
		}
	}
	if s.cfg.DryRun {
		return nil
	}
	var invalidated []int
	updated, err := s.store.Update(ctx, func(st *State) error {
		invalidated = st.InvalidateClosedOnePass(repo, open, s.cfg.Host, s.clock())
		if len(invalidated) == 0 {
			return ErrNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return err
	}
	if err == nil {
		s.sync(ctx, updated)
	}
	for _, pr := range invalidated {
		if s.log != nil {
			s.log.Printf("watch: %s#%d retired its one-pass merge hand-off: pr closed", repo, pr)
		}
	}
	return nil
}

// noteSessionDetail records what a running session is working on, for the
// reader. Best-effort: this is display bookkeeping, and failing a fix session
// over it would trade something that matters for something that does not.
func (s *Service) noteSessionDetail(ctx context.Context, report NextReport, token, logPath string, findings int) {
	_, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(report.Repo, report.PR)
		if r == nil || r.Dispatch == nil || r.Dispatch.Token != token {
			return ErrNoChange
		}
		mirror := st.Dispatches[QueueKey(report.Repo, report.PR)]
		if r.Dispatch.Log == logPath && r.Dispatch.Findings == findings &&
			mirror.Token == token && mirror.Log == logPath && mirror.Findings == findings {
			return ErrNoChange
		}
		r.Dispatch.Log, r.Dispatch.Findings = logPath, findings
		st.PutRound(*r)
		// The session list reads the MIRRORED claim, not the round, so the same
		// write has to reach both. Left to the heartbeat, these fields appeared
		// a third of a TTL late — and never at all for a session that finished
		// sooner, which is most of them.
		st.RememberDispatch(report.Repo, report.PR, *r.Dispatch)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) && s.log != nil {
		s.log.Printf("warning: recording session detail for %s#%d: %v", report.Repo, report.PR, err)
	}
}
