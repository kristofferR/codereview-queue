package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// EnrollmentView is one repository's enrollment as it will actually be applied.
type EnrollmentView struct {
	Repo string `json:"repo"`
	// Source is where the answer comes from, which matters more than the answer:
	// a repository forced on by a host's env cannot be turned off from here, and
	// saying "managed" would invite someone to try.
	//
	//	state    — a record in shared state decided it
	//	env      — this host's CRQ_REPOS lists it, with no record either way
	//	excluded — this host's CRQ_EXCLUDE names it (absolute; a kill switch)
	//	scope    — no allow-list at all, so everything in CRQ_SCOPE is reviewed
	//	off      — no record, no env mention, and an allow-list that omits it
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	// EnvConflict says the host's env and the shared record disagree. The record
	// wins, but silently overriding a file someone edited is how a fleet grows a
	// mystery, so it is reported.
	EnvConflict bool `json:"env_conflict,omitempty"`
	// ClearEnables says removing this shared record hands the repository to an
	// env/scope policy that reviews it. The dashboard previews that clear as an
	// enable because it can enqueue and spend against the backlog.
	ClearEnables bool     `json:"clear_enables,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	By           string   `json:"by,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
	Lagging      []string `json:"lagging_hosts,omitempty"`
}

// Enrollment reports whether crq reviews repo, and why.
func (s *Service) Enrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return EnrollmentView{}, err
	}
	return s.enrollmentOf(st, repo), nil
}

// enrollmentOf resolves one repository against the record and this host's env.
//
// Precedence, and the reasoning for it: CRQ_EXCLUDE is absolute, because it is a
// per-host kill switch and the machine that has one usually has a reason the
// fleet does not know. Otherwise an explicit record wins in BOTH directions — an
// Off switch that does nothing but explain which file to go and edit on another
// machine is not a switch. Env alone still enrolls, so nothing changes for a
// fleet that never touches this.
func (s *Service) enrollmentOf(st State, repo string) EnrollmentView {
	repo = NormalizeRepo(repo)
	cfg := s.cfg.WithFleet(st.Fleet)
	view := EnrollmentView{Repo: repo}
	inEnv := cfg.AllowRepos[repo]
	switch {
	case cfg.ExcludeRepos[repo]:
		view.Source, view.Enabled = "excluded", false
		return view
	case repo == NormalizeRepo(cfg.GateRepo):
		// The gate repository holds the queue's own state and dashboard;
		// reviewing it would be crq reviewing its own bookkeeping.
		view.Source, view.Enabled = "excluded", false
		return view
	}
	if rec, ok := st.Enrollment(repo); ok {
		view.Source, view.Enabled = "state", rec.Enabled
		view.Reason, view.By = rec.Reason, rec.By
		if rec.UpdatedAt != nil {
			view.UpdatedAt = rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		// Only one direction is a disagreement: a record that turns off a
		// repository a host's CRQ_REPOS lists. A record that turns one ON that
		// env never mentioned is the feature working, not a conflict.
		view.EnvConflict = inEnv && !rec.Enabled
		view.ClearEnables = !rec.Enabled &&
			(inEnv || (len(cfg.AllowRepos) == 0 && repoInScope(cfg, repo)))
		// The autofix watcher reads this record too, and it holds neither the
		// leader lease nor the fire slot — so an old one went on scanning a
		// repository the off switch had just abandoned while the save reported
		// no lagging host at all. Autoreview needs no naming here: it scans only
		// as leader, which is an identity LaggingWriters already covers.
		view.Lagging = st.LaggingRoleWriters(CapsEnrollment, s.clock().UTC(), "autofix")
		return view
	}
	switch {
	case inEnv:
		view.Source, view.Enabled = "env", true
	case len(cfg.AllowRepos) == 0:
		view.Source, view.Enabled = "scope", true
	default:
		view.Source, view.Enabled = "off", false
	}
	return view
}

// SetEnrollment records whether crq reviews repo. Turning one off needs a
// reason: the repository disappears from every queue, and "why did this stop
// being reviewed" is a question the fleet should be able to answer itself.
func (s *Service) SetEnrollment(ctx context.Context, repo string, enabled bool, reason string) (EnrollmentView, error) {
	return s.SetEnrollmentAt(ctx, repo, enabled, reason, nil)
}

// SetEnrollmentAt records an enrollment only if the shared state still has the
// revision a dashboard preview described. CLI callers use SetEnrollment and
// keep the ordinary latest-state behavior.
func (s *Service) SetEnrollmentAt(ctx context.Context, repo string, enabled bool, reason string, expectedRev *int64) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	if !enabled && strings.TrimSpace(reason) == "" {
		return EnrollmentView{}, errors.New("turning a repository off needs --reason (every screen that shows it will show why)")
	}
	if repo == NormalizeRepo(s.cfg.GateRepo) {
		return EnrollmentView{}, fmt.Errorf("%s is the gate repository: it holds crq's own state and dashboard", repo)
	}
	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if expectedRev != nil && st.Rev != *expectedRev {
			return fmt.Errorf("fleet state moved from revision %d to %d; preview enrollment again", *expectedRev, st.Rev)
		}
		if enabled && s.cfg.WithFleet(st.Fleet).ExcludeRepos[repo] {
			return fmt.Errorf("%s is in the fleet CRQ_EXCLUDE policy — shared enrollment does not override it", repo)
		}
		if cur, ok := st.Enrollment(repo); ok && cur.Enabled == enabled && cur.Reason == reason {
			return ErrNoChange
		}
		if !enabled && claimedTriggerRepo(st, repo, now) {
			return errors.New("a review trigger is already being posted; wait for it to finish before turning the repository off")
		}
		// Edited, not rebuilt. A record carries the members a NEWER binary wrote
		// inside it, and a fresh value starts with none: constructing one here
		// made an older binary's toggle erase the newer setting on its next CAS,
		// which is exactly what the tolerant round trip exists to stop.
		rec, _ := st.Enrollment(repo)
		rec.Enabled, rec.Reason, rec.By, rec.UpdatedAt = enabled, reason, s.cfg.Host, &now
		st.SetEnrollment(repo, rec)
		if !enabled {
			s.abandonPendingRounds(st, repo)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

func claimedTriggerRepo(st *State, repo string, now time.Time) bool {
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) == repo && triggerPostClaimed(&round) {
			return true
		}
	}
	for i := range st.Archive {
		round := &st.Archive[i]
		if NormalizeRepo(round.Repo) == repo && archivedTriggerPostClaimed(round, now) {
			return true
		}
	}
	return false
}

// An archived round has no live queue owner left to clear a crashed poster's
// claim. Keep the network-race guard for one lease, then let administrative
// changes proceed instead of treating the tombstone as an eternal post.
func archivedTriggerPostClaimed(round *Round, now time.Time) bool {
	// Abandon deliberately preserves ReservedAt even though it clears the live
	// slot token. A primary poster can still be inside PostIssueComment after a
	// concurrent supersede archives its round, so that timestamp is the only
	// durable evidence that the network call may still resume and post.
	if round.ReservedAt != nil && now.Before(round.ReservedAt.UTC().Add(triggerClaimTTL)) {
		return true
	}
	for _, co := range round.CoBots {
		if co.ClaimedAt != nil && now.Before(co.ClaimedAt.UTC().Add(triggerClaimTTL)) {
			return true
		}
	}
	return false
}

// abandonPendingRounds drops the rounds a repository being turned off would
// otherwise still fire.
//
// The off switch is advertised as "crq does not go here", and every SCAN path
// honours it — but Pump chooses from Rounds through NextEligible, which asks
// nothing about enrollment. A repository with a queued or awaiting-retry round
// therefore kept its place in the queue and spent the shared allowance on a
// metered review minutes after being stopped.
//
// Only the phases that have not spent anything are touched. A fired or
// reviewing round already posted its command and holds the fire slot; the money
// is gone and the answer is worth having, so it is left to finish.
func (s *Service) abandonPendingRounds(st *State, repo string) {
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) != NormalizeRepo(repo) {
			continue
		}
		switch round.Phase {
		case PhaseQueued, PhaseAwaitingRetry:
		default:
			continue
		}
		// EndRound, not a bare Abandon: it archives the round rather than
		// leaving it in Rounds as a "this head was dealt with" marker. Turning
		// the repository back on then enqueues its current head again, which is
		// what an off switch that can be undone has to mean.
		st.EndRound(round.Repo, round.PR, "repository turned off")
		releaseSlot(st, QueueKey(round.Repo, round.PR), round.Token)
		if s.log != nil {
			s.log.Printf("enrollment: dropped queued round %s#%d@%s — the repository was turned off",
				round.Repo, round.PR, round.Head)
		}
	}
}

// ClearEnrollment drops the record, handing the repository back to the hosts'
// env files.
func (s *Service) ClearEnrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	return s.ClearEnrollmentAt(ctx, repo, nil)
}

// ClearEnrollmentAt binds a backlog preview to the state revision it priced.
// CLI callers omit expectedRev and retain latest-state behavior.
func (s *Service) ClearEnrollmentAt(ctx context.Context, repo string, expectedRev *int64) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if expectedRev != nil && st.Rev != *expectedRev {
			return fmt.Errorf("fleet state moved from revision %d to %d; preview enrollment again", *expectedRev, st.Rev)
		}
		if !st.ClearEnrollment(repo) {
			return ErrNoChange
		}
		// Clearing hands the repository back to this host's env, which may well
		// not list it: a record that said ON becomes an effective OFF without
		// SetEnrollment ever being called. Pump chooses from Rounds without
		// rechecking enrollment, so the queued rounds have to go the same way
		// they do when the switch is thrown explicitly — resolved from the state
		// the write lands on, not from the one before the clear.
		if !s.enrollmentOf(*st, repo).Enabled {
			if claimedTriggerRepo(st, repo, now) {
				return errors.New("a review trigger is already being posted; wait for it to finish before turning the repository off")
			}
			s.abandonPendingRounds(st, repo)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

// Enrollments lists every repository this host would act on, plus every one a
// record mentions — including the ones turned off, because an "off" nobody can
// see is how a repository quietly stops being reviewed.
func (s *Service) Enrollments(ctx context.Context) ([]EnrollmentView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.cfg.WithFleet(st.Fleet)
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		repo = NormalizeRepo(repo)
		if repo == "" || seen[repo] {
			return
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	for repo := range cfg.AllowRepos {
		add(repo)
	}
	for _, repo := range st.EnrolledRepos() {
		add(repo)
	}
	sort.Strings(repos)
	out := make([]EnrollmentView, 0, len(repos))
	for _, repo := range repos {
		out = append(out, s.enrollmentOf(st, repo))
	}
	return out, nil
}

// reviewsRepo is the one question every scan path asks: may this host enqueue
// work for repo? Sharing it is what keeps autoreview, watch and autofix from
// each growing their own slightly different answer.
func (s *Service) reviewsRepo(st State, repo string) bool {
	return s.enrollmentOf(st, repo).Enabled
}

// scanTargets is the list a pass should search: every repository enrolled by
// env or by record. scoped reports that there is no allow-list ANYWHERE, which
// is the signal to search CRQ_SCOPE owner-wide as WELL as the returned targets.
//
// The two answers are separate on purpose. An empty list with scoped false is an
// allow-list whose every entry is switched off, which is not the same thing at
// all: searching the owner for it walks the organisation's whole open-PR result
// set only for reviewsRepo to reject every row, spending the shared REST quota
// on a host that has no eligible repository to find.
func (s *Service) scanTargets(st State) (targets []string, scoped bool) {
	cfg := s.cfg.WithFleet(st.Fleet)
	// An empty CRQ_REPOS means this host searches CRQ_SCOPE owner-wide. Records
	// must not narrow that to themselves: enrolling one repository would then
	// silently stop every other one from being scanned.
	//
	// They may still WIDEN it. `crq repos add` accepts any well-shaped
	// repository, and one whose owner is outside CRQ_SCOPE is reported as
	// enrolled by every screen while no owner-wide search can ever reach it —
	// enrolled, counted, and never enqueued. Those are named individually
	// alongside the scope search rather than in place of it.
	if len(cfg.AllowRepos) == 0 {
		return s.enrolledOutsideScope(st), true
	}
	seen := map[string]bool{}
	var out []string
	for repo := range cfg.AllowRepos {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	for _, repo := range st.EnrolledRepos() {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out, false
}

// enrolledOutsideScope names the enabled repositories an owner-wide CRQ_SCOPE
// search cannot reach, so a scope-wide host searches them by name as well.
//
// Only the ones outside the scope: a repository the search already covers would
// be walked twice, and every pull request it found would be examined against
// the same scan budget twice over.
func (s *Service) enrolledOutsideScope(st State) []string {
	cfg := s.cfg.WithFleet(st.Fleet)
	var out []string
	for _, repo := range st.EnrolledRepos() {
		if repoInScope(cfg, repo) || !s.reviewsRepo(st, repo) {
			continue
		}
		out = append(out, NormalizeRepo(repo))
	}
	sort.Strings(out)
	return out
}

// repoInScope uses the same owner normalization as the owner-wide scan. It is
// shared by discovery and ClearEnables so the dashboard never claims that
// clearing an off record will enable a repository the scope search cannot find.
func repoInScope(cfg Config, repo string) bool {
	owner, _, _ := strings.Cut(NormalizeRepo(repo), "/")
	for _, scopedOwner := range cfg.Scope {
		if strings.EqualFold(strings.TrimSpace(scopedOwner), owner) {
			return true
		}
	}
	return false
}

// EnrollmentIn answers for an already-loaded state, so a caller rendering many
// repositories does not re-read the ref once per row.
func (s *Service) EnrollmentIn(st State, repo string) EnrollmentView {
	return s.enrollmentOf(st, repo)
}

// scopeRepoLimit bounds the listing per owner. Discovery asks for one sentinel
// row beyond it so exactly 1,000 repositories is not mislabeled as truncated.
const scopeRepoLimit = 1000

// ScopeRepos lists the repositories in CRQ_SCOPE, for choosing one to enroll.
// It is the one genuinely expensive read in the dashboard — a multi-page REST
// walk per owner — so callers are expected to cache it.
//
// The second return names the owners whose listing hit the bound, most recently
// pushed first being all that survived it. A picker that silently offered a
// subset was the worse failure: a repository past the bound is perfectly
// eligible for review, and nothing said why it could not be found — so the
// truncation is reported, and `crq repos add <owner>/<name>` enrolls one by name
// without any listing at all.
func (s *Service) ScopeRepos(ctx context.Context) ([]ghapi.Repo, []string, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	cfg := s.cfg.WithFleet(st.Fleet)
	seen := map[string]bool{}
	var out []ghapi.Repo
	var truncated []string
	for _, owner := range cfg.Scope {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		repos, err := s.gh.ListOwnerRepos(ctx, owner, scopeRepoLimit+1)
		if err != nil {
			return nil, nil, err
		}
		if len(repos) > scopeRepoLimit {
			truncated = append(truncated, owner)
			repos = repos[:scopeRepoLimit]
		}
		for _, r := range repos {
			key := NormalizeRepo(r.FullName)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out, truncated, nil
}

// EnrollImpact is what enrolling a repository would actually do, before it is
// done.
//
// This is the one click in the product that can spend real money: a repository
// with a dozen open pull requests becomes a dozen metered reviews on the next
// pass. The dialog that offers it should say so in the terms the bill arrives
// in, not in "7 pull requests".
type EnrollImpact struct {
	Rev  int64  `json:"rev"`
	Repo string `json:"repo"`
	// Open is every open pull request; Eligible is what would actually be
	// enqueued once the skip rules are applied. The gap between them is worth
	// showing: "12 open, 9 eligible" answers the question the raw count raises.
	Open     int `json:"open"`
	Eligible int `json:"eligible"`
	// Skipped explains the gap, per reason.
	Skipped map[string]int `json:"skipped,omitempty"`
	// Metered is how many of those would spend the shared review allowance.
	Metered int `json:"metered"`
	// Low/High bound the cost of reviewing the backlog, summed over the
	// eligible pull requests. Estimates, with the same honesty as crq cost.
	Low  float64 `json:"low"`
	High float64 `json:"high"`
	// Unpriced counts the pull requests whose cost could not be read — a spent
	// REST quota, an unreadable diff, or a reviewer crq has no price for. They
	// are reported rather than dropped:
	// leaving them out of the total makes an unknown price look like a free one,
	// which is the one thing this dialog exists to prevent.
	Unpriced int `json:"unpriced,omitempty"`
	// Unexamined counts the open pull requests this preview stopped short of
	// reading. Eligible and the cost below are then floors, not answers, and
	// saying so is the same honesty Unpriced exists for.
	Unexamined      int    `json:"unexamined,omitempty"`
	Summary         string `json:"summary"`
	PricesCheckedAt string `json:"prices_checked_at"`
}

// maxExamined bounds the per-pull-request reads one preview makes. Every
// non-skipped pull request costs a head read and a review list, so a repository
// with hundreds of them would spend hundreds of requests from the same GitHub
// quota the queue runs on — for a dialog somebody is waiting in front of.
//
// The pricing loop below inherits the bound rather than setting its own: it
// prices what was found eligible, and nothing past this can be.
const maxExamined = 25

// PreviewEnroll reports what enrolling repo would do. It costs a head read and
// a review list per examined pull request — the same questions the scan asks —
// plus a diff read for each one it prices, which is why it is a separate call
// the dialog makes rather than something every repository row carries, and why
// the examining is bounded by maxExamined.
func (s *Service) PreviewEnroll(ctx context.Context, repo string) (EnrollImpact, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollImpact{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return EnrollImpact{}, err
	}
	cfg := s.cfgFor(st, repo)
	impact := EnrollImpact{Rev: st.Rev, Repo: repo, Skipped: map[string]int{}, PricesCheckedAt: dialect.PricesCheckedAt}

	var eligible []int
	examined := 0
	err = s.gh.EachOpenPR(ctx, repo, true, func(pr ghapi.SearchPR) (bool, error) {
		if NormalizeRepo(pr.Repo) != repo {
			return false, nil
		}
		impact.Open++
		switch {
		case cfg.SkipAuthors[dialect.NormalizeBotName(strings.ToLower(pr.Author))]:
			impact.Skipped["author is skipped"]++
		case cfg.SkipsReview(pr.Body):
			impact.Skipped["carries the skip marker"]++
		case examined >= maxExamined:
			// Past the bound. The listing already carries the author and body, so
			// the two rules above stay free and keep being applied — only the
			// reads stop. Counted rather than guessed either way: calling it
			// eligible promises a bill crq never checked for, and calling it
			// skipped hides one.
			impact.Unexamined++
		default:
			examined++
			// The scan's own predicate, not just the skip rules. A repository
			// being re-enabled keeps its completed rounds, and a pull request
			// reviewed outside crq is answered for at its head — counting either
			// as newly eligible promised a backlog, and a bill for it, that the
			// next auto-review pass deduplicates on sight. One-pass campaigns
			// use the same PR-wide cap as the scan; ordinary enrollment previews
			// retain the daemon's incremental policy.
			need, _, nerr := s.reviewNeeded(ctx, st, repo, pr.Number, !cfg.OnePass, cfg.OnePass, noAnnounce)
			if nerr != nil {
				if ghapi.IsThrottled(nerr) {
					return false, nerr
				}
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				if !ghapi.IsRecoverableRead(nerr) {
					return false, nerr
				}
				impact.Unexamined++
				return false, nil
			}
			if !need {
				impact.Skipped["already reviewed at head"]++
				return false, nil
			}
			eligible = append(eligible, pr.Number)
		}
		return false, nil
	})
	if err != nil {
		return impact, err
	}
	impact.Eligible = len(eligible)

	// One read per eligible pull request, for the diff each would be reviewed
	// at. Already bounded by maxExamined above — nothing past it was examined,
	// so nothing past it can be eligible.
	//
	// The allowance is spent down as the backlog is priced, because that is what
	// enrolling would do to it: every metered review here takes one of the
	// account's included reviews, and pricing each pull request against the same
	// unchanged count called a whole backlog free on the strength of the one
	// review left for its first member.
	allowance := accountAllowance(st)
	meteredPerPR := 0
	for _, reviewer := range cfg.Reviewers {
		if dialect.EstimateCost(reviewer.Login, cfg.Bot, dialect.DiffStat{}, allowance).Metered {
			meteredPerPR++
		}
	}
	spendAllowance := func() {
		impact.Metered += meteredPerPR
		allowance.Remaining = max(0, allowance.Remaining-meteredPerPR)
	}
	for _, pr := range eligible {
		// costFrom, not Cost: the state is already in hand, and Cost would load
		// the ref again per pull request — four requests each, 100 across the
		// bound, for an answer that cannot have changed since the load above.
		pull, cerr := s.gh.GetPull(ctx, repo, pr)
		if cerr != nil {
			impact.Unpriced++
			spendAllowance()
			continue
		}
		cost := s.costWith(st, repo, pr, pull.Head.SHA, dialect.DiffStat{
			Additions:    pull.Additions,
			Deletions:    pull.Deletions,
			ChangedFiles: pull.ChangedFiles,
		}, allowance)
		if len(cost.Unpriced) > 0 {
			// A reviewer crq cannot price makes this pull request's total a
			// floor, not an answer. Adding it in would let the sentence below
			// call an unknown price free, which is what it exists to prevent.
			impact.Unpriced++
		}
		impact.Low += cost.Low
		impact.High += cost.High
		spendAllowance()
	}
	impact.Summary = enrollSummary(impact)
	return impact, nil
}

func enrollSummary(i EnrollImpact) string {
	if i.Eligible == 0 && i.Unexamined == 0 {
		return fmt.Sprintf("%d open pull request(s), none of which would be enqueued", i.Open)
	}
	// "no per-review cost" is only ever said about a backlog crq actually
	// priced. A pull request whose price could not be read is an unknown, and an
	// unknown that renders as free is the one way this sentence can mislead
	// somebody into spending money.
	cost := "no per-review cost"
	switch {
	case i.High > 0 && i.Low != i.High:
		cost = fmt.Sprintf("roughly $%.2f–$%.2f", i.Low, i.High)
	case i.High > 0:
		cost = fmt.Sprintf("about $%.2f", i.High)
	case i.Unpriced > 0:
		cost = "a cost crq could not read"
	}
	count := fmt.Sprint(i.Eligible)
	if i.Unexamined > 0 {
		count = "at least " + count
	}
	out := fmt.Sprintf("would enqueue %s of %d open pull request(s) on the next pass — %s",
		count, i.Open, cost)
	if i.Unpriced > 0 && i.High > 0 {
		out += fmt.Sprintf(", plus %d that could not be priced", i.Unpriced)
	}
	if i.Unexamined > 0 {
		// Said plainly, because both numbers above are floors once this is set:
		// a reader who takes them for the whole backlog is being told the wrong
		// price for the click this dialog is asking them to make.
		out += fmt.Sprintf("; %d more were not examined", i.Unexamined)
	}
	return out
}
