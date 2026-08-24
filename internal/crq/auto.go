package crq

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

type AutoOptions struct {
	Once        bool
	Incremental bool
}

// errLostLeadership aborts a pass when a standby stole the lease mid-scan.
var errLostLeadership = errors.New("lost autoreview leadership mid-pass")

func (s *Service) AutoReview(ctx context.Context, opts AutoOptions) error {
	// The same identity capabilities are recorded under, so LaggingWriters can
	// ask whether the process holding the lease understands what it is deciding
	// from. Spelling it out here again would let the two drift apart.
	owner := s.cfg.WriterID()
	token := randomToken()
	// Report what this machine can reach before doing anything else, and again
	// as it goes: a fleet's tool inventory is only useful if it is a fleet's,
	// and a daemon is the only thing that knows the PATH its service runs with.
	s.ReportHost(ctx, "autoreview")
	// Pacing is a fleet setting, so it is resolved from the state this loop
	// already reads rather than frozen at startup: a poll interval the dashboard
	// records and then reports as the effective fleet value has to actually pace
	// the daemon reading it. This host's env is the answer until the first read
	// lands, and whenever one fails.
	poll := s.cfg.AutoReviewPoll
	for {
		state, held, err := s.acquireLeader(ctx, owner, token)
		if err != nil {
			if _, ok := ghapi.ThrottleWait(err); ok {
				if cont, serr := s.sleepThrottle(ctx, opts, poll, "leader", err); serr != nil || !cont {
					return serr
				}
				continue
			}
			return err
		}
		// Positive only. A record of "0s" would otherwise turn every daemon in
		// the fleet into a spin loop from one save, which is a far worse thing
		// for one setting to be able to do than to be ignored.
		if resolved := s.cfg.WithFleet(state.Fleet).AutoReviewPoll; resolved > 0 {
			poll = resolved
		}
		if !held {
			if opts.Once {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
				continue
			}
		}
		passErr := s.autoReviewPass(ctx, opts, owner, token)
		var passFailure error
		if errors.Is(passErr, errLostLeadership) {
			if s.log != nil {
				s.log.Printf("autoreview: lost leadership mid-pass; standing by")
			}
		} else {
			// A throttled pass means the following API calls will keep failing —
			// sleep out the window instead of immediately pumping and re-scanning,
			// which would just hammer the quota. Skip Pump entirely in that case.
			if _, ok := ghapi.ThrottleWait(passErr); ok {
				if cont, serr := s.sleepThrottle(ctx, opts, poll, "pass", passErr); serr != nil || !cont {
					if opts.Once {
						return s.finishAutoReview(ctx, token, serr)
					}
					return serr
				}
				continue
			}
			if ghapi.IsServerUnreachable(passErr) {
				// Not a warning to log and poll past: without the control plane
				// nothing below can run, and looping keeps the service "healthy"
				// while it does nothing. Exit and let the service manager retry.
				//
				// Hand back the lease on the way out, daemon or one-shot. A lease
				// is keyed by TOKEN, and a restart draws a new one, so an exit
				// that keeps it makes the recovered process stand by behind its
				// own dead self until the TTL runs out. The attempt is bounded
				// and best-effort: it goes through the same server, so while that
				// server is down it fails, and the TTL is the backstop it always
				// was.
				return s.finishAutoReview(ctx, token, passErr)
			}
			if passErr != nil && s.log != nil {
				s.log.Printf("warning: autoreview pass failed: %v", passErr)
			}
			passFailure = passErr
			pumped, err := s.Pump(ctx)
			// A round that just progressed is the moment its trigger comments
			// stop being needed, so tidying here costs one observation on a PR
			// crq was already looking at rather than a sweep of the fleet.
			if err == nil {
				err = s.tidyAfterPump(ctx, state, pumped)
			}
			if err != nil {
				if _, ok := ghapi.ThrottleWait(err); ok {
					if cont, serr := s.sleepThrottle(ctx, opts, poll, "pump", err); serr != nil || !cont {
						if opts.Once {
							return s.finishAutoReview(ctx, token, serr)
						}
						return serr
					}
					continue
				}
				if ghapi.IsServerUnreachable(err) {
					return s.finishAutoReview(ctx, token, err)
				}
				if s.log != nil {
					s.log.Printf("warning: autoreview pump failed: %v", err)
				}
				if passFailure == nil {
					passFailure = err
				}
			}
		}
		if opts.Once {
			// A one-shot run must surface a real (non-throttle) scan/pump failure —
			// e.g. a permission or owner-lookup error — so cron/CI doesn't see success
			// when nothing was scanned or enqueued. The daemon keeps going (logged).
			return s.finishAutoReview(ctx, token, passFailure)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// finishAutoReview hands the leader lease back on the way out and returns the
// error that ended the run. The release is best-effort — it writes through the
// same server whose loss usually causes the exit — and bounded, on a context
// detached from the caller's so a cancelled run still gets its attempt.
func (s *Service) finishAutoReview(ctx context.Context, token string, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if rerr := s.releaseLeader(ctx, token); err == nil && rerr != nil {
		return rerr
	}
	return err
}

// throttleBackoff bounds how long the autoreview daemon sleeps when GitHub
// throttles it: at least one poll interval, plus a small buffer past the
// reset, capped at an hour so a bogus reset header can't wedge the daemon.
// poll is the caller's resolved interval, so a fleet-recorded one paces the
// backoff too.
func (s *Service) throttleBackoff(wait, poll time.Duration) time.Duration {
	if poll <= 0 {
		poll = s.cfg.AutoReviewPoll
	}
	if wait <= 0 {
		wait = poll
	}
	wait += 5 * time.Second
	if wait < poll {
		wait = poll
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	return wait
}

// sleepThrottle waits out a GitHub throttle window that an autoreview
// leader/pass/pump step hit, using the same bounded backoff as the leader path so
// a throttle pauses the daemon instead of spinning failing API calls. cause must
// be a throttle error (the caller checks ghapi.ThrottleWait first). It returns
// cont=false (nil error) when opts.Once means we should stop after the wait, and a
// non-nil error only when the context is cancelled mid-wait.
func (s *Service) sleepThrottle(ctx context.Context, opts AutoOptions, poll time.Duration, stage string, cause error) (cont bool, err error) {
	wait, _ := ghapi.ThrottleWait(cause)
	wait = s.throttleBackoff(wait, poll)
	if s.log != nil {
		s.log.Printf("autoreview: %s throttled (%v); sleeping %s before next pass", stage, cause, wait.Round(time.Second))
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(wait):
	}
	if opts.Once {
		return false, nil
	}
	return true, nil
}

// acquireLeader claims or extends the lease and hands back the state it read,
// so the caller can resolve the fleet's pacing from a read it already paid for.
func (s *Service) acquireLeader(ctx context.Context, owner, token string) (State, bool, error) {
	state, held, err := s.renewLeader(ctx, owner, token)
	if err != nil {
		return State{}, false, err
	}
	if held {
		s.sync(ctx, state)
	}
	return state, held, nil
}

func (s *Service) releaseLeader(ctx context.Context, token string) error {
	released := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Leader == nil || st.Leader.Token != token {
			return ErrNoChange
		}
		st.Leader = nil
		st.LeaderCapabilities = nil
		released = true
		return nil
	})
	if err != nil {
		return err
	}
	if released {
		s.sync(ctx, state)
	}
	return nil
}

// leaderTTL is how long a lease survives without a heartbeat, resolved the way
// every other fleet setting is: this host's env, then whatever the fleet
// recorded. A lease length the dashboard displays as the fleet's answer has to
// be the one the daemon actually renews against — otherwise saving it changes a
// number on a page and nothing else. Non-positive is ignored, since a recorded
// 0 would expire every lease the instant it was written.
func (s *Service) leaderTTL(st State) time.Duration {
	if resolved := s.cfg.WithFleet(st.Fleet).LeaderTTL; resolved > 0 {
		return resolved
	}
	return s.cfg.LeaderTTL
}

// feedbackWait is how long a fired round is waited on, resolved for the same
// reason and in the same way. The deadline is STAMPED into the round, so the
// startup value did not merely mispace one poll: it outlived the setting on
// every round fired after the fleet shortened or lengthened the wait.
func (s *Service) feedbackWait(st State) time.Duration {
	if resolved := s.cfg.WithFleet(st.Fleet).FeedbackWaitTimeout; resolved > 0 {
		return resolved
	}
	return s.cfg.FeedbackWaitTimeout
}

// renewLeader claims or extends the leader lease via compare-and-swap on the
// state ref. It does not sync the dashboard, so it's cheap enough to call as an
// in-pass heartbeat. held is false when another live lease holder owns it.
func (s *Service) renewLeader(ctx context.Context, owner, token string) (State, bool, error) {
	now := time.Now().UTC()
	held := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Leader != nil && st.Leader.ExpiresAt.After(now) && st.Leader.Token != token {
			held = false
			return ErrNoChange
		}
		st.Leader = &LeaderLease{
			Owner:        owner,
			Token:        token,
			ExpiresAt:    now.Add(s.leaderTTL(*st)),
			UpdatedAt:    now,
			Capabilities: []string{leaderCapabilityHolds},
		}
		st.LeaderCapabilities = &LeaderCapabilityLease{
			Token:        token,
			Capabilities: []string{leaderCapabilityHolds},
		}
		held = true
		return nil
	})
	if err != nil {
		return State{}, false, err
	}
	return state, held, nil
}

func (s *Service) autoReviewPass(ctx context.Context, opts AutoOptions, owner, token string) error {
	// Refreshed each pass. ReportHost writes nothing when nothing changed and
	// the record is still fresh, so this is a probe, not a write.
	s.ReportHost(ctx, "autoreview")
	// Load the queue snapshot once per pass and reuse it across candidates: a
	// git-backed Load is GetRef+GetCommit+GetTree+GetBlob, so reloading it per PR
	// would burn the shared REST quota on a large scan. The heartbeat refreshes it,
	// and enqueueBatch re-checks Contains under CAS, so a slightly stale snapshot
	// during collection is safe.
	//
	// It is also what says which repositories are enrolled, which is why it is
	// read BEFORE the targets are chosen: a repository enrolled from the
	// dashboard has to be searched on the very next pass, not after a restart.
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	fleetCfg := s.cfg.WithFleet(state.Fleet)
	// The scan bound is a fleet setting like the pacing above, resolved from the
	// snapshot this pass just read rather than from the env the process started
	// with. Positive only, for the same reason: a recorded 0 would stop the
	// whole fleet scanning anything, silently.
	maxScan := s.cfg.AutoReviewMaxScan
	if resolved := fleetCfg.AutoReviewMaxScan; resolved > 0 {
		maxScan = resolved
	}
	// Owner-wide searches only when there is no allow-list at all. An allow-list
	// with nothing left active in it scans NOTHING: falling back to CRQ_SCOPE
	// there made every pass walk the organisation's whole open-PR result set for
	// reviewsRepo to reject each row before the scan counter even advanced.
	//
	// A scope-wide host still searches its individually enrolled repositories by
	// name — scanTargets returns the ones no owner in CRQ_SCOPE covers — so the
	// two search shapes are mixed in one pass and each target says which it is.
	targets, scoped := s.scanTargets(state)
	byRepo := func(target string) bool { return !scoped || strings.Contains(target, "/") }
	if scoped {
		targets = append(targets, fleetCfg.Scope...)
	}
	var candidates []queueCandidate
	var titles []queueCandidate
	// The skip rules below are per-repository settings like everything else, and
	// the enrollment preview already answers with the resolved ones. Reading
	// s.cfg here instead made this pass enqueue — and spend the shared allowance
	// on — pull requests the dialog had just promised would be skipped. Memoized
	// per repository: cfgFor re-parses the merged env, and a scan sees the same
	// repository many times over.
	skipCfg := map[string]Config{}
	repoSkips := func(repo string) Config {
		cfg, ok := skipCfg[repo]
		if !ok {
			cfg = s.cfgFor(state, repo)
			skipCfg[repo] = cfg
		}
		return cfg
	}
	lastBeat := time.Now()
	for _, target := range targets {
		// Per-target scan budget so one large scope can't consume the whole budget
		// and starve later scopes when CRQ_SCOPE lists multiple owners/orgs.
		scanned := 0
		// Stream results and stop once the post-filter scan budget is spent, so
		// excluded/gate-repo results can't crowd out in-scope PRs (a fixed pre-filter
		// limit would never reach them) while we still don't over-fetch pages.
		err := s.gh.EachOpenPR(ctx, target, byRepo(target), func(pr ghapi.SearchPR) (bool, error) {
			if scanned >= maxScan {
				return true, nil
			}
			repo := NormalizeRepo(pr.Repo)
			// One question, one answer: env allow/exclude, the gate repository
			// and any enrollment record all resolve in enrollmentOf, so a scope
			// search cannot enqueue what a by-repo search would have skipped.
			if !s.reviewsRepo(state, repo) {
				return false, nil
			}
			skips := repoSkips(repo)
			if skips.SkipAuthors[dialect.NormalizeBotName(strings.ToLower(pr.Author))] {
				return false, nil
			}
			if skips.SkipsReview(pr.Body) {
				return false, nil
			}
			scanned++
			// Heartbeat: renew the lease partway through a long pass so a standby
			// can't steal it mid-scan and cause brief double-leadership (#4).
			// Half of the SAME lease length renewLeader writes, so a fleet that
			// shortens the lease starts beating faster to match.
			if ttl := s.leaderTTL(state); ttl > 0 && time.Since(lastBeat) >= ttl/2 {
				st, held, herr := s.renewLeader(ctx, owner, token)
				if herr != nil {
					return false, herr
				}
				if !held {
					return false, errLostLeadership
				}
				state = st     // reuse the freshly written snapshot for later candidates
				clear(skipCfg) // the memo above answers for the snapshot it was built from
				lastBeat = time.Now()
			}
			// A round that already exists at this head is not a candidate, so it
			// would never reach enqueueBatch and would never learn its title.
			// The scan already has one; recording it here is what stops every
			// existing round reading as a bare number for ever.
			if pr.Title != "" {
				titles = append(titles, queueCandidate{Repo: repo, PR: pr.Number, Title: pr.Title})
			}
			// A one-pass campaign asks whether this PR has ever received its one
			// review, regardless of later heads. The watcher owns the single fixer
			// and exact-head merge after that boundary.
			reviewCfg := repoSkips(repo)
			incremental := opts.Incremental && !reviewCfg.OnePass
			need, head, nerr := s.reviewNeeded(ctx, state, repo, pr.Number, incremental, reviewCfg.OnePass, s.logEnqueue)
			if nerr != nil {
				// A throttle must abort the pass so AutoReview's outer backoff kicks
				// in, instead of scanning the rest of the candidates under the same
				// throttle (and skipping them until a later poll). An unreachable
				// control plane is the same shape: skipping candidate by candidate
				// spends the offline cap once per PR while the pass still reports
				// success, so the outage surfaces minutes late, if at all.
				if abortsPass(nerr) {
					return false, nerr
				}
				if s.log != nil {
					s.log.Printf("warning: autoreview skipped %s#%d: %v", repo, pr.Number, nerr)
				}
				return false, nil
			}
			if need {
				solver := state.EffectiveSolver(repo)
				candidates = append(candidates, queueCandidate{
					Repo: repo, PR: pr.Number, Head: head, Title: pr.Title,
					PolicyChecked: true, OnePass: reviewCfg.OnePass, OnePassCampaign: solver.OnePassCampaign,
				})
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	// Titles first, in one write: they change nothing about scheduling, so a
	// failure here must not stop the enqueue that does.
	s.noteTitles(ctx, titles)
	// One batched write for the whole pass instead of N (#2).
	return s.enqueueBatch(ctx, candidates)
}

// needsReview reports whether an open PR should be enqueued for review, and its
// current head. A round already tracking that exact head (any phase — Pump owns
// re-firing an awaiting_retry round once its window passes) means "no". Only a
// PR with no round at the current head runs the live checks: (incremental) the
// bot's last review commit differs from the head, or (first-review) the bot has
// never reviewed and no review-done marker is present. It reads the caller's
// preloaded state snapshot so a pass doesn't reload git-backed state per candidate.
//
// It announces each "yes" to the log, which is what makes an autoreview pass
// explain the work it queued.
func (s *Service) needsReview(ctx context.Context, state State, repo string, pr int, incremental bool) (bool, string, error) {
	return s.reviewNeeded(ctx, state, repo, pr, incremental, false, s.logEnqueue)
}

// reviewNeeded is the predicate itself, with the announcement injected. The
// enrollment preview asks exactly this question about pull requests it is not
// enqueueing, and an "enqueue …" line from a dialog that wrote nothing is how an
// estimate reads as an action already taken.
func (s *Service) reviewNeeded(ctx context.Context, state State, repo string, pr int, incremental, anyConfigured bool, announce func(repo string, pr int, head, reason string)) (bool, string, error) {
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return false, "", err
	}
	cfg := s.cfgFor(state, repo)
	campaign := state.EffectiveSolver(repo).OnePassCampaign
	reviewScope := map[string]bool{}
	if anyConfigured {
		reviewScope = onePassReviewerScope(state, cfg, repo, campaign)
		if state.OnePassReviewed(repo, pr, campaign) || state.OnePassReviewerAnswered(repo, pr, campaign) {
			return false, head, nil
		}
		// Programmatic/legacy callers can opt into the broad one-pass predicate
		// before a campaign identity has been persisted. Keep their durable
		// co-reviewer evidence meaningful too.
		for login := range reviewScope {
			if state.CoReviewerAnswered(repo, pr, login) {
				return false, head, nil
			}
		}
	}
	if r := state.Round(repo, pr); r != nil && r.Head == head {
		// Unless the round is a marker for reviewers that changed while this PR
		// was closed: it answered for a set that no longer gates the head, and
		// enqueueBatch reopens it. Deciding here rather than falling through to
		// the live checks is also the only correct answer — those read Review
		// objects, which a co-reviewer answering by comment never submits.
		if !r.ReviewersChanged {
			return false, head, nil
		}
		announce(repo, pr, head, "reviewer configuration changed while the pr was closed")
		return true, head, nil
	}
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, "", err
	}
	bot := dialect.NormalizeBotName(cfg.Bot)
	lastBotReview := ""
	reviewedEver := map[string]bool{}
	// Every reviewer this repository gates on, not just the primary. A repo that
	// requires Codex or Bugbot and already has a CodeRabbit review at the head
	// would otherwise never be enqueued, so the reviewer it chose is never asked.
	reviewedHere := map[string]string{}
	for _, review := range reviews {
		login := dialect.NormalizeBotName(review.User.Login)
		if review.CommitID == "" {
			continue
		}
		// Match observe's carrier filter. CodeRabbit posts an empty COMMENTED
		// review object before its real review body and inline comments finish;
		// that transport shell cannot consume a campaign's only review round.
		if cfg.isConfiguredBot(review.User.Login) &&
			strings.EqualFold(review.State, "COMMENTED") && strings.TrimSpace(review.Body) == "" {
			continue
		}
		if strings.EqualFold(review.State, "PENDING") {
			continue
		}
		if login == bot {
			lastBotReview = dialect.ShortOID(review.CommitID)
		}
		reviewedHere[login] = dialect.ShortOID(review.CommitID)
		reviewedEver[login] = true
	}
	if incremental {
		need := lastBotReview != head
		missing := cfg.Bot
		if !need {
			for _, login := range cfg.RequiredBots {
				if reviewedHere[dialect.NormalizeBotName(login)] != head {
					need, missing = true, login
					break
				}
			}
		}
		if need {
			announce(repo, pr, head, "no "+dialect.NormalizeBotName(missing)+" review at head")
		}
		return need, head, nil
	}
	// Ordinary --no-incremental retains its historical contract: it asks only
	// whether the primary has ever reviewed this PR. A one-pass campaign opts
	// into the broader PR-wide cap, where any configured reviewer consumes the
	// single allowed review.
	configuredScope := anyConfigured || cfg.PrimaryOff
	if !configuredScope && lastBotReview != "" {
		// Programmatic/legacy Config values predate the canonical reviewer list;
		// Bot is their primary reviewer and remains a valid one-round marker.
		return false, head, nil
	}
	if configuredScope {
		if len(cfg.Reviewers) == 0 && lastBotReview != "" {
			return false, head, nil
		}
		// Review objects span the whole PR, but check runs do not: GitHub lists
		// them by ref. The state index is the durable PR-scoped record that a
		// check-bearing reviewer completed a review on an older head. Generic bot
		// activity cannot consume the cap because it may be an in-progress,
		// auxiliary, or failed check that delivered no review.
		for login := range reviewScope {
			if reviewedEver[login] {
				return false, head, nil
			}
		}
		if !anyConfigured {
			for _, reviewer := range cfg.Reviewers {
				login := dialect.NormalizeBotName(reviewer.Login)
				if reviewedEver[login] || state.CoReviewerAnswered(repo, pr, login) {
					return false, head, nil
				}
			}
		}
		if cfg.coChecksRelevant() {
			runs, err := s.gh.ListCheckRuns(ctx, repo, head)
			if err != nil {
				return false, "", err
			}
			for _, run := range runs {
				login, verdict := dialect.ClassifyCheckRun(run.App.Slug, run.Name, run.Output.Title, run.Output.Summary, run.Status, run.Conclusion)
				if cfg.coBotEnabled(login) && (verdict == dialect.CheckDone || verdict == dialect.CheckDoneClean) {
					return false, head, nil
				}
			}
		}
	}
	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return false, "", err
	}
	marker := strings.TrimSpace(s.cfg.ReviewDoneMarker)
	classifier := dialect.Classifier{
		CodeRabbit: s.cr, Bot: cfg.Bot, ReviewCommand: cfg.ReviewCommand,
		Primary: cfg.classifierPrimary(), CoReviewers: cfg.classifierCoReviewers(),
	}
	for _, comment := range comments {
		event := classifier.Classify(comment.User.Login, comment.Body, comment.ID, comment.CreatedAt, comment.UpdatedAt)
		if anyConfigured && dialect.NormalizeBotName(comment.User.Login) == bot &&
			event.Kind == dialect.EvNoAction && !event.SummaryOnly {
			return false, head, nil
		}
		if marker != "" && dialect.NormalizeBotName(comment.User.Login) == bot && strings.Contains(comment.Body, marker) {
			// The marker appears in CodeRabbit's summary-only, skipped,
			// in-progress and failed comments too. Ordinary first-review mode
			// retains its legacy marker behavior, but a one-pass campaign may
			// merge after this decision and therefore requires a classified clean
			// completion when no submitted review object exists.
			if !anyConfigured {
				return false, head, nil
			}
		}
		if configuredScope && cfg.coBotEnabled(comment.User.Login) && event.Kind == dialect.EvCoClean {
			return false, head, nil
		}
	}
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return false, "", err
	}
	// A pull-request author controls this body. It remains a compatibility
	// marker for ordinary first-review enrollment, never campaign merge proof.
	if !anyConfigured && marker != "" && strings.Contains(pull.Body, marker) {
		return false, head, nil
	}
	announce(repo, pr, head, "never reviewed")
	return true, head, nil
}

// noAnnounce is reviewNeeded's announcement for a caller that is only asking.
func noAnnounce(string, int, string, string) {}

// logEnqueue records one line per autoreview enqueue decision so a runaway is
// visible in the daemon log (repo#pr, head, and why it was queued).
func (s *Service) logEnqueue(repo string, pr int, head, reason string) {
	if s.log != nil {
		s.log.Printf("enqueue %s@%s reason=%q", QueueKey(repo, pr), head, reason)
	}
}
