package crq

import (
	"fmt"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
	crqstate "github.com/kristofferR/coderabbit-queue/internal/state"
)

// The persisted schema and its store live in internal/state (v3). These
// aliases keep the crq orchestration code referring to State/Round/… without
// the package qualifier, and without colliding with the many `state`/`st`
// variable names in this package.
type (
	State          = crqstate.State
	Round          = crqstate.Round
	Hold           = crqstate.Hold
	Phase          = crqstate.Phase
	FireSlot       = crqstate.FireSlot
	AccountQuota   = crqstate.AccountQuota
	LeaderLease    = crqstate.LeaderLease
	PostedCommand  = crqstate.PostedCommand
	WorkClaim      = crqstate.WorkClaim
	RepoReviewers  = crqstate.RepoReviewers
	RepoEnrollment = crqstate.RepoEnrollment
	FleetDefaults  = crqstate.FleetDefaults
	SolverSettings = crqstate.SolverSettings
	CoBotRound     = crqstate.CoBotRound
	HostReport     = crqstate.HostReport
	ToolReport     = crqstate.ToolReport
	Revision       = crqstate.Revision
	StateStore     = crqstate.StateStore
	StoreConfig    = crqstate.StoreConfig

	LeaderCapabilityLease = crqstate.LeaderCapabilityLease

	// RepoAutofixSwitch is one repository's answer to whether crq may fix it.
	RepoAutofixSwitch = crqstate.RepoAutofixSwitch
)

const (
	ArchiveMax         = crqstate.ArchiveMax
	PhaseQueued        = crqstate.PhaseQueued
	PhaseReserved      = crqstate.PhaseReserved
	PhaseFired         = crqstate.PhaseFired
	PhaseReviewing     = crqstate.PhaseReviewing
	PhaseAwaitingRetry = crqstate.PhaseAwaitingRetry
	PhaseCompleted     = crqstate.PhaseCompleted
	PhaseAbandoned     = crqstate.PhaseAbandoned

	// DispatchTTL is how long a watcher's fix claim survives without a heartbeat.
	DispatchTTL = crqstate.DispatchTTL
	// CapsWorkClaims is the capability an autofix host needs to honour
	// interactive PR ownership.
	CapsWorkClaims = crqstate.CapsWorkClaims
	// AutofixUnhealthyAfter is how many passes may fail to dispatch before crq says so.
	AutofixUnhealthyAfter = crqstate.AutofixUnhealthyAfter
	// CapsRepoOverrides is the binary capability per-repo reviewer overrides need.
	CapsRepoOverrides = crqstate.CapsRepoOverrides
	// CapsPrimaryOff is the capability a host needs to honour a repository that
	// turned the metered primary off.
	CapsPrimaryOff = crqstate.CapsPrimaryOff
	// CapsEnrollment is the capability a host needs to honour enrollment records.
	CapsEnrollment = crqstate.CapsEnrollment
	// CapsFleetDefaults is the capability a host needs to honour fleet defaults.
	CapsFleetDefaults = crqstate.CapsFleetDefaults
	// CapsSolver is the capability a host needs to honour solver settings.
	CapsSolver = crqstate.CapsSolver
	// CapsPreflightSkipBlocked is the capability a host needs to honour the
	// shared blocked-preflight policy.
	CapsPreflightSkipBlocked = crqstate.CapsPreflightSkipBlocked
	// WriterCaps is what THIS binary understands, recorded in each host's
	// self-report so a fleet can see why a host ignores a setting.
	WriterCaps = crqstate.WriterCaps
	// HostReportTTL is how long a host's self-report counts as current.
	HostReportTTL = crqstate.HostReportTTL
)

var (
	ErrCASConflict = crqstate.ErrCASConflict
	ErrNoChange    = crqstate.ErrNoChange
	cloneState     = crqstate.Clone
)

func (c Config) storeConfig() StoreConfig {
	return StoreConfig{
		GateRepo:       c.GateRepo,
		StateRef:       c.StateRef,
		DashboardIssue: c.DashboardIssue,
		Timezone:       c.Timezone,
		Scope:          c.Scope,
		CoReviewers:    c.coReviewerSummary(),
		ResolveCoReviewers: func(fleet FleetDefaults) string {
			return c.WithFleet(fleet).coReviewerSummary()
		},
		Host:        c.WriterID(),
		MinInterval: c.MinInterval,
	}
}

// coReviewerSummary renders the enabled co-reviewers for the dashboard row:
// "codex (required, always) · bugbot (selfheal) · macroscope (selfheal)".
// Empty with no co-bots so existing dashboards stay byte-identical.
func (c Config) coReviewerSummary() string {
	parts := make([]string, 0, len(c.CoBots))
	for _, cb := range c.CoBots {
		attrs := []string{}
		if cb.Required {
			attrs = append(attrs, "required")
		}
		if cb.Trigger != "" && cb.Trigger != engine.TriggerNever {
			attrs = append(attrs, string(cb.Trigger))
		}
		part := cb.Name
		if len(attrs) > 0 {
			part += " (" + strings.Join(attrs, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// NewGitStateStore builds the git-ref-backed store. Dashboard rendering resolves
// the fleet record from the state being written rather than using this host's
// frozen startup configuration.
func NewGitStateStore(cfg Config, gh *ghapi.GitHub, log Logger) *crqstate.GitStateStore {
	store := crqstate.NewGitStateStore(cfg.storeConfig(), gh, log)
	store.SetRenderConfig(func(st State) StoreConfig {
		return cfg.WithFleet(st.Fleet).storeConfig()
	})
	return store
}

func NewMemoryStore(cfg Config) *crqstate.MemoryStore {
	return crqstate.NewMemoryStore(cfg.storeConfig())
}

// DefaultState returns a fresh current-schema state seeded with the configured scope, used
// by tests and init.
func DefaultState(cfg Config) State {
	st := crqstate.New()
	st.Account.Scope = strings.Join(cfg.Scope, ",")
	st.Account.Source = "init"
	return st
}

func renderDashboard(st State, cfg Config) string {
	return crqstate.RenderDashboard(st, cfg.WithFleet(st.Fleet).storeConfig())
}
func renderTitle(st State, cfg Config) string {
	return crqstate.RenderTitle(st, cfg.WithFleet(st.Fleet).storeConfig())
}

// StatusLine renders the queue as a single line for a harness status bar.
func StatusLine(st State, cfg Config) string {
	return crqstate.StatusLine(st, cfg.WithFleet(st.Fleet).storeConfig())
}

func issueBody(st State, cfg Config) (string, error) {
	return crqstate.IssueBody(st, cfg.WithFleet(st.Fleet).storeConfig())
}

// policy assembles the engine Policy from config.
func (c Config) policy() engine.Policy {
	p := engine.Policy{
		Bot:                c.Bot,
		RequiredBots:       c.RequiredBots,
		PrimaryOff:         c.PrimaryOff,
		MinInterval:        c.MinInterval,
		InflightTimeout:    c.InflightTimeout,
		RateLimitFallback:  c.RateLimitFallback,
		RateLimitCoDegrade: c.RateLimitCoDegrade,
	}
	for _, cb := range c.CoBots {
		// A registry-backed primary keeps a silenced CoBots entry so observation
		// can use that registry's wording and check hooks. It is still the
		// primary, not a dynamic co-reviewer convergence gate.
		if sameBot(cb.Login, c.Bot) {
			continue
		}
		p.CoReviewers = append(p.CoReviewers, engine.CoReviewerPolicy{
			Login:         cb.Login,
			Command:       cb.Command,
			Trigger:       cb.Trigger,
			SelfHealGrace: cb.SelfHealGrace,
		})
	}
	return p
}

func NormalizeRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimSuffix(repo, ".git")
	return strings.ToLower(repo)
}

func QueueKey(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", NormalizeRepo(repo), pr)
}
