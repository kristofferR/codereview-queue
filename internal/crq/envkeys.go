package crq

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// EnvKey describes one setting: what it means, what shape its value has, and —
// the part that matters — whether it can honestly be recorded for the whole
// fleet or only ever belongs to one machine.
//
// Not every setting is fleet-settable, and pretending otherwise would be worse
// than not offering it. Three kinds are deliberately excluded:
//
//   - IDENTITY. The gate repository, the state ref, the dashboard issue. These
//     say WHERE the queue lives; a dashboard writing to that ref cannot change
//     which ref it is without cutting the branch it is sitting on.
//   - MACHINE. Paths, the host name, the fix agent's binary, the workspace
//     root. A value that is only true of one filesystem is not a fleet setting,
//     and recording one would break every other host.
//   - CREDENTIALS. Never in state: the ref is a git branch on a repository
//     other people can be given read access to.
type EnvKey struct {
	Key  string
	Kind string // duration | int | bool | text | list
	// Group orders the settings page: pacing, review, autofix, reporting.
	Group string
	Label string
	Help  string
	// PerHost marks a setting that stays this machine's. It is still shown —
	// "why can I not change this here" is a fair question — but not editable.
	PerHost bool
	// Identity marks bootstrap settings that must not be edited at all.
	Identity bool
	// AllowEmpty means an explicitly recorded empty string changes behaviour
	// rather than handing the setting back to each host's environment.
	AllowEmpty bool
	// ReviewImpact means changing this value can change who reviews or gates a
	// pull request. The dashboard previews those writes because they can reopen
	// completed rounds and spend the shared allowance.
	ReviewImpact bool
}

// envKeys is every setting the dashboard knows about. Anything absent is not
// offered rather than offered and quietly ignored.
var envKeys = []EnvKey{
	{Key: "CRQ_MIN_INTERVAL", Kind: "duration", Group: "pacing", Label: "Minimum interval",
		Help: "Smallest gap between two metered review fires, fleet-wide."},
	{Key: "CRQ_INFLIGHT_TIMEOUT", Kind: "duration", Group: "pacing", Label: "In-flight timeout",
		Help: "How long a fired round waits for any bot response before it is retried."},
	{Key: "CRQ_RL_FALLBACK", Kind: "duration", Group: "pacing", Label: "Rate-limit fallback",
		Help: "Block window assumed when the bot's \"available in\" cannot be parsed."},
	{Key: "CRQ_CALIBRATE_TTL", Kind: "duration", Group: "pacing", Label: "Calibration freshness",
		Help: "How long an account-quota calibration reply remains current."},
	{Key: "CRQ_WEEKLY_LIMIT", Kind: "int", Group: "pacing", Label: "Weekly fair-use limit",
		Help: "Reviews per rolling week before the vendor throttles: 60 on Pro, 90 on Pro+. 0 counts without forecasting."},
	{Key: "CRQ_AUTOREVIEW_POLL", Kind: "duration", Group: "pacing", Label: "Auto-review poll",
		Help: "How often the daemon scans for pull requests needing a review."},
	{Key: "CRQ_AUTOREVIEW_MAX_SCAN", Kind: "int", Group: "pacing", Label: "Scan budget",
		Help: "Most pull requests examined per scope per pass, so one large scope cannot starve the others."},
	{Key: "CRQ_LEADER_TTL", Kind: "duration", Group: "pacing", Label: "Leader lease",
		Help: "How long a daemon's claim to drive the queue survives without a heartbeat."},

	{Key: "CRQ_BOT", Kind: "text", Group: "review", Label: "Primary reviewer",
		Help: "The metered reviewer's login. Changing it changes which bot's wording crq reads.", ReviewImpact: true},
	{Key: "CRQ_REVIEW_CMD", Kind: "text", Group: "review", Label: "Review command",
		Help: "The comment crq posts to ask the primary for a review.", ReviewImpact: true},
	{Key: "CRQ_COBOTS", Kind: "list", Group: "review", Label: "Co-reviewers",
		Help: "Which co-reviewers run. Editable as toggles above; this is the same setting.", AllowEmpty: true, ReviewImpact: true},
	{Key: "CRQ_REQUIRED_BOTS", Kind: "list", Group: "review", Label: "Required reviewers",
		Help: "Which reviewers convergence waits for.", ReviewImpact: true},
	{Key: "CRQ_FEEDBACK_BOTS", Kind: "list", Group: "review", Label: "Feedback reviewers",
		Help: "Whose findings are surfaced. Defaults to everyone who reviews; widen it to read a bot without waiting for it."},
	{Key: "CRQ_SETTLE", Kind: "duration", Group: "review", Label: "Settle window",
		Help: "How long a converged loop keeps watching, so a trailing review wave is caught by crq rather than by you."},
	{Key: "CRQ_FEEDBACK_WAIT_TIMEOUT", Kind: "duration", Group: "review", Label: "Feedback wait",
		Help: "How long crq loop waits for a round before reporting a timeout."},
	{Key: "CRQ_RL_CO_DEGRADE", Kind: "bool", Group: "review", Label: "Degrade on block",
		Help: "During a quota block, ask the co-reviewers now and keep the primary queued, instead of waiting the block out."},
	{Key: "CRQ_PREFLIGHT_SKIP_BLOCKED", Kind: "bool", Group: "review", Label: "Skip blocked preflight",
		Help: "Satisfy local preflight from a live shared account block instead of calling CodeRabbit."},
	{Key: "CRQ_AUTOREVIEW_SKIP_AUTHORS", Kind: "list", Group: "review", Label: "Skip authors",
		Help: "Pull-request authors auto-review never enqueues.", AllowEmpty: true},
	{Key: "CRQ_AUTOREVIEW_SKIP_MARKER", Kind: "text", Group: "review", Label: "Skip marker",
		Help: "A pull-request body containing this is left alone by fleet auto-review.", AllowEmpty: true},

	// Per-bot trigger policy. Generated rather than listed, so adding a
	// co-reviewer to the registry adds its settings here too — the alternative
	// is a catalogue that silently stops covering the fleet it describes.
	{Key: "CRQ_WATCH_INTERVAL", Kind: "duration", Group: "autofix", Label: "Watch interval",
		Help: "How often the watcher looks for pull requests needing a fix session."},
	{Key: "CRQ_DISPATCH_MAX_ATTEMPTS", Kind: "int", Group: "autofix", Label: "Max attempts",
		Help: "Failed code-fix sessions per head before crq stops. Provider outages do not count. Also settable per repository."},
	{Key: "CRQ_DISPATCH_FORKS", Kind: "bool", Group: "autofix", Label: "Fix fork PRs",
		Help: "Allow sessions on pull requests from another repository. Off by default."},
	{Key: "CRQ_DISPATCH_CONCURRENCY", Kind: "int", Group: "autofix", Label: "Concurrency",
		Help:    "Cap on simultaneous fix sessions. 0 means no cap — fixing spends no quota.",
		PerHost: true},

	{Key: "CRQ_TZ", Kind: "text", Group: "reporting", Label: "Timezone",
		Help: "How times are rendered in the GitHub issue dashboard."},
	{Key: "CRQ_TIDY", Kind: "bool", Group: "reporting", Label: "Tidy trigger comments",
		Help: "Delete crq's own spent trigger comments as rounds progress. Opt-in while older binaries share the ref."},

	// Shown, never editable here.
	{Key: "CRQ_REPO", Kind: "text", Group: "identity", Label: "Gate repository",
		Help: "Holds the state ref and the dashboard issue. Changing it is a re-init, not a setting.", Identity: true},
	{Key: "CRQ_STATE_REF", Kind: "text", Group: "identity", Label: "State ref",
		Help: "The git ref this queue lives in. A dashboard writing to it cannot move it.", Identity: true},
	{Key: "CRQ_ISSUE", Kind: "int", Group: "identity", Label: "Dashboard issue",
		Help: "The GitHub issue the legacy dashboard is rendered into.", Identity: true},
	{Key: "CRQ_SCOPE", Kind: "list", Group: "identity", Label: "Scope",
		Help: "Owners searched for pull requests. Enrollment is per repository — see Repos.", Identity: true},
	{Key: "CRQ_REPOS", Kind: "list", Group: "identity", Label: "Allow-list (legacy)",
		Help: "This host's repository list. Enrollment records supersede it — see Repos.", PerHost: true},
	{Key: "CRQ_EXCLUDE", Kind: "list", Group: "identity", Label: "Exclude (kill switch)",
		Help: "This host refuses to touch these, whatever shared state says.", PerHost: true},
	{Key: "CRQ_HOST", Kind: "text", Group: "identity", Label: "Host name",
		Help: "How this machine identifies itself in the state ref.", PerHost: true},
	{Key: "CRQ_WORKSPACE", Kind: "text", Group: "identity", Label: "Workspace root",
		Help: "Where this machine keeps its mirrors and worktrees.", PerHost: true},
	{Key: "CRQ_DISPATCH_CMD", Kind: "text", Group: "identity", Label: "Fix command",
		Help: "The session script this machine runs. Written by crq autofix install.", PerHost: true},
}

// EnvKeys returns the settings catalogue, in display order, including the
// per-co-reviewer trigger settings derived from the registry.
func EnvKeys() []EnvKey {
	out := append([]EnvKey(nil), envKeys...)
	for _, co := range dialect.KnownCoReviewers() {
		up := strings.ToUpper(co.Name)
		out = append(out,
			EnvKey{Key: "CRQ_COBOT_" + up + "_TRIGGER", Kind: "text", Group: "review",
				Label: co.Name + " trigger",
				Help: "never (crq stays out of its way) · selfheal (ask only if it has not shown up) · " +
					"always (ask on every round).", ReviewImpact: true},
			EnvKey{Key: "CRQ_COBOT_" + up + "_GRACE", Kind: "duration", Group: "review",
				Label: co.Name + " self-heal grace",
				Help:  "How long to wait for " + co.Name + " to review on its own before asking."},
			EnvKey{Key: "CRQ_COBOT_" + up + "_CMD", Kind: "text", Group: "review",
				Label:      co.Name + " command",
				Help:       "The comment crq posts to ask " + co.Name + " for a review.",
				AllowEmpty: true, ReviewImpact: true},
		)
	}
	return out
}

// envKeyByName indexes the catalogue.
func envKeyByName(key string) (EnvKey, bool) {
	for _, k := range EnvKeys() {
		if k.Key == key {
			return k, true
		}
	}
	return EnvKey{}, false
}

// fleetSettable reports whether a key may be recorded for the whole fleet.
func fleetSettable(key string) bool {
	k, ok := envKeyByName(key)
	return ok && !k.PerHost && !k.Identity
}

// fleetReadable includes the three policies v4 stored in shared state but v5
// no longer lets an operator write there. Migration must keep honoring the
// recorded answer until it is deliberately removed; SetEnv still uses the
// narrower fleetSettable predicate, so no new shared identity/per-host value
// can be created.
func fleetReadable(key string) bool {
	if fleetSettable(key) {
		return true
	}
	switch key {
	case "CRQ_SCOPE", "CRQ_REPOS", "CRQ_EXCLUDE":
		return true
	default:
		return false
	}
}

// positiveOnly names the settings whose CONSUMER applies a recorded value only
// when it is greater than zero.
//
// Shape is not enough for these. "0s" parses, and -1 is an integer, so the
// generic check above accepted both — and then every daemon fell back to its
// own startup value while this page went on reporting the saved number as the
// fleet's. A save that cannot come into force has to fail here instead: the
// whole point of the settings page is that what it shows is what is running.
var positiveOnly = map[string]bool{
	"CRQ_INFLIGHT_TIMEOUT":      true,
	"CRQ_RL_FALLBACK":           true,
	"CRQ_CALIBRATE_TTL":         true,
	"CRQ_AUTOREVIEW_POLL":       true,
	"CRQ_AUTOREVIEW_MAX_SCAN":   true,
	"CRQ_LEADER_TTL":            true,
	"CRQ_FEEDBACK_WAIT_TIMEOUT": true,
	"CRQ_WATCH_INTERVAL":        true,
	"CRQ_DISPATCH_MAX_ATTEMPTS": true,
}

// validateEnvValue checks a value against its key's shape and range, so a
// setting that would fail to parse — or that would parse and then be ignored by
// the consumer it is meant for — is refused at edit time rather than silently
// falling back to a default on every host at once.
func validateEnvValue(key, value string) error {
	k, ok := envKeyByName(key)
	if !ok {
		return fmt.Errorf("%s is not a setting crq knows", key)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		// Empty either clears the setting or is the setting's explicit value;
		// both shapes are valid, and SetEnv keeps that distinction.
		return nil
	}
	if key == "CRQ_TZ" {
		if value == "Local" {
			return fmt.Errorf("%s must be an IANA timezone name, not Local", key)
		}
		if _, err := time.LoadLocation(value); err != nil {
			return fmt.Errorf("%s is not a valid IANA timezone: %w", key, err)
		}
	}
	switch k.Kind {
	case "duration":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if d < 0 {
			return fmt.Errorf("%s cannot be negative", key)
		}
		if d == 0 && positiveOnly[key] {
			return fmt.Errorf("%s has to be longer than 0s — a zero is ignored, and every host stays on its own value", key)
		}
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if n < 0 {
			return fmt.Errorf("%s cannot be negative", key)
		}
		if n == 0 && positiveOnly[key] {
			return fmt.Errorf("%s has to be greater than 0 — a zero is ignored, and every host stays on its own value", key)
		}
	case "bool":
		if value != "0" && value != "1" {
			return fmt.Errorf("%s takes 0 or 1", key)
		}
	}
	if strings.HasSuffix(key, "_TRIGGER") {
		switch value {
		case "never", "selfheal", "always":
		default:
			return fmt.Errorf("%s takes never, selfheal or always", key)
		}
	}
	return nil
}

// EnvSetting is one setting as the dashboard shows it.
type EnvSetting struct {
	EnvKey
	// Value is what this host will actually use, after the fleet record.
	Value string `json:"value"`
	// Source is "fleet" when a record decided it, "env" when this host's file
	// did, "default" when neither names it.
	Source string `json:"source"`
	// HostValue is what this host's own environment says, shown when a fleet
	// record is overriding it — otherwise the override is invisible.
	HostValue string `json:"host_value,omitempty"`
}

// EnvSettings reports every setting with its effective value and source.
func (s *Service) EnvSettings(st State) []EnvSetting {
	host := s.cfg.Env()
	effective := s.cfg.WithFleet(st.Fleet)
	keys := EnvKeys()
	out := make([]EnvSetting, 0, len(keys))
	for _, k := range keys {
		hostValue, present := host[k.Key]
		set := EnvSetting{EnvKey: k, Value: configValueOf(effective, k.Key), Source: "default"}
		if present {
			set.Source = "env"
		}
		// Four settings have a typed home on the record — with their own
		// validation and impact preview — so the generic map is not the only
		// place to look. Missing that made an adopted setting keep reporting
		// "env", which is the exact mislabel this page exists to remove.
		if fleet, ok := fleetValueOf(st.Fleet, k.Key); ok {
			_ = fleet // Value comes from the parsed effective configuration.
			set.HostValue, set.Source = strings.TrimSpace(hostValue), "fleet"
		} else if _, ok := st.Fleet.Env[k.Key]; ok && fleetReadable(k.Key) {
			set.HostValue, set.Source = strings.TrimSpace(hostValue), "fleet"
		}
		out = append(out, set)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return groupOrder(out[i].Group) < groupOrder(out[j].Group)
	})
	return out
}

func groupOrder(group string) int {
	switch group {
	case "pacing":
		return 0
	case "review":
		return 1
	case "autofix":
		return 2
	case "reporting":
		return 3
	default:
		return 4
	}
}

// fleetValueOf renders a recorded setting that lives in a typed field rather
// than in the generic map.
func fleetValueOf(fd FleetDefaults, key string) (string, bool) {
	switch key {
	case "CRQ_COBOTS":
		if fd.SetCoBots {
			return strings.Join(fd.CoBots, ","), true
		}
	case "CRQ_REQUIRED_BOTS":
		if fd.SetRequired {
			return strings.Join(fd.Required, ","), true
		}
	case "CRQ_MIN_INTERVAL":
		if strings.TrimSpace(fd.MinInterval) != "" {
			return fd.MinInterval, true
		}
	case "CRQ_WEEKLY_LIMIT":
		if fd.WeeklyLimit != nil {
			return strconv.Itoa(*fd.WeeklyLimit), true
		}
	}
	return "", false
}

// typedEnvKey reports whether a write routes to a typed field, so a
// setting never ends up recorded in two places with one of them shadowed.
func typedEnvKey(key string) bool {
	switch key {
	case "CRQ_COBOTS", "CRQ_REQUIRED_BOTS", "CRQ_MIN_INTERVAL", "CRQ_WEEKLY_LIMIT":
		return true
	}
	return false
}
