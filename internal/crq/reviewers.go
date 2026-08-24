package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// Reviewer is one configured reviewer, and the single description of it.
//
// crq carried four lists for this: Bot named the primary, RequiredBots gated
// convergence, FeedbackBots decided whose findings surfaced, and CoBots held
// per-bot trigger policy. Each answered a different question about the same set,
// so adding a reviewer meant editing four places and keeping them consistent, and
// reading the configuration meant cross-referencing them.
//
// Budget is the field that matters most, because it is the only reason a queue
// exists: an account-metered reviewer must be serialized against a shared
// allowance, and one that costs nothing has no reason to wait behind anybody.
// Saying so as data is what lets the rules ask what a reviewer costs rather than
// whether it happens to be CodeRabbit.
type Reviewer struct {
	Login  string
	Name   string
	Budget dialect.Budget
	// Required gates convergence: crq will not call a round done until this
	// reviewer has answered for the head.
	Required bool
	// Command and Trigger describe how crq asks for a review. An empty Command
	// means crq cannot ask at all, whatever the mode says.
	Command       string
	Trigger       engine.TriggerMode
	SelfHealGrace time.Duration
}

// Metered reports whether this reviewer spends the shared account allowance —
// the property the fire slot and the quota gate exist for.
func (r Reviewer) Metered() bool { return r.Budget == dialect.BudgetAccount }

// Primary returns the account-metered reviewer, which is the one the queue
// serializes. Exactly one is configured today; the second return is false when
// none is.
func (c Config) Primary() (Reviewer, bool) {
	for _, r := range c.Reviewers {
		if r.Metered() {
			return r, true
		}
	}
	return Reviewer{}, false
}

// FreeRunning returns the reviewers that cost the account nothing, so they never
// take the fire slot and never wait on the quota window.
func (c Config) FreeRunning() []Reviewer {
	var out []Reviewer
	for _, r := range c.Reviewers {
		if !r.Metered() {
			out = append(out, r)
		}
	}
	return out
}

// reviewerLogins is the login list for a predicate over the configured
// reviewers, in configuration order.
func (c Config) reviewerLogins(want func(Reviewer) bool) []string {
	var out []string
	for _, r := range c.Reviewers {
		if want(r) {
			out = append(out, r.Login)
		}
	}
	return out
}

// buildReviewers assembles the one list from what the environment parsed: the
// configured primary, the enabled co-reviewers, and any other login the operator
// required that neither covers.
//
// It must describe the existing configuration exactly, not a tidier version of
// it. Four ways of getting that wrong showed up in review, and each was a silent
// behaviour change hiding inside a "pure refactor":
//
//   - a required bot outside the primary and the registry (say sonar[bot]) has no
//     entry to build from, and dropping it would stop gating a reviewer the
//     operator asked to wait for;
//   - CRQ_REQUIRED_BOTS may deliberately exclude the primary, so requiredness is
//     read, never assumed;
//   - CRQ_BOT may name a registry bot, which would otherwise appear twice —
//     once metered, once free-running;
//   - the primary's trigger lives in ReviewCommand, and an empty Command means
//     "crq cannot ask this reviewer", which is false for it.
func buildReviewers(primary, primaryCommand string, required []string, coBots []CoBotConfig, primaryOff bool) []Reviewer {
	requiredSet := map[string]bool{}
	for _, login := range required {
		if login = strings.TrimSpace(login); login != "" {
			requiredSet[dialect.NormalizeBotName(login)] = true
		}
	}
	seen := map[string]bool{}
	out := make([]Reviewer, 0, len(coBots)+len(required)+1)

	// A repository that turned the primary off has no metered reviewer at all:
	// leaving the entry in place with Required false would still let Primary()
	// find something to fire.
	if primary != "" {
		key := dialect.NormalizeBotName(primary)
		// Even when this repository disables the primary, reserve its identity.
		// A registry-backed primary also has a silenced CoBots entry carrying
		// classifier hooks; without this marker the loop below rebuilt that
		// entry as a participating free reviewer.
		seen[key] = true
		if !primaryOff {
			out = append(out, Reviewer{
				Login:    primary,
				Name:     key,
				Budget:   dialect.BudgetAccount,
				Required: requiredSet[key],
				Command:  primaryCommand,
				Trigger:  engine.TriggerAlways,
			})
		}
	}
	for _, cb := range coBots {
		key := dialect.NormalizeBotName(cb.Login)
		if seen[key] {
			continue // already configured as the primary
		}
		seen[key] = true
		out = append(out, Reviewer{
			Login:         cb.Login,
			Name:          cb.Name,
			Budget:        dialect.BudgetNone,
			Required:      cb.Required || requiredSet[key],
			Command:       cb.Command,
			Trigger:       cb.Trigger,
			SelfHealGrace: cb.SelfHealGrace,
		})
	}
	// Whatever else the operator required. crq knows nothing about how to trigger
	// it, but it still gates convergence, which is the whole reason it was listed.
	for _, login := range required {
		login = strings.TrimSpace(login)
		key := dialect.NormalizeBotName(login)
		if login == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Reviewer{
			Login:    login,
			Name:     key,
			Budget:   dialect.BudgetNone,
			Required: true,
			Trigger:  engine.TriggerNever,
		})
	}
	return out
}

// silenceTrigger stops crq posting a co-reviewer trigger for login while leaving
// its entry — and therefore its registry wording and check hooks — in place.
// Used for a primary that is also a registry bot: it is triggered as the
// primary, and asking it twice is the bug, but its evidence is still read here.
func silenceTrigger(coBots []CoBotConfig, login string) []CoBotConfig {
	if login == "" {
		return coBots
	}
	key := dialect.NormalizeBotName(login)
	out := make([]CoBotConfig, 0, len(coBots))
	for _, cb := range coBots {
		if dialect.NormalizeBotName(cb.Login) == key {
			cb.Trigger = engine.TriggerNever
		}
		out = append(out, cb)
	}
	return out
}

// evidenceBots is the set whose output crq reads: everyone whose findings are
// surfaced, plus everyone it waits for. The two must never diverge — a bot crq
// gates on whose findings it did not surface would hang the round forever.
func (c Config) evidenceBots() map[string]struct{} {
	return dialect.BotSet(unionBots(c.FeedbackBots, c.RequiredBots))
}

// ForRepo applies a repository's reviewer override to the fleet configuration.
//
// The primary is deliberately NOT overridable. Its markers and command are
// injected into the dialect classifiers when the Service is constructed, so a
// per-repo primary would mean per-repo classifiers — a much larger change than
// "which co-reviewers run here", which is what the request actually is.
func (c Config) ForRepo(ov RepoReviewers) Config {
	if !ov.SetCoBots && !ov.SetRequired && !ov.PrimaryOff {
		return c
	}
	out := c
	out.PrimaryOff = ov.PrimaryOff

	// The effective required set, whichever half the override named.
	required := c.RequiredBots
	if ov.SetRequired {
		required = ov.Required
	}
	// A reviewer that does not run here cannot gate here. The fleet default
	// required set names the primary, so a repository that turns it off without
	// also rewriting that list would otherwise wait for ever on a bot crq never
	// asks. Dropping it is not a silent override of the operator's choice: it is
	// the same choice, read consistently.
	if ov.PrimaryOff {
		required = withoutBot(required, c.Bot)
	}

	// The effective co-reviewer set. Required implies enabled — the rule the
	// fleet parse already follows, and it has to hold when only the required
	// half is overridden too: a bot that gates but is never triggered makes the
	// round wait for evidence crq never asks for.
	enabled := ov.CoBots
	if !ov.SetCoBots {
		enabled = nil
		for _, cb := range c.CoBots {
			enabled = append(enabled, cb.Login)
		}
	}
	for _, login := range required {
		if sameBot(login, c.Bot) || containsBot(enabled, login) {
			continue
		}
		if _, ok := dialect.CoReviewerByName(login); ok {
			enabled = append(enabled, login)
		}
	}

	keep := make([]CoBotConfig, 0, len(enabled)+1)
	have := map[string]bool{}
	for _, cb := range c.CoBots {
		switch {
		case sameBot(cb.Login, c.Bot):
			// A primary that is itself a registry bot keeps its silenced entry
			// whatever the override says: that entry carries its wording and
			// check-run hooks, and dropping it costs the PRIMARY its evidence.
		case containsBot(enabled, cb.Login):
			cb.Required = containsBot(required, cb.Login)
			cb = reconcileTrigger(cb)
		default:
			continue
		}
		keep = append(keep, cb)
		have[dialect.NormalizeBotName(cb.Login)] = true
	}
	// A primary that is itself a registry bot but that the fleet neither enabled
	// nor required has no entry to preserve above — and this repository naming it
	// is exactly when its evidence has to be read. Add the silenced entry the
	// fleet parse would have carried: without it observation loses that bot's
	// wording and check-run hooks, so a check-only clean result is never fetched
	// and the primary stays pending until the round times out. Recording it here
	// also keeps the loop below from adding it as an ordinary co-reviewer, which
	// would ask the same bot twice.
	if primary := c.Bot; primary != "" && !have[dialect.NormalizeBotName(primary)] &&
		(containsBot(required, primary) || containsBot(enabled, primary)) {
		if cb, ok := c.knownCoBot(primary); ok {
			cb.Required = containsBot(required, primary)
			cb.Trigger = engine.TriggerNever // triggered as the primary; asking twice is the bug
			keep = append(keep, cb)
			have[dialect.NormalizeBotName(primary)] = true
		}
	}
	// A repository may choose a bot the fleet does not enable — otherwise
	// "which bots for which project" only ever subtracts. Its configuration
	// comes from the registry, since there is no per-bot environment for a repo.
	for _, login := range enabled {
		if have[dialect.NormalizeBotName(login)] {
			continue
		}
		// From the operator's resolved settings, not registry defaults: a bot the
		// fleet disabled still has CRQ_COBOT_<NAME>_CMD/TRIGGER/GRACE, and
		// ignoring them silently gives this repository a different bot than the
		// one that was configured.
		if cb, ok := c.knownCoBot(login); ok {
			cb.Required = containsBot(required, login)
			keep = append(keep, reconcileTrigger(cb))
			have[dialect.NormalizeBotName(login)] = true
		}
	}
	out.CoBots = keep
	out.RequiredBots = append([]string(nil), required...)

	// Rebuild the derived views from the overridden lists, exactly as
	// LoadConfig does, so no view can answer differently from another.
	out.Reviewers = buildReviewers(out.Bot, out.ReviewCommand, out.RequiredBots, out.CoBots, out.PrimaryOff)
	out.RequiredBots = out.reviewerLogins(func(r Reviewer) bool { return r.Required })
	if !c.FeedbackBotsExplicit {
		out.FeedbackBots = out.reviewerLogins(func(Reviewer) bool { return true })
	}
	return out
}

// reconcileTrigger recomputes a co-reviewer's implicit mode after a repository
// override changes its requiredness.
//
// A fleet entry carries the trigger its OWN required-ness produced: Codex
// defaults to never and only becomes always when required. Retaining that entry
// while changing requiredness must therefore handle both directions: promote a
// newly required Codex, and demote one a repository made optional. Explicit
// operator settings still win. Reviewer-change reopens carry their one-shot
// force on the Round instead, so the override does not permanently change how
// that bot runs on later heads.
func reconcileTrigger(cb CoBotConfig) CoBotConfig {
	if cb.TriggerExplicit {
		return cb
	}
	co, ok := dialect.CoReviewerByName(cb.Name)
	if !ok {
		return cb
	}
	cb.Trigger = triggerMode(co.DefaultTrigger, engine.TriggerSelfHeal)
	if cb.Required && co.RequiredTrigger != "" {
		cb.Trigger = triggerMode(co.RequiredTrigger, cb.Trigger)
	}
	if cb.Command == "" {
		cb.Trigger = engine.TriggerNever
	}
	return cb
}

// sameBot reports whether two spellings name the same reviewer.
func sameBot(a, b string) bool {
	return dialect.NormalizeBotName(a) == dialect.NormalizeBotName(b)
}

func containsBot(logins []string, login string) bool {
	key := dialect.NormalizeBotName(login)
	for _, candidate := range logins {
		if dialect.NormalizeBotName(candidate) == key {
			return true
		}
	}
	return false
}

// cfgFor is the configuration crq should use for one repository: this host's
// env, then the fleet's recorded defaults, then that repository's override.
//
// The order is the whole point. Env is the oldest and least specific layer, a
// per-host file that predates any of this; the fleet record is one answer every
// host shares; the repository override is the most specific and wins outright.
func (s *Service) cfgFor(st State, repo string) Config {
	base := s.cfg.WithFleet(st.Fleet).withSolver(st.EffectiveSolver(repo))
	ov, ok := st.RepoOverride(repo)
	if !ok {
		return base
	}
	out := base.ForRepo(ov)
	out.OverrideAt = ov.UpdatedAt
	return out
}

// WithFleet applies the fleet's recorded defaults to this host's configuration.
// An absent field keeps the env value, so a fleet that has never written a
// record behaves exactly as it did before the record existed.
// Each field decides its own absence, deliberately: gating the whole function
// on UpdatedAt would make a PROPOSED record — one built for a preview and not
// yet stamped — read as no change at all, which is the one answer a preview
// must never give.
func (c Config) WithFleet(fd FleetDefaults) Config {
	out := c
	// The generic layer first, since the typed fields below are refinements of
	// the same settings and must win over it. Re-parsing the merged map rather
	// than patching fields is what keeps one meaning per setting: a duration
	// string becomes a duration the same way whichever layer supplied it.
	if len(fd.Env) > 0 && c.env != nil {
		merged := c.Env()
		applied := false
		for key, value := range fd.Env {
			if !fleetReadable(key) {
				continue // identity and per-host settings are not the fleet's to set
			}
			merged[key] = value
			applied = true
		}
		if applied {
			if rebuilt, err := BuildConfig(merged); err == nil {
				// Everything a caller already resolved for THIS process stays:
				// the rebuild answers for settings, not for which repository or
				// host is being acted on.
				rebuilt.WorkDir, rebuilt.OverrideAt = out.WorkDir, out.OverrideAt
				out = rebuilt
			}
		}
	}
	// Stamped whatever the record says, and before the early return below: what
	// a fire revalidates is that the record it decided from is still the record,
	// and a fleet that changed only settings this build ignores has still
	// replaced the one it read. Set after the rebuild above, which returns a
	// configuration parsed from scratch.
	out.FleetAt = fd.UpdatedAt
	if d, err := time.ParseDuration(strings.TrimSpace(fd.MinInterval)); err == nil && fd.MinInterval != "" {
		out.MinInterval = d
	}
	if fd.WeeklyLimit != nil {
		out.WeeklyReviewLimit = *fd.WeeklyLimit
	}
	if !fd.SetCoBots && !fd.SetRequired {
		return out
	}
	// Reuse ForRepo's resolution rather than reimplementing it: "which bots run
	// and which of them gate" is one algorithm, and the fleet default and a
	// per-repo override are the same question asked at different scopes.
	out = out.ForRepo(RepoReviewers{
		CoBots: fd.CoBots, SetCoBots: fd.SetCoBots,
		Required: fd.Required, SetRequired: fd.SetRequired,
	})
	// ForRepo stamps nothing, but this is a DEFAULT, not an override: a
	// repository with no record of its own must still read as "following the
	// fleet", and OverrideAt is what says otherwise.
	out.OverrideAt = nil
	return out
}

// reviewersChanged reports whether the recorded configuration repo's reviewers
// are resolved from — the fleet defaults, then repo's own override — differs
// from the one cfg was built from.
//
// Deciding and writing are two steps. An operator removing a co-reviewer between
// them would otherwise have crq claim and post that bot's trigger anyway, on the
// authority of a configuration that no longer exists. It is therefore called
// from INSIDE the CAS mutation that commits the decision, where the state it
// reads is the state the write lands on; a separate read beforehand would leave
// the same window one step earlier.
//
// BOTH layers, because both decide who reviews: the fleet record names the
// primary, the co-reviewers and the required set for every repository that has
// no override of its own, so asking only about the override let a fleet change
// that had already reported success be overtaken by the decision it replaced.
func reviewersChanged(st *State, repo string, cfg Config) bool {
	ov, _ := st.RepoOverride(repo)
	return stampChanged(ov.UpdatedAt, cfg.OverrideAt) ||
		stampChanged(st.Fleet.UpdatedAt, cfg.FleetAt)
}

// stampChanged compares a record's write time with the one a configuration was
// built from, treating absent and present as different answers.
func stampChanged(recorded, built *time.Time) bool {
	switch {
	case recorded == nil && built == nil:
		return false
	case recorded == nil || built == nil:
		return true
	default:
		return !recorded.Equal(*built)
	}
}

// knownCoBot is a registry co-reviewer with the operator's environment applied,
// enabled fleet-wide or not.
func (c Config) knownCoBot(login string) (CoBotConfig, bool) {
	for _, cb := range c.KnownCoBots {
		if sameBot(cb.Login, login) || strings.EqualFold(cb.Name, strings.TrimSpace(login)) {
			return cb, true
		}
	}
	if co, ok := dialect.CoReviewerByName(login); ok {
		return defaultCoBot(co, false), true
	}
	return CoBotConfig{}, false
}

// withoutBot drops login from a reviewer list, comparing the way every other
// reviewer path does.
func withoutBot(list []string, login string) []string {
	if login == "" {
		return list
	}
	out := make([]string, 0, len(list))
	for _, l := range list {
		if !sameBot(l, login) {
			out = append(out, l)
		}
	}
	return out
}

// withSolver applies the resolved fix-session settings — the fleet default with
// this repository's own record already layered over it by EffectiveSolver.
//
// An absent field keeps the env value, so a fleet that records nothing runs
// exactly the sessions it ran before.
func (c Config) withSolver(sv SolverSettings) Config {
	out := c
	if sv.SetModels || len(sv.Models) > 0 || sv.Model != "" {
		out.FixModels = sv.RankedModels()
		out.FixModel = ""
		if len(out.FixModels) > 0 {
			out.FixModel = out.FixModels[0]
		}
	}
	if sv.SetEffort || sv.Effort != "" {
		out.FixEffort = sv.Effort
	}
	if sv.SetPrompt || sv.Prompt != "" {
		out.FixPrompt = sv.Prompt
	}
	if sv.MaxAttempts != nil {
		out.DispatchMaxAttempts = *sv.MaxAttempts
	}
	if sv.SetSeverities || len(sv.Severities) > 0 {
		out.FixSeverities = severitySet(strings.Join(sv.Severities, ","))
	}
	if sv.SetAskMode || sv.AskMode != "" {
		out.FixAskMode = sv.AskMode
	}
	if sv.Forks != nil {
		out.DispatchForks = *sv.Forks
	}
	if sv.SetSkipAuthors {
		out.SkipAuthors = authorSet(strings.Join(sv.SkipAuthors, ","))
	}
	if sv.SetOnePass {
		out.OnePass = sv.OnePass
	}
	if sv.SetMerge {
		out.MergeMethod = sv.MergeMethod
	}
	return out
}

// repoCfg is cfgFor for a caller that has no state in hand. It reads the ref,
// so it belongs on paths that act on ONE repository at a time — a dispatch, a
// fork check — never inside a loop over the fleet.
//
// A read failure falls back to this host's own configuration rather than
// failing the operation: the settings it would have applied are refinements,
// and refusing to fix a pull request because a setting could not be read would
// turn a cosmetic outage into a functional one.
//
// DispatchForks is the exception, because it is not a refinement. It is the
// line between running an agent over code the operator wrote and running it
// over a stranger's, and the record that turns it off is exactly what a failed
// read cannot see. Falling back to a permissive env value would re-enable fork
// dispatches precisely while the shared safety policy is unavailable, so the
// fallback denies them instead: the cost of being wrong is one contributor pull
// request left unfixed until the ref reads again.
// It takes the caller's context because the read behind it retries: a state ref
// that has become unreachable would otherwise keep a stopped watcher — or a
// cancelled session — waiting on a fallback it has already decided to take.
func (s *Service) repoCfg(ctx context.Context, repo string) Config {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		fallback := s.cfg
		fallback.DispatchForks = false
		return fallback
	}
	return s.cfgFor(st, repo)
}

// fleetCfg is repoCfg for a caller acting on no particular repository: this
// host's environment with the fleet record applied and no override on top.
//
// A read failure falls back to this host's own configuration for the same
// reason repoCfg does — the fleet layer refines settings, and refusing to act
// because the ref could not be read turns a cosmetic outage into a functional
// one. DispatchForks does not arise here: nothing that asks for the fleet's own
// answer is deciding whose code to run an agent over.
func (s *Service) fleetCfg(ctx context.Context) Config {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return s.cfg
	}
	return s.cfgFor(st, "")
}

// ConfigIn is the configuration for one repository against an already-loaded
// state — cfgFor, exported for callers that render many repositories from one
// read. An empty repo answers for the fleet: env with the fleet record applied
// and no repository override, which is exactly what a repository that has never
// been ruled on gets.
func (s *Service) ConfigIn(st State, repo string) Config {
	return s.cfgFor(st, repo)
}
