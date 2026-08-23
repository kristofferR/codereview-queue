package crq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// FleetView is the fleet's effective defaults and where each one comes from.
type FleetView struct {
	// Reviewers is the default set every repository inherits, resolved the same
	// way a per-repo view is, so the two can be read side by side.
	Reviewers []ReviewerDetail `json:"reviewers"`
	// Recorded says a fleet record exists at all; without one every value below
	// is this host's env and changing it means editing a file on each machine.
	Recorded    bool   `json:"recorded"`
	MinInterval string `json:"min_interval"`
	WeeklyLimit int    `json:"weekly_limit"`
	// AutofixDefault is whether a repository with no explicit switch is fixed.
	AutofixDefault bool `json:"autofix_default"`
	// Sources names, per setting, whether the value came from the record or from
	// this host's env — the distinction that decides whether changing it here
	// will actually change anything for the other hosts.
	Sources map[string]string `json:"sources"`
	// Overriding names the repositories that have their own answer, so a fleet
	// default can say who it does NOT reach. A count with no names is a number
	// you cannot act on.
	Overriding []string `json:"overriding,omitempty"`

	By        string   `json:"by,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Lagging   []string `json:"lagging_hosts,omitempty"`
}

// FleetImpact is what a proposed change would do, in product terms, before it
// is made. The plan for the dashboard asked for this and it is the reason the
// fleet form is a separate verb from the per-repo one: a per-repo save affects
// the repository you are looking at, and a fleet save affects every repository
// that has not overridden the setting — which is most of them.
type FleetImpact struct {
	// Rev binds a preview to the state it described.
	Rev int64 `json:"rev"`
	// Repos is how many repositories inherit the changed setting.
	Repos int `json:"repos"`
	// Reopened is how many completed rounds a reviewer change would requeue,
	// because their heads would suddenly be missing a required answer.
	Reopened int `json:"reopened"`
	// Overridden is how many repositories would NOT be affected, because they
	// have their own answer already.
	Overridden int      `json:"overridden"`
	Changes    []string `json:"changes"`
	Summary    string   `json:"summary"`
}

// FleetSettings reports the fleet's effective defaults.
func (s *Service) FleetSettings(ctx context.Context) (FleetView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetView{}, err
	}
	return s.fleetViewOf(st), nil
}

func (s *Service) fleetViewOf(st State) FleetView {
	cfg := s.cfg.WithFleet(st.Fleet)
	view := FleetView{
		Recorded:       !st.Fleet.Empty(),
		MinInterval:    cfg.MinInterval.String(),
		WeeklyLimit:    cfg.WeeklyReviewLimit,
		AutofixDefault: st.AutofixDefaultOn(),
		Reviewers:      []ReviewerDetail{},
		Sources:        map[string]string{},
	}
	// "env" and "default" are different answers and the page must not merge
	// them: telling someone a value comes from their env file, when nothing in
	// that file mentions it, sends them looking for a line that is not there.
	host := s.cfg.Env()
	from := func(key string, recorded bool, envKeys ...string) {
		switch {
		case recorded:
			view.Sources[key] = "fleet"
		default:
			view.Sources[key] = "default"
			for _, ek := range envKeys {
				if _, ok := host[ek]; ok {
					view.Sources[key] = "env"
					break
				}
			}
		}
	}
	recorded := func(keys ...string) bool {
		for _, key := range keys {
			if fleetGoverns(st.Fleet, key) {
				return true
			}
		}
		return false
	}
	from("reviewers", recorded("CRQ_BOT", "CRQ_COBOTS", "CRQ_REQUIRED_BOTS"), "CRQ_BOT", "CRQ_COBOTS", "CRQ_REQUIRED_BOTS")
	from("min_interval", recorded("CRQ_MIN_INTERVAL"), "CRQ_MIN_INTERVAL")
	from("weekly_limit", recorded("CRQ_WEEKLY_LIMIT"), "CRQ_WEEKLY_LIMIT")
	from("autofix_default", st.Fleet.AutofixDefault != nil)

	for _, r := range cfg.Reviewers {
		view.Reviewers = append(view.Reviewers, ReviewerDetail{
			Login: r.Login, Budget: string(r.Budget), Required: r.Required, Trigger: string(r.Trigger),
		})
	}
	for repo := range st.Repos {
		if ov, ok := st.RepoOverride(repo); ok && (ov.SetCoBots || ov.SetRequired || ov.PrimaryOff) {
			view.Overriding = append(view.Overriding, repo)
		}
	}
	sort.Strings(view.Overriding)
	if st.Fleet.UpdatedAt != nil {
		view.By = st.Fleet.By
		view.UpdatedAt = st.Fleet.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		// The autofix watcher consumes fleet defaults too, but holds neither the
		// autoreview leader lease nor the fire slot. Include its role report or
		// an old watcher can ignore an autofix-off switch while this page says
		// every acting host understands the record.
		caps := CapsFleetDefaults
		if fleetGoverns(st.Fleet, "CRQ_PREFLIGHT_SKIP_BLOCKED") && cfg.PreflightSkipBlocked {
			caps = CapsPreflightSkipBlocked
		}
		view.Lagging = st.LaggingRoleWriters(caps, s.clock().UTC(), "autofix")
	}
	return view
}

// FleetSettingsIn resolves the fleet view from an already-loaded state so one
// dashboard snapshot cannot mix two revisions or pay for the state ref twice.
func (s *Service) FleetSettingsIn(st State) FleetView {
	return s.fleetViewOf(st)
}

// FleetChange is a proposed edit. Every field is a pointer or a nil slice
// meaning "leave this one alone": a form that posts its whole state would
// otherwise overwrite a setting another host changed a second earlier.
type FleetChange struct {
	CoBots         []string `json:"cobots"`
	Required       []string `json:"required"`
	MinInterval    *string  `json:"min_interval"`
	WeeklyLimit    *int     `json:"weekly_limit"`
	AutofixDefault *bool    `json:"autofix_default"`
	// ExpectedRev makes a confirmed dashboard preview a compare-and-swap.
	// CLI callers omit it and retain the ordinary latest-state behavior.
	ExpectedRev *int64 `json:"expected_rev,omitempty"`
	// Unset* removes one typed field from the record, handing that setting back
	// to each host's env. "Leave this alone" and "the fleet has no answer" are
	// different instructions, and a nil pointer or slice can only express the
	// first — so unsetting a co-reviewer list used to report success and change
	// nothing.
	UnsetCoBots      bool `json:"unset_cobots,omitempty"`
	UnsetRequired    bool `json:"unset_required,omitempty"`
	UnsetMinInterval bool `json:"unset_min_interval,omitempty"`
	UnsetWeeklyLimit bool `json:"unset_weekly_limit,omitempty"`
	// Clear drops the whole record, returning every setting to this host's env.
	Clear bool `json:"clear"`
}

// PreviewFleet reports what a change would do without making it.
func (s *Service) PreviewFleet(ctx context.Context, change FleetChange) (FleetImpact, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetImpact{}, err
	}
	next, err := s.applyFleetChange(st, change)
	if err != nil {
		return FleetImpact{}, err
	}
	open, err := s.openPRsForReviewerChange(ctx, st, next)
	if err != nil {
		return FleetImpact{}, err
	}
	return s.fleetImpact(st, next, open), nil
}

// SetFleetSettings records the change and requeues whatever it invalidates.
func (s *Service) SetFleetSettings(ctx context.Context, change FleetChange) (FleetView, FleetImpact, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	if err := checkFleetPreviewRevision(st, change.ExpectedRev); err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	next, err := s.applyFleetChange(st, change)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	// Read before the write, for the same reason SetReviewers does: the requeue
	// may only touch live pull requests and the CAS closure cannot ask GitHub.
	open, err := s.openPRsForReviewerChange(ctx, st, next)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	impact := s.fleetImpact(st, next, open)

	now := s.clock().UTC()
	written, err := s.store.Update(ctx, func(st *State) error {
		if err := checkFleetPreviewRevision(*st, change.ExpectedRev); err != nil {
			return err
		}
		var applied FleetDefaults
		if change.Clear {
			if st.Fleet.Empty() {
				return ErrNoChange
			}
		} else {
			var aerr error
			applied, aerr = s.applyFleetChange(*st, change)
			if aerr != nil {
				return aerr
			}
		}
		if err := s.rejectClaimedReviewerChanges(st, applied, now); err != nil {
			return err
		}
		before := map[string]Config{}
		for _, repo := range s.reposWithChangedReviewers(*st, applied) {
			before[repo] = s.cfgFor(*st, repo)
		}
		if change.Clear {
			st.Fleet = FleetDefaults{}
		} else {
			st.SetFleetDefaults(applied, s.cfg.Host, now)
		}
		for repo, was := range before {
			s.reopenForChangedReviewers(st, repo, was, s.cfgFor(*st, repo), open[repo])
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return FleetView{}, FleetImpact{}, err
	}
	if err == nil {
		s.sync(ctx, written)
	} else {
		written, _, err = s.store.Load(ctx)
		if err != nil {
			return FleetView{}, FleetImpact{}, err
		}
	}
	return s.fleetViewOf(written), impact, nil
}

func checkFleetPreviewRevision(st State, expected *int64) error {
	if expected != nil && st.Rev != *expected {
		return fmt.Errorf("fleet state moved from revision %d to %d; preview the change again", *expected, st.Rev)
	}
	return nil
}

// applyFleetChange folds a change onto the current record, validating it. It
// does not write, so preview and save cannot disagree about what a change means.
func (s *Service) applyFleetChange(st State, change FleetChange) (FleetDefaults, error) {
	if change.Clear {
		return FleetDefaults{}, nil
	}
	existingRequired := s.cfg.WithFleet(st.Fleet).RequiredBots
	fd := st.Fleet
	switch {
	case change.UnsetCoBots:
		fd.CoBots, fd.SetCoBots = nil, false
	case change.CoBots != nil:
		resolved, err := resolveCoBotLogins(change.CoBots)
		if err != nil {
			return fd, err
		}
		fd.CoBots, fd.SetCoBots = resolved, true
	}
	switch {
	case change.UnsetRequired:
		fd.Required, fd.SetRequired = nil, false
	case change.Required != nil:
		if len(change.Required) == 0 {
			return fd, errors.New("the required set cannot be empty: a round that gates on nobody converges before any reviewer runs")
		}
		resolved, err := resolveRequiredLoginsPreserving(
			change.Required, s.cfg.WithFleet(fd).Bot, existingRequired)
		if err != nil {
			return fd, err
		}
		fd.Required, fd.SetRequired = resolved, true
		if len(s.cfg.WithFleet(fd).RequiredBots) == 0 {
			return fd, errors.New("the required set resolves to no enabled reviewer")
		}
	}
	switch {
	case change.UnsetMinInterval:
		fd.MinInterval = ""
	case change.MinInterval != nil:
		text := strings.TrimSpace(*change.MinInterval)
		d, err := time.ParseDuration(text)
		if err != nil {
			return fd, fmt.Errorf("min interval: %w", err)
		}
		if d < 0 {
			return fd, errors.New("min interval cannot be negative")
		}
		// The pacing floor is the fleet's protection against spending the
		// account faster than the vendor will refill it. A very small one is
		// legal but worth refusing to set by accident.
		if d > 0 && d < 5*time.Second {
			return fd, errors.New("min interval below 5s would fire faster than any review completes")
		}
		fd.MinInterval = d.String()
	}
	switch {
	case change.UnsetWeeklyLimit:
		fd.WeeklyLimit = nil
	case change.WeeklyLimit != nil:
		if *change.WeeklyLimit < 0 {
			return fd, errors.New("weekly limit cannot be negative")
		}
		limit := *change.WeeklyLimit
		fd.WeeklyLimit = &limit
	}
	if change.AutofixDefault != nil {
		on := *change.AutofixDefault
		fd.AutofixDefault = &on
	}
	return fd, nil
}

// fleetImpact describes, in product terms, what moving from st to next would do.
func (s *Service) fleetImpact(st State, next FleetDefaults, open map[string]map[int]bool) FleetImpact {
	after := st
	after.Fleet = next
	impact := FleetImpact{Rev: st.Rev, Changes: []string{}}

	// Which repositories a change reaches depends on WHICH setting changed, and
	// the answers differ: a reviewer default stops at a repository that answers
	// both reviewer questions itself, the autofix default stops at one with its
	// own switch, and pacing or the weekly limit stop nowhere at all — they are
	// account-wide. Counting reviewer overrides for every kind of change told an
	// operator turning autofix on that the repository with custom reviewers was
	// "unaffected", moments before agents started running in it.
	all := s.fleetRepos(st)
	following := s.reposFollowingFleet(st)
	affected := map[string]bool{}
	reaches := func(repos []string) {
		for _, repo := range repos {
			affected[repo] = true
		}
	}

	beforeCfg, afterCfg := s.cfg.WithFleet(st.Fleet), s.cfg.WithFleet(next)
	changedReviewers := s.reposWithChangedReviewers(st, next)
	// Which reviewers RUN and which of them GATE are separate questions and
	// both are changes: turning a bot off stops its findings arriving at all,
	// which is not the same as it merely no longer holding the round open.
	beforeCo := beforeCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	afterCo := afterCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	if !sameBot(beforeCfg.Bot, afterCfg.Bot) {
		impact.Changes = append(impact.Changes, fmt.Sprintf("primary reviewer: %s → %s",
			shortBots([]string{beforeCfg.Bot}), shortBots([]string{afterCfg.Bot})))
		reaches(changedReviewers)
	}
	if !sameLogins(beforeCo, afterCo) {
		impact.Changes = append(impact.Changes, fmt.Sprintf("co-reviewers running: %s → %s",
			shortBots(beforeCo), shortBots(afterCo)))
		reaches(following)
	}
	if !sameLogins(beforeCfg.RequiredBots, afterCfg.RequiredBots) {
		impact.Changes = append(impact.Changes, fmt.Sprintf("required reviewers: %s → %s",
			shortBots(beforeCfg.RequiredBots), shortBots(afterCfg.RequiredBots)))
		reaches(following)
	}
	if sameBot(beforeCfg.Bot, afterCfg.Bot) && sameLogins(beforeCo, afterCo) &&
		!sameTriggerSettings(beforeCfg.Reviewers, afterCfg.Reviewers) {
		impact.Changes = append(impact.Changes, "reviewer trigger policy or command changed")
		reaches(changedReviewers)
	}
	if beforeCfg.MinInterval != afterCfg.MinInterval {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("pacing: %s → %s", beforeCfg.MinInterval, afterCfg.MinInterval))
		reaches(all) // one queue, one fire slot: pacing is not a per-repo answer
	}
	if beforeCfg.WeeklyReviewLimit != afterCfg.WeeklyReviewLimit {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("weekly limit: %d → %d", beforeCfg.WeeklyReviewLimit, afterCfg.WeeklyReviewLimit))
		reaches(all) // one account allowance, likewise
	}
	if st.AutofixDefaultOn() != after.AutofixDefaultOn() {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("autofix default: %s → %s", onOff(st.AutofixDefaultOn()), onOff(after.AutofixDefaultOn())))
		// A default reaches every repository without a switch of its own,
		// whatever it may have decided about its reviewers.
		for _, repo := range all {
			if _, explicit := st.AutofixSwitch(repo); !explicit {
				affected[repo] = true
			}
		}
	}
	impact.Repos = len(affected)
	if len(impact.Changes) > 0 {
		for _, repo := range all {
			if !affected[repo] {
				impact.Overridden++
			}
		}
	}

	// A completed round is the "this head was reviewed" marker. Requiring a
	// reviewer it never had means that marker is now wrong, and the round has to
	// be reopened — which is the consequence worth stating before the click.
	for _, repo := range changedReviewers {
		wasCfg, isCfg := s.cfgFor(st, repo), s.cfgFor(after, repo)
		wasCo := wasCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		isCo := isCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		// The same question reopenForChangedReviewers asks, or the preview
		// warns about work that will not happen: only ADDING a reviewer
		// invalidates a finished round, so a narrowing reopens nothing.
		if !addedReviewers(wasCfg, isCfg, wasCo, isCo) {
			continue
		}
		for _, r := range st.Rounds {
			if NormalizeRepo(r.Repo) == repo && r.Phase == PhaseCompleted && open[repo][r.PR] {
				impact.Reopened++
			}
		}
	}

	switch {
	case len(impact.Changes) == 0:
		impact.Summary = "nothing would change"
	case impact.Reopened > 0:
		impact.Summary = fmt.Sprintf("affects %d repositories; %d completed round(s) would be reopened and reviewed again",
			impact.Repos, impact.Reopened)
	default:
		impact.Summary = fmt.Sprintf("affects %d repositories; no round would be reopened", impact.Repos)
	}
	if impact.Overridden > 0 {
		impact.Summary += fmt.Sprintf(" (%d with their own answer to what changed are unaffected)", impact.Overridden)
	}
	return impact
}

// openPRsForReviewerChange reads the live set only for repositories where an
// added reviewer invalidates completed dedup markers. Both the preview count and
// the subsequent write use this same set, so the dialog describes work that
// will actually be requeued rather than every historical completed round.
func (s *Service) openPRsForReviewerChange(ctx context.Context, st State, next FleetDefaults) (map[string]map[int]bool, error) {
	after := st
	after.Fleet = next
	open := map[string]map[int]bool{}
	for _, repo := range s.reposWithChangedReviewers(st, next) {
		wasCfg, isCfg := s.cfgFor(st, repo), s.cfgFor(after, repo)
		wasCo := wasCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		isCo := isCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		if !addedReviewers(wasCfg, isCfg, wasCo, isCo) {
			continue
		}
		hasCompleted := false
		for _, r := range st.Rounds {
			if NormalizeRepo(r.Repo) == repo && r.Phase == PhaseCompleted {
				hasCompleted = true
				break
			}
		}
		if !hasCompleted {
			continue
		}
		prs, err := s.openPRs(ctx, repo)
		if err != nil {
			return nil, err
		}
		open[repo] = prs
	}
	return open, nil
}

// reposWithChangedReviewers resolves both sides for every active repository.
// This is intentionally broader than reposFollowingFleet: a repository can
// override both co-reviewer choices and still inherit the primary, so clearing
// a fleet CRQ_BOT reaches it unless PrimaryOff says otherwise.
func (s *Service) reposWithChangedReviewers(st State, next FleetDefaults) []string {
	after := st
	after.Fleet = next
	var out []string
	for _, repo := range s.fleetRepos(st) {
		if !sameReviewers(s.cfgFor(st, repo), s.cfgFor(after, repo)) {
			out = append(out, repo)
		}
	}
	return out
}

// fullyOverridesReviewers reports whether repo answers BOTH reviewer questions
// itself, so no fleet reviewer default reaches it.
//
// The test is AND, not OR, and that is the whole point: a repository that
// overrides only its co-reviewers still inherits the fleet's required set, and
// vice versa. Treating either half as a complete override dropped those
// repositories from the impact preview and from the requeue — cfgFor handed
// them the new reviewer, their completed round stayed a "this head was
// reviewed" marker, and the reviewer somebody had just required was never
// asked.
func fullyOverridesReviewers(st State, repo string) bool {
	ov, ok := st.RepoOverride(repo)
	return ok && ov.SetCoBots && ov.SetRequired
}

// reposFollowingFleet is every repository crq knows about that inherits at
// least one half of the fleet's REVIEWER default — the ones a change to it can
// actually reach. Settings that are not per repository (pacing, the weekly
// limit) reach fleetRepos instead; this exclusion is about reviewers only.
func (s *Service) reposFollowingFleet(st State) []string {
	return s.fleetReposWhere(st, func(repo string) bool {
		return !fullyOverridesReviewers(st, repo)
	})
}

// fleetRepos is every repository crq knows about and would act on at all.
func (s *Service) fleetRepos(st State) []string {
	return s.fleetReposWhere(st, func(string) bool { return true })
}

func (s *Service) fleetReposWhere(st State, keep func(repo string) bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(repo string) {
		repo = NormalizeRepo(repo)
		if repo == "" || seen[repo] {
			return
		}
		if !keep(repo) {
			return
		}
		// Use the complete effective decision: shared enrollment, the host's
		// absolute exclusion list, scope, and gate-repository protection.
		if !s.reviewsRepo(st, repo) {
			return
		}
		seen[repo] = true
		out = append(out, repo)
	}
	for repo := range s.cfg.AllowRepos {
		add(repo)
	}
	for _, repo := range st.EnrolledRepos() {
		add(repo)
	}
	for _, r := range st.Rounds {
		add(r.Repo)
	}
	sort.Strings(out)
	return out
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// shortBots renders a login list the way a person reads it.
func shortBots(logins []string) string {
	if len(logins) == 0 {
		return "none"
	}
	out := make([]string, 0, len(logins))
	for _, l := range logins {
		out = append(out, dialect.NormalizeBotName(l))
	}
	return strings.Join(out, ", ")
}

// AdoptedSetting is one value moved from this host's environment into the
// fleet record.
type AdoptedSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Skipped says why a setting was left alone, when it was.
	Skipped string `json:"skipped,omitempty"`
}

// AdoptEnv records this host's settings for the whole fleet.
//
// It exists because crq predates the dashboard: a fleet configured before any
// of this has every answer in one machine's env file, and the dashboard says
// "env" beside all of them — which is true, and useless. Adopting copies those
// values into the shared record, so they become the fleet's answer and every
// host reads the same one.
//
// Only settings that CAN be fleet-wide are taken. Identity (which repository
// holds the queue) and per-host values (paths, this machine's name, the fix
// agent's binary) are reported as skipped rather than silently dropped, since
// "why is that one still env" is the obvious next question.
//
// Values equal to the default are skipped too: recording them would pin a
// default that a later crq might improve, and pin it invisibly.
//
// So is a value shared state already decides. The CAS below deliberately leaves
// an existing record alone — it was set on purpose and outranks whatever this
// machine happens to carry — and reporting that as adopted told an operator
// their differing value had become the fleet's when state had kept the old one.
func (s *Service) AdoptEnv(ctx context.Context, dryRun bool) ([]AdoptedSetting, error) {
	host := s.cfg.Env()
	// Per-bot REQUIRED keys remain valid file-level compatibility aliases, but
	// the dashboard has one required-reviewer editor and one typed state field.
	// Fold those aliases into that canonical list at the adoption boundary
	// rather than publishing controls whose generic writes the typed field
	// would silently override.
	if strings.TrimSpace(host["CRQ_REQUIRED_BOTS"]) == "" && hasPerBotRequiredEnv(host) {
		host["CRQ_REQUIRED_BOTS"] = strings.Join(s.cfg.RequiredBots, ",")
	}
	defaults, err := BuildConfig(map[string]string{})
	if err != nil {
		return nil, err
	}
	defaultEnv := map[string]string{}
	for _, k := range EnvKeys() {
		defaultEnv[k.Key] = defaultValueOf(defaults, k.Key)
	}
	// Read BEFORE the results are built, so what this reports and what the write
	// below does are decided from one view of the record.
	current, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}

	var adopted []AdoptedSetting
	take := map[string]string{}
	for _, k := range EnvKeys() {
		value := strings.TrimSpace(host[k.Key])
		invalid := validateEnvValue(k.Key, value)
		switch {
		case k.Identity:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "identity: it says where the queue lives, not how it behaves"})
		case k.PerHost:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "per-host: recording one machine's answer would break the others"})
		case value == "":
			// Nothing set here, so there is nothing of this host's to adopt.
		case invalid != nil:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "invalid: " + invalid.Error()})
		case value == defaultEnv[k.Key]:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "same as the default: recording it would pin today's default invisibly"})
		case fleetGoverns(current.Fleet, k.Key):
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "the fleet already records this setting, and a record outranks one host's value"})
		default:
			take[k.Key] = value
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value})
		}
	}
	if dryRun || len(take) == 0 {
		return adopted, nil
	}

	// Adoption can change the effective reviewers through either their typed
	// fields or a generic setting such as CRQ_BOT or a trigger policy. Fetch
	// the live PR set before the write; the CAS closure cannot ask GitHub.
	preview, _ := adoptFleetEnv(current.Fleet, take, s.cfg, nil)
	open, err := s.openPRsForReviewerChange(ctx, current, preview)
	if err != nil {
		return nil, err
	}

	now := s.clock().UTC()
	// What the write actually landed. The classification above answers from the
	// record as it was READ, and between that and the CAS a key can gain a
	// record — or a typed value can fail to parse into its field — leaving a
	// setting reported as adopted that shared state never took.
	applied := map[string]bool{}
	st, err := s.store.Update(ctx, func(st *State) error {
		clear(applied) // a CAS conflict runs this closure again
		before := map[string]Config{}
		for _, repo := range s.reposFollowingFleet(*st) {
			before[repo] = s.cfgFor(*st, repo)
		}
		fd, changed := adoptFleetEnv(st.Fleet, take, s.cfg, applied)
		if !changed {
			return ErrNoChange
		}
		if err := s.rejectClaimedReviewerChanges(st, fd, now); err != nil {
			return err
		}
		st.SetFleetDefaults(fd, s.cfg.Host, now)
		for repo, was := range before {
			s.reopenForChangedReviewers(st, repo, was, s.cfgFor(*st, repo), open[repo])
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return nil, err
	}
	if err == nil {
		s.sync(ctx, st)
	}
	for i := range adopted {
		if adopted[i].Skipped == "" && take[adopted[i].Key] != "" && !applied[adopted[i].Key] {
			adopted[i].Skipped = "shared state kept the value it already had"
		}
	}
	return adopted, nil
}

func hasPerBotRequiredEnv(env map[string]string) bool {
	for _, co := range dialect.KnownCoReviewers() {
		key := "CRQ_COBOT_" + strings.ToUpper(co.Name) + "_REQUIRED"
		if boolEnv(env, key, false) {
			return true
		}
	}
	return false
}

// adoptFleetEnv folds the settings this host offered onto a fleet record.
// Keeping preview and CAS application on one path makes the open-PR prefetch
// describe the reviewer change that can actually land.
func adoptFleetEnv(fd FleetDefaults, take map[string]string, cfg Config, applied map[string]bool) (FleetDefaults, bool) {
	fd.Env = maps.Clone(fd.Env)
	changed := false
	record := func(key string) {
		if applied != nil {
			applied[key] = true
		}
		changed = true
	}
	set := func(key, value string) {
		// A record already there was set deliberately and outranks a value this
		// host happens to carry.
		if fd.Env == nil {
			fd.Env = map[string]string{}
		}
		if _, exists := fd.Env[key]; exists {
			return
		}
		fd.Env[key] = value
		record(key)
	}

	// Required-reviewer aliases are resolved against the effective primary.
	// Apply a newly adopted primary first instead of letting map iteration
	// decide which primary CRQ_REQUIRED_BOTS sees.
	if value, ok := take["CRQ_BOT"]; ok {
		set("CRQ_BOT", value)
	}
	for key, value := range take {
		if key == "CRQ_BOT" {
			continue
		}
		// Four settings have a typed home. Recording them in Env as well would
		// give one setting two places to live, one of them shadowed.
		switch key {
		case "CRQ_COBOTS":
			if !fd.SetCoBots {
				if logins, err := resolveCoBotLogins(splitCommas(value)); err == nil {
					fd.CoBots, fd.SetCoBots = logins, true
					record(key)
				}
			}
		case "CRQ_REQUIRED_BOTS":
			if !fd.SetRequired {
				if logins, err := resolveRequiredLogins(splitCommas(value), cfg.WithFleet(fd).Bot); err == nil {
					fd.Required, fd.SetRequired = logins, true
					record(key)
				}
			}
		case "CRQ_MIN_INTERVAL":
			if fd.MinInterval == "" {
				fd.MinInterval = value
				record(key)
			}
		case "CRQ_WEEKLY_LIMIT":
			if fd.WeeklyLimit == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
					fd.WeeklyLimit = &n
					record(key)
				}
			}
		default:
			set(key, value)
		}
	}
	return fd, changed
}

// fleetGoverns reports whether the fleet record already decides key, in either
// of the two places a setting can live: its typed field, or the generic map.
// Both have to be asked — a value adopted into the typed field is invisible in
// Env, and reporting it as unrecorded would offer to adopt it again on every
// run.
func fleetGoverns(fd FleetDefaults, key string) bool {
	if _, ok := fleetValueOf(fd, key); ok {
		return true
	}
	_, ok := fd.Env[key]
	return ok
}

// defaultValueOf renders what a setting would be with nothing configured, so
// adoption can tell "this host chose this" from "this is just the default".
func defaultValueOf(defaults Config, key string) string {
	return configValueOf(defaults, key)
}

// configValueOf renders a parsed configuration back into the dashboard's
// setting vocabulary. The dashboard must show the effective default too:
// absence in the raw environment is not an empty value (notably for booleans
// whose default is on).
func configValueOf(cfg Config, key string) string {
	switch key {
	case "CRQ_MIN_INTERVAL":
		return cfg.MinInterval.String()
	case "CRQ_INFLIGHT_TIMEOUT":
		return cfg.InflightTimeout.String()
	case "CRQ_RL_FALLBACK":
		return cfg.RateLimitFallback.String()
	case "CRQ_CALIBRATE_TTL":
		return cfg.CalibrationTTL.String()
	case "CRQ_WEEKLY_LIMIT":
		return fmt.Sprint(cfg.WeeklyReviewLimit)
	case "CRQ_AUTOREVIEW_POLL":
		return cfg.AutoReviewPoll.String()
	case "CRQ_AUTOREVIEW_MAX_SCAN":
		return fmt.Sprint(cfg.AutoReviewMaxScan)
	case "CRQ_LEADER_TTL":
		return cfg.LeaderTTL.String()
	case "CRQ_BOT":
		return cfg.Bot
	case "CRQ_REVIEW_CMD":
		return cfg.ReviewCommand
	case "CRQ_COBOTS":
		names := make([]string, 0, len(cfg.CoBots))
		for _, bot := range cfg.CoBots {
			names = append(names, bot.Name)
		}
		return strings.Join(names, ",")
	case "CRQ_REQUIRED_BOTS":
		return strings.Join(cfg.RequiredBots, ",")
	case "CRQ_FEEDBACK_BOTS":
		return strings.Join(cfg.FeedbackBots, ",")
	case "CRQ_SETTLE":
		return cfg.SettleWindow.String()
	case "CRQ_FEEDBACK_WAIT_TIMEOUT":
		return cfg.FeedbackWaitTimeout.String()
	case "CRQ_RL_CO_DEGRADE":
		return boolString(cfg.RateLimitCoDegrade)
	case "CRQ_PREFLIGHT_SKIP_BLOCKED":
		return boolString(cfg.PreflightSkipBlocked)
	case "CRQ_AUTOREVIEW_SKIP_AUTHORS":
		return strings.Join(sortedTrueKeys(cfg.SkipAuthors), ",")
	case "CRQ_WATCH_INTERVAL":
		return cfg.WatchInterval.String()
	case "CRQ_DISPATCH_MAX_ATTEMPTS":
		return fmt.Sprint(cfg.DispatchMaxAttempts)
	case "CRQ_DISPATCH_FORKS":
		return boolString(cfg.DispatchForks)
	case "CRQ_DISPATCH_CONCURRENCY":
		return fmt.Sprint(cfg.DispatchConcurrency)
	case "CRQ_AUTOREVIEW_SKIP_MARKER":
		return cfg.SkipMarker
	case "CRQ_TZ":
		return cfg.Timezone
	case "CRQ_TIDY":
		return boolString(cfg.Tidy)
	case "CRQ_REPO":
		return cfg.GateRepo
	case "CRQ_STATE_REF":
		return cfg.StateRef
	case "CRQ_STATE_GIT_AUTHOR_NAME":
		return cfg.StateGitAuthorName
	case "CRQ_STATE_GIT_AUTHOR_EMAIL":
		return cfg.StateGitAuthorEmail
	case "CRQ_ISSUE":
		return fmt.Sprint(cfg.DashboardIssue)
	case "CRQ_SCOPE":
		return strings.Join(cfg.Scope, ",")
	case "CRQ_REPOS":
		return strings.Join(sortedTrueKeys(cfg.AllowRepos), ",")
	case "CRQ_EXCLUDE":
		return strings.Join(sortedTrueKeys(cfg.ExcludeRepos), ",")
	case "CRQ_HOST":
		return cfg.Host
	case "CRQ_WORKSPACE":
		return cfg.WorkspaceRoot
	case "CRQ_DISPATCH_CMD":
		return strings.Join(cfg.DispatchCommand, " ")
	}
	for _, known := range dialect.KnownCoReviewers() {
		prefix := "CRQ_COBOT_" + strings.ToUpper(known.Name) + "_"
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		bot, _ := cfg.knownCoBot(known.Name)
		switch strings.TrimPrefix(key, prefix) {
		case "REQUIRED":
			return boolString(bot.Required)
		case "TRIGGER":
			if bot.TriggerExplicit {
				return string(bot.Trigger)
			}
			return ""
		case "GRACE":
			return bot.SelfHealGrace.String()
		case "CMD":
			return bot.Command
		}
	}
	return ""
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func sortedTrueKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value, on := range values {
		if on {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// normalizeEnvEdit validates one raw setting edit and applies the dashboard and
// CLI convention that a blank non-empty-capable value means "inherit".
func normalizeEnvEdit(key, value string, unset bool) (string, string, bool, error) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !fleetSettable(key) {
		if _, known := envKeyByName(key); known {
			return "", "", false, fmt.Errorf("%s is not a fleet setting: it belongs to one machine, or says where the queue lives", key)
		}
		return "", "", false, fmt.Errorf("%s is not a setting crq knows", key)
	}
	if !unset {
		if err := validateEnvValue(key, value); err != nil {
			return "", "", false, err
		}
	}
	envKey, _ := envKeyByName(key)
	if value == "" && !envKey.AllowEmpty {
		unset = true
	}
	return key, value, unset, nil
}

// typedFleetChangeForEnv maps the four settings with a typed home into the
// same change model used by the fleet editor.
func typedFleetChangeForEnv(key, value string, unset bool) (FleetChange, bool, error) {
	if !typedEnvKey(key) {
		return FleetChange{}, false, nil
	}
	change := FleetChange{}
	// Keys with a typed home go there, so one setting never lives in two places
	// with one of them silently shadowed by the other.
	// Unset is its own instruction, not a change to some other value. Encoding
	// it as one meant a cleared list read as "leave it alone" and a cleared
	// weekly limit wrote 60 — so a host whose env said 90 never got it back.
	switch key {
	case "CRQ_COBOTS":
		if unset {
			change.UnsetCoBots = true
		} else {
			change.CoBots = append([]string{}, splitCommas(value)...)
		}
	case "CRQ_REQUIRED_BOTS":
		if unset {
			change.UnsetRequired = true
		} else {
			change.Required = splitCommas(value)
		}
	case "CRQ_MIN_INTERVAL":
		if unset {
			change.UnsetMinInterval = true
		} else {
			v := value
			change.MinInterval = &v
		}
	case "CRQ_WEEKLY_LIMIT":
		if unset {
			change.UnsetWeeklyLimit = true
		} else {
			n, err := strconv.Atoi(value)
			if err != nil {
				return FleetChange{}, true, fmt.Errorf("%s: %w", key, err)
			}
			change.WeeklyLimit = &n
		}
	}
	return change, true, nil
}

// PreviewEnv reports the reviewer and requeue impact of a raw environment-key
// edit without writing it.
func (s *Service) PreviewEnv(ctx context.Context, key, value string, unset bool) (FleetImpact, error) {
	key, value, unset, err := normalizeEnvEdit(key, value, unset)
	if err != nil {
		return FleetImpact{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetImpact{}, err
	}
	change, typed, err := typedFleetChangeForEnv(key, value, unset)
	if err != nil {
		return FleetImpact{}, err
	}
	var next FleetDefaults
	if typed {
		next, err = s.applyFleetChange(st, change)
		if err != nil {
			return FleetImpact{}, err
		}
	} else {
		next = fleetEnvSet(st.Fleet, key, value, unset)
	}
	open, err := s.openPRsForReviewerChange(ctx, st, next)
	if err != nil {
		return FleetImpact{}, err
	}
	return s.fleetImpact(st, next, open), nil
}

// SetEnv records or clears one fleet setting against the latest state. CLI
// callers use this form; dashboard confirmations use SetEnvAt.
func (s *Service) SetEnv(ctx context.Context, key, value string, unset bool) (FleetView, error) {
	view, _, err := s.SetEnvAt(ctx, key, value, unset, nil)
	return view, err
}

// SetEnvAt records a setting only if the confirmed preview revision is still
// current, and returns the impact that was applied.
func (s *Service) SetEnvAt(ctx context.Context, key, value string, unset bool, expectedRev *int64) (FleetView, FleetImpact, error) {
	key, value, unset, err := normalizeEnvEdit(key, value, unset)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	change, typed, err := typedFleetChangeForEnv(key, value, unset)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	if typed {
		change.ExpectedRev = expectedRev
		return s.SetFleetSettings(ctx, change)
	}

	// A generic setting can still change WHO reviews: CRQ_BOT names the primary,
	// and a per-bot trigger key decides whether a co-reviewer runs at all. That
	// is a reviewer change like the typed ones, and a completed round is the
	// "this head was reviewed" marker — left alone, the reviewer just configured
	// is never asked until somebody happens to push. Read the open pull requests
	// before the write, for the same reason SetFleetSettings does: the requeue
	// may only touch live pull requests and the CAS closure cannot ask GitHub.
	loaded, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	if err := checkFleetPreviewRevision(loaded, expectedRev); err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	next := fleetEnvSet(loaded.Fleet, key, value, unset)
	open, err := s.openPRsForReviewerChange(ctx, loaded, next)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	impact := s.fleetImpact(loaded, next, open)

	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if err := checkFleetPreviewRevision(*st, expectedRev); err != nil {
			return err
		}
		if _, ok := st.Fleet.Env[key]; unset && !ok {
			return ErrNoChange
		}
		if cur, ok := st.Fleet.Env[key]; !unset && ok && cur == value {
			return ErrNoChange
		}
		before := map[string]Config{}
		// Recompute this set from the state revision the write will update. A
		// concurrent override clear can make a repository start following the
		// fleet after the open-PR prefetch above. It has no prefetched open set,
		// so reopenForChangedReviewers conservatively marks its completed rounds
		// for reopening when that PR is next observed alive.
		applied := fleetEnvSet(st.Fleet, key, value, unset)
		if err := s.rejectClaimedReviewerChanges(st, applied, now); err != nil {
			return err
		}
		for _, repo := range s.reposWithChangedReviewers(*st, applied) {
			before[repo] = s.cfgFor(*st, repo)
		}
		st.SetFleetDefaults(applied, s.cfg.Host, now)
		for repo, was := range before {
			s.reopenForChangedReviewers(st, repo, was, s.cfgFor(*st, repo), open[repo])
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return FleetView{}, FleetImpact{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else if st, _, err = s.store.Load(ctx); err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	return s.fleetViewOf(st), impact, nil
}

// rejectClaimedReviewerChanges keeps a committed configuration edit from being
// followed by a trigger chosen under the old configuration. Co-review posts are
// claimed before their network call, so the claim is the exact interval in
// which a reviewer-changing fleet write must wait.
func (s *Service) rejectClaimedReviewerChanges(st *State, next FleetDefaults, now time.Time) error {
	for _, repo := range s.reposWithChangedReviewers(*st, next) {
		if claimedTriggerRepo(st, repo, now) {
			return fmt.Errorf("a review trigger is already being posted for %s; wait for it to finish before changing fleet reviewers", repo)
		}
	}
	return nil
}

// fleetEnvSet returns fd with one generic setting recorded or removed.
//
// The map is COPIED rather than written through. A caller comparing the result
// against the record it came from would otherwise be comparing one map with
// itself, and the whole point of building it is to ask what the change would do.
func fleetEnvSet(fd FleetDefaults, key, value string, unset bool) FleetDefaults {
	env := make(map[string]string, len(fd.Env)+1)
	for k, v := range fd.Env {
		env[k] = v
	}
	if unset {
		delete(env, key)
	} else {
		env[key] = value
	}
	if len(env) == 0 {
		env = nil
	}
	fd.Env = env
	return fd
}

// splitCommas is the list form every reviewer setting uses.
func splitCommas(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
