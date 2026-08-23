package crq

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

const Version = "2.0.0"

// The GitHub transport tags its User-Agent with the crq version. Version lives
// in this package, so the wiring stays here now that the gh alias layer is gone.
func init() { ghapi.UserAgent = "crq/" + Version }

type Config struct {
	GateRepo       string
	DashboardIssue int
	CalibrationPR  int
	Scope          []string
	AllowRepos     map[string]bool
	ExcludeRepos   map[string]bool
	// SkipAuthors lists PR authors autoreview never enqueues (normalized: lowercase,
	// no "[bot]" suffix). Defaults to dependabot; set CRQ_AUTOREVIEW_SKIP_AUTHORS=""
	// to review bot PRs too. Manual `crq review` is unaffected.
	SkipAuthors map[string]bool
	// SkipMarker suppresses fleet auto-review when present in a PR body.
	// Manual `crq loop` remains unaffected so an explicit review can override it.
	// Read it through SkipsReview, never with a plain substring test.
	SkipMarker    string
	StateRef      string
	Bot           string
	RequiredBots  []string
	FeedbackBots  []string
	ReviewCommand string
	// CoBots are the enabled co-reviewer bots (CRQ_COBOTS + per-bot
	// CRQ_COBOT_<NAME>_* keys). An entry exists for every wanted or required
	// co-reviewer; required ones are already folded into RequiredBots.
	CoBots []CoBotConfig
	// KnownCoBots is every registry co-reviewer with the operator's per-bot
	// environment applied, enabled here or not. A repository override picks from
	// this, so enabling a bot the fleet disabled still honours CRQ_COBOT_<NAME>_*
	// instead of silently falling back to registry defaults.
	KnownCoBots []CoBotConfig
	// Reviewers is the single description of who reviews and what they cost.
	// Bot / RequiredBots / FeedbackBots / CoBots above are DERIVED from it and
	// kept only so existing consumers keep compiling; new code should read this.
	Reviewers []Reviewer
	// PrimaryOff is set by ForRepo when a repository turned the metered primary
	// off. It is never a fleet setting: CRQ_BOT always names one, and "the fleet
	// has no primary" is expressed by requiring nobody, not by this.
	PrimaryOff bool
	// WatchInterval paces `crq watch`; DispatchCommand is the fix session it
	// runs with --dispatch, argv-style; DispatchMaxAttempts bounds dispatches per
	// head so a fix that keeps not working stops.
	WatchInterval   time.Duration
	DispatchCommand []string
	// FixAgent is the agent binary a fix session execs, chosen at install time
	// and exported to the autofix unit as CRQ_FIX_AGENT. It is per HOST, not per
	// repository: switching between claude and codex is a different command line
	// rather than a different flag. Empty on a machine that runs no fix sessions,
	// which is not the same as any particular agent.
	FixAgent            string
	DispatchMaxAttempts int
	// FixModels/FixEffort/FixPrompt are the per-repository solver settings, put
	// into the fix session's ENVIRONMENT rather than its argv. Argv is fixed
	// when the watcher starts; the environment is built per dispatch, which is
	// the only layer that can differ between two repositories the same watcher
	// is handling.
	FixModels []string
	// FixModel is the preferred entry, retained for the session environment and
	// older callers that only understand one model.
	FixModel  string
	FixEffort string
	FixPrompt string
	// FixSeverities is the set of finding severities a session may act on.
	// FixAskMode controls when it should stop and surface a clarification.
	FixSeverities map[string]bool
	FixAskMode    string
	// OnePass caps automated review at the first round for a pull request.
	// MergeMethod optionally merges the exact head a successful fixer released.
	// Both come from repository/fleet solver state, never the host environment.
	OnePass     bool
	MergeMethod string
	// DispatchForks allows fix sessions on pull requests whose head branch lives
	// in another repository. Off by default: a session runs an agent over that
	// branch's code with approvals bypassed and a write token in reach.
	DispatchForks bool
	// DispatchConcurrency caps concurrent fix sessions. 0 (the default) means no
	// cap: fixing findings spends no account quota, so it does not belong in a
	// queue. It is a resource valve for a machine that cannot take the load.
	DispatchConcurrency int
	// WorkspaceRoot holds crq's own mirrors and worktrees. Read here rather than
	// from the process environment, so a value in ~/.config/crq/env — the
	// documented place for crq settings — is actually used.
	WorkspaceRoot string
	// WorkDir is the checkout the local-work probe inspects. Empty means the
	// process's own directory, which is what an agent running crq from its
	// working copy means. Set programmatically by a caller working in a
	// worktree it made, since that caller is not standing in one.
	WorkDir string
	// WorkOwner is the optional stable identity for interactive work claims.
	// It is parsed with the rest of the configuration so ~/.config/crq/env and
	// the process environment follow the same precedence.
	WorkOwner string
	// FeedbackBotsExplicit records that CRQ_FEEDBACK_BOTS was set. It is the one
	// list an operator may widen beyond who reviews, so neither LoadConfig's
	// derivation nor a per-repo override may quietly replace it.
	FeedbackBotsExplicit bool
	// OverrideAt is when the per-repo reviewer override this configuration was
	// built from was last written, so a fire can tell whether it still holds.
	OverrideAt *time.Time
	// FleetAt is the same fact one layer up: when the fleet defaults this
	// configuration was built from were last written. Both are needed, because
	// either layer can name the primary, the co-reviewers or the required set —
	// and a fire revalidating only the repository override would post the old
	// commands on the authority of fleet settings that were replaced while it
	// was deciding.
	FleetAt           *time.Time
	RateLimitCommand  string
	RateLimitMarker   string
	CalibrationMarker string
	ReviewDoneMarker  string
	// CompletionMarker identifies the bot's reply to a processed review command
	// (CodeRabbit: "Review finished."). Feedback uses it to count a command
	// round that produced no review object toward convergence.
	CompletionMarker  string
	Host              string
	Timezone          string
	MinInterval       time.Duration
	InflightTimeout   time.Duration
	PollInterval      time.Duration
	WaitTimeout       time.Duration
	CalibrationTTL    time.Duration
	RateLimitFallback time.Duration
	AutoReviewPoll    time.Duration
	AutoReviewMaxScan int
	// WeeklyReviewLimit is the vendor's weekly fair-use threshold, past which
	// reviews are throttled to roughly one an hour. Configurable because it is
	// plan-dependent (60 on Pro, 90 on Pro+) and no API reports it; 0 disables
	// the forecast without disabling the count.
	WeeklyReviewLimit int
	LeaderTTL         time.Duration
	FiredMax          int
	// Tidy removes crq's own spent review-trigger comments as rounds progress.
	// It is opt-in while older fleet binaries share the state ref: those
	// binaries preserve tombstones as unknown fields but cannot use them when
	// pairing a delayed reply. CRQ_TIDY=1 turns it on.
	Tidy                bool
	NoOpen              bool
	DryRun              bool
	FeedbackWaitTimeout time.Duration
	// SettleWindow keeps a converged loop polling briefly before it exits 0, so
	// a trailing review wave (a Codex auto-review of the just-pushed head, a
	// CodeRabbit review following its comment shells) is caught by crq instead
	// of by a human re-checking the PR. 0 disables.
	SettleWindow time.Duration
	// RateLimitCoDegrade degrades an account-blocked round to co-reviewers only
	// (return their findings promptly, keep CodeRabbit queued for the window)
	// instead of waiting the block out. CRQ_RL_CO_DEGRADE, default on.
	RateLimitCoDegrade bool
	// PreflightSkipBlocked lets local preflight satisfy its gate from a live
	// shared account block instead of making a CodeRabbit request that must fail.
	// CRQ_PREFLIGHT_SKIP_BLOCKED, default on.
	PreflightSkipBlocked bool

	// env is the map this configuration was parsed from, so WithFleet can
	// re-parse it with the fleet's overrides layered on.
	env map[string]string
}

// fixAgent is the agent this host's fix sessions would exec: the one the
// autofix install chose, or the first word of a legacy CRQ_DISPATCH_CMD. Empty
// on a machine that runs none.
func (c Config) fixAgent() string {
	if c.FixAgent != "" {
		return c.FixAgent
	}
	if len(c.DispatchCommand) > 0 {
		return c.DispatchCommand[0]
	}
	return ""
}

// Env is a copy of the environment this configuration was parsed from, for
// callers that need to show or compare it. A copy, because handing out the map
// would let a caller change a parsed configuration by mutating its input.
func (c Config) Env() map[string]string {
	out := make(map[string]string, len(c.env))
	for k, v := range c.env {
		out[k] = v
	}
	return out
}

// ConfigPath is the file crq reads its settings from: CRQ_CONFIG, or
// ~/.config/crq/env. Empty when neither can be resolved.
//
// Exported because a service unit has to be pointed at the SAME file the install
// read. An autofix service that loads a different configuration is one that watches nothing
// while reporting itself started.
//
// Absolute, always: a relative CRQ_CONFIG resolves against the invoking shell's
// directory, and the service starts somewhere else entirely — so the install
// would read the file, write that relative string into the unit, and the service
// would load no configuration at all.
func ConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("CRQ_CONFIG")); path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "crq", "env")
}

func LoadConfig() (Config, error) {
	env := map[string]string{}
	configPath := ConfigPath()
	if configPath != "" {
		values, err := readEnvFile(configPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		for k, v := range values {
			env[k] = v
		}
		// The GitHub credential is resolved from the PROCESS environment
		// (internal/gh, and `git` through it), not from this map — so a token
		// configured here would authenticate nothing. Exporting it is what lets a
		// service unit carry no secret of its own: point it at this file.
		// gh treats these as aliases, with GITHUB_TOKEN taking precedence. Treat
		// them as one setting here too: exporting a file GITHUB_TOKEN when the
		// process supplied GH_TOKEN would silently replace the caller's explicit
		// credential with the file's account.
		if os.Getenv("GITHUB_TOKEN") == "" && os.Getenv("GH_TOKEN") == "" {
			for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
				if v := strings.TrimSpace(values[key]); v != "" {
					os.Setenv(key, v)
				}
			}
		}
	}
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			env[k] = v
		}
	}
	return BuildConfig(env)
}

// BuildConfig parses one env map into a configuration.
//
// Split out of LoadConfig so the SAME parse can be re-run over a merged map:
// this host's environment with the fleet's recorded overrides layered on top.
// Re-parsing rather than patching individual fields is what makes any setting
// fleet-settable without each one needing its own plumbing — and what stops the
// two paths ever disagreeing about what a value means.
func BuildConfig(env map[string]string) (Config, error) {
	host, _ := os.Hostname()
	bot := stringEnv(env, "CRQ_BOT", "coderabbitai[bot]")
	requiredBots := listEnv(env, "CRQ_REQUIRED_BOTS", bot)
	coBots, err := parseCoBots(env, requiredBots)
	if err != nil {
		return Config{}, err
	}
	// A required co-reviewer gates convergence via RequiredBots membership —
	// that list stays the single source of required-ness.
	for _, cb := range coBots {
		if cb.Required {
			requiredBots = unionBots(requiredBots, []string{cb.Login})
		}
	}
	// CRQ_BOT may name a registry bot — pointing crq at Codex as the primary is a
	// real configuration. It is then the primary and must not ALSO be driven as a
	// co-reviewer: DecideFire posts its review command and fireCoOnly would post
	// its co-reviewer trigger, asking the same reviewer twice.
	//
	// Silenced, not removed. The registry entry is also where that bot's wording
	// and check-run hooks come from (classifierCoReviewers, coChecksRelevant both
	// walk CoBots), so dropping it would cost the primary its evidence: a Codex
	// clean summary would read as a generic no-action event, and a check-only
	// result would not be fetched at all — crq would fire and then time out with
	// current-head evidence sitting in front of it. Every trigger post goes
	// through engine.DecideCoPost, which posts nothing for a never trigger.
	coBots = silenceTrigger(coBots, bot)
	// Enabled co-reviewers surface findings without gating: their logins join
	// the feedback set unless CRQ_FEEDBACK_BOTS overrides explicitly.
	coLogins := make([]string, 0, len(coBots))
	for _, cb := range coBots {
		coLogins = append(coLogins, cb.Login)
	}
	fixSeverities := severitySet(stringEnv(env, "CRQ_FIX_SEVERITIES", strings.Join(dialect.KnownSeverities(), ",")))
	for severity := range fixSeverities {
		if !dialect.IsSeverity(severity) {
			return Config{}, fmt.Errorf("CRQ_FIX_SEVERITIES: severity %q is not one of %s",
				severity, strings.Join(dialect.KnownSeverities(), ", "))
		}
	}
	cfg := Config{
		GateRepo:          env["CRQ_REPO"],
		DashboardIssue:    intEnv(env, "CRQ_ISSUE", 0),
		CalibrationPR:     intEnv(env, "CRQ_CAL_PR", 0),
		Scope:             listEnv(env, "CRQ_SCOPE", ownerOf(env["CRQ_REPO"])),
		AllowRepos:        repoSet(env["CRQ_REPOS"]),
		ExcludeRepos:      repoSet(env["CRQ_EXCLUDE"]),
		SkipAuthors:       authorSet(stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_AUTHORS", "dependabot[bot]")),
		SkipMarker:        stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_MARKER", "<!-- crq:skip-autoreview -->"),
		StateRef:          stringEnv(env, "CRQ_STATE_REF", "crq-state-v3"),
		Bot:               bot,
		RequiredBots:      requiredBots,
		CoBots:            coBots,
		FeedbackBots:      listEnv(env, "CRQ_FEEDBACK_BOTS", strings.Join(unionBots(requiredBots, coLogins), ",")),
		ReviewCommand:     stringEnv(env, "CRQ_REVIEW_CMD", "@coderabbitai review"),
		RateLimitCommand:  stringEnv(env, "CRQ_RATELIMIT_CMD", dialect.DefaultRateLimitCommand),
		RateLimitMarker:   stringEnv(env, "CRQ_RL_MARKER", dialect.DefaultRateLimitMarker),
		CalibrationMarker: stringEnv(env, "CRQ_CAL_REPLY_MARKER", "auto-generated reply by CodeRabbit"),
		ReviewDoneMarker:  stringEnv(env, "CRQ_REVIEW_DONE_MARKER", "summarize by coderabbit.ai"),
		CompletionMarker:  stringEnvAllowEmpty(env, "CRQ_COMPLETION_MARKER", "Review finished"),
		Host:              stringEnv(env, "CRQ_HOST", host),
		Timezone:          env["CRQ_TZ"],
		MinInterval:       durationEnv(env, "CRQ_MIN_INTERVAL", 90*time.Second),
		InflightTimeout:   durationEnv(env, "CRQ_INFLIGHT_TIMEOUT", 15*time.Minute),
		PollInterval:      durationEnv(env, "CRQ_POLL", 15*time.Second),
		WaitTimeout:       durationEnv(env, "CRQ_WAIT_TIMEOUT", 0),
		CalibrationTTL:    durationEnv(env, "CRQ_CALIBRATE_TTL", 2*time.Minute),
		RateLimitFallback: durationEnv(env, "CRQ_RL_FALLBACK", 15*time.Minute),
		AutoReviewPoll:    durationEnv(env, "CRQ_AUTOREVIEW_POLL", time.Minute),
		AutoReviewMaxScan: intEnv(env, "CRQ_AUTOREVIEW_MAX_SCAN", 400),
		// The vendor's weekly fair-use threshold, past which reviews are
		// throttled to roughly one an hour. Configurable because it is
		// plan-dependent (60 on Pro, 90 on Pro+) and there is no API to ask.
		WeeklyReviewLimit:   intEnv(env, "CRQ_WEEKLY_LIMIT", 60),
		LeaderTTL:           durationEnv(env, "CRQ_LEADER_TTL", 3*time.Minute),
		FiredMax:            intEnv(env, "CRQ_FIRED_MAX", 500),
		WatchInterval:       durationEnv(env, "CRQ_WATCH_INTERVAL", 2*time.Minute),
		DispatchCommand:     SplitArgv(env["CRQ_DISPATCH_CMD"]),
		FixAgent:            strings.TrimSpace(env["CRQ_FIX_AGENT"]),
		FixModels:           configuredModels(env["CRQ_FIX_MODELS"], env["CRQ_FIX_MODEL"]),
		FixModel:            strings.TrimSpace(env["CRQ_FIX_MODEL"]),
		FixEffort:           strings.TrimSpace(env["CRQ_FIX_EFFORT"]),
		FixPrompt:           strings.TrimSpace(env["CRQ_FIX_PROMPT"]),
		FixSeverities:       fixSeverities,
		FixAskMode:          askModeEnv(env["CRQ_FIX_ASK"]),
		DispatchMaxAttempts: positiveIntEnv(env, "CRQ_DISPATCH_MAX_ATTEMPTS", 5),
		DispatchForks:       boolEnv(env, "CRQ_DISPATCH_FORKS", false),
		DispatchConcurrency: intEnv(env, "CRQ_DISPATCH_CONCURRENCY", 0),
		WorkspaceRoot:       env["CRQ_WORKSPACE"],
		WorkOwner:           strings.TrimSpace(env["CRQ_WORK_OWNER"]),
		Tidy:                stringEnv(env, "CRQ_TIDY", "0") == "1",
		NoOpen:              env["CRQ_NO_OPEN"] != "",
		DryRun:              env["CRQ_DRY_RUN"] == "1",
		FeedbackWaitTimeout: durationEnv(env, "CRQ_FEEDBACK_WAIT_TIMEOUT", 20*time.Minute),
		SettleWindow:        durationEnv(env, "CRQ_SETTLE", 90*time.Second),

		RateLimitCoDegrade:   stringEnv(env, "CRQ_RL_CO_DEGRADE", stringEnv(env, "CRQ_RL_CODEX_DEGRADE", "1")) != "0",
		PreflightSkipBlocked: boolEnv(env, "CRQ_PREFLIGHT_SKIP_BLOCKED", true),
	}
	// Kept so a fleet record can be layered over it and the whole thing
	// re-parsed. Unexported: it is an input, not part of the configuration.
	cfg.env = maps.Clone(env)
	if len(cfg.Scope) == 0 && cfg.GateRepo != "" {
		cfg.Scope = []string{ownerOf(cfg.GateRepo)}
	}
	// Built here, after the command is resolved, because the primary's trigger is
	// part of describing it.
	cfg.KnownCoBots = parseAllCoBots(env)
	cfg.Reviewers = buildReviewers(cfg.Bot, cfg.ReviewCommand, requiredBots, coBots, false)
	// The legacy lists are now VIEWS of cfg.Reviewers rather than parallel
	// parses, so they cannot answer differently from it. An explicit
	// CRQ_FEEDBACK_BOTS still wins: it is the one list an operator may widen
	// beyond who reviews (to surface a bot's findings without waiting for it).
	cfg.RequiredBots = cfg.reviewerLogins(func(r Reviewer) bool { return r.Required })
	// Present-but-empty is not a choice: an operator who exports the variable
	// blank has named nobody, and treating that as explicit would freeze the
	// derived list and, through ForRepo, ignore every per-repo override.
	explicitFeedback := strings.TrimSpace(env["CRQ_FEEDBACK_BOTS"]) != ""
	cfg.FeedbackBotsExplicit = explicitFeedback
	if !explicitFeedback {
		// Requiredness decides convergence, not visibility. An optional reviewer
		// still ran and its unresolved findings are still work; hiding them made
		// the dashboard claim two open threads on a PR where GitHub had seventy.
		// PrimaryOff and co-reviewer selection remove reviewers from this list
		// structurally, while an explicit CRQ_FEEDBACK_BOTS remains the escape
		// hatch for fleets that intentionally want a narrower feed.
		cfg.FeedbackBots = cfg.reviewerLogins(func(Reviewer) bool { return true })
	}
	if len(cfg.FixModels) > 0 {
		cfg.FixModel = cfg.FixModels[0]
	}
	return cfg, nil
}

func severitySet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, severity := range strings.Split(raw, ",") {
		severity = strings.ToLower(strings.TrimSpace(severity))
		if severity != "" {
			out[severity] = true
		}
	}
	return out
}

func askModeEnv(raw string) string {
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case "uncertain", "ambiguous":
		return mode
	default:
		return "blocked"
	}
}

func configuredModels(ranked, legacy string) []string {
	raw := ranked
	if strings.TrimSpace(raw) == "" {
		raw = legacy
	}
	seen := map[string]bool{}
	out := []string{}
	for _, model := range strings.Split(raw, ",") {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

func (c Config) RequireState() error {
	if c.GateRepo == "" {
		return errors.New("CRQ_REPO is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func (c Config) RequireDashboard() error {
	if err := c.RequireState(); err != nil {
		return err
	}
	if c.DashboardIssue <= 0 {
		return errors.New("CRQ_ISSUE is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if v[0] == '\'' && v[len(v)-1] == '\'' {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	return out, scanner.Err()
}

func stringEnv(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return fallback
}

func stringEnvAllowEmpty(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return fallback
}

func intEnv(env map[string]string, key string, fallback int) int {
	v := env[key]
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func positiveIntEnv(env map[string]string, key string, fallback int) int {
	n := intEnv(env, key, fallback)
	if n <= 0 {
		return fallback
	}
	return n
}

func durationEnv(env map[string]string, key string, fallback time.Duration) time.Duration {
	v := env[key]
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func listEnv(env map[string]string, key, fallback string) []string {
	value := env[key]
	if value == "" {
		value = fallback
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// CoBotConfig is one enabled co-reviewer: "wanted" (findings surface, dynamic
// gate engages when the bot is observed active) unless Required, which gates
// convergence via RequiredBots membership. Trigger and SelfHealGrace shape
// when crq may post Command (see engine.DecideCoPost).
type CoBotConfig struct {
	Login   string
	Name    string
	Command string
	Trigger engine.TriggerMode
	// TriggerExplicit records that CRQ_COBOT_<NAME>_TRIGGER named this mode,
	// rather than it falling out of the registry defaults. The fleet parse lets
	// that value win over the registry's required trigger, so a per-repo override
	// promoting the bot to required must not quietly overrule it either — an
	// operator who disabled a bot's command asked for that on every repository.
	TriggerExplicit bool
	Required        bool
	SelfHealGrace   time.Duration
}

// parseCoBots resolves the enabled co-reviewers from CRQ_COBOTS (default all
// known; explicitly empty disables all) plus the per-bot CRQ_COBOT_<NAME>_*
// keys. A co-reviewer login listed in CRQ_REQUIRED_BOTS is required+enabled
// even when CRQ_COBOTS omits it. Codex keeps its historical defaults: trigger
// `always` iff required (else never), command from CRQ_CODEX_CMD as the
// legacy alias of CRQ_COBOT_CODEX_CMD. Bugbot/Macroscope default to
// `selfheal` — they auto-review pushes, so crq only nudges one that went
// silent on a head it should have covered.
// resolveCoBot applies the operator's per-bot environment to a registry entry.
// Uniform across bots: the per-bot key wins, then the registry's legacy alias if
// it declares one, then its default command. No bot is named here — the policy
// travels as registry metadata.
func resolveCoBot(env map[string]string, co dialect.CoReviewer, required bool) CoBotConfig {
	key := strings.ToUpper(co.Name)
	base := defaultCoBot(co, required)
	command := base.Command
	if v, ok := env["CRQ_COBOT_"+key+"_CMD"]; ok {
		command = v
	} else if co.LegacyCommandEnv != "" {
		command = stringEnvAllowEmpty(env, co.LegacyCommandEnv, command)
	}
	command = strings.TrimSpace(command)
	trigger, explicit := base.Trigger, false
	switch v := engine.TriggerMode(strings.ToLower(strings.TrimSpace(env["CRQ_COBOT_"+key+"_TRIGGER"]))); v {
	case engine.TriggerNever, engine.TriggerSelfHeal, engine.TriggerAlways:
		trigger, explicit = v, true
	}
	if command == "" {
		// No trigger command means crq can never post one, whatever the mode.
		trigger = engine.TriggerNever
	}
	base.Command, base.Trigger, base.TriggerExplicit = command, trigger, explicit
	base.SelfHealGrace = durationEnv(env, "CRQ_COBOT_"+key+"_GRACE", defaultSelfHealGrace)
	return base
}

// parseAllCoBots resolves every registry co-reviewer with the environment
// applied, regardless of whether CRQ_COBOTS enables it.
func parseAllCoBots(env map[string]string) []CoBotConfig {
	out := make([]CoBotConfig, 0, 4)
	for _, co := range dialect.KnownCoReviewers() {
		out = append(out, resolveCoBot(env, co, false))
	}
	return out
}

func parseCoBots(env map[string]string, requiredBots []string) ([]CoBotConfig, error) {
	enabled := map[string]bool{}
	var unknown []string
	// Preserve the pre-registry contract: absent means every built-in
	// co-reviewer, while explicitly empty disables all. Treating both as empty
	// silently disabled working reviewers on upgrade.
	items := []string{}
	if raw, exists := env["CRQ_COBOTS"]; exists {
		items = splitList(raw)
	} else {
		for _, co := range dialect.KnownCoReviewers() {
			items = append(items, co.Name)
		}
	}
	for _, item := range items {
		co, ok := dialect.CoReviewerByName(item)
		if !ok {
			// Refuse rather than skip: silently dropping a typo disables the
			// co-reviewer the operator asked for, and the symptom (a bot that
			// never runs) looks nothing like its cause.
			unknown = append(unknown, item)
			continue
		}
		enabled[co.Name] = true
	}
	if len(unknown) > 0 {
		known := make([]string, 0, 3)
		for _, co := range dialect.KnownCoReviewers() {
			known = append(known, co.Name)
		}
		return nil, fmt.Errorf("CRQ_COBOTS: unknown co-reviewer %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	requiredSet := map[string]bool{}
	for _, bot := range requiredBots {
		requiredSet[dialect.NormalizeBotName(strings.TrimSpace(bot))] = true
	}
	var out []CoBotConfig
	for _, co := range dialect.KnownCoReviewers() {
		key := strings.ToUpper(co.Name)
		required := boolEnv(env, "CRQ_COBOT_"+key+"_REQUIRED", false) || requiredSet[dialect.NormalizeBotName(co.Login)]
		if !enabled[co.Name] && !required {
			continue
		}
		out = append(out, resolveCoBot(env, co, required))
	}
	return out, nil
}

const defaultSelfHealGrace = 10 * time.Minute

// defaultCoBot is a co-reviewer's configuration from the registry alone, with no
// environment applied. parseCoBots layers env on top of it, and a per-repo
// override uses it directly for a bot the fleet default does not enable — which
// is the whole point of choosing reviewers per project.
func defaultCoBot(co dialect.CoReviewer, required bool) CoBotConfig {
	trigger := triggerMode(co.DefaultTrigger, engine.TriggerSelfHeal)
	if required && co.RequiredTrigger != "" {
		trigger = triggerMode(co.RequiredTrigger, trigger)
	}
	command := strings.TrimSpace(co.Command)
	if command == "" {
		trigger = engine.TriggerNever
	}
	return CoBotConfig{
		Login:         co.Login,
		Name:          co.Name,
		Command:       command,
		Trigger:       trigger,
		Required:      required,
		SelfHealGrace: defaultSelfHealGrace,
	}
}

// triggerMode converts a registry trigger string to the engine mode, falling
// back when a bot declares none.
func triggerMode(name string, fallback engine.TriggerMode) engine.TriggerMode {
	switch m := engine.TriggerMode(strings.ToLower(strings.TrimSpace(name))); m {
	case engine.TriggerNever, engine.TriggerSelfHeal, engine.TriggerAlways:
		return m
	}
	return fallback
}

// splitList splits a comma-separated list, dropping blanks (an all-blank or
// empty value yields nil — unlike listEnv it never falls back).
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func boolEnv(env map[string]string, key string, fallback bool) bool {
	v := strings.TrimSpace(env[key])
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// unionBots concatenates bot lists, dropping blanks and case-insensitively
// de-duplicating on the normalized login (so "coderabbitai" and
// "coderabbitai[bot]" collapse to one), preserving first-seen order.
func unionBots(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key := dialect.NormalizeBotName(item)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}

func repoSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = NormalizeRepo(item)
		if item != "" {
			set[item] = true
		}
	}
	return set
}

// authorSet normalizes a comma-separated login list the same way scan results
// are matched: lowercase with the "[bot]" suffix stripped, so "dependabot",
// "Dependabot" and "dependabot[bot]" all name the same author.
func authorSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = dialect.NormalizeBotName(strings.ToLower(strings.TrimSpace(item)))
		if item != "" {
			set[item] = true
		}
	}
	return set
}

func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

// SplitArgv splits a configured command into argv on whitespace, keeping
// quoted runs together: 'claude -p "fix these findings"' is three arguments,
// not five with stray quote characters in them.
//
// Quoting is ALL it understands. It is deliberately not a shell: a dispatch
// command runs directly, so nothing here expands a variable, a glob, or a pipe
// that the operator did not write. Both quote styles behave the same way, and
// an unclosed quote simply runs to the end rather than failing a config load
// over it.
func SplitArgv(value string) []string {
	var argv []string
	var arg strings.Builder
	quote := rune(0)
	quoted := false // "" is an argument, even though it contributes no characters
	for _, r := range value {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, quoted = r, true
		case quote == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if arg.Len() > 0 || quoted {
				argv = append(argv, arg.String())
				arg.Reset()
				quoted = false
			}
		default:
			arg.WriteRune(r)
		}
	}
	if arg.Len() > 0 || quoted {
		argv = append(argv, arg.String())
	}
	return argv
}

// JoinArgv renders argv as the single string SplitArgv reads back UNCHANGED.
//
// It exists because argv makes a round trip through a service unit's
// environment: an install parses the operator's `--agent-args` into arguments,
// writes them into the unit as one value, and the session splits them again.
// Joining on spaces loses the boundaries — `--config "value with space"` came
// back as three arguments — so every installed session either failed on its
// first flag or ran with the wrong option.
func JoinArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

// quoteArg wraps one argument so SplitArgv yields it whole.
//
// SplitArgv has no escape character — a quote merely toggles — so an argument
// containing both quote styles is emitted as adjacent quoted runs with nothing
// between them, which SplitArgv concatenates back into one argument.
func quoteArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\r\"'") {
		return arg
	}
	parts := strings.Split(arg, `"`)
	for i, part := range parts {
		parts[i] = `"` + part + `"`
	}
	return strings.Join(parts, `'"'`)
}

// processRun distinguishes this RUN of crq from an earlier one that happened to
// get the same pid. Host and pid do not identify a process over time — a
// containerized daemon restarts as pid 1, and ordinary pids are reused — so
// without it a replacement process inherits its predecessor's recorded
// capabilities: an old binary restarted into a capable one's pid would vouch for
// itself, and LaggingWriters would report no incompatible writer while that
// daemon ignores every repository override.
var processRun = randomToken()[:8]

// WriterID identifies this PROCESS in the shared state, and is what the
// autoreview leader records as its owner. A new CLI and an old daemon commonly
// run on one machine, so a per-host key would let the upgraded CLI's write vouch
// for the daemon that has not been upgraded.
func (c Config) WriterID() string {
	return fmt.Sprintf("host=%s pid=%d run=%s", c.Host, os.Getpid(), processRun)
}
