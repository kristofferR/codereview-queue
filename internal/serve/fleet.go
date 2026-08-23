package serve

import (
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// FleetConfig is the configuration the dashboard displays. It is a copy rather
// than a reference to crq.Config so this package never imports the orchestrator
// (which would make the dependency graph a cycle) and so what the UI can see is
// an explicit, reviewable list.
type FleetConfig struct {
	GateRepo       string   `json:"gate_repo"`
	StateRef       string   `json:"state_ref"`
	DashboardIssue int      `json:"dashboard_issue,omitempty"`
	CalibrationPR  int      `json:"calibration_pr,omitempty"`
	Scope          []string `json:"scope,omitempty"`
	AllowRepos     []string `json:"allow_repos,omitempty"`
	ExcludeRepos   []string `json:"exclude_repos,omitempty"`
	SkipAuthors    []string `json:"skip_authors,omitempty"`
	SkipMarker     string   `json:"skip_marker,omitempty"`

	MinInterval     Dur `json:"min_interval"`
	InflightTimeout Dur `json:"inflight_timeout"`
	WatchInterval   Dur `json:"watch_interval"`

	Reviewers []ReviewerCfg `json:"reviewers"`

	AutofixCommand     []string `json:"autofix_command,omitempty"`
	AutofixMaxAttempts int      `json:"autofix_max_attempts,omitempty"`
	AutofixConcurrency int      `json:"autofix_concurrency,omitempty"`
	AutofixForks       bool     `json:"autofix_forks,omitempty"`
	WorkspaceRoot      string   `json:"workspace_root,omitempty"`
}

// Dur renders as the duration string a person would type into the env file,
// not as nanoseconds.
type Dur time.Duration

func (d Dur) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

type ReviewerCfg struct {
	Login    string `json:"login"`
	Name     string `json:"name"`
	Primary  bool   `json:"primary"`
	Required bool   `json:"required"`
	Metered  bool   `json:"metered"`
	Command  string `json:"command,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
	Grace    Dur    `json:"grace,omitempty"`
}

// Snapshot is everything the dashboard reads, reduced once per state change.
// One payload keeps every page consistent: two endpoints could otherwise
// disagree about the same revision.
type Snapshot struct {
	Overview Overview     `json:"overview"`
	Repos    []RepoRow    `json:"repos"`
	Bots     []BotCard    `json:"bots"`
	Setup    SetupView    `json:"setup"`
	Settings SettingsView `json:"settings"`
	// Events is live-only, since this server started. See events.go.
	Events []Event `json:"events"`
	// Stale says this snapshot is the last one that loaded and the state ref has
	// not been readable since. Everything else here is a past that may already
	// have moved, and a dashboard presenting it as live is the one failure a
	// live dashboard must not have.
	Stale *Staleness `json:"stale,omitempty"`
}

// Staleness is why the snapshot stopped being current, and since when.
type Staleness struct {
	Error string    `json:"error"`
	Since time.Time `json:"since"`
}

// RepoRow is one repository as the Repos page lists it.
type RepoRow struct {
	Repo string `json:"repo"`
	// Enrollment is where the decision comes from, not merely whether it is on:
	// a repo forced in by a host's env file cannot be turned off from here, and
	// saying "managed" would invite someone to try.
	Enrollment string `json:"enrollment"` // state|env|excluded|scope|off
	EnvHost    string `json:"env_host,omitempty"`
	// Reviewed is the resolved answer; Enrollment is only where it came from.
	Reviewed     bool       `json:"reviewed"`
	EnvConflict  bool       `json:"env_conflict,omitempty"`
	ClearEnables bool       `json:"clear_enables,omitempty"`
	EnrollReason string     `json:"enroll_reason,omitempty"`
	EnrollBy     string     `json:"enroll_by,omitempty"`
	EnrollAt     *time.Time `json:"enroll_at,omitempty"`

	// Reviewers/Required are the RESOLVED sets — what will actually run here,
	// not the raw override. PrimaryOff is called out separately because it is
	// the one absence a reader would otherwise misread as a fleet without a
	// metered reviewer at all.
	Reviewers  []string   `json:"reviewers"`
	Required   []string   `json:"required"`
	PrimaryOff bool       `json:"primary_off,omitempty"`
	Override   bool       `json:"override"`
	OverrideBy string     `json:"override_by,omitempty"`
	OverrideAt *time.Time `json:"override_at,omitempty"`

	Autofix       string     `json:"autofix"` // default|on|off
	AutofixReason string     `json:"autofix_reason,omitempty"`
	AutofixBy     string     `json:"autofix_by,omitempty"`
	AutofixAt     *time.Time `json:"autofix_at,omitempty"`

	// Solver is how a fix session runs here, resolved through env → fleet →
	// this repository, with a source per setting.
	Solver *RepoSolver `json:"solver,omitempty"`

	ActiveRounds int `json:"active_rounds"`
	QueuedRounds int `json:"queued_rounds"`
	HeldPRs      int `json:"held_prs"`
	Fixing       int `json:"fixing"`
}

// RepoSolver mirrors crq.SolverView on the wire.
type RepoSolver struct {
	Overridden   bool              `json:"overridden"`
	Agent        string            `json:"agent,omitempty"`
	Models       []string          `json:"models"`
	ModelChoices []string          `json:"model_choices"`
	Model        string            `json:"model,omitempty"`
	Effort       string            `json:"effort,omitempty"`
	Prompt       string            `json:"prompt,omitempty"`
	MaxAttempts  int               `json:"max_attempts"`
	Severities   []string          `json:"severities"`
	AskMode      string            `json:"ask_mode"`
	Forks        bool              `json:"forks"`
	SkipAuthors  []string          `json:"skip_authors"`
	OnePass      bool              `json:"one_pass"`
	MergeMethod  string            `json:"merge_method,omitempty"`
	Sources      map[string]string `json:"sources"`
	By           string            `json:"by,omitempty"`
	Lagging      []string          `json:"lagging_hosts,omitempty"`
	// AgentOn says, per host, whether the configured fix agent is reachable
	// there. Capability, not policy: a repository can be set to a model no
	// host can run, and the settings alone would never say so.
	AgentOn []HostHas `json:"agent_on,omitempty"`
}

// HostHas is one host's answer to "can you run this".
type HostHas struct {
	Host string `json:"host"`
	// Has is nil when that host has never reported, which is not the same as
	// "no" — and saying no would blame a machine for crq's own blind spot.
	Has   *bool  `json:"has,omitempty"`
	Path  string `json:"path,omitempty"`
	Stale bool   `json:"stale,omitempty"`
}

// SolverFor resolves one repository's fix-session settings. Supplied by the
// command layer for the same reason the reviewer resolver is: the layering
// belongs to internal/crq, and two answers to it would be one too many.
type SolverFor func(st state.State, repo string) RepoSolver

// HostTools is one machine's self-report.
type HostTools struct {
	Host    string     `json:"host"`
	Agent   string     `json:"agent,omitempty"`
	Version string     `json:"version,omitempty"`
	Caps    int        `json:"caps,omitempty"`
	Roles   []string   `json:"roles,omitempty"`
	Tools   []ToolSeen `json:"tools"`
	At      *time.Time `json:"at,omitempty"`
	// Stale says the host has not reported recently, so everything above is
	// what it LAST said rather than what is true now.
	Stale bool `json:"stale,omitempty"`
	// Behind marks a host running an older crq than the newest reporting one —
	// the single most common cause of "that setting did nothing".
	Behind bool `json:"behind,omitempty"`
}

type ToolSeen struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// BotCard is one reviewer on the Bots page. "Last seen" is deliberately what
// crq itself recorded — a trigger it posted or a claim it observed — rather
// than a vendor status we would have to guess at.
type BotCard struct {
	Login        string `json:"login"`
	Name         string `json:"name"`
	Primary      bool   `json:"primary"`
	Metered      bool   `json:"metered"`
	Enabled      bool   `json:"enabled"`
	Required     bool   `json:"required"`
	Configurable bool   `json:"configurable"`
	Command      string `json:"command,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
	Grace        Dur    `json:"grace,omitempty"`
	// LastSeen is when crq observed this bot ANSWER; LastAsked is when crq last
	// posted its trigger. A bot that has been asked and never answered is the
	// case worth surfacing, and needs both to state it.
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	LastAsked *time.Time `json:"last_asked,omitempty"`
	SeenOn    string     `json:"seen_on,omitempty"`
	RepoCount int        `json:"repo_count"`

	// Status is what crq can honestly say about setup, from its OWN records —
	// a trigger it posted, a claim it saw — never a status read from the vendor:
	//
	//	working    crq saw it answer within the week
	//	quiet      it answered once, but not lately
	//	silent     crq has asked it and never seen an answer — the case that
	//	           matters, and the whole reason answering is tracked apart
	//	           from asking
	//	unverified enabled, but crq has never even asked it here
	//	off        not enabled on this fleet
	Status string `json:"status"`

	// The guide half. Everything below describes the vendor rather than crq's
	// configuration of it, and none of it affects a decision crq makes.
	Site     string   `json:"site,omitempty"`
	Docs     string   `json:"docs,omitempty"`
	Pitch    string   `json:"pitch,omitempty"`
	Cost     string   `json:"cost,omitempty"`
	Setup    []string `json:"setup,omitempty"`
	SuitedTo string   `json:"suited_to,omitempty"`
	// PricesCheckedAt dates the cost line, because an undated price is a price
	// that has quietly stopped being true.
	PricesCheckedAt string `json:"prices_checked_at,omitempty"`
	// Suggested says crq has a REASON to recommend this bot here, and Because
	// is that reason. A badge with no stated criterion is an advertisement.
	Suggested bool   `json:"suggested,omitempty"`
	Because   string `json:"because,omitempty"`
}

type SetupView struct {
	Checks []Check    `json:"checks"`
	Tools  []Tool     `json:"tools"`
	Hosts  []HostInfo `json:"hosts"`
	// Fleet is every host's own report of what it can reach — the answer to
	// "is claude installed" that is actually useful, since it differs per host
	// and, on any one host, between the shell and the service.
	Fleet []HostTools `json:"fleet,omitempty"`
	// Ready/Attention/Optional summarise the checks, so the page opens with a
	// verdict instead of a list to count.
	Ready     int `json:"ready"`
	Attention int `json:"attention"`
	Optional  int `json:"optional"`
	// ToolsHost names the machine the tool list describes. crq stores no tool
	// inventory for other hosts, so claiming a fleet-wide view would be a lie.
	ToolsHost string `json:"tools_host"`
}

type Check struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok|warn|bad|unknown
	Detail string `json:"detail,omitempty"`
}

type Tool struct {
	Name     string `json:"name"`
	Purpose  string `json:"purpose"`
	Required bool   `json:"required"`
	Found    bool   `json:"found"`
	Path     string `json:"path,omitempty"`
	// Fix is what to run when it is missing. A checklist that reports a
	// problem and leaves you to search for the remedy is a checklist that has
	// done the easy half.
	Fix []string `json:"fix,omitempty"`
}

type HostInfo struct {
	Name      string     `json:"name"`
	Roles     []string   `json:"roles,omitempty"`
	Health    string     `json:"health,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Failures  int        `json:"failures,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	// Caps is the capability bitmask the writer reported. A host on an older
	// binary reports fewer bits and silently ignores settings it cannot honor.
	Caps int `json:"caps,omitempty"`
}

type SettingsView struct {
	Config   FleetConfig `json:"config"`
	Quota    Quota       `json:"quota"`
	Plumbing []KV        `json:"plumbing"`
	// Env is every individual setting with its effective value and the layer
	// that decided it. This is what makes "I see env all over the dashboard"
	// actionable rather than just true.
	Env []EnvSetting `json:"env,omitempty"`
	// Fleet is the editable half: the defaults recorded for the whole fleet,
	// with a source per setting so a reader can tell which values changing here
	// would actually change everywhere, and which are this host's env alone.
	Fleet *FleetSettings `json:"fleet,omitempty"`
}

// EnvSetting is one configuration setting as the dashboard shows it.
type EnvSetting struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Group string `json:"group"`
	Label string `json:"label"`
	Help  string `json:"help"`
	// PerHost and Identity say WHY a setting is not editable here, which is a
	// more useful answer than a disabled control with no explanation.
	PerHost  bool `json:"per_host,omitempty"`
	Identity bool `json:"identity,omitempty"`
	// ReviewImpact marks settings whose save can reopen completed rounds. Those
	// go through the same live preview and revision-bound confirmation as the
	// fleet reviewer editor.
	ReviewImpact bool `json:"review_impact,omitempty"`

	Value  string `json:"value"`
	Source string `json:"source"` // fleet | env | default
	// HostValue is what this machine's own environment says, shown only when a
	// fleet record is overriding it — otherwise the override is invisible.
	HostValue string `json:"host_value,omitempty"`
}

// FleetSettings mirrors crq.FleetView on the wire.
type FleetSettings struct {
	Recorded       bool              `json:"recorded"`
	Reviewers      []FleetReviewer   `json:"reviewers"`
	MinInterval    string            `json:"min_interval"`
	WeeklyLimit    int               `json:"weekly_limit"`
	AutofixDefault bool              `json:"autofix_default"`
	Sources        map[string]string `json:"sources"`
	Overriding     []string          `json:"overriding,omitempty"`
	By             string            `json:"by,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
	Lagging        []string          `json:"lagging_hosts,omitempty"`
}

type FleetReviewer struct {
	Login    string `json:"login"`
	Budget   string `json:"budget"`
	Required bool   `json:"required"`
	Trigger  string `json:"trigger,omitempty"`
}

// FleetImpact is what a proposed fleet change would do, shown before it is made.
type FleetImpact struct {
	Rev        int64    `json:"rev"`
	Repos      int      `json:"repos"`
	Reopened   int      `json:"reopened"`
	Overridden int      `json:"overridden"`
	Changes    []string `json:"changes"`
	Summary    string   `json:"summary"`
}

type KV struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
}

// BuildFleet reduces everything the non-overview pages read.
func BuildFleet(st state.State, cfg FleetConfig, ov Overview, tools []Tool, toolsHost string, now time.Time, botsFor BotsFor, enrollFor EnrollFor, fleet *FleetSettings, solverFor SolverFor, env []EnvSetting) Snapshot {
	return Snapshot{
		Overview: ov,
		Repos:    repoRows(st, cfg, now, botsFor, enrollFor, solverFor),
		Bots:     botCards(st, cfg, botsFor, now),
		Setup:    setupView(st, cfg, ov, tools, toolsHost),
		Settings: SettingsView{Config: cfg, Quota: ov.Quota, Plumbing: plumbing(st, cfg), Fleet: fleet, Env: env},
	}
}

func repoRows(st state.State, cfg FleetConfig, now time.Time, botsFor BotsFor, enrollFor EnrollFor, solverFor SolverFor) []RepoRow {
	rows := map[string]*RepoRow{}
	get := func(repo string) *RepoRow {
		key := strings.ToLower(repo)
		if r, ok := rows[key]; ok {
			return r
		}
		r := &RepoRow{Repo: repo, Enrollment: "off", Autofix: "default"}
		rows[key] = r
		return r
	}

	// Every source that can mention a repo contributes a row. A repo that only
	// appears as a hold still belongs in the list: a hold is an operator
	// decision, and dropping it would hide one.
	for _, r := range st.Rounds {
		row := get(r.Repo)
		switch r.Phase {
		case state.PhaseQueued, state.PhaseAwaitingRetry:
			row.QueuedRounds++
			row.ActiveRounds++
		case state.PhaseReserved, state.PhaseFired, state.PhaseReviewing:
			row.ActiveRounds++
		}
	}
	for key := range st.Holds {
		repo, _ := splitKey(key)
		get(repo).HeldPRs++
	}
	for key, d := range st.Dispatches {
		// Live claims only, as the sessions table counts them: a crashed watcher's
		// claim outlives its process and must not leave a repository reading as
		// permanently under repair.
		if !d.Live(now) {
			continue
		}
		repo, _ := splitKey(key)
		get(repo).Fixing++
	}
	for repo, rv := range st.Repos {
		row := get(repo)
		row.Override = rv.SetCoBots || rv.SetRequired || rv.PrimaryOff
		row.OverrideBy, row.OverrideAt = rv.By, rv.UpdatedAt
		row.PrimaryOff = rv.PrimaryOff
	}
	for repo, sw := range st.RepoAutofix {
		row := get(repo)
		if sw.Enabled {
			row.Autofix = "on"
		} else {
			row.Autofix = "off"
		}
		row.AutofixReason, row.AutofixBy, row.AutofixAt = sw.Reason, sw.By, sw.UpdatedAt
	}
	for repo := range cfg.allowSet() {
		get(repo)
	}
	// A repository turned off from here has no rounds, no holds and no env
	// mention, so nothing above would list it — and an "off" nobody can see is
	// how a project quietly stops being reviewed.
	for _, repo := range st.EnrolledRepos() {
		get(repo)
	}
	// Solver settings are accepted for any repository, including one recorded
	// before it had a round or an enrollment. Its row is where that record is
	// read and cleared, so leaving it out hides an override nothing else shows.
	for _, repo := range st.SolverRepos() {
		get(repo)
	}

	// Resolved, not merged here: an override names co-reviewers by login while
	// the fleet default names them by short name, and half a repository's
	// answer (say, only its required set) still inherits the other half. Asking
	// the resolver for every row is the only way the list means one thing.
	for _, row := range rows {
		row.Reviewers, row.Required = nil, nil
		for _, b := range botsFor(row.Repo) {
			row.Reviewers = append(row.Reviewers, b.Name)
			if b.Required {
				row.Required = append(row.Required, b.Name)
			}
		}
		if enrollFor != nil {
			e := enrollFor(st, row.Repo)
			row.Enrollment, row.Reviewed, row.EnvConflict, row.ClearEnables = e.Source, e.Enabled, e.EnvConflict, e.ClearEnables
			row.EnrollReason, row.EnrollBy, row.EnrollAt = e.Reason, e.By, e.UpdatedAt
		} else {
			row.Enrollment = cfg.enrollmentOf(row.Repo)
			row.Reviewed = row.Enrollment == "env" || row.Enrollment == "scope"
		}
		if solverFor != nil {
			sv := solverFor(st, row.Repo)
			row.Solver = &sv
		}
	}

	out := make([]RepoRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

func (c FleetConfig) allowSet() map[string]bool {
	set := map[string]bool{}
	for _, r := range c.AllowRepos {
		set[r] = true
	}
	return set
}

func (c FleetConfig) enrollmentOf(repo string) string {
	lower := strings.ToLower(repo)
	for _, r := range c.ExcludeRepos {
		if strings.ToLower(r) == lower {
			return "excluded"
		}
	}
	for _, r := range c.AllowRepos {
		if strings.ToLower(r) == lower {
			return "env"
		}
	}
	if len(c.AllowRepos) == 0 {
		// With no allowlist the whole scope is in play, so a repo crq has seen
		// is being reviewed by virtue of its owner.
		return "scope"
	}
	return "unknown"
}

func botCards(st state.State, cfg FleetConfig, botsFor BotsFor, now time.Time) []BotCard {
	fleetBots := botsFor("")
	seen := map[string]*time.Time{}
	asked := map[string]*time.Time{}
	where := map[string]string{}
	noteAsked := func(login string, at *time.Time) {
		if at == nil {
			return
		}
		key := dialect.NormalizeBotName(login)
		if cur, ok := asked[key]; !ok || cur == nil || at.After(*cur) {
			asked[key] = at
		}
	}
	note := func(login string, at *time.Time, repo string, pr int) {
		if at == nil {
			return
		}
		key := dialect.NormalizeBotName(login)
		if cur, ok := seen[key]; !ok || cur == nil || at.After(*cur) {
			seen[key] = at
			where[key] = state.Key(repo, pr)
		}
	}
	// Two different facts, kept apart on purpose. AnsweredAt is the BOT: crq
	// observed it review this head. CommandedAt/ClaimedAt are crq: what it
	// posted and what it claimed the right to post. Reading the second as the
	// first is how a bot nobody has an account for reads as working — crq asks,
	// records that it asked, and nothing ever answers.
	// Whether the answer log has anything in it AT ALL, for any reviewer. It is
	// written only when crq observes a round, so on a fleet that has just
	// upgraded it is empty — and "no answer recorded" then means "not looked
	// yet", not "never answered". Claiming the second would accuse a working bot
	// of being unconfigured on the strength of a field introduced five minutes
	// ago. The primary counts towards it like any other reviewer now that its
	// answer is recorded rather than inferred from the phase.
	answerLog := false
	effectivePrimary := cfg.primaryLogin()
	for _, bot := range fleetBots {
		if bot.Primary {
			effectivePrimary = bot.Login
			break
		}
	}
	scan := func(r state.Round) {
		for login, co := range r.CoBots {
			if co.AnsweredAt != nil {
				answerLog = true
				note(login, co.AnsweredAt, r.Repo, r.PR)
			}
			if at := co.CommandedAt; at != nil {
				noteAsked(login, at)
			} else if co.ClaimedAt != nil {
				noteAsked(login, co.ClaimedAt)
			}
		}
		// The primary is different: its round holds no per-bot entry, so the two
		// facts come from two fields. FiredAt is crq's command going out — what
		// it ASKED. PrimaryAnsweredAt is what crq observed the primary do.
		//
		// The phase cannot stand in for the second. A required set that omits the
		// primary completes as soon as its co-reviewers answer and the primary
		// acknowledges the metered command, so reading a completed round as
		// review evidence labelled a reviewer as working on the strength of its
		// own acknowledgement.
		if r.FiredAt != nil && !r.CoOnly {
			by := effectivePrimary
			for _, posted := range r.PostedCommands {
				if posted.ID == r.CommandID && posted.Bot != "" {
					by = posted.Bot
					break
				}
			}
			noteAsked(by, r.FiredAt)
		}
		if r.PrimaryAnsweredAt != nil {
			answerLog = true
			// Whoever the primary was WHEN it answered, not whoever this
			// process calls its primary now. A fleet that changes CRQ_BOT would
			// otherwise hand the retired bot's evidence to the new one, showing
			// the one that has never run as working and the one that did as
			// silent. Rounds recorded before the login was stored have none, and
			// the running primary is the best guess left for those.
			by := r.PrimaryAnsweredBy
			if by == "" {
				by = effectivePrimary
			}
			note(by, r.PrimaryAnsweredAt, r.Repo, r.PR)
		}
	}
	for _, r := range st.Rounds {
		scan(r)
	}
	for _, r := range st.Archive {
		scan(r)
	}

	repoCount := map[string]int{}
	repoBots := map[string]BotName{}
	repoRequired := map[string]bool{}
	repos := make([]string, 0, len(st.Repos))
	for repo := range st.Repos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, b := range botsFor(repo) {
			key := dialect.NormalizeBotName(b.Login)
			repoCount[key]++
			repoBots[key] = b
			repoRequired[key] = repoRequired[key] || b.Required
		}
	}

	// The EFFECTIVE reviewer set, which is env plus whatever the fleet recorded
	// — not cfg.Reviewers, which is this server's startup environment and would
	// keep showing the old answer after a fleet default changed it.
	running := map[string]BotName{}
	for _, b := range fleetBots {
		running[dialect.NormalizeBotName(b.Login)] = b
	}

	// Every registry bot gets a card, running here or not. A page that lists
	// only the enabled ones cannot answer "why is Bugbot not reviewing this",
	// and it cannot offer the switch that would change the answer.
	out := make([]BotCard, 0, len(cfg.Reviewers)+len(dialect.KnownCoReviewers()))
	add := func(login, name string, primary, metered bool, from *ReviewerCfg) {
		key := dialect.NormalizeBotName(login)
		b, on := running[key]
		if !on {
			b, on = repoBots[key]
		}
		_, configurable := dialect.CoReviewerByName(login)
		card := BotCard{
			Login: login, Name: name, Primary: primary, Metered: metered,
			Enabled: on, Required: on && (b.Required || repoRequired[key]), Configurable: configurable,
			LastSeen: seen[key], LastAsked: asked[key], SeenOn: where[key],
			RepoCount: repoCount[key],
			Status:    botStatus(on, seen[key], asked[key], now, answerLog),
		}
		if v, ok := dialect.VendorFor(login); ok {
			card.Site, card.Docs = v.Site, v.Docs
			card.Pitch, card.Cost, card.Setup, card.SuitedTo = v.Pitch, v.Cost, v.Setup, v.SuitedTo
			card.PricesCheckedAt = dialect.PricesCheckedAt
			// A suggestion is only worth making when crq can name the evidence.
			// The local signal is the honest one it has: a CLI on a host means
			// an account behind it, which is the thing that decides whether a
			// bot will ever answer.
			// Anything crq has not seen working. That covers the two cases worth
			// a nudge: a bot switched off that you evidently have an account
			// for, and one switched ON that has never answered — where the CLI
			// being present says the account is fine and the setup is not.
			if card.Status != "working" {
				if host, path := whereTool(st, name); path != "" {
					card.Suggested = true
					card.Because = "the " + name + " CLI is on " + host +
						", so the account behind it is probably already yours"
				}
			}
		}
		if on {
			card.Command, card.Trigger, card.Grace = b.Command, b.Trigger, b.Grace
		} else if from != nil {
			card.Command, card.Trigger, card.Grace = from.Command, from.Trigger, from.Grace
		}
		out = append(out, card)
	}
	seenCard := map[string]bool{}
	primaryKey := dialect.NormalizeBotName(effectivePrimary)
	// Start with the effective fleet set, including its current primary. The
	// startup config is metadata and fallback only; it must not keep the retired
	// primary marked primary after shared settings replace it.
	for _, b := range fleetBots {
		key := dialect.NormalizeBotName(b.Login)
		seenCard[key] = true
		from := &ReviewerCfg{
			Login: b.Login, Name: b.Name, Primary: b.Primary, Required: b.Required,
			Metered: b.Primary, Command: b.Command, Trigger: b.Trigger, Grace: b.Grace,
		}
		add(b.Login, b.Name, b.Primary, b.Primary, from)
	}
	for i := range cfg.Reviewers {
		r := cfg.Reviewers[i]
		key := dialect.NormalizeBotName(r.Login)
		if seenCard[key] {
			continue
		}
		seenCard[key] = true
		primary := key == primaryKey
		add(r.Login, r.Name, primary, primary && r.Metered, &r)
	}
	for _, co := range dialect.KnownCoReviewers() {
		if seenCard[dialect.NormalizeBotName(co.Login)] {
			continue
		}
		add(co.Login, co.Name, false, false, nil)
	}
	return out
}

func (c FleetConfig) primaryLogin() string {
	for _, r := range c.Reviewers {
		if r.Primary {
			return r.Login
		}
	}
	return c.GateRepo // never matches a bot; keeps note() harmless
}

func setupView(st state.State, cfg FleetConfig, ov Overview, tools []Tool, toolsHost string) SetupView {
	v := SetupView{Tools: tools, ToolsHost: toolsHost, Checks: []Check{}, Hosts: []HostInfo{}}
	v.Fleet = hostTools(st, ov.Now)

	add := func(key, label, status, detail string) {
		v.Checks = append(v.Checks, Check{Key: key, Label: label, Status: status, Detail: detail})
	}
	add("state", "Queue home", "ok",
		cfg.GateRepo+" · ref "+cfg.StateRef+" · rev "+itoa(ov.Rev))
	if cfg.DashboardIssue > 0 {
		add("dashboard", "Markdown dashboard", "ok", "issue #"+itoa(int64(cfg.DashboardIssue)))
	} else {
		add("dashboard", "Markdown dashboard", "warn", "not configured (CRQ_ISSUE)")
	}
	if cfg.CalibrationPR > 0 {
		add("calibration", "Quota calibration", "ok", "PR #"+itoa(int64(cfg.CalibrationPR)))
	} else {
		add("calibration", "Quota calibration", "warn", "no calibration PR — quota is guessed from notices")
	}
	switch {
	case ov.Leader == nil:
		add("leader", "Review daemon", "bad", "no host holds the leader lease")
	case ov.Leader.Expired:
		add("leader", "Review daemon", "warn", "lease expired · last held by "+ov.Leader.Host)
	default:
		add("leader", "Review daemon", "ok", "leader "+ov.Leader.Host)
	}
	missing := 0
	for _, t := range tools {
		if t.Required && !t.Found {
			missing++
		}
	}
	if missing == 0 {
		add("tools", "Required tools", "ok", "present on "+toolsHost)
	} else {
		add("tools", "Required tools", "bad", itoa(int64(missing))+" missing on "+toolsHost)
	}
	if len(ov.Autofix.Hosts) == 0 {
		add("autofix", "Autofix", "unknown", "no host has reported a fix session")
	} else {
		bad := 0
		for _, h := range ov.Autofix.Hosts {
			if h.Health == "unhealthy" {
				bad++
			}
		}
		if bad > 0 {
			add("autofix", "Autofix", "bad", itoa(int64(bad))+" host(s) failing")
		} else {
			add("autofix", "Autofix", "ok", itoa(int64(len(ov.Autofix.Hosts)))+" host(s) reporting")
		}
	}

	// Hosts merge three sources: who writes state, who runs autofix, and who
	// holds the lease.
	hosts := map[string]*HostInfo{}
	get := func(name string) *HostInfo {
		if h, ok := hosts[name]; ok {
			return h
		}
		h := &HostInfo{Name: name}
		hosts[name] = h
		return h
	}
	for id, w := range st.Writers {
		h := get(hostOf(id))
		if h.LastSeen == nil || w.At.After(*h.LastSeen) {
			at := w.At
			h.LastSeen = &at
			h.Caps = w.Caps
		}
	}
	for _, ah := range ov.Autofix.Hosts {
		h := get(ah.Name)
		h.Health, h.Failures, h.LastError = ah.Health, ah.Failures, ah.LastError
		h.Roles = append(h.Roles, "autofix")
	}
	if ov.Leader != nil && !ov.Leader.Expired {
		get(ov.Leader.Host).Roles = append(get(ov.Leader.Host).Roles, "leader")
	}
	for _, h := range hosts {
		sort.Strings(h.Roles)
		v.Hosts = append(v.Hosts, *h)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Name < v.Hosts[j].Name })
	for _, c := range v.Checks {
		switch c.Status {
		case "ok":
			v.Ready++
		case "bad", "warn":
			v.Attention++
		}
	}
	for _, t := range v.Tools {
		switch {
		case t.Found:
			v.Ready++
		case t.Required:
			v.Attention++
		default:
			v.Optional++
		}
	}
	return v
}

func plumbing(st state.State, cfg FleetConfig) []KV {
	out := []KV{
		{Key: "Gate repo", Value: cfg.GateRepo, Detail: "holds the state ref, dashboard issue and calibration PR"},
		{Key: "State ref", Value: cfg.StateRef, Detail: "schema v" + itoa(int64(st.Version))},
		{Key: "Revision", Value: itoa(st.Rev)},
	}
	if st.UpdatedAt != nil {
		out = append(out, KV{Key: "Last written", Value: st.UpdatedAt.Format(time.RFC3339)})
	}
	if st.Leader != nil {
		out = append(out, KV{Key: "Leader lease", Value: hostOf(st.Leader.Owner),
			Detail: "expires " + st.Leader.ExpiresAt.Format(time.RFC3339)})
	}
	if cfg.DashboardIssue > 0 {
		out = append(out, KV{Key: "Markdown dashboard", Value: "issue #" + itoa(int64(cfg.DashboardIssue)),
			Detail: "still updated — the web dashboard is additive"})
	}
	if cfg.CalibrationPR > 0 {
		out = append(out, KV{Key: "Calibration PR", Value: "#" + itoa(int64(cfg.CalibrationPR))})
	}
	out = append(out, KV{Key: "Writers seen", Value: itoa(int64(len(st.Writers))), Detail: "24h window"})
	return out
}

// LocalTools probes this machine only. crq keeps no tool inventory for other
// hosts, and inventing one from a version string would be worse than saying so.
func LocalTools() []Tool {
	want := []struct {
		name, purpose string
		required      bool
		fix           []string
	}{
		{"crq", "the binary itself — every host must run the same version", true,
			[]string{"go build -o ~/.local/bin/crq ./cmd/crq", "crq doctor   # check for a second, older install"}},
		{"git", "repository mirrors and worktrees for fix sessions", true, nil},
		{"gh", "GitHub CLI — where the token comes from", true,
			[]string{"gh auth login", "crq doctor"}},
		{"claude", "fix agent — writes the fixes. Not a reviewer.", false,
			[]string{"npm i -g @anthropic-ai/claude-code", "crq autofix install   # point the service at it"}},
		// Named the same as the review bot and completely unrelated to it: the
		// Codex REVIEWER is a GitHub app and needs nothing installed here. A
		// host without this CLI reviews perfectly well.
		{"codex", "fix agent — unrelated to the Codex reviewer, which is a GitHub app", false,
			[]string{"npm i -g @openai/codex", "crq autofix install --agent \"$(command -v codex)\""}},
		{"coderabbit", "local preflight before pushing — unrelated to the CodeRabbit reviewer", false,
			[]string{"curl -fsSL https://cli.coderabbit.ai/install.sh | sh"}},
		{"macroscope", "local preflight before pushing — unrelated to the Macroscope reviewer", false,
			[]string{"see https://docs.macroscope.com for the CLI"}},
	}
	out := make([]Tool, 0, len(want))
	for _, w := range want {
		t := Tool{Name: w.name, Purpose: w.purpose, Required: w.required, Fix: w.fix}
		if path, err := exec.LookPath(w.name); err == nil {
			t.Found, t.Path = true, path
		}
		out = append(out, t)
	}
	return out
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// botStatus is what crq can say about a bot's setup WITHOUT asking the vendor,
// which it has no way to do for any of them.
//
// "Enabled" and "working" are different claims and the page must not merge
// them: a bot enabled by a default nobody chose, on an account nobody has, is
// exactly the case that looks configured and reviews nothing.
func botStatus(enabled bool, lastSeen, lastAsked *time.Time, now time.Time, logWarm bool) string {
	switch {
	case !enabled:
		return "off"
	case lastSeen == nil && !logWarm:
		// Nothing has been observed since crq started recording answers, so
		// there is no basis to say this bot has never answered.
		return "unverified"
	case lastSeen == nil && lastAsked != nil:
		// Asked and never answered. The most useful thing the page can say, and
		// the reason answering is tracked separately from asking at all.
		return "silent"
	case lastSeen == nil:
		return "unverified"
	case now.Sub(*lastSeen) <= 7*24*time.Hour:
		return "working"
	default:
		return "quiet"
	}
}

// hostTools turns every host's self-report into the matrix the setup page
// shows: one row per host, one column per tool.
//
// A stale report is kept and marked rather than dropped. "atlas last said it
// had claude, two days ago" is a more useful thing to read than an empty row,
// and a host that has stopped reporting is itself the finding.
func hostTools(st state.State, now time.Time) []HostTools {
	reports := st.HostReportList()
	if len(reports) == 0 {
		return nil
	}
	newest := ""
	for _, r := range reports {
		if newerVersion(r.Version, newest) {
			newest = r.Version
		}
	}
	out := make([]HostTools, 0, len(reports))
	for _, r := range reports {
		at := r.At
		row := HostTools{
			Host: hostOf(r.Host), Version: r.Version, Caps: r.Caps, Roles: r.Roles, Agent: r.Agent,
			At: &at, Stale: now.Sub(r.At) > state.HostReportTTL,
			// Behind is "not the newest crq in the fleet", so it only has to be
			// as right as newest is. That is why newest is picked numerically:
			// as strings, 2.9.0 outranks 2.10.0 and the table inverts — the
			// upgraded hosts get the warning and the stale ones read as current.
			Behind: r.Version != "" && newest != "" && r.Version != newest,
			Tools:  []ToolSeen{},
		}
		for _, t := range r.Tools {
			row.Tools = append(row.Tools, ToolSeen{Name: t.Name, Path: t.Path, Version: t.Version})
		}
		out = append(out, row)
	}
	return out
}

// newerVersion reports whether a is a later crq than b, comparing the
// dot-separated components as numbers. Text order is wrong the first time the
// fleet crosses a digit boundary — "2.9.0" sorts above "2.10.0" — and picking
// the wrong newest inverts the whole table. A component that is not a number
// falls back to text order, so an unexpected shape is still ordered rather than
// ignored.
func newerVersion(a, b string) bool {
	if a == "" || a == b {
		return false
	}
	if b == "" {
		return true
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := versionPart(as, i), versionPart(bs, i)
		if x == y {
			continue
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		if xerr == nil && yerr == nil {
			return xn > yn
		}
		return x > y
	}
	return false
}

// versionPart is one component of a version, or "0" past its end: "2.1" and
// "2.1.0" are the same release.
func versionPart(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return "0"
}

// whereTool finds a host that reports having a tool, and where. Used for bot
// suggestions: a CLI present on a machine is evidence of an account behind it,
// which is what decides whether that bot will ever answer.
//
// It reports the FIRST host that has it rather than all of them, because the
// suggestion only needs to be true somewhere — and naming one machine reads as
// evidence where naming three reads as a survey.
func whereTool(st state.State, tool string) (string, string) {
	for _, r := range st.HostReportList() {
		for _, t := range r.Tools {
			if t.Name == tool && t.Path != "" {
				return hostOf(r.Host), t.Path
			}
		}
	}
	return "", ""
}
