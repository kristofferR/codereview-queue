package crq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type Logger interface {
	Printf(string, ...any)
}

type GitHubAPI interface {
	GetPull(context.Context, string, int) (ghapi.Pull, error)
	GetCommit(context.Context, string, string) (ghapi.Commit, error)
	ListReviews(context.Context, string, int) ([]ghapi.Review, error)
	ListIssueComments(context.Context, string, int) ([]ghapi.IssueComment, error)
	GetIssueComment(context.Context, string, int64) (ghapi.IssueComment, error)
	ListIssueCommentsPage(context.Context, string, int, int, int) ([]ghapi.IssueComment, error)
	ListReviewComments(context.Context, string, int) ([]ghapi.ReviewComment, error)
	ListIssueReactions(context.Context, string, int) ([]ghapi.Reaction, error)
	ListCommentReactions(context.Context, string, int64) ([]ghapi.Reaction, error)
	ListCheckRuns(context.Context, string, string) ([]ghapi.CheckRun, error)
	PostIssueComment(context.Context, string, int, string) (ghapi.IssueComment, error)
	DeleteIssueComment(context.Context, string, int64) error
	CreateIssue(context.Context, string, string, string) (ghapi.Issue, error)
	SearchOpenPRs(context.Context, string, bool, int) ([]ghapi.SearchPR, error)
	EachOpenPR(context.Context, string, bool, func(ghapi.SearchPR) (bool, error)) error
	// ListOwnerRepos backs the dashboard's repository picker: it is the only
	// call that asks about repositories crq does not already follow.
	ListOwnerRepos(context.Context, string, int) ([]ghapi.Repo, error)
	GraphQL(context.Context, string, map[string]any, any) error
	// ListPulls finds pull requests, filtered by the query. crq uses it to map a
	// checkout's branch to the PR it belongs to.
	ListPulls(context.Context, string, url.Values) ([]ghapi.Pull, error)
	// GetRef reads a ref's SHA. It is the cheapest "did anything change?" probe
	// crq has — a conditional GET that costs no quota while the ref is
	// unchanged — which is what `crq wait` idles on.
	GetRef(context.Context, string, string) (string, error)
}

type Service struct {
	cfg   Config
	cr    dialect.CodeRabbit
	gh    GitHubAPI
	store StateStore
	log   Logger
	// lastParkedSweep rotates sweepParkedClosed's candidate across pumps (see
	// there); in-memory only, single-writer (the pump caller).
	lastParkedSweep string
	// watchOffset rotates where a watch pass starts, so a PR at the tail is not
	// starved of dispatch slots forever by the ones ahead of it; in-memory only,
	// single-writer (the watch caller).
	watchOffset int
	// scanOffset rotates the bounded quota-free rescue scan's window so a round
	// past the first few is not starved forever; in-memory only, same writer.
	scanOffset int
	// now overrides the wall clock for the scheduling DECISIONS in the
	// pump/enqueue/sweep/wait paths (see clock). nil in production; the replay
	// suite injects a controllable fake so an incident can be re-enacted
	// deterministically. It intentionally does NOT reach logging/jitter/token or
	// the fake GitHub timestamps, which stay on real time.
	now func() time.Time
	// localWorkFn overrides Next's "does the caller hold changes the PR head
	// lacks" probe, which otherwise shells out to git in the process's working
	// directory. nil in production; tests inject an answer instead of depending
	// on the checkout they happen to run in.
	localWorkFn func(ctx context.Context, head string) (bool, string)
	// sleepFn overrides how the waiter idles. Only tests set it — a replay must
	// not spend real seconds to prove what the injected clock already decides.
	sleepFn func(ctx context.Context, d time.Duration) error
}

func NewService(cfg Config, gh GitHubAPI, store StateStore, log Logger) *Service {
	cr := dialect.CodeRabbit{
		CompletionMarker:  cfg.CompletionMarker,
		RateLimitMarker:   cfg.RateLimitMarker,
		CalibrationMarker: cfg.CalibrationMarker,
	}
	return &Service{cfg: cfg, cr: cr, gh: gh, store: store, log: log}
}

// clock is the service's notion of "now" (UTC) for scheduling decisions: retry
// windows, fire pacing, adoption cutoffs, feedback deadlines. Tests inject s.now
// to drive these deterministically; production leaves it nil and reads the wall
// clock.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// warnRateLimited is the requeue reason for a fire that came back account
// blocked. It matches the engine's Transition.Reason (both reference the one
// dialect constant) and is surfaced via AccountQuota, not the sticky Warn field.
const warnRateLimited = dialect.ReasonRateLimited

const leaderCapabilityHolds = "administrative-holds"

var errDispatchClaimLost = errors.New("dispatch claim is no longer owned by this watcher")

type dispatchOwnership struct {
	head  string
	token string
}

// Hold takes a PR out of the review queue in one write.
//
// Holding used to need two commands that could not be one: the skip marker
// stops fleet auto-review from enqueueing, `crq cancel` stops the pump, and
// between the two a daemon fired anyway. A hold is one fact, recorded where
// every firing path already looks, so there is no window between the halves.
//
// It does not cancel a round already in flight: that review is bought and its
// findings are still worth having. It stops the next one.
func (s *Service) Hold(ctx context.Context, repo string, pr int, reason string) (HoldResult, error) {
	return s.hold(ctx, repo, pr, reason, nil)
}

// holdDispatch records an agent's clarification only while that agent still
// owns the dispatch claim. The ownership check and hold write share one CAS
// update, so a watcher that lost its claim cannot stop the PR.
func (s *Service) holdDispatch(
	ctx context.Context,
	report NextReport,
	token string,
	reason string,
) (HoldResult, error) {
	return s.hold(ctx, report.Repo, report.PR, reason, &dispatchOwnership{
		head: report.Head, token: token,
	})
}

func (s *Service) hold(
	ctx context.Context,
	repo string,
	pr int,
	reason string,
	owner *dispatchOwnership,
) (HoldResult, error) {
	repo = NormalizeRepo(repo)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return HoldResult{}, errors.New("a hold needs a reason: it is a note to whoever finds the PR stopped")
	}
	now := s.clock().UTC()
	result := HoldResult{Repo: repo, PR: pr, Held: true, Reason: reason, By: s.cfg.Host, At: &now}
	if s.cfg.DryRun {
		return result, nil
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		if owner != nil {
			round := st.Round(repo, pr)
			if round == nil || round.Head != owner.head ||
				round.Dispatch == nil || round.Dispatch.Token != owner.token ||
				!round.DispatchHeld(now) {
				return errDispatchClaimLost
			}
		}
		// An older daemon preserves Holds as unknown JSON but cannot enforce it.
		// Require a capable live leader: without one, an older standby could
		// acquire the expired/empty lease immediately after this write.
		if st.Leader == nil || !st.Leader.ExpiresAt.After(now) || !st.LeaderHasCapability(leaderCapabilityHolds) {
			return errors.New("administrative holds require a live hold-capable autoreview leader; start or upgrade the daemon and try again")
		}
		if triggerPostClaimed(st.Round(repo, pr)) {
			return errors.New("a review trigger is already being posted; wait for it to finish before holding the PR")
		}
		st.Hold(repo, pr, reason, s.cfg.Host, now)
		return nil
	})
	if err != nil {
		return HoldResult{}, err
	}
	s.sync(ctx, state)
	if s.log != nil {
		s.log.Printf("%s#%d held: %s", repo, pr, reason)
	}
	return result, nil
}

// Unhold puts a PR back in the queue.
func (s *Service) Unhold(ctx context.Context, repo string, pr int) (HoldResult, error) {
	repo = NormalizeRepo(repo)
	result := HoldResult{Repo: repo, PR: pr, Held: false}
	if s.cfg.DryRun {
		return result, nil
	}
	released := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if !st.Unhold(repo, pr) {
			return ErrNoChange
		}
		released = true
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return HoldResult{}, err
	}
	if released {
		s.sync(ctx, state)
		if s.log != nil {
			s.log.Printf("%s#%d released", repo, pr)
		}
	}
	return result, nil
}

// Prioritize moves a tracked PR ahead of every other round. The queue sequence
// is shared by autoreview and autofix, so one action accelerates whichever kind
// of work the PR needs next.
func (s *Service) Prioritize(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	if s.cfg.DryRun {
		return fmt.Errorf("dry run: would move %s#%d to the top of the queue", repo, pr)
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		if !st.MoveToFront(repo, pr) {
			return fmt.Errorf("%s#%d is not currently queued", repo, pr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, updated)
	if s.log != nil {
		s.log.Printf("%s#%d moved to the top of the queue", repo, pr)
	}
	return nil
}

// triggerPostClaimed reports the two claim states after which Hold cannot
// promise that no new review command will be posted. The hold and each claim
// are CAS writes, so checking them in the hold write makes either ordering
// safe: an existing claim rejects the hold, and an existing hold rejects a
// later claim.
func triggerPostClaimed(r *Round) bool {
	if r == nil {
		return false
	}
	if r.Phase == PhaseReserved {
		return true
	}
	for _, co := range r.CoBots {
		// A claim's retry lease may expire while its original poster is still
		// inside the transport's unbounded network retry. Until a command id is
		// recorded, that worker can still resume and post, so Hold cannot promise
		// that the PR is trigger-free.
		if co.ClaimedAt != nil {
			return true
		}
	}
	return false
}

type EnqueueResult struct {
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	Queued        bool   `json:"queued"`
	AlreadyQueued bool   `json:"already_queued"`
	Deduped       bool   `json:"deduped"`
	Head          string `json:"head,omitempty"`
	Seq           int64  `json:"seq,omitempty"`
	// Held says the PR is out of the queue on purpose. A caller that treats a
	// no-op as "try again" would otherwise spin: nothing about a held PR changes
	// by asking again.
	Held bool `json:"held,omitempty"`
	// Reason explains a no-op, which today means the PR is held.
	Reason string `json:"reason,omitempty"`
}

// Enqueue records a review round for repo#pr's current head. A round already
// tracking the head is reported (queued/deduped) instead of duplicated; a round
// on a stale head is superseded to track the new one.
func (s *Service) Enqueue(ctx context.Context, repo string, pr int) (EnqueueResult, error) {
	repo = NormalizeRepo(repo)
	result := EnqueueResult{Repo: repo, PR: pr}
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return result, err
	}
	// Always report the head this actually read. It used to be set only on the
	// deduped path, so a caller comparing it against its own observation could
	// not detect the very case that matters — a head that moved between the two.
	result.Head = head
	state, err := s.store.Update(ctx, func(st *State) error {
		now := s.clock()
		if h, held := st.HeldPR(repo, pr); held {
			// Enqueueing a held PR would queue a round nothing may fire, which
			// reads on the dashboard as work waiting its turn.
			result.Held = true
			result.Reason = "held: " + h.Reason
			return ErrNoChange
		}
		// An EXPLICIT off switch, asked here for the same reason enqueueBatch
		// asks it: turning a repository off abandons its pending rounds, and a
		// `crq next` or `crq loop` run against it afterwards recreated one that
		// Pump — which asks nothing about enrollment — went on to spend a
		// metered review on. Only a record, never a mere absence from this
		// host's CRQ_REPOS: a manual run on a repository the fleet does not
		// scan is the ordinary way this command is used.
		if rec, ok := st.Enrollment(repo); ok && !rec.Enabled {
			result.Held = true
			result.Reason = "the repository is turned off"
			if rec.Reason != "" {
				result.Reason += ": " + rec.Reason
			}
			return ErrNoChange
		}
		r := st.Round(repo, pr)
		if r != nil && r.Head == head {
			// A PR reopened after its reviewers changed: the completed round is a
			// marker for requirements that no longer hold, so it goes back in the
			// queue instead of deduping the enqueue that would have asked.
			if requeueIfReviewersChanged(st, r) {
				result.Queued = true
				result.Seq = r.Seq
				return nil
			}
			switch r.Phase {
			case PhaseFired, PhaseReviewing, PhaseCompleted:
				result.Deduped = true
				result.Head = head
			default:
				result.AlreadyQueued = true
			}
			return ErrNoChange
		}
		var nr *Round
		if r != nil {
			// The tracked head is stale — supersede to the current one.
			nr, err = st.Supersede(repo, pr, head, now)
		} else {
			nr, err = st.NewRound(repo, pr, head, now)
		}
		if err != nil {
			return err
		}
		result.Queued = true
		result.Seq = nr.Seq
		return nil
	})
	if err != nil {
		return result, err
	}
	s.sync(ctx, state)
	return result, nil
}

// queueCandidate is one PR the autoreview pass decided to enqueue, carrying the
// head it resolved so enqueueBatch can create the round without re-fetching.
type queueCandidate struct {
	Repo string
	PR   int
	Head string
	// Title travels with the candidate because the scan already has it: the
	// search result carries it, so recording it costs nothing and spares every
	// later list a request per row.
	Title string
}

// enqueueBatch appends several PRs in a single compare-and-swap write plus one
// dashboard sync, so a large autoreview pass doesn't produce N separate state
// writes / issue edits. A PR already tracked at the same head is skipped; a
// stale head is superseded. The DecideFire dedup still backstops at pump time.
func (s *Service) enqueueBatch(ctx context.Context, items []queueCandidate) error {
	if len(items) == 0 || s.cfg.DryRun {
		return nil
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		now := s.clock()
		added := 0
		for _, it := range items {
			repo := NormalizeRepo(it.Repo)
			if _, held := st.HeldPR(repo, it.PR); held {
				continue
			}
			// Asked again here, against the state this write lands on, for the
			// same reason reviewersChanged is: the scan decided from a snapshot,
			// and a repository turned off since then has already had its pending
			// rounds abandoned. Creating one now would put a metered review back
			// in the queue after the off switch reported success.
			if !s.reviewsRepo(*st, repo) {
				continue
			}
			if r := st.Round(repo, it.PR); r != nil {
				// A title arriving for a round that already exists is still
				// news: it may have been renamed, or recorded before titles
				// were kept at all.
				if it.Title != "" && r.Title != it.Title {
					updated := *r
					updated.Title = it.Title
					st.PutRound(updated)
					r = st.Round(repo, it.PR)
					added++
				}
				if r.Head == it.Head {
					if requeueIfReviewersChanged(st, r) {
						added++
					}
					continue
				}
				superseded, err := st.Supersede(repo, it.PR, it.Head, now)
				if err != nil {
					return err
				}
				if it.Title != "" {
					superseded.Title = it.Title
					st.PutRound(*superseded)
				}
				added++
				continue
			}
			fresh, err := st.NewRound(repo, it.PR, it.Head, now)
			if err != nil {
				return err
			}
			if it.Title != "" {
				fresh.Title = it.Title
				st.PutRound(*fresh)
			}
			added++
		}
		if added == 0 {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, state)
	return nil
}

type PumpResult struct {
	Action string `json:"action"`
	Repo   string `json:"repo,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Head   string `json:"head,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Pump advances the queue by one observe → engine → apply step: it progresses
// the round holding the fire slot, sweeps one reviewing round toward
// completion, then fires the next eligible round. In DryRun it computes the
// same decisions but writes and posts nothing.
func (s *Service) Pump(ctx context.Context) (PumpResult, error) {
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, err
	}

	// 1. The round holding the fire slot: progress it first, mirroring v2's
	//    "handle in-flight first" so a single pump never both progresses and
	//    fires. It does not end the pump, though. The slot serializes the METERED
	//    fire and nothing else, so rounds whose next step spends no CodeRabbit
	//    quota still get their turn below.
	if slot := st.SlotRound(); slot != nil {
		res, err := s.progressSlotRound(ctx, *slot)
		if err != nil {
			return res, err
		}
		// Reload: progressing the slot round may have released it, and the sweep
		// must not decide against a snapshot that says otherwise.
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return PumpResult{}, err
		}
		if free, handled, err := s.sweepQuotaFree(ctx, st, s.clock(), slot.Repo, slot.PR); err != nil {
			return PumpResult{}, err
		} else if handled {
			// The quota-free result is the one Pump exposes to its caller, so
			// preserve the cleanup hook for the slot result it replaces.
			if err := s.tidyAfterPump(ctx, st, res); err != nil {
				return res, err
			}
			return free, nil
		}
		return res, nil
	}

	// 2. Reviewing rounds no longer hold the slot; sweep the oldest one toward
	//    completion/retry (bounded to one per pump, like v2's feedback sweep).
	if updated, err := s.sweepReviewing(ctx, st, now); err != nil {
		return PumpResult{}, err
	} else {
		st = updated
	}

	// 2b. A PR closed during a cooldown or hold parks invisibly (NextEligible
	//    skips it): sweep one such round's PR state so closure abandons it now
	//    instead of waiting for the cooldown or an operator to release the hold.
	if res, handled, err := s.sweepParkedClosed(ctx, st); err != nil {
		return PumpResult{}, err
	} else if handled {
		return res, nil
	}

	// 3. Fire the next eligible round.
	next := st.NextEligible(now)
	if next == nil {
		return PumpResult{Action: "idle"}, nil
	}
	// Terminal cleanup is independent of quota and pacing: drop a closed/merged
	// PR before either gate so it leaves on this pump instead of lingering.
	if _, open, err := s.pullHead(ctx, next.Repo, next.PR); err != nil {
		return PumpResult{}, err
	} else if !open {
		return s.abandonRound(ctx, *next, "pr closed", "skipped")
	}
	// A dry-run pump reports decisions and writes nothing — that includes the
	// calibration probe RefreshQuota may post; decide from the loaded snapshot.
	if !s.cfg.DryRun {
		if refreshed, err := s.RefreshQuota(ctx); err == nil {
			st = refreshed
		} else {
			return PumpResult{}, err
		}
	}
	now = s.clock()
	// No early blocked/min-interval return here: DecideFire owns those gates and
	// deliberately resolves the quota-free verdicts (dedupe, FireCoOnly,
	// FireCoReviewWait) before them, so an account block from another PR does not
	// delay resolutions that spend no CodeRabbit quota. mapFireNo still reports
	// "blocked"/"min_interval" for real fires; observing while blocked costs
	// ETag-cached 304s.
	next = st.NextEligible(now)
	if next == nil {
		return PumpResult{Action: "idle"}, nil
	}
	cfg := s.cfgFor(st, next.Repo)
	obs, err := s.observe(ctx, cfg, next.Repo, next.PR, next, collectPosted(st, next.Repo, next.PR).commands, now)
	if err != nil {
		return PumpResult{}, err
	}
	// Record which co-reviewers have answered, from the observation this pass
	// already paid for. Done HERE and not only on the reviewing sweep: a round
	// that never fires — because the account is blocked, or because the primary
	// does not run here — is observed on this path and nowhere else, so a
	// co-reviewer could review it every time and crq would never notice. That
	// is exactly the case that made a working Codex read as "never answered".
	s.noteCoAnswers(ctx, cfg, *next, obs.eng, now)
	// Record a rate-limit notice before deciding, whichever round it answered.
	// A session's push supersedes the round that asked, and the reply used to be
	// archived unread — so crq believed the account was free and posted the
	// command again minutes after being told to wait.
	if updated, err := s.recordObservedBlock(ctx, cfg, obs, st, now); err != nil {
		return PumpResult{}, err
	} else if updated != nil {
		st = *updated
	}
	global := s.global(st, now)
	decision := engine.DecideFire(global, *next, obs.eng, now, cfg.policy())
	result, err := s.applyFire(ctx, cfg, *next, obs.eng, decision, now)
	if err != nil {
		return result, err
	}
	// A blocked or orphaned-slot-held front of the queue must not starve later
	// PRs of resolutions that spend NO CodeRabbit quota — a co-reviewer defer,
	// or a summary-only round whose review is never coming from CodeRabbit at all.
	accountBlocked := global.BlockedUntil != nil && global.BlockedUntil.After(now)
	if decision.Verdict == engine.FireNo && (accountBlocked || st.SlotHeld(now)) {
		if free, handled, err := s.sweepQuotaFree(ctx, st, now, next.Repo, next.PR); err != nil {
			return PumpResult{}, err
		} else if handled {
			return free, nil
		}
	}
	return result, nil
}

// sweepQuotaFree gives the queued rounds that spend no CodeRabbit quota their
// turn when the metered front cannot move — because another PR holds the fire
// slot, or the account window is shut.
//
// Neither of those has any authority here. The slot exists to serialize one
// account-metered review, and the window bounds that same allowance; a
// co-reviewer defer or a summary-only round asks for neither. Leaving them in
// the FIFO is what stranded PRs behind an unrelated block for hours.
//
// skipRepo/skipPR is the round the caller already decided about, so one pump
// never applies two verdicts to it.
//
// It observes at most scanBudget rounds per pump and rotates where it starts.
// A fixed start meant that when the rounds behind the head all needed
// CodeRabbit, every pump observed those same few and stopped — a quota-free
// round further back was never even looked at until the unrelated block cleared.
func (s *Service) sweepQuotaFree(ctx context.Context, st State, now time.Time, skipRepo string, skipPR int) (PumpResult, bool, error) {
	const scanBudget = 3
	queued := st.QueuedRounds(now)
	if len(queued) == 0 {
		return PumpResult{}, false, nil
	}
	global := s.global(st, now)
	scanned := 0
	defer func() { s.scanOffset = (s.scanOffset + scanned + 1) % len(queued) }()
	for i := range queued {
		round := queued[(i+s.scanOffset)%len(queued)]
		if round.Repo == skipRepo && round.PR == skipPR {
			continue
		}
		if scanned >= scanBudget {
			break
		}
		// No cheap pre-filter here. "Every trigger already posted" is not proof
		// that nothing is left to do: a primary-unavailable round in that state
		// still needs its quota-free FireDedupe/FireCoReviewWait, and a co-review
		// answer to an earlier deferred command needs collecting. Skipping those
		// left them behind the account block for hours. The budget above bounds
		// the cost instead.
		scanned++
		cfg := s.cfgFor(st, round.Repo)
		obs, err := s.observe(ctx, cfg, round.Repo, round.PR, &round, collectPosted(st, round.Repo, round.PR).commands, now)
		if err != nil {
			continue
		}
		d := engine.DecideFire(global, round, obs.eng, now, cfg.policy())
		if !quotaFreeVerdict(d.Verdict) {
			continue
		}
		res, err := s.applyFire(ctx, cfg, round, obs.eng, d, now)
		if err != nil {
			return PumpResult{}, false, err
		}
		return res, true, nil
	}
	return PumpResult{}, false, nil
}

// advanceQuotaFree resolves ONE PR's round directly, bypassing the account-wide
// FIFO, when its verdict spends no CodeRabbit quota. It is the loop's escape
// hatch from a queue it does not belong in: a summary-only round has no review
// coming from CodeRabbit ever, so neither the fire slot, the global pacing
// interval, nor an account block has any authority over it — the only thing it
// waits for is its own co-reviewers.
//
// handled is false when the round is gone, not fire-eligible, or its verdict
// would spend quota; the caller then falls back to the normal global pump.
func (s *Service) advanceQuotaFree(ctx context.Context, repo string, pr int) (PumpResult, bool, error) {
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, false, err
	}
	round := st.Round(repo, pr)
	if round == nil || !round.FireEligible(now) {
		return PumpResult{}, false, nil
	}
	if _, held := st.HeldPR(repo, pr); held {
		return PumpResult{}, false, nil
	}
	cfg := s.cfgFor(st, repo)
	obs, err := s.observe(ctx, cfg, repo, pr, round, collectPosted(st, repo, pr).commands, now)
	if err != nil {
		return PumpResult{}, false, err
	}
	d := engine.DecideFire(s.global(st, now), *round, obs.eng, now, cfg.policy())
	if !quotaFreeVerdict(d.Verdict) {
		return PumpResult{}, false, nil
	}
	res, err := s.applyFire(ctx, cfg, *round, obs.eng, d, now)
	if err != nil {
		return PumpResult{}, false, err
	}
	if s.log != nil {
		s.log.Printf("%s#%d resolved without the review queue: %s", repo, pr, d.Reason)
	}
	return res, true, nil
}

// recordObservedBlock folds any rate-limit notice in this observation into the
// shared account quota, independent of which round it answered.
//
// Progress only derives the window for a round that still exists and fired
// before the notice. Neither survives a fix session's push: the head moves, the
// round is superseded, and the reply it was waiting for is archived unread. The
// allowance is not a property of a round, so any current notice from the primary
// counts here. AcceptAccountBlock still decides whether it replaces the standing
// window, and never shortens it.
func (s *Service) recordObservedBlock(ctx context.Context, cfg Config, obs observation, st State, now time.Time) (*State, error) {
	blk := engine.ObservedAccountBlock(obs.eng, cfg.policy(), st.Account, now)
	if blk == nil || s.cfg.DryRun || !observedAccountBlockChanges(st.Account, blk) {
		return nil, nil
	}
	updated, err := s.store.Update(ctx, func(w *State) error {
		if !observedAccountBlockChanges(w.Account, blk) {
			return ErrNoChange
		}
		applyAccountBlock(w, blk, now)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil, nil
		}
		return nil, err
	}
	// Like every other writer of this window: the dashboard is where an operator
	// reads whether the account is available, and the decision that follows this
	// is usually FireNo — which writes nothing further, leaving the issue claiming
	// a free account for the whole block while state and `crq status` say
	// otherwise.
	s.sync(ctx, updated)
	if s.log != nil {
		s.log.Printf("account blocked until %s (observed, not tied to a round)", blk.Until.UTC().Format(time.RFC3339))
	}
	return &updated, nil
}

// observedAccountBlockChanges accepts a longer window, or a distinct notice
// whose shorter window the engine has already clamped to the standing block.
// The latter still has to advance the notice watermark so it cannot become a
// fresh fallback block when the standing window expires.
func observedAccountBlockChanges(q AccountQuota, blk *engine.AccountBlock) bool {
	if engine.AcceptAccountBlock(q.BlockedUntil, blk.Until) {
		return true
	}
	if q.BlockedUntil == nil || !blk.Until.Equal(*q.BlockedUntil) || blk.CommentID == 0 {
		return false
	}
	if q.RLCommentUpdated != nil && !blk.CommentUpdated.After(*q.RLCommentUpdated) {
		return false
	}
	if blk.CommentID != q.RLCommentID || q.RLCommentUpdated == nil {
		return true
	}
	return true
}

// recordDismissal is the effects executor for `crq dismiss`: the CAS write that
// records which findings a round has accounted for. Dismiss decides WHETHER a
// dismissal is legitimate; this performs it, so the write surface stays in one
// file with every other one.
//
// seenSeq is the Seq of the round Dismiss read the findings against (0 for
// none), which is what separates a round left on the PREVIOUS head — the
// ordinary state after a push — from one another worker moved forward while
// this call was deciding. Seq is the state's own counter rather than a
// timestamp, so the two cannot be confused by a fleet whose hosts disagree
// about the time.
//
// allowCreate says whether a round may be created for this head. It is false
// when other blocking findings remain, because a fresh round is fire-eligible
// and DecideFire — which never sees findings — could not hold it back.
//
// A round tracking a DIFFERENT head is refused when it is not the one Dismiss
// read: the head moved after the findings were read, so this decision is about
// a commit nobody is looking at any more, and superseding would archive the
// newer round and point the queue back at the stale head.
func (s *Service) recordDismissal(ctx context.Context, repo string, pr int, head string, ids []string, reason string, allowCreate bool, seenSeq int64) (dismissed, already []string, err error) {
	mutate := func(st *State) error {
		round := st.Round(repo, pr)
		switch {
		case round != nil && round.Head != head && round.Seq != seenSeq:
			// Not the round Dismiss read: something moved the PR forward
			// underneath this call, so the decision is about a commit nobody is
			// looking at any more.
			return fmt.Errorf("%s#%d moved to %s while dismissing; re-read the findings", repo, pr, round.Head)
		case !allowCreate && (round == nil || round.Head != head ||
			(engine.CanStillFire(*round) && !round.DispatchHeld(s.clock()))):
			// The guard is about whether a FIRE-ELIGIBLE round would exist with
			// other findings still open, not about whether a round object
			// exists. A queued round is just as dangerous as a new one: Pump can
			// hand it to DecideFire, which sees no findings and cannot enforce
			// fix-first. A live dispatch claim is the exception: it excludes
			// this exact round from Pump until the session pushes or releases
			// it, so a session may dismiss one finding at a time. So is a round
			// on the previous head, because the supersede below would replace it
			// with a fresh queued one.
			return fmt.Errorf("%s#%d has other unaddressed findings at %s: dismiss or resolve them in the same pass, so no round is queued while work is open", repo, pr, head)
		case round == nil:
			var err error
			if round, err = st.NewRound(repo, pr, head, s.clock()); err != nil {
				return err
			}
		case round.Head != head:
			// The ordinary state after a push: the stored round is still on the
			// PREVIOUS head, because `crq next` returns on a current-head finding
			// before it enqueues. Refusing here would leave the new head in the
			// exact fix-first deadlock this command exists to end.
			var err error
			if round, err = st.Supersede(repo, pr, head, s.clock()); err != nil {
				return err
			}
		}
		// Reset on every CAS attempt: a retry replays this closure, and appending
		// to the outer slices would report each dismissal once per attempt.
		dismissed, already = []string{}, nil
		for _, id := range ids {
			if round.Dismiss(id, reason) {
				dismissed = append(dismissed, id)
			} else {
				already = append(already, id)
			}
		}
		st.PutRound(*round)
		return nil
	}
	// A dry run must report what a real one would do, refusals included. The one
	// way it cannot drift from the write is to BE the write, run against a
	// throwaway copy of the state that nothing stores.
	if s.cfg.DryRun {
		st, _, err := s.store.Load(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := mutate(&st); err != nil {
			return nil, nil, err
		}
		return dismissed, already, nil
	}
	state, err := s.store.Update(ctx, mutate)
	if err != nil {
		return nil, nil, err
	}
	s.sync(ctx, state)
	return dismissed, already, nil
}

// applyAccountBlock records an observed account-quota block, whatever observed
// it. It is the single writer for that transition: the decision of whether the
// block counts is engine.AcceptAccountBlock's, and executing it belongs here with
// the rest of the CAS writes rather than beside each evidence source.
//
// It returns whether anything was written, the block that stands either way,
// and whether the CLI organisation still matches the fleet snapshot.
func (s *Service) applyAccountBlock(
	ctx context.Context, until time.Time, source string, cfg Config, cliOrg string,
) (bool, *time.Time, bool, error) {
	now := s.clock()
	// Update swallows ErrNoChange, so whether anything was written has to be
	// recorded by the mutation itself. It is assigned on every attempt because a
	// CAS conflict runs the closure again.
	applied := false
	matched := true
	state, err := s.store.Update(ctx, func(st *State) error {
		current := s.cfg.WithFleet(st.Fleet)
		if stampChanged(st.Fleet.UpdatedAt, cfg.FleetAt) || !cliOrgMatches(current, cliOrg) {
			applied, matched = false, false
			return ErrNoChange
		}
		matched = true
		if !engine.AcceptAccountBlock(st.Account.BlockedUntil, until) {
			applied = false
			return ErrNoChange
		}
		applied = true
		at := until
		st.Account.BlockedUntil = &at
		st.Account.CheckedAt = &now
		st.Account.Source = source
		st.Account.Scope = strings.Join(current.Scope, ",")
		// A reading from outside the PR says nothing about how many reviews are
		// left, and a stale count beside a fresh block would read as authoritative.
		st.Account.Remaining = nil
		// The standing block is no longer the one a PR comment produced, so its
		// provenance must not survive it: a later edit of that comment would be
		// matched as a repeat and its window reused, and a pending calibration
		// would make the next pump resume the older probe and overwrite this.
		st.Account.RLCommentID = 0
		st.Account.RLCommentUpdated = nil
		st.Account.CalibAskedAt = nil
		return nil
	})
	if err != nil {
		return false, nil, false, err
	}
	if applied {
		s.sync(ctx, state)
	}
	return applied, state.Account.BlockedUntil, matched, nil
}

// quotaFreeVerdict reports whether a fire verdict can be applied while another
// PR holds the slot or the account is blocked: none of these post
// `@coderabbitai review`, reserve the FireSlot, or spend account quota.
func quotaFreeVerdict(v engine.FireVerdict) bool {
	switch v {
	case engine.FireCoDeferred, engine.FireCoOnly, engine.FireCoReviewWait, engine.FireDedupe:
		return true
	}
	return false
}

func (s *Service) global(st State, now time.Time) engine.Global {
	return engine.Global{
		SlotFree:     !st.SlotHeld(now),
		BlockedUntil: st.Account.BlockedUntil,
		LastFired:    st.LastFired,
	}
}

// progressSlotRound observes and progresses the round holding the fire slot.
func (s *Service) progressSlotRound(ctx context.Context, slot Round) (PumpResult, error) {
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, err
	}
	cfg := s.cfgFor(st, slot.Repo)
	obs, err := s.observe(ctx, cfg, slot.Repo, slot.PR, &slot, collectPosted(st, slot.Repo, slot.PR).commands, now)
	if err != nil {
		return PumpResult{}, err
	}
	s.selfHealCoReviewers(ctx, cfg, slot, obs.eng, now)
	// Here as well as in the reviewing sweep: this observation can take the round
	// straight from fired to completed, and a completed round is never looked at
	// again. The co-reviewer that answered it would be lost with it, leaving a
	// working bot shown as silent for want of the one field that says otherwise.
	s.noteCoAnswers(ctx, cfg, slot, obs.eng, now)
	tr := engine.Progress(slot, st.Account, obs.eng, now, cfg.policy())
	if tr.Outcome == engine.KeepWaiting {
		return PumpResult{Action: "waiting", Repo: slot.Repo, PR: slot.PR, Reason: tr.Reason}, nil
	}
	if s.cfg.DryRun {
		return slotResult(slot, tr), nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(slot.Repo, slot.PR)
		if r == nil || st.FireSlot == nil || st.FireSlot.Token != slot.Token {
			return ErrNoChange
		}
		return s.applyTransition(st, r, tr, now, cfg)
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	if s.log != nil && (tr.Outcome == engine.OutRetry || tr.Outcome == engine.OutReleaseSlot) {
		blockedUntil := "-"
		if updated.Account.BlockedUntil != nil {
			blockedUntil = updated.Account.BlockedUntil.UTC().Format(time.RFC3339)
		}
		s.log.Printf("requeue %s@%s reason=%q blocked_until=%s", QueueKey(slot.Repo, slot.PR), slot.Head, tr.Reason, blockedUntil)
	}
	return slotResult(slot, tr), nil
}

// applyTransition applies a fired/reviewing round's engine Transition to state:
// the round transition plus any fire-slot release and account-quota block.
//
// cfg is the configuration Progress decided from, revalidated here for the same
// reason applyFire revalidates its verdicts — this runs inside the CAS mutation,
// where the state it reads is the state the write lands on.
func (s *Service) applyTransition(st *State, r *Round, tr engine.Transition, now time.Time, cfg Config) error {
	key := QueueKey(r.Repo, r.PR)
	ownerToken := r.Token
	if tr.Outcome == engine.OutRetry {
		current := s.cfgFor(*st, r.Repo)
		fallbackChanged := current.RateLimitFallback != cfg.RateLimitFallback
		policyChanged := reviewersChanged(st, r.Repo, cfg) ||
			!sameBot(current.Bot, cfg.Bot) ||
			current.PrimaryOff != cfg.PrimaryOff ||
			!sameReviewers(current, cfg) ||
			current.InflightTimeout != cfg.InflightTimeout ||
			fallbackChanged
		if policyChanged {
			// The account block is independent evidence unless its window was
			// derived from the fallback that just changed. Preserve a parsed block
			// while dropping the stale round transition; a fallback-derived one is
			// recomputed from the observation on the next pump.
			if tr.Blocked != nil && !fallbackChanged &&
				observedAccountBlockChanges(st.Account, tr.Blocked) {
				applyAccountBlock(st, tr.Blocked, now)
				return nil
			}
			return ErrNoChange
		}
	} else if tr.Outcome == engine.OutReleaseSlot &&
		reviewersChanged(st, r.Repo, cfg) {
		return ErrNoChange
	}
	switch tr.Outcome {
	case engine.OutComplete:
		// The completed round is the "this head was reviewed" dedup marker, and
		// reopenForChangedReviewers deliberately leaves an in-flight round alone —
		// it is already going to answer. A reviewer change that commits between
		// the decision and this write would therefore be answered by neither: the
		// marker dedupes the head under the set that no longer gates it. Drop the
		// stale transition; the next pump decides again under the new set.
		if reviewersChanged(st, r.Repo, cfg) {
			return ErrNoChange
		}
		if err := r.Complete(); err != nil {
			return err
		}
	case engine.OutReviewing:
		if err := r.Acknowledge(); err != nil {
			return err
		}
	case engine.OutRetry:
		if tr.Blocked != nil {
			applyAccountBlock(st, tr.Blocked, now)
		}
		// Turning a repository off leaves an already-purchased response in
		// flight, because that answer is still worth collecting. If the answer
		// instead says to retry, the next command would be a new purchase after
		// the switch was thrown. Keep any account-wide block learned above, but
		// end this repository's round before it becomes fire-eligible again.
		if !s.reviewsRepo(*st, r.Repo) {
			st.EndRound(r.Repo, r.PR, "repository turned off before retry")
			releaseSlot(st, key, ownerToken)
			return nil
		}
		if err := r.AwaitRetry(tr.RetryAt, tr.Reason, now); err != nil {
			return err
		}
	case engine.OutReleaseSlot:
		if err := r.ReleaseToQueue(tr.Reason, now); err != nil {
			return err
		}
	case engine.OutAbandon:
		st.EndRound(r.Repo, r.PR, tr.Reason)
		releaseSlot(st, key, ownerToken)
		return nil
	default:
		return nil
	}
	st.PutRound(*r)
	releaseSlot(st, key, ownerToken)
	return nil
}

// releaseSlot clears only the exact fire-slot claim a round acquired. A
// superseding round reuses the queue key with a new token while the original
// command's HoldUntil deliberately survives it.
func releaseSlot(st *State, key, token string) {
	if st.FireSlot != nil && st.FireSlot.Key == key && st.FireSlot.Token == token {
		st.FireSlot = nil
		st.ClearSlotHold()
	}
}

// sameRound reports whether the stored round r is still the one that was
// observed — same Seq and Head. Every CAS mutation guards on it so a concurrent
// supersede (which archives the old round and creates a fresh one with a new Seq)
// between observe and write reads as a benign lost race rather than a mutation of
// the wrong round.
func sameRound(r *Round, want Round) bool {
	return r != nil && r.Seq == want.Seq && r.Head == want.Head
}

// archivedRound returns the exact round a concurrent supersede moved out of
// Rounds. A trigger poster still owns claims on that object until its network
// call returns, so its outcome must be recorded there rather than lost merely
// because the pull request acquired a new head meanwhile.
func archivedRound(st *State, want Round) *Round {
	for i := len(st.Archive) - 1; i >= 0; i-- {
		if sameRound(&st.Archive[i], want) {
			return &st.Archive[i]
		}
	}
	return nil
}

// firable is the fire guard every reserving CAS uses: eligible, and not held.
//
// Selecting the round and writing the reservation are two steps, and a hold that
// commits between them would otherwise be too late — the daemon has already
// decided. Re-reading it here is what makes `crq hold` mean something the moment
// it returns, rather than from the next pass onward.
func firable(st *State, r *Round, now time.Time) bool {
	if r == nil || !r.FireEligible(now) {
		return false
	}
	_, held := st.HeldPR(r.Repo, r.PR)
	return !held
}

// applyAccountBlock ports requeueInflight's account-quota bookkeeping. The window
// (including same-comment reuse) was resolved by the engine, so only the store
// write happens here.
func applyAccountBlock(st *State, blk *engine.AccountBlock, now time.Time) {
	until := blk.Until.UTC()
	zero := 0
	st.Account.BlockedUntil = &until
	st.Account.Remaining = &zero
	st.Account.Source = "warning"
	st.Account.CheckedAt = &now
	if blk.CommentID != 0 &&
		(st.Account.RLCommentUpdated == nil || blk.CommentUpdated.After(*st.Account.RLCommentUpdated)) {
		st.Account.RLCommentID = blk.CommentID
		u := blk.CommentUpdated.UTC()
		st.Account.RLCommentUpdated = &u
	}
	st.Warn = ""
}

func slotResult(slot Round, tr engine.Transition) PumpResult {
	r := PumpResult{Repo: slot.Repo, PR: slot.PR, Head: slot.Head, Reason: tr.Reason}
	switch tr.Outcome {
	case engine.OutComplete, engine.OutReviewing:
		r.Action = "cleared"
	case engine.OutRetry, engine.OutReleaseSlot:
		r.Action = "requeued"
	case engine.OutAbandon:
		r.Action = "cleared"
	default:
		r.Action = "waiting"
	}
	return r
}

// sweepReviewing progresses the oldest fired/reviewing round that is not holding
// the fire slot, so a round whose slot was released on a bot ack still reaches
// completion (or parks) without a Loop running. Bounded to one per pump.
func (s *Service) sweepReviewing(ctx context.Context, st State, now time.Time) (State, error) {
	if s.cfg.DryRun {
		return st, nil
	}
	var target *Round
	for key := range st.Rounds {
		r := st.Rounds[key]
		if r.Phase != PhaseFired && r.Phase != PhaseReviewing {
			continue
		}
		if target == nil || firedOrEnqueuedAt(r).Before(firedOrEnqueuedAt(*target)) {
			c := r
			target = &c
		}
	}
	if target == nil {
		return st, nil
	}
	cfg := s.cfgFor(st, target.Repo)
	obs, err := s.observe(ctx, cfg, target.Repo, target.PR, target, collectPosted(st, target.Repo, target.PR).commands, now)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: reviewing-round sweep for %s#%d failed: %v", target.Repo, target.PR, err)
		}
		return st, nil
	}
	s.selfHealCoReviewers(ctx, cfg, *target, obs.eng, now)
	s.noteCoAnswers(ctx, cfg, *target, obs.eng, now)
	tr := engine.Progress(*target, st.Account, obs.eng, now, cfg.policy())
	if tr.Outcome == engine.KeepWaiting {
		return st, nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(target.Repo, target.PR)
		// Guard on round identity: a supersede/cancel-and-re-enqueue between observe
		// and this CAS could otherwise apply the old head's Progress to a replacement
		// round for a newer head, deduping or cooling it on stale observations.
		if r == nil || !sameRound(r, *target) || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		return s.applyTransition(st, r, tr, now, cfg)
	})
	if err != nil {
		return st, err
	}
	s.sync(ctx, updated)
	// A round completed here is invisible to the caller — Pump goes on to report
	// whatever it fires next, or idle — so this is the only moment that knows the
	// PR's trigger comments are spent.
	if err := s.tidyProgressed(ctx, updated, target.Repo, target.PR); err != nil {
		return updated, err
	}
	return updated, nil
}

func firedOrEnqueuedAt(r Round) time.Time {
	if r.FiredAt != nil {
		return *r.FiredAt
	}
	return r.EnqueuedAt
}

// applyFire executes a DecideFire verdict.
//
// The decision was made from a configuration that may have been replaced since.
// Acting on it would post a trigger for a co-reviewer an operator has just
// removed, skip one they have just required, or record that a head is reviewed
// by a set that no longer gates it — so the verdicts the reviewer configuration
// decides revalidate it (reviewersChanged) inside their own commit point, the CAS
// mutation that claims the trigger, reserves the slot or writes the dedupe
// marker. Checking it in a separate read here would leave exactly the window it
// is meant to close: SetReviewers commits in between, and the mutation goes on
// to apply the decision the new configuration would not have made.
func (s *Service) applyFire(ctx context.Context, cfg Config, round Round, obs engine.Observation, d engine.FireDecision, now time.Time) (PumpResult, error) {
	switch d.Verdict {
	case engine.FireDrop:
		return s.abandonRound(ctx, round, "pr closed", "skipped")
	case engine.FireDedupe:
		return s.dedupeRound(ctx, cfg, round, now, d.Reason)
	case engine.FireCoOnly:
		return s.fireCoOnly(ctx, cfg, round, d.PostCo, d.Reason, now)
	case engine.FireCoDeferred:
		return s.fireCoDeferred(ctx, cfg, round, d, now)
	case engine.FireCoReviewWait:
		return s.fireCoReviewWait(ctx, cfg, round, obs, d.Reason, now)
	case engine.FireSupersede:
		return s.supersedeRound(ctx, round, obs.Head, now)
	case engine.FireAdopt:
		return s.fireRound(ctx, cfg, round, obs, false, d.AdoptCommandID, d.AdoptAt, d.Reason, d.PostCo, now)
	case engine.FirePost:
		return s.fireRound(ctx, cfg, round, obs, true, 0, time.Time{}, "", d.PostCo, now)
	default: // FireNo
		return PumpResult{Action: mapFireNo(d.Reason), Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: d.Reason}, nil
	}
}

func mapFireNo(reason string) string {
	switch {
	case strings.Contains(reason, "could not read head"):
		return "skipped"
	case strings.Contains(reason, "min interval"):
		return "min_interval"
	case strings.Contains(reason, "account blocked"):
		return "blocked"
	case strings.Contains(reason, "fire slot busy"):
		return "lost_race"
	default:
		return "waiting"
	}
}

// abandonRound ends a round (closed/merged PR) without consuming review
// readiness. The identity guard makes a concurrent cancel or supersede between
// observe and write a benign lost race, never an abandon of a replacement round.
func (s *Service) abandonRound(ctx context.Context, round Round, reason, action string) (PumpResult, error) {
	result := PumpResult{Action: action, Repo: round.Repo, PR: round.PR, Reason: reason}
	if s.cfg.DryRun {
		return result, nil
	}
	ended := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		if !sameRound(st.Round(round.Repo, round.PR), round) {
			return ErrNoChange
		}
		st.EndRound(round.Repo, round.PR, reason)
		releaseSlot(st, QueueKey(round.Repo, round.PR), round.Token)
		ended = true
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !ended {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	return result, nil
}

// dedupeRound completes a not-yet-fired round because the bot already reviewed
// its head, leaving the completed round as the dedupe marker (v2's Fired[key]).
func (s *Service) dedupeRound(ctx context.Context, cfg Config, round Round, now time.Time, reason string) (PumpResult, error) {
	result := PumpResult{Action: "deduped", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}
	if s.cfg.DryRun {
		return result, nil
	}
	deduped := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		deduped = false
		r := st.Round(round.Repo, round.PR)
		// The marker this writes asserts that everyone the repository gates on has
		// already answered the head, so a reviewer change committed since the
		// decision voids it — and voids it permanently: dedupe posts nothing, so
		// SetReviewers cannot see it coming, the round is still queued when the
		// override lands (requeuing a queued round is a no-op), yet the marker is
		// exactly what stops the newly required reviewer from ever being asked.
		if !sameRound(r, round) || !firable(st, r, now) || reviewersChanged(st, round.Repo, cfg) {
			return ErrNoChange
		}
		if err := r.Dedupe(now); err != nil {
			return err
		}
		st.PutRound(*r)
		deduped = true
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !deduped {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	return result, nil
}

// supersedeRound retargets a queued round whose live head moved since it was
// enqueued; the fresh round fires on a later pump.
func (s *Service) supersedeRound(ctx context.Context, round Round, head string, now time.Time) (PumpResult, error) {
	result := PumpResult{Action: "requeued", Repo: round.Repo, PR: round.PR, Head: head, Reason: "head moved"}
	if s.cfg.DryRun || head == "" {
		result.Action = "skipped"
		return result, nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		if !sameRound(st.Round(round.Repo, round.PR), round) {
			return ErrNoChange
		}
		_, err := st.Supersede(round.Repo, round.PR, head, now)
		return err
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	return result, nil
}

// fireRound posts (or adopts) the review command and records the fire on the
// round, reserving the global slot under compare-and-swap. postCo lists the
// co-reviewer logins whose trigger commands are posted alongside (non-fatal
// on failure — the self-heal path retries).
func (s *Service) fireRound(ctx context.Context, cfg Config, round Round, obs engine.Observation, post bool, adoptID int64, adoptAt time.Time, reason string, postCo []string, now time.Time) (PumpResult, error) {
	key := QueueKey(round.Repo, round.PR)
	if s.cfg.DryRun {
		return PumpResult{Action: "dry_run", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
	}
	token := randomToken()

	if !post {
		// Adopt an already-posted command: reserve the slot and record the fire in
		// one write (no network post in between).
		firedAt := adoptAt.UTC()
		if firedAt.IsZero() {
			firedAt = now
		}
		recorded := false
		updated, err := s.store.Update(ctx, func(st *State) error {
			recorded = false
			if st.SlotHeld(now) {
				return ErrNoChange
			}
			r := st.Round(round.Repo, round.PR)
			// postCo below was chosen by cfg's reviewers; a change since means the
			// claims written here are for a set the operator has replaced.
			if !sameRound(r, round) || !firable(st, r, now) || reviewersChanged(st, round.Repo, cfg) {
				return ErrNoChange
			}
			if err := r.Reserve(token, s.cfg.WriterID(), now); err != nil {
				return err
			}
			if err := r.Fire(adoptID, firedAt); err != nil {
				return err
			}
			lf := firedAt
			st.LastFired = &lf
			// The rolling fair-use log. Written in the same CAS as the fire it
			// records, so a count can never include a fire that did not land.
			st.NoteObservedFire(firedAt, now)
			dl := firedAt.Add(s.feedbackWait(*st))
			r.WaitDeadline = &dl
			st.Warn = ""
			st.FireSlot = &FireSlot{Key: key, Token: token, Since: now}
			// Claim the co-reviewer posts in the SAME write: the fired state must
			// never be visible with no recorded command and no claim, or another
			// daemon can self-heal-post in the gap before fireCoTrigger below runs.
			for _, login := range postCo {
				r.ClaimCo(login, now)
			}
			// For bots crq is NOT posting because a live trigger already answers
			// this head — record its id now. The self-heal scan anchors on FiredAt
			// and would miss a command posted before the adopted CodeRabbit command,
			// posting a duplicate; recording it here keeps the round "asked". Its
			// timestamp anchors that bot's cutoff too, or a SHA-less answer that
			// landed before this adopted fire would never bind to the round.
			for _, cp := range cfg.policy().CoReviewerPolicies() {
				if hasLogin(postCo, cp.Login) || r.Co(cp.Login).CommandID != 0 {
					continue
				}
				cmds := obs.CoSeenFor(cp.Login).Commands
				if id := newestCommandID(cmds); id != 0 {
					r.SetCoCommand(cp.Login, id, commandCreatedAt(cmds, id, now))
				}
			}
			st.PutRound(*r)
			recorded = true
			return nil
		})
		if err != nil {
			return PumpResult{}, err
		}
		if !recorded {
			return PumpResult{Action: "lost_race"}, nil
		}
		s.sync(ctx, updated)
		if s.log != nil {
			s.log.Printf("fire %s@%s (adopted existing review command)", key, round.Head)
		}
		for _, login := range postCo {
			s.fireCoTrigger(ctx, cfg, round, login)
		}
		return PumpResult{Action: "fired", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
	}

	// Reserve the slot, then post the command. The reservation is this fire's
	// commit point — nothing is posted before it — so it is where the reviewer
	// configuration the decision used is revalidated.
	reserved, err := s.store.Update(ctx, func(st *State) error {
		if st.SlotHeld(now) {
			return ErrNoChange
		}
		r := st.Round(round.Repo, round.PR)
		if !sameRound(r, round) || !firable(st, r, now) || reviewersChanged(st, round.Repo, cfg) {
			return ErrNoChange
		}
		if err := r.Reserve(token, s.cfg.WriterID(), now); err != nil {
			return err
		}
		st.FireSlot = &FireSlot{Key: key, Token: token, Since: now}
		st.PutRound(*r)
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if reserved.FireSlot == nil || reserved.FireSlot.Token != token {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, reserved)

	// cfg, not s.cfg: the decision that reserved the account's quota was made
	// from the resolved configuration, so the command posted has to be the one it
	// decided for. Posting this host's startup value instead asked the previous
	// primary for a review the round is not waiting for, and the round then timed
	// out waiting for a bot nobody addressed.
	comment, err := s.gh.PostIssueComment(ctx, round.Repo, round.PR, cfg.ReviewCommand)
	if err != nil {
		updated, uerr := s.store.Update(ctx, func(st *State) error {
			r := st.Round(round.Repo, round.PR)
			if r == nil || r.Token != token {
				return ErrNoChange
			}
			if rerr := r.AwaitRetry(now.Add(postFailureBackoff), "failed to post review command: "+err.Error(), now); rerr != nil {
				return rerr
			}
			releaseSlot(st, key, token)
			st.Warn = "failed to post review command: " + err.Error()
			st.PutRound(*r)
			return nil
		})
		if uerr == nil {
			s.sync(ctx, updated)
		}
		return PumpResult{Action: "post_failed", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: err.Error()}, err
	}
	// Baseline the fire on the comment's GitHub timestamp, not a local clock that
	// may run ahead of GitHub's — a completion landing in the same second must
	// still count against a strict After check.
	firedAt := comment.CreatedAt.UTC()
	if firedAt.IsZero() {
		firedAt = now
	}
	// Post the co-reviewer triggers before recording so their ids land in the
	// same fire write. A failed post returns 0 (logged) and the self-heal path
	// retries.
	var coPosts []coPost
	for _, login := range postCo {
		if id, at := s.postCoTrigger(ctx, cfg, round, login); id != 0 {
			coPosts = append(coPosts, coPost{login: login, id: id, at: at})
		}
	}
	updated, err := s.recordFire(ctx, cfg, round, token, comment.ID, coPosts, firedAt, now)
	if err != nil {
		if errors.Is(err, ErrNoChange) {
			return PumpResult{Action: "lost_race"}, nil
		}
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	if s.log != nil {
		s.log.Printf("fire %s@%s (posted %s)", key, round.Head, strings.TrimSpace(cfg.ReviewCommand))
	}
	return PumpResult{Action: "fired", Repo: round.Repo, PR: round.PR, Head: round.Head}, nil
}

// coPost is one posted (or to-post) co-reviewer trigger comment.
type coPost struct {
	login string
	id    int64
	at    time.Time
}

func hasLogin(logins []string, login string) bool {
	for _, have := range logins {
		if dialect.NormalizeBotName(have) == dialect.NormalizeBotName(login) {
			return true
		}
	}
	return false
}

// coCommandFor resolves the trigger comment body crq posts for login.
func (c Config) coCommandFor(login string) string {
	for _, cb := range c.CoBots {
		if dialect.NormalizeBotName(cb.Login) == dialect.NormalizeBotName(login) {
			return cb.Command
		}
	}
	return ""
}

// fireCoOnly handles a round whose primary is already finished, disabled, or
// unavailable for this head but whose gating co-reviewers have not answered.
// It posts ONLY their trigger commands and records the round as fired with the
// first trigger as the CommandID anchor. No primary quota is spent and therefore
// NO FireSlot is taken: the per-round trigger claims (CoBots[login].ClaimedAt,
// CAS-set before the network post) are the concurrency guard.
func (s *Service) fireCoOnly(ctx context.Context, cfg Config, round Round, logins []string, reason string, now time.Time) (PumpResult, error) {
	key := QueueKey(round.Repo, round.PR)
	if s.cfg.DryRun {
		return PumpResult{Action: "dry_run", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
	}
	var claimed []string
	updated, err := s.store.Update(ctx, func(st *State) error {
		claimed = claimed[:0]
		r := st.Round(round.Repo, round.PR)
		// The claim is what authorizes the posts below, so the reviewer set that
		// chose them must still be the configured one when it commits.
		if !sameRound(r, round) || !firable(st, r, now) || reviewersChanged(st, round.Repo, cfg) {
			return ErrNoChange
		}
		for _, login := range logins {
			c := r.Co(login)
			if c.CommandID != 0 {
				continue
			}
			if c.ClaimedAt != nil && now.Sub(c.ClaimedAt.UTC()) < triggerClaimTTL {
				continue
			}
			r.ClaimCo(login, now)
			claimed = append(claimed, login)
		}
		if len(claimed) == 0 {
			return ErrNoChange
		}
		st.PutRound(*r)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return PumpResult{}, err
	}
	if len(claimed) == 0 {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)

	var posts []coPost
	for _, login := range claimed {
		if id, at := s.postCoTrigger(ctx, cfg, round, login); id != 0 {
			posts = append(posts, coPost{login: login, id: id, at: at})
		}
	}
	if len(posts) == 0 {
		// Every post failed: park with the post-failure cooldown. The claims
		// stay — their TTL is the retry backoff.
		postErr := errors.New("failed to post co-reviewer trigger command")
		parked, uerr := s.store.Update(ctx, func(st *State) error {
			r := st.Round(round.Repo, round.PR)
			if !sameRound(r, round) {
				if archived := archivedRound(st, round); archived != nil {
					// The network calls returned, so no poster remains behind
					// these claims and an archived round cannot retry them.
					for _, login := range claimed {
						archived.ClearCoClaim(login)
					}
					return nil
				}
				return ErrNoChange
			}
			// AwaitRetry is only legal from reserved/fired/reviewing; pass a
			// still-queued round through Reserve (a pure phase transition — no
			// global FireSlot is registered) so the park is a legal edge.
			if r.FireEligible(now) {
				if rerr := r.Reserve(randomToken(), s.cfg.WriterID(), now); rerr != nil {
					return rerr
				}
			}
			if rerr := r.AwaitRetry(now.Add(postFailureBackoff), postErr.Error(), now); rerr != nil {
				return rerr
			}
			st.Warn = postErr.Error()
			st.PutRound(*r)
			return nil
		})
		if uerr == nil {
			s.sync(ctx, parked)
		}
		return PumpResult{Action: "post_failed", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: postErr.Error()}, postErr
	}
	firedAt := posts[0].at.UTC()
	if firedAt.IsZero() {
		firedAt = now
	}
	// The first trigger comment anchors the round as its fired command; every
	// posted trigger is recorded per bot so self-heal will not re-post them. A
	// partially failed post set keeps its claims and heals after the TTL.
	recorded := false
	updated, err = s.store.Update(ctx, func(st *State) error {
		recorded = false
		r := st.Round(round.Repo, round.PR)
		if !sameRound(r, round) {
			r = archivedRound(st, round)
			if r == nil {
				return ErrNoChange
			}
			for _, login := range claimed {
				r.ClearCoClaim(login)
			}
			for _, p := range posts {
				r.RecordPosted(p.login, p.id, p.at)
				if r.Co(p.login).CommandID == 0 {
					r.SetCoCommand(p.login, p.id, p.at)
				}
			}
			recorded = true
			return nil
		}
		if r.FireEligible(now) {
			if err := r.Reserve(randomToken(), s.cfg.WriterID(), now); err != nil {
				return err
			}
			if err := r.Fire(posts[0].id, firedAt); err != nil {
				return err
			}
			r.Token = ""
			dl := firedAt.Add(s.feedbackWait(*st))
			r.WaitDeadline = &dl
			r.CoOnly = true // no primary review was requested for this round
			st.Warn = ""
		}
		for _, p := range posts {
			// Recorded against the co-reviewer it was addressed to, including the
			// one anchoring the round: a co-only round's CommandID is that same
			// comment, and calling it the primary's would let unrelated CodeRabbit
			// activity pass for the answer this trigger is still waiting on.
			r.RecordPosted(p.login, p.id, p.at)
			if r.Co(p.login).CommandID == 0 {
				r.SetCoCommand(p.login, p.id, p.at)
			}
		}
		st.PutRound(*r)
		recorded = true
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !recorded {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	if s.log != nil {
		posted := make([]string, 0, len(posts))
		for _, post := range posts {
			posted = append(posted, post.login)
		}
		s.log.Printf("fire %s@%s (%s; posted co-reviewer triggers for %s)", key, round.Head, reason, strings.Join(posted, ","))
	}
	return PumpResult{Action: "fired", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
}

// newestCommandID returns the id of the most recently created adoptable command,
// or 0 when there are none.
func newestCommandID(cmds []engine.CommandSeen) int64 {
	var best engine.CommandSeen
	for _, c := range cmds {
		if best.ID == 0 || c.CreatedAt.After(best.CreatedAt) {
			best = c
		}
	}
	return best.ID
}

// commandCreatedAt resolves a command comment's creation time, falling back
// to the caller's clock when GitHub omitted it.
func commandCreatedAt(commands []engine.CommandSeen, id int64, fallback time.Time) time.Time {
	for _, command := range commands {
		if command.ID == id && !command.CreatedAt.IsZero() {
			return command.CreatedAt.UTC()
		}
	}
	return fallback.UTC()
}

// fireCoReviewWait bounds a co-review wait: CodeRabbit already reviewed the head
// but a gating Codex has not yet, and crq must not post (Codex auto-reviews, or a
// command is already outstanding). Leaving the round queued with no WaitDeadline
// is the hang — Wait then loops forever. Park it in reviewing with a WaitDeadline
// instead: no slot is reserved and no command is posted. An existing `@codex
// review` command on the PR is adopted as the round's CodexCommandID so the
// self-heal path (which anchors on the round's fire time, later than a pre-existing
// command) does not re-post it.
func (s *Service) fireCoReviewWait(ctx context.Context, cfg Config, round Round, obs engine.Observation, reason string, now time.Time) (PumpResult, error) {
	result := PumpResult{Action: "waiting", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}
	if s.cfg.DryRun {
		return result, nil
	}
	// The anchor is the wait's evidence floor, so it must be when this HEAD
	// appeared — not when crq happened to notice it. Defaulting to now was a
	// repeatable 20-minute timeout on a primary-unavailable round: with no
	// CodeRabbit review to anchor on and no trigger posted (the co-reviewer
	// auto-reviews), the floor landed after the co-reviewer's existing answer
	// for this head, so the wait could never be satisfied and the bot had no
	// reason to speak again. The candidates below only narrow it further.
	anchor := now
	if !obs.HeadAt.IsZero() {
		anchor = obs.HeadAt
	}
	// cfg.Bot, not this process's startup one: the primary is a fleet setting
	// like any other, and reading the stale one made a review by the primary
	// somebody had just changed fail to move the floor — so with no HeadAt to
	// fall back on, a co-reviewer answer that had already arrived sat below it
	// and the round waited out its timeout for an answer it already had.
	for _, rv := range obs.Reviews {
		if isConfiguredBotLogin(cfg.Bot, rv.Bot) && rv.Commit != "" && strings.HasPrefix(rv.Commit, round.Head) &&
			!rv.SubmittedAt.IsZero() && rv.SubmittedAt.Before(anchor) {
			anchor = rv.SubmittedAt
		}
	}
	type adoptCmd struct {
		login string
		id    int64
		at    time.Time
	}
	var adopts []adoptCmd
	for _, cp := range cfg.policy().CoReviewerPolicies() {
		cmds := obs.CoSeenFor(cp.Login).Commands
		id := newestCommandID(cmds)
		if id == 0 {
			continue
		}
		at := commandCreatedAt(cmds, id, now)
		adopts = append(adopts, adoptCmd{login: cp.Login, id: id, at: at})
		// The anchor is the round's evidence FLOOR, so it takes the earliest
		// candidate. Overwriting it per co-reviewer both discarded the primary
		// head-review floor above and left whichever policy config happened to
		// list last — so with Codex commanded at 10:00 and Bugbot at 10:30 a
		// SHA-less Codex clean summary at 10:05 fell below the floor and was
		// ignored, exactly the loss this anchor exists to prevent.
		if at.Before(anchor) {
			anchor = at
		}
	}
	changed := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(round.Repo, round.PR)
		if !sameRound(r, round) || !firable(st, r, now) {
			return ErrNoChange
		}
		deadline := now.Add(s.feedbackWait(*st))
		if err := r.AwaitCoReview(deadline, anchor); err != nil {
			return err
		}
		r.CoOnly = true // waiting on co-reviewers; nothing was asked of the primary
		// Existing trigger commands are adopted as the round's command anchors
		// so the self-heal path (which anchors on the round's fire time, later
		// than a pre-existing command) does not re-post them.
		for _, a := range adopts {
			if r.Co(a.login).CommandID == 0 {
				r.SetCoCommand(a.login, a.id, a.at)
			}
		}
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !changed {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	return result, nil
}

// recordFire records the posted command on the reserved round, with a 30s retry
// on a transient state-write failure so a fired command is never lost. coPosts
// are the co-reviewer trigger comments posted alongside (empty when none),
// recorded in the same write.
//
// cfg is the configuration the fire was decided and posted from: the command is
// attributed to the primary that configuration names, not to whichever one this
// process happened to start with.
func (s *Service) recordFire(ctx context.Context, cfg Config, round Round, token string, commandID int64, coPosts []coPost, firedAt, now time.Time) (State, error) {
	record := func(c context.Context) (State, bool, error) {
		recorded := false
		st, err := s.store.Update(c, func(st *State) error {
			recorded = false
			r := st.Round(round.Repo, round.PR)
			if !sameRound(r, round) || st.FireSlot == nil || st.FireSlot.Token != token || r.Token != token {
				return ErrNoChange
			}
			if err := r.Fire(commandID, firedAt); err != nil {
				return err
			}
			// Recorded as crq's own whatever the round then does with it: the
			// comment is on the PR either way, and the record is the only proof
			// crq (rather than a person) wrote it.
			r.RecordPosted(cfg.Bot, commandID, firedAt)
			for _, p := range coPosts {
				r.RecordPosted(p.login, p.id, p.at)
				if r.Co(p.login).CommandID == 0 {
					r.SetCoCommand(p.login, p.id, p.at)
				}
			}
			lf := firedAt
			st.LastFired = &lf
			// The rolling fair-use log. Written in the same CAS as the fire it
			// records, so a count can never include a fire that did not land.
			st.NoteFire(firedAt)
			dl := firedAt.Add(s.feedbackWait(*st))
			r.WaitDeadline = &dl
			st.Warn = ""
			st.PutRound(*r)
			recorded = true
			return nil
		})
		return st, recorded, err
	}
	st, recorded, err := record(ctx)
	if err != nil {
		retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		st, recorded, err = record(retryCtx)
	}
	if err != nil {
		return State{}, err
	}
	if !recorded {
		return st, ErrNoChange
	}
	return st, nil
}

// postCoTrigger posts login's trigger command and returns its comment id, or
// 0 on failure. A failed post is non-fatal: it logs and leaves the round's
// command unset so a later pump's self-heal retries. The fresh-fire path
// folds the returned id into recordFire's write.
func (s *Service) postCoTrigger(ctx context.Context, cfg Config, round Round, login string) (int64, time.Time) {
	command := strings.TrimSpace(cfg.coCommandFor(login))
	if command == "" {
		return 0, time.Time{}
	}
	comment, err := s.gh.PostIssueComment(ctx, round.Repo, round.PR, command)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: %s trigger post failed for %s@%s: %v (will retry on a later pump)", dialect.NormalizeBotName(login), QueueKey(round.Repo, round.PR), round.Head, err)
		}
		return 0, time.Time{}
	}
	if s.log != nil {
		s.log.Printf("fire %s@%s (posted %s)", QueueKey(round.Repo, round.PR), round.Head, command)
	}
	// The command's GitHub timestamp is the evidence anchor: a fast reply can
	// otherwise land before a local post-return clock reading and fall outside
	// the stored cutoff forever (with no repost possible).
	at := comment.CreatedAt.UTC()
	if at.IsZero() {
		at = s.clock().UTC()
	}
	return comment.ID, at
}

// fireCoTrigger posts login's trigger command for an already-fired round and
// records its id under CAS. It is used by the adopt fire path and the
// self-heal retry (the fresh-post path records the id inside recordFire
// instead). The CAS guard (same head, command still unset) makes a concurrent
// post benign.
func (s *Service) fireCoTrigger(ctx context.Context, cfg Config, round Round, login string) {
	id, at := s.postCoTrigger(ctx, cfg, round, login)
	if id == 0 {
		// Failed post: KEEP the claim — its TTL is the retry backoff. Clearing it
		// here would let the very next pump repost, bypassing triggerClaimTTL.
		// An archived round has no next pump, and this returning call proves its
		// poster is no longer in flight.
		updated, err := s.store.Update(ctx, func(st *State) error {
			if sameRound(st.Round(round.Repo, round.PR), round) {
				return ErrNoChange
			}
			r := archivedRound(st, round)
			if r == nil {
				return ErrNoChange
			}
			r.ClearCoClaim(login)
			return nil
		})
		if err == nil {
			s.sync(ctx, updated)
		}
		return
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(round.Repo, round.PR)
		// Identity guard: a same-head replacement round (new Seq) must not
		// inherit this post's result.
		if !sameRound(r, round) {
			r = archivedRound(st, round)
			if r == nil {
				return ErrNoChange
			}
		}
		r.RecordPosted(login, id, at)
		if r.Co(login).CommandID == 0 {
			r.SetCoCommand(login, id, at)
		} else {
			r.ClearCoClaim(login)
		}
		if sameRound(st.Round(round.Repo, round.PR), round) {
			st.PutRound(*r)
		}
		return nil
	})
	if err != nil {
		if s.log != nil && !errors.Is(err, ErrNoChange) {
			s.log.Printf("warning: failed to record %s command %d for %s: %v", dialect.NormalizeBotName(login), id, QueueKey(round.Repo, round.PR), err)
		}
		return
	}
	s.sync(ctx, updated)
}

// fireCoDeferred posts (or adopts) ONLY co-reviewer trigger commands for a
// round the CodeRabbit account block (or a busy slot) is holding back. Unlike
// fireCoOnly (CodeRabbit already reviewed the head) the round stays
// queued/awaiting-retry: the CodeRabbit review is still owed and fires
// normally the moment the window opens, at which point the recorded command
// ids keep the triggers from re-posting. The claim-then-post shape mirrors
// selfHealCoReviewers — this path is not serialized by the fire slot either.
func (s *Service) fireCoDeferred(ctx context.Context, cfg Config, round Round, d engine.FireDecision, now time.Time) (PumpResult, error) {
	action := func(base string) string {
		// Preserve the historical action names for the Codex-only case.
		if len(d.PostCo) <= 1 && len(d.AdoptCo) <= 1 {
			onlyCodex := true
			for _, login := range d.PostCo {
				onlyCodex = onlyCodex && dialect.IsCodexBot(login)
			}
			for login := range d.AdoptCo {
				onlyCodex = onlyCodex && dialect.IsCodexBot(login)
			}
			if onlyCodex {
				return "codex_" + base
			}
		}
		return "co_" + base
	}
	result := PumpResult{Action: action("fired"), Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: d.Reason}
	if s.cfg.DryRun {
		return result, nil
	}

	adopted, claimed := 0, []string{}
	updated, err := s.store.Update(ctx, func(st *State) error {
		adopted, claimed = 0, claimed[:0]
		r := st.Round(round.Repo, round.PR)
		// As in fireCoOnly: the claims and adoptions written here name the
		// co-reviewers cfg chose, so a reviewer change since voids them.
		if !sameRound(r, round) || (r.Phase != PhaseQueued && r.Phase != PhaseAwaitingRetry) ||
			reviewersChanged(st, round.Repo, cfg) {
			return ErrNoChange
		}
		if _, held := st.HeldPR(round.Repo, round.PR); held {
			return ErrNoChange
		}
		changed := false
		for login, cmd := range d.AdoptCo {
			c := r.Co(login)
			if c.CommandID != 0 {
				continue
			}
			// A concurrent worker may already be posting the command represented
			// by this observation. Let its fresh claim finish instead of adopting
			// and racing its state write; a stale claim no longer blocks recovery.
			if c.ClaimedAt != nil && now.Sub(c.ClaimedAt.UTC()) < triggerClaimTTL {
				continue
			}
			at := cmd.CreatedAt
			if at.IsZero() {
				at = cmd.UpdatedAt
			}
			if at.IsZero() {
				at = now
			}
			r.SetCoCommand(login, cmd.ID, at)
			adopted++
			changed = true
		}
		for _, login := range d.PostCo {
			c := r.Co(login)
			if c.CommandID != 0 {
				continue
			}
			if c.ClaimedAt != nil && now.Sub(c.ClaimedAt.UTC()) < triggerClaimTTL {
				continue
			}
			r.ClaimCo(login, now)
			claimed = append(claimed, login)
			changed = true
		}
		if !changed {
			return ErrNoChange
		}
		r.Note = "co-review requested; coderabbit deferred (account rate-limited)"
		st.PutRound(*r)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return result, err
	}
	if adopted == 0 && len(claimed) == 0 {
		result.Action = "deduped"
		result.Reason = "co-reviewer commands already claimed, adopted, or posted"
		return result, nil
	}
	s.sync(ctx, updated)
	for _, login := range claimed {
		s.fireCoTrigger(ctx, cfg, round, login)
	}
	if len(claimed) == 0 {
		result.Action = action("adopted")
	}
	return result, nil
}

// selfHealCoReviewers posts (or re-posts) co-reviewer trigger commands for a
// fired/reviewing round: an always-mode bot whose initial post failed (its
// command id still 0), or a selfheal-mode bot observed active that missed the
// head past its grace period. It runs on the daemon's progress/sweep paths;
// idempotence comes from the observation — the bot's evidence, a live trigger
// command, or an account that reviews on its own all suppress it (see
// DecideCoPost) — not a retry counter.
func (s *Service) selfHealCoReviewers(ctx context.Context, cfg Config, round Round, obs engine.Observation, now time.Time) {
	if s.cfg.DryRun || round.FiredAt == nil || obs.Head != round.Head {
		return
	}
	firedAt := round.FiredAt.UTC()
	for _, cp := range cfg.policy().CoReviewerPolicies() {
		if round.Co(cp.Login).CommandID != 0 || (dialect.IsCodexBot(cp.Login) && round.CodexCommandID != 0) {
			continue
		}
		commandPresent := engine.CoCommandSince(obs, cp.Login, firedAt)
		if !engine.DecideCoPost(round, obs, cp, commandPresent, firedAt, now) {
			continue
		}
		// Claim the post under CAS BEFORE the network call: this sweep path is
		// not serialized by the fire slot, so two concurrent pumps observing an
		// unset command would otherwise both post. A claim older than
		// triggerClaimTTL is stale (the poster died mid-flight) and may be
		// re-claimed. As in fireCoOnly, the claim is what authorizes the post, so
		// the reviewer set that chose this bot must still be the configured one
		// when it commits — otherwise a bot `crq reviewers set` has just removed
		// is asked for a review anyway.
		login := cp.Login
		claimed := false
		updated, err := s.store.Update(ctx, func(st *State) error {
			r := st.Round(round.Repo, round.PR)
			if !sameRound(r, round) || r.Co(login).CommandID != 0 || reviewersChanged(st, round.Repo, cfg) {
				return ErrNoChange
			}
			if _, held := st.HeldPR(round.Repo, round.PR); held {
				return ErrNoChange
			}
			if c := r.Co(login); c.ClaimedAt != nil && now.Sub(c.ClaimedAt.UTC()) < triggerClaimTTL {
				return ErrNoChange
			}
			r.ClaimCo(login, now)
			st.PutRound(*r)
			claimed = true
			return nil
		})
		if err != nil || !claimed {
			continue
		}
		s.sync(ctx, updated)
		s.fireCoTrigger(ctx, cfg, round, login)
	}
}

// triggerClaimTTL bounds a co-reviewer trigger claim: past it, a claim whose
// poster never recorded a command id is stale and the post may be retried.
const triggerClaimTTL = 2 * time.Minute

func (s *Service) Cancel(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	state, err := s.store.Update(ctx, func(st *State) error {
		round := st.Round(repo, pr)
		if round == nil {
			return ErrNoChange
		}
		token := round.Token
		st.EndRound(repo, pr, "cancelled")
		releaseSlot(st, QueueKey(repo, pr), token)
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, state)
	return nil
}

func (s *Service) Status(ctx context.Context) (State, string, error) {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, "", err
	}
	return state, renderDashboard(state, s.cfg), nil
}

func (s *Service) RefreshQuota(ctx context.Context) (State, error) {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, err
	}
	// Dry-run is a local process safety promise. A shared fleet setting must not
	// turn it off and let a direct `debug refresh` post or mutate quota state.
	if s.cfg.DryRun {
		return state, nil
	}
	quotaService := *s
	quotaService.cfg = s.cfg.WithFleet(state.Fleet)
	if quotaService.cfg.CalibrationPR <= 0 {
		return state, nil
	}
	now := s.clock()
	// Honor the freshness shortcut only when the last reading was conclusive. If a
	// probe is still pending (CalibAskedAt set, no reply yet), keep re-checking so a
	// late "account blocked" reply isn't ignored for the full TTL.
	if state.Account.CalibAskedAt == nil && state.Account.CheckedAt != nil && now.Sub(*state.Account.CheckedAt) < quotaService.cfg.CalibrationTTL {
		return state, nil
	}
	quota, evidenceAt, err := quotaService.readQuota(
		ctx, quotaService.calibrationIssue(state), now, state.Account.CalibAskedAt,
	)
	if err != nil {
		return state, err
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		currentCfg := s.cfg.WithFleet(st.Fleet)
		if stampChanged(st.Fleet.UpdatedAt, quotaService.cfg.FleetAt) {
			return ErrNoChange
		}
		if evidenceAt != nil && !evidenceAt.After(now.Add(-currentCfg.CalibrationTTL)) {
			// The fleet may have shortened the evidence window while the GitHub
			// read was in flight. Never make that now-stale reply look fresh by
			// stamping CheckedAt below.
			return ErrNoChange
		}
		if st.Account.CalibAskedAt == nil && st.Account.CheckedAt != nil && now.Sub(*st.Account.CheckedAt) < currentCfg.CalibrationTTL {
			return ErrNoChange
		}
		// Calibration owns only its reading fields. Edit those on the existing
		// record so the rolling fire history and members written by a newer
		// binary survive this older writer's routine probe.
		rlID, rlUpdated := st.Account.RLCommentID, st.Account.RLCommentUpdated
		prevBlock, prevRemaining := st.Account.BlockedUntil, st.Account.Remaining
		st.Account.Scope = quota.Scope
		st.Account.BlockedUntil = quota.BlockedUntil
		st.Account.Remaining = quota.Remaining
		st.Account.Source = quota.Source
		st.Account.CheckedAt = quota.CheckedAt
		st.Account.CalibAskedAt = quota.CalibAskedAt
		if st.Account.RLCommentID == 0 {
			st.Account.RLCommentID = rlID
			st.Account.RLCommentUpdated = rlUpdated
		}
		// A calibration reading must not SHORTEN a standing block, which is the
		// rule every other writer follows (engine.AcceptAccountBlock) and the one
		// place that bypassed it.
		//
		// Two ways a probe reports no block: it is still awaiting a reply, or its
		// reply carried no parseable reset. Neither is evidence the account is
		// clear, and both used to erase a window a PR's own rate-limit notice had
		// stated — after which Pump fires inside that window, which is the
		// duplicate-review behaviour this whole system exists to prevent.
		//
		// A LONGER window from the probe still wins: that is new information.
		//
		// So does a reply that states reviews are left. That is not "no block
		// observed", it is the account reporting itself available — restoring the
		// old window over it would leave state saying both at once and hold every
		// metered round until a window the bot has just contradicted expires.
		reviewsLeft := st.Account.Remaining != nil && *st.Account.Remaining > 0 &&
			st.Account.BlockedUntil == nil && st.Account.CalibAskedAt == nil
		if reviewsLeft {
			// The reading ends the old window, but the notice watermark must
			// survive. Otherwise an unchanged, windowless historical notice looks
			// new on the next observation and recreates a fallback block forever.
			// Reuse is still visible because CodeRabbit advances UpdatedAt when it
			// edits the comment for a later fire.
			st.Account.RLCommentID = rlID
			st.Account.RLCommentUpdated = rlUpdated
		}
		if !reviewsLeft && prevBlock != nil && prevBlock.After(now) &&
			(st.Account.BlockedUntil == nil || prevBlock.After(*st.Account.BlockedUntil)) {
			st.Account.BlockedUntil = prevBlock
			if st.Account.Remaining == nil {
				st.Account.Remaining = prevRemaining
			}
		}
		if st.Warn == warnRateLimited && (st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.After(now)) {
			st.Warn = ""
		}
		return nil
	})
	if err != nil {
		return State{}, err
	}
	s.sync(ctx, updated)
	return updated, nil
}

// calibrationIssue returns the calibration issue to probe: the rotated
// replacement recorded in state after a cap wedge, or the configured one.
func (s *Service) calibrationIssue(state State) int {
	if state.CalibrationIssue > 0 {
		return state.CalibrationIssue
	}
	return s.cfg.CalibrationPR
}

const calibrationIssueBody = "crq probes CodeRabbit's account-wide review quota here with `" + dialect.DefaultRateLimitCommand + "` so it never spends a real review to calibrate. Auto-created after a prior calibration thread hit GitHub's 2500-comment cap. Managed by crq — safe to leave alone."

// rotateCalibration creates a fresh calibration issue and records its number in
// the shared state so the whole fleet abandons a thread that hit GitHub's hard
// 2500-comment cap.
func (s *Service) rotateCalibration(ctx context.Context, oldIssue int) (int, error) {
	issue, err := s.gh.CreateIssue(ctx, s.cfg.GateRepo, "crq account-quota calibration", calibrationIssueBody)
	if err != nil {
		return 0, err
	}
	if issue.Number <= 0 {
		return 0, fmt.Errorf("calibration rotation: created issue has no number")
	}
	if _, err := s.store.Update(ctx, func(st *State) error {
		if st.CalibrationIssue == issue.Number {
			return ErrNoChange
		}
		st.CalibrationIssue = issue.Number
		return nil
	}); err != nil && !errors.Is(err, ErrNoChange) {
		return 0, err
	}
	if s.log != nil {
		s.log.Printf("calibration issue #%d hit the comment cap; rotated to fresh issue #%d", oldIssue, issue.Number)
	}
	return issue.Number, nil
}

func (s *Service) readQuota(
	ctx context.Context, issue int, now time.Time, pendingAsked *time.Time,
) (AccountQuota, *time.Time, error) {
	quota := AccountQuota{Scope: strings.Join(s.cfg.Scope, ","), Source: "calibrate", CheckedAt: &now}
	cutoff := now.Add(-s.cfg.CalibrationTTL)
	keepAfter := now.Add(-2 * s.cfg.CalibrationTTL)
	if reply, ok, err := s.latestCalibrationReply(ctx, issue, cutoff); err != nil {
		return quota, nil, err
	} else if ok {
		remaining, reset := dialect.ParseQuota(reply.Body, reply.UpdatedAt)
		quota.Remaining = remaining
		quota.BlockedUntil = reset
		s.pruneCalibration(ctx, issue, keepAfter, 80)
		return quota, &reply.UpdatedAt, nil
	}
	// A probe from a previous call is still pending and not yet stale, and no
	// reply to it was found: keep waiting for its (possibly late) reply instead of
	// posting another probe every cycle.
	if pendingAsked != nil && pendingAsked.After(cutoff) {
		quota.CalibAskedAt = pendingAsked
		return quota, nil, nil
	}
	asked, err := s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
	if err != nil {
		// The calibration thread hit GitHub's 2500-comment cap. Prune old probe
		// comments and retry once; if pruning can't drop under the cap, rotate to a
		// fresh issue and retry there instead of failing every cycle.
		if isCommentCapError(err) {
			if pruned := s.pruneCalibration(ctx, issue, keepAfter, 100); pruned > 0 {
				asked, err = s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
			}
			if err != nil && isCommentCapError(err) {
				if newIssue, rerr := s.rotateCalibration(ctx, issue); rerr == nil {
					issue = newIssue
					asked, err = s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
				} else if s.log != nil {
					s.log.Printf("calibration rotation failed: %v", rerr)
				}
			}
		}
		if err != nil {
			if s.log != nil {
				s.log.Printf("calibration probe on #%d failed: %v", issue, err)
			}
			return quota, nil, err
		}
	}
	quota.CalibAskedAt = &asked.CreatedAt
	for i := 0; i < 6; i++ {
		select {
		case <-ctx.Done():
			return quota, nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		reply, ok, err := s.latestCalibrationReply(ctx, issue, asked.CreatedAt.Add(-time.Second))
		if err != nil {
			return quota, nil, err
		}
		if ok {
			remaining, reset := dialect.ParseQuota(reply.Body, reply.UpdatedAt)
			quota.Remaining = remaining
			quota.BlockedUntil = reset
			quota.CalibAskedAt = nil
			s.pruneCalibration(ctx, issue, keepAfter, 80)
			return quota, &reply.UpdatedAt, nil
		}
	}
	return quota, nil, nil
}

// pruneCalibration deletes crq's old calibration probe comments and CodeRabbit's
// replies so the thread never reaches GitHub's hard 2500-comment cap.
func (s *Service) pruneCalibration(ctx context.Context, issue int, keepAfter time.Time, max int) int {
	if issue <= 0 || max <= 0 {
		return 0
	}
	comments, err := s.gh.ListIssueCommentsPage(ctx, s.cfg.GateRepo, issue, 1, 100)
	if err != nil {
		if s.log != nil {
			s.log.Printf("calibration prune: list failed: %v", err)
		}
		return 0
	}
	deleted := 0
	for _, c := range comments {
		if deleted >= max {
			break
		}
		if c.CreatedAt.After(keepAfter) || c.UpdatedAt.After(keepAfter) {
			continue
		}
		if !s.isCalibrationNoise(c) {
			continue
		}
		if err := s.gh.DeleteIssueComment(ctx, s.cfg.GateRepo, c.ID); err != nil {
			if s.log != nil {
				s.log.Printf("calibration prune: delete %d failed: %v", c.ID, err)
			}
			break
		}
		deleted++
	}
	if deleted > 0 && s.log != nil {
		s.log.Printf("calibration prune: removed %d old comment(s) from #%d", deleted, issue)
	}
	return deleted
}

// isCalibrationNoise reports whether a comment is a spent calibration artifact:
// one of crq's account-quota probes or a CodeRabbit auto-reply.
func (s *Service) isCalibrationNoise(c ghapi.IssueComment) bool {
	if strings.TrimSpace(c.Body) == strings.TrimSpace(s.cfg.RateLimitCommand) {
		return true
	}
	return s.cfg.isConfiguredBot(c.User.Login) && strings.Contains(c.Body, s.cfg.CalibrationMarker)
}

func (s *Service) latestCalibrationReply(ctx context.Context, issue int, after time.Time) (ghapi.IssueComment, bool, error) {
	comments, err := s.gh.ListIssueComments(ctx, s.cfg.GateRepo, issue)
	if err != nil {
		return ghapi.IssueComment{}, false, err
	}
	var best ghapi.IssueComment
	ok := false
	for _, comment := range comments {
		if !s.cfg.isConfiguredBot(comment.User.Login) || !comment.UpdatedAt.After(after) {
			continue
		}
		if !strings.Contains(comment.Body, s.cfg.CalibrationMarker) {
			continue
		}
		if !ok || comment.UpdatedAt.After(best.UpdatedAt) {
			best = comment
			ok = true
		}
	}
	return best, ok, nil
}

// isConfiguredBotLogin is isConfiguredBot for callers holding only the config
// value, and reviewedByConfiguredBot checks a ReviewedBy map with the same
// suffix tolerance.
func isConfiguredBotLogin(bot, login string) bool {
	return dialect.NormalizeBotName(login) == dialect.NormalizeBotName(bot)
}

func reviewedByConfiguredBot(reviewedBy map[string]bool, bot string) bool {
	for login, ok := range reviewedBy {
		if ok && isConfiguredBotLogin(bot, login) {
			return true
		}
	}
	return false
}

func (c Config) isConfiguredBot(login string) bool {
	return dialect.NormalizeBotName(login) == dialect.NormalizeBotName(c.Bot)
}

// notBefore reports whether t is at or after baseline. GitHub timestamps are
// second-granular, so a bot completion in the same second as the trigger must
// still count — a strict After would miss it and refire a duplicate review.
func notBefore(t, baseline time.Time) bool { return !t.Before(baseline) }

func allReviewed(reviewedBy map[string]bool) bool {
	for _, reviewed := range reviewedBy {
		if !reviewed {
			return false
		}
	}
	return true
}

func (s *Service) headShort(ctx context.Context, repo string, pr int) (string, error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return "", err
	}
	if len(pull.Head.SHA) < 9 {
		return "", fmt.Errorf("invalid head sha")
	}
	return pull.Head.SHA[:9], nil
}

// pullHead returns the PR's short head SHA and whether it is still open (neither
// closed nor merged), so a PR closed after it was queued is dropped instead of
// firing a review at a dead PR.
func (s *Service) pullHead(ctx context.Context, repo string, pr int) (head string, open bool, err error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return "", false, err
	}
	open = pull.State == "open" && !pull.Merged
	if !open {
		return "", false, nil
	}
	if len(pull.Head.SHA) < 9 {
		return "", open, fmt.Errorf("invalid head sha")
	}
	return pull.Head.SHA[:9], open, nil
}

func (s *Service) sync(ctx context.Context, state State) {
	if s.log == nil || s.cfg.DashboardIssue <= 0 {
		return
	}
	if err := s.store.SyncDashboard(ctx, state); err != nil {
		s.log.Printf("warning: dashboard sync failed: %v", err)
	}
}

func randomToken() string {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return fallbackToken(time.Now())
	}
	return hex.EncodeToString(buf[:])
}

func fallbackToken(now time.Time) string {
	return fmt.Sprintf("%016x", uint64(now.UnixNano()))
}

// isCommentCapError reports whether err is GitHub's hard cap of 2500 comments per
// issue ("Commenting is disabled on issues with more than 2500 comments").
func isCommentCapError(err error) bool {
	var api *ghapi.APIError
	if !errors.As(err, &api) {
		return false
	}
	b := strings.ToLower(api.Body)
	return strings.Contains(b, "commenting is disabled") || strings.Contains(b, "more than 2500 comments")
}

// Wait enqueues repo#pr and pumps until a review fires for its head (code 0),
// current-head feedback is already available (code 3), the wait times out (code
// 2), or the PR is closed or held (code 2). The wait IS the round: a fired/reviewing
// round for the head is the in-flight wait, a completed round is the "already
// reviewed" dedup marker, and firedMarker/waitingHead read those states off the
// round rather than a separate wait record.
// postFailureBackoff parks a round after a review-command post fails, so a
// persistent failure (auth, a 4xx, GitHub down past the client's own retries)
// retries on a bounded cadence instead of re-posting on every pump.
const postFailureBackoff = 2 * time.Minute

func (s *Service) Wait(ctx context.Context, repo string, pr int) (PumpResult, int, error) {
	repo = NormalizeRepo(repo)
	// The slot-wait timeout anchors on the injectable clock so replay tests can
	// drive it deterministically; cadence timers below stay on the wall clock
	// because they gate real sleeps.
	start := s.clock()
	enqueued := false
	var lastLog time.Time
	var lastFeedbackCheck time.Time
	// primaryUnavailable is refreshed by each feedback pass and gates the
	// quota-free bypass, so the common round pays for one observation per tick.
	var primaryUnavailable bool
	feedbackCheckEvery := queuedFeedbackCheckEvery(s.cfg.PollInterval)
	for {
		if s.cfg.WaitTimeout > 0 && s.clock().Sub(start) > s.cfg.WaitTimeout {
			return PumpResult{Action: "timeout", Repo: repo, PR: pr}, 2, nil
		}
		if !enqueued {
			result, err := s.Enqueue(ctx, repo, pr)
			if err != nil {
				return PumpResult{}, 1, err
			}
			if result.Held {
				// Code 0, not 2. The loop's exit codes are frozen — 0 converged
				// or skipped, 10 findings, 2 TIMEOUT — and a hold is none of the
				// last: it is indefinite and ends when a person says so, while 2
				// tells every caller scripted against that contract to retry. It
				// is the same shape as "skipped": terminal for this run, and the
				// status field is what says which.
				return PumpResult{
					Action: "held",
					Repo:   repo,
					PR:     pr,
					Head:   result.Head,
					Reason: result.Reason,
				}, 0, nil
			}
			enqueued = result.Queued || result.AlreadyQueued
			if result.Deduped {
				state, _, err := s.store.Load(ctx)
				if err != nil {
					return PumpResult{}, 1, err
				}
				if state.WaitingHead(repo, pr) == result.Head {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				report, err := s.Feedback(ctx, repo, pr)
				if err != nil {
					return PumpResult{}, 1, err
				}
				if len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 || allReviewed(report.ReviewedBy) {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				// A real primary review at this head is NOT a poisoned marker even with
				// a co-bot pending (a deliberate dedupe when Codex is unobtainable).
				// Deleting it would requeue the same head into ack-and-dedupe churn.
				// The primary is the report's, not this process's startup one: a
				// review by a primary the fleet changed would otherwise read as
				// nobody's, and the marker it justifies would be deleted and rebought.
				if reviewedByConfiguredBot(report.ReviewedBy, report.config.Bot) {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				// A completed round at this head with no real head review is a poisoned
				// dedup marker (a mistaken completion). Drop it and enqueue the real
				// replacement review.
				updated, err := s.store.Update(ctx, func(st *State) error {
					r := st.Round(repo, pr)
					if r == nil || r.Head != result.Head || r.Phase != PhaseCompleted {
						return ErrNoChange
					}
					st.EndRound(repo, pr, "repair: completed head lacked a real review")
					return nil
				})
				if err != nil {
					return PumpResult{}, 1, err
				}
				s.sync(ctx, updated)
				enqueued = false
				continue
			}
		}
		if lastFeedbackCheck.IsZero() || time.Since(lastFeedbackCheck) >= feedbackCheckEvery {
			report, err := s.Feedback(ctx, repo, pr)
			if err != nil {
				return PumpResult{}, 1, err
			}
			lastFeedbackCheck = time.Now()
			primaryUnavailable = report.PrimaryUnavailable
			// Return current-head findings immediately, plus a delayed review
			// attached to the previous commit when it actually arrived after this
			// round was queued. The latter is real new feedback, not the old carried
			// prompt that must remain excluded from replacement-review gating.
			state, _, err := s.store.Load(ctx)
			if err != nil {
				return PumpResult{}, 1, err
			}
			enqueuedAt := time.Time{}
			if round := state.Round(repo, pr); round != nil && round.Head == report.Head {
				enqueuedAt = round.EnqueuedAt
			}
			if len(engine.FindingsForActiveRound(report.Findings, report.Head, enqueuedAt)) > 0 {
				if s.log != nil {
					s.log.Printf("%s#%d feedback already available on %s; leaving review slot wait", repo, pr, report.Head)
				}
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: report.Head, Reason: "feedback already available"}, 3, nil
			}
			// A clean Codex answer on a degraded (rate-limited) round is the
			// round's verdict for now — hand off to the feedback poll loop
			// instead of spinning here until the CodeRabbit window opens.
			if report.Status == "deferred" {
				if s.log != nil {
					s.log.Printf("%s#%d codex answered on %s; coderabbit deferred — leaving review slot wait", repo, pr, report.Head)
				}
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: report.Head, Reason: "codex answered; coderabbit deferred"}, 3, nil
			}
		}
		// A round that spends no CodeRabbit quota is not a queue citizen. The Seq
		// FIFO and the FireSlot exist for exactly one purpose: serializing
		// CodeRabbit's account-wide review limit. A summary-only plan (CodeRabbit
		// Free on a private repo) never produces a review at all, so parking it
		// behind other PRs — or behind an account block that cannot apply to it —
		// is pure latency on a round whose co-reviewers are ready to run now.
		// Resolve THIS PR's round directly instead of waiting for the global pump
		// to select it.
		// Only attempt the bypass when the last feedback pass said the primary
		// will not review this head. Running it unconditionally observed the PR
		// in full and then let Pump observe the very same PR again a few lines
		// later — two round trips per tick on the hot polling loop, for a
		// bypass that applies to a small minority of rounds.
		result, handled := PumpResult{}, false
		if primaryUnavailable {
			var aerr error
			result, handled, aerr = s.advanceQuotaFree(ctx, repo, pr)
			if aerr != nil {
				return PumpResult{}, 1, aerr
			}
		}
		if !handled {
			var perr error
			result, perr = s.Pump(ctx)
			if perr != nil {
				return PumpResult{}, 1, perr
			}
		}
		state, _, err := s.store.Load(ctx)
		if err != nil {
			return PumpResult{}, 1, err
		}
		if hold, held := state.HeldPR(repo, pr); held {
			// Terminal with code 0, as above: a hold is not an elapsed wait.
			head := ""
			if round := state.Round(repo, pr); round != nil {
				head = round.Head
			}
			return PumpResult{
				Action: "held",
				Repo:   repo,
				PR:     pr,
				Head:   head,
				Reason: "held: " + hold.Reason,
			}, 0, nil
		}
		if r := state.Round(repo, pr); r != nil && (r.Phase == PhaseFired || r.Phase == PhaseReviewing) {
			// A reviewing round is in flight (a fired ack, or a bounded co-review
			// wait): advance from the slot wait into feedback polling, which the
			// WaitDeadline bounds — don't spin here.
			return PumpResult{Action: "fired", Repo: repo, PR: pr, Head: r.Head}, 0, nil
		}
		if !state.ContainsActive(repo, pr) {
			head, open, herr := s.pullHead(ctx, repo, pr)
			if herr == nil && !open {
				// PR closed/merged and dropped — nothing to review; stop the loop.
				return PumpResult{Action: "skipped", Repo: repo, PR: pr, Reason: "pr closed"}, 2, nil
			}
			if herr == nil && head != "" && state.FiredMarker(repo, pr) == head {
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: head}, 3, nil
			}
			if result.Action == "fired" && result.Repo == repo && result.PR == pr {
				return result, 0, nil
			}
			enqueued = false
			continue
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			reason := result.Reason
			if reason == "" {
				reason = result.Action
			}
			s.log.Printf("%s#%d waiting for a review slot — %s (%s elapsed)", repo, pr, reason, s.clock().Sub(start).Round(time.Second))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return PumpResult{}, 1, ctx.Err()
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func queuedFeedbackCheckEvery(poll time.Duration) time.Duration {
	if poll <= 0 {
		return 30 * time.Second
	}
	if poll < 30*time.Second {
		return poll
	}
	return 30 * time.Second
}

// sweepParkedClosed abandons one waiting round whose PR has been closed or
// merged. One pull read per pump, ETag-cached, rotating across candidates.
//
// Every waiting round is a candidate, not only the parked ones. NextEligible
// returns the FRONT of the queue and the pump examines that round alone, so a
// round is invisible for as many reasons as it can be behind something: a
// cooldown or a hold on itself, and — the one this missed — an account-blocked
// round ahead of it. Four merged pull requests sat in the rendered queue for an
// afternoon behind one blocked round, and every pump reported that round again
// rather than looking past it.
//
// The pump's own closed check stays where it is. It drops the front before the
// quota and pacing gates, which is what keeps a merged PR from holding the slot
// it is about to be given; this one reaches the rest.
func (s *Service) sweepParkedClosed(ctx context.Context, st State) (PumpResult, bool, error) {
	var keys []string
	for key := range st.Rounds {
		r := st.Rounds[key]
		if r.Phase == PhaseAwaitingRetry || r.Phase == PhaseQueued {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return PumpResult{}, false, nil
	}
	// Rotate the inspected candidate across pumps: always taking the oldest
	// would let one long-cooldown open PR starve the closed-PR check for every
	// parked round behind it. In-memory rotation is enough — only the leader
	// daemon sweeps, and a restart merely restarts the cycle.
	sort.Strings(keys)
	next := keys[0]
	for _, k := range keys {
		if k > s.lastParkedSweep {
			next = k
			break
		}
	}
	s.lastParkedSweep = next
	r := st.Rounds[next]
	target := &r
	_, open, err := s.pullHead(ctx, target.Repo, target.PR)
	if err != nil {
		return PumpResult{}, false, err
	}
	if open {
		return PumpResult{}, false, nil
	}
	if s.cfg.DryRun {
		return PumpResult{
			Action: "skipped",
			Repo:   target.Repo,
			PR:     target.PR,
			Head:   target.Head,
			Reason: "pr closed",
		}, true, nil
	}
	res, err := s.abandonRound(ctx, *target, "pr closed", "skipped")
	return res, true, err
}

// noteCoAnswers records which reviewers have actually answered for this head,
// from an observation crq has already paid for.
//
// It exists because nothing else in the round says a bot did anything. The
// trigger bookkeeping — the command crq posted, the claim it took — is all
// about crq, so a bot with no account behind it looks identical to one working
// perfectly: crq asks, records that it asked, and nothing answers. These are the
// only fields that can tell those apart, which is what the bot guide's setup
// status is read from.
//
// The primary is recorded here too, by the same generic head-evidence test and
// for exactly the same reason. Its round carries no per-bot entry, so the only
// thing left to read it from was the phase — and a required set that omits the
// primary completes on its co-reviewers' answers while the primary has done
// nothing but acknowledge the command.
func (s *Service) noteCoAnswers(ctx context.Context, cfg Config, round Round, obs engine.Observation, now time.Time) {
	if s.cfg.DryRun {
		return // DryRun writes nothing, bookkeeping included
	}
	var answered []string
	for _, cb := range cfg.CoBots {
		// Setup status is not head-scoped: activity on an earlier head still
		// proves that the reviewer is installed and working.
		if engine.CoReviewerActive(obs, cb.Login) {
			answered = append(answered, cb.Login)
		}
	}
	primary := cfg.Bot != "" &&
		(engine.CoReviewedHead(obs, cfg.Bot) || engine.PrimaryCompletedRound(round, obs, cfg.policy()))
	if len(answered) == 0 && !primary {
		return
	}
	if _, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(round.Repo, round.PR)
		if r == nil || !sameRound(r, round) {
			return ErrNoChange
		}
		before, beforePrimary := r.CoBots, r.PrimaryAnsweredAt
		for _, login := range answered {
			r.NoteCoAnswer(login, now)
		}
		if primary {
			r.NotePrimaryAnswer(cfg.Bot, now)
		}
		if sameCoAnswers(before, r.CoBots) && beforePrimary == r.PrimaryAnsweredAt {
			return ErrNoChange
		}
		st.PutRound(*r)
		return nil
	}); err != nil && !errors.Is(err, ErrNoChange) && s.log != nil {
		// Display-only bookkeeping: worth a line, never worth failing a round.
		s.log.Printf("warning: recording reviewer answers for %s#%d: %v", round.Repo, round.PR, err)
	}
}

func sameCoAnswers(before, after map[string]CoBotRound) bool {
	for login, a := range after {
		b := before[login]
		switch {
		case a.AnsweredAt == nil && b.AnsweredAt == nil:
		case a.AnsweredAt == nil || b.AnsweredAt == nil:
			return false
		case !a.AnsweredAt.Equal(*b.AnsweredAt):
			return false
		}
	}
	return true
}

// LoadState reads the current state, for callers that need it to resolve a
// setting without acting on it.
func (s *Service) LoadState(ctx context.Context) (State, error) {
	st, _, err := s.store.Load(ctx)
	return st, err
}

// noteTitles records pull-request titles on rounds that already exist.
//
// A round is created with the title the scan saw, but a round already at the
// current head is never a candidate, so it would never be written again and
// would read as a bare number for ever. This is the one write that fixes that,
// and it converges: once every round has its title, nothing changes and the
// CAS reports no change.
//
// Best-effort by construction. A title is what a list SAYS, not what the queue
// decides, so failing a pass over one would trade something that matters for
// something that does not.
func (s *Service) noteTitles(ctx context.Context, items []queueCandidate) {
	if s.cfg.DryRun || len(items) == 0 {
		return
	}
	_, err := s.store.Update(ctx, func(st *State) error {
		changed := false
		for _, it := range items {
			r := st.Round(NormalizeRepo(it.Repo), it.PR)
			if r == nil || it.Title == "" || r.Title == it.Title {
				continue
			}
			updated := *r
			updated.Title = it.Title
			st.PutRound(updated)
			changed = true
		}
		if !changed {
			return ErrNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) && s.log != nil {
		s.log.Printf("warning: recording pull request titles: %v", err)
	}
}
