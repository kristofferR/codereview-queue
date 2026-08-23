package crq

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// fixPrompt is what a dispatched session is told. It is embedded so there is one
// copy: the file in examples/ is the same bytes crq installs, and a rule learned
// the hard way cannot drift between them.
//
//go:embed dispatch/fix-prompt.txt
var fixPrompt string

// AutofixInstall describes what an install would do, so --dry-run can print it and
// the result can be reported.
type AutofixInstall struct {
	Platform string `json:"platform"`
	Prompt   string `json:"prompt"`
	// Binary is the crq the unit runs. There is no wrapper script and no
	// session script: the unit runs `crq watch -- crq fix-session`, so one
	// binary starts the watcher and one binary runs each session. Two generated
	// bash files used to sit in between, which meant three things had to agree
	// about the configuration and two of them were text on disk no test ever
	// ran — a setting reached a fleet only after every host reinstalled, and
	// nothing said which had not.
	Binary    string `json:"binary"`
	Unit      string `json:"unit"`
	LogDir    string `json:"log_dir"`
	Workspace string `json:"workspace,omitempty"`
	Agent     string `json:"agent"`
	// Invocation is the exact command the unit runs, so --dry-run shows it.
	Invocation string `json:"invocation,omitempty"`
	// AgentArgs are the operator's extra arguments, passed to the session
	// through the unit's environment.
	AgentArgs []string `json:"agent_args,omitempty"`
	Repos     []string `json:"repos"`
	Commands  []string `json:"commands"`
	// Retire is the pre-rename watcher this install shuts down, when one is
	// still on disk. Empty when there is nothing to retire.
	Retire string `json:"retire,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// SkipAuthCheck skips only the local Git credential check. The configured
	// crq server is always probed because its reachability and write capability
	// are unambiguous from any session.
	SkipAuthCheck bool `json:"skip_auth_check,omitempty"`
	Started       bool `json:"started,omitempty"`
}

// InstallAutofix sets up unattended autofix: the prompt, a wrapper, a
// service definition, and whatever the platform needs to keep it running across
// a logout.
//
// It exists because the alternative was a README asking somebody to copy three
// files, remember `loginctl enable-linger`, and get the environment right — and
// a setup people get wrong is a setup that silently does nothing, which is the
// failure this whole feature is about.
func (s *Service) InstallAutofix(ctx context.Context, agent string, agentArgs []string, repos []string, dryRun, skipAuth bool) (AutofixInstall, error) {
	effectiveDryRun := dryRun || s.cfg.DryRun
	plan, err := AutofixPlan(s.cfg, agent, agentArgs, repos, effectiveDryRun, skipAuth)
	if err != nil || effectiveDryRun {
		return plan, err
	}
	return s.applyAutofix(ctx, plan)
}

// AutofixPlan computes what an install WOULD write and run, from configuration
// alone.
//
// Separate from applying it because `crq autofix install --dry-run` is documented
// as a preview and must work for somebody who has not authenticated yet: the
// plan reads no GitHub state, so requiring a token to see it turned the one
// command for inspecting the setup into another thing to set up first.
func AutofixPlan(cfg Config, agent string, agentArgs []string, repos []string, dryRun, skipAuth bool) (AutofixInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AutofixInstall{}, err
	}
	if agent = strings.TrimSpace(agent); agent == "" {
		agent, err = exec.LookPath("claude")
		if err != nil {
			return AutofixInstall{}, fmt.Errorf("no fix agent found: pass --agent <path> (tried \"claude\" on PATH)")
		}
	} else {
		// A typo here is not a smaller mistake than a missing default: the install
		// would report success and every dispatch would fail to start a session,
		// which is the silent nothing this command exists to prevent.
		resolved, lerr := exec.LookPath(agent)
		if lerr != nil {
			return AutofixInstall{}, fmt.Errorf("fix agent %q cannot be run: %w", agent, lerr)
		}
		agent = resolved
	}
	agent, err = filepath.Abs(agent)
	if err != nil {
		return AutofixInstall{}, fmt.Errorf("resolving fix agent %q: %w", agent, err)
	}
	if len(repos) == 0 {
		// Sorted, because the plan is what --dry-run shows and what the unit
		// records: map order would make two identical installs disagree.
		repos = sortedRepoList(cfg.AllowRepos)
	}
	// The service writes its output here. systemd refuses to start a unit whose
	// StandardOutput path cannot be opened (209/STDOUT), so the directory has to
	// exist before the unit does — a service that will not start is exactly the
	// silent nothing this command exists to prevent.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := AutofixInstall{
		Platform:      runtime.GOOS,
		LogDir:        logDir,
		Prompt:        filepath.Join(home, ".local", "share", "crq", "fix-prompt.txt"),
		Agent:         agent,
		AgentArgs:     agentArgs,
		Repos:         repos,
		DryRun:        dryRun,
		SkipAuthCheck: skipAuth,
	}
	if root := strings.TrimSpace(cfg.WorkspaceRoot); root != "" {
		plan.Workspace, err = filepath.Abs(root)
		if err != nil {
			return plan, fmt.Errorf("resolving autofix workspace %q: %w", root, err)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-autofix.plist")
		plan.Retire = legacyWatcherUnit(filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-drain.plist"))
		plan.Commands = []string{
			"launchctl bootout gui/$(id -u)/no.kristofferr.crq-autofix",
			"launchctl bootstrap gui/$(id -u) " + plan.Unit,
		}
		if plan.Retire != "" {
			// Both halves: bootout stops it now, disable keeps launchd from
			// bootstrapping the plist again at the next login.
			plan.Commands = append([]string{
				"launchctl disable gui/$(id -u)/no.kristofferr.crq-drain",
				"launchctl bootout gui/$(id -u)/no.kristofferr.crq-drain",
			}, plan.Commands...)
		}
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-autofix.service")
		plan.Retire = legacyWatcherUnit(filepath.Join(home, ".config", "systemd", "user", "crq-drain.service"))
		plan.Commands = []string{
			"loginctl enable-linger " + os.Getenv("USER"),
			"systemctl --user daemon-reload",
			"systemctl --user enable crq-autofix",
			// restart, not "enable --now": --now does nothing to a unit that is
			// already running, so reinstalling with a different agent or prompt
			// silently kept the old one going. An install that appears to succeed
			// and changes nothing is the worst of both.
			"systemctl --user restart crq-autofix",
		}
		if plan.Retire != "" {
			plan.Commands = append([]string{"systemctl --user disable --now crq-drain"}, plan.Commands...)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return plan, fmt.Errorf("resolving the crq binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	plan.Binary = self
	// What the unit runs, shown by --dry-run: one binary starting the watcher,
	// and the same binary running each session.
	plan.Invocation = strings.Join(autofixArgv(plan), " ")
	return plan, nil
}

// autofixArgv is the unit's command line: the watcher, told to dispatch each
// fix session by running crq again.
func autofixArgv(plan AutofixInstall) []string {
	return []string{plan.Binary, "watch", "--", plan.Binary, "fix-session"}
}

// applyAutofix writes the plan to disk and starts the service.
func (s *Service) applyAutofix(ctx context.Context, plan AutofixInstall) (AutofixInstall, error) {
	logDir := plan.LogDir
	if !plan.SkipAuthCheck {
		if err := serviceCanAuthenticate(ctx, "autofix"); err != nil {
			return plan, err
		}
	}
	if err := s.serviceCanUseGateway(ctx, "autofix"); err != nil {
		return plan, err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return plan, err
	}

	for _, f := range []struct {
		path string
		body string
		mode os.FileMode
	}{
		{plan.Prompt, fixPrompt, 0o644},
		// The gateway bearer token is part of the effective daemon config, so
		// the service definition is private to the installing user.
		{plan.Unit, s.autofixUnit(plan), 0o600},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return plan, err
		}
		if err := writeAutofixFile(f.path, f.body, f.mode); err != nil {
			return plan, err
		}
	}

	// The files are the durable part, and a machine without systemd/launchd still
	// gets something runnable by hand — but a failure here means the autofix service is not
	// running, and reporting `started` for that is the same silent nothing this
	// command exists to prevent. Say which command failed, and let the caller see
	// the paths that were written.
	var failed []string
	commandArgs := autofixCommandArgs(plan)
	if len(plan.Commands) != len(commandArgs) {
		return plan, errors.New("autofix start commands are incomplete")
	}
	for i, line := range plan.Commands {
		args := commandArgs[i]
		if len(args) == 0 {
			return plan, fmt.Errorf("autofix start command %q has no executable", line)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil && launchdJobAbsent(line, output) {
			continue
		}
		if err != nil {
			detail := err.Error()
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += ": " + text
			}
			if s.log != nil {
				s.log.Printf("autofix install: %s: %s", line, detail)
			}
			failed = append(failed, fmt.Sprintf("%s: %s", line, detail))
		}
	}
	if len(failed) > 0 {
		return plan, fmt.Errorf("the autofix files are installed, but starting it failed — run these by hand: %s",
			strings.Join(failed, "; "))
	}
	plan.Started = true
	return plan, nil
}

func writeAutofixFile(path, body string, mode os.FileMode) error {
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func autofixCommandArgs(plan AutofixInstall) [][]string {
	if plan.Platform == "darwin" {
		domain := "gui/" + currentUID()
		args := [][]string{
			{"launchctl", "bootout", domain + "/no.kristofferr.crq-autofix"},
			{"launchctl", "bootstrap", domain, plan.Unit},
		}
		if plan.Retire != "" {
			args = append([][]string{
				{"launchctl", "disable", domain + "/no.kristofferr.crq-drain"},
				{"launchctl", "bootout", domain + "/no.kristofferr.crq-drain"},
			}, args...)
		}
		return args
	}
	args := [][]string{
		{"loginctl", "enable-linger", os.Getenv("USER")},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "crq-autofix"},
		{"systemctl", "--user", "restart", "crq-autofix"},
	}
	if plan.Retire != "" {
		args = append([][]string{{"systemctl", "--user", "disable", "--now", "crq-drain"}}, args...)
	}
	return args
}

// legacyWatcherUnit reports the pre-rename watcher's unit, if it is still there.
//
// The rename is a hard break in the state and the CLI, but a break in naming
// stops nothing that is already running: a host that ran `crq drain install`
// keeps an enabled crq-drain unit, and installing autofix beside it leaves two
// watchers scanning the same fleet and racing each other's dispatch claims.
// Detected from disk rather than attempted blindly, so a first install neither
// runs a pointless command nor has to read failure text to know it was benign.
func legacyWatcherUnit(path string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func currentUID() string { return fmt.Sprint(os.Getuid()) }

// launchctl bootout is the replacement half of a launchd reinstall. The job
// does not exist on the first install, which is already the desired state; only
// that explicit response is benign. Bootstrap failures and every other bootout
// failure still fail the install.
func launchdJobAbsent(command string, output []byte) bool {
	if !strings.HasPrefix(strings.TrimSpace(command), "launchctl bootout ") {
		return false
	}
	text := strings.ToLower(string(output))
	return strings.Contains(text, "no such process") || strings.Contains(text, "could not find service")
}

// serviceCanAuthenticate reports whether the named SERVICE will find a GitHub
// credential — which is not the same question as whether this shell has one.
//
// crq resolves a token from GITHUB_TOKEN/GH_TOKEN or `gh auth token`, and the
// unit inherits none of this shell's variables. A token exported in a profile
// therefore authenticates the install and nothing afterwards: every pass fails
// to read a pull request while the install reports Started, which is the silent
// nothing this command exists to prevent. Writing the token into the unit is not
// the answer — that file is readable by every local user — so the credential has
// to be one the service can resolve itself.
// A macOS host is the case this cannot decide for itself: gh keeps its token in
// the login keychain, which a launchd agent in the GUI domain can read and an
// SSH session cannot. Over SSH, `gh auth status` there reports the account by
// name and the token as invalid — which is also exactly what a genuinely
// expired token looks like. Since the two are indistinguishable from outside
// that session, the escape hatch is an explicit flag rather than a guess: an
// operator typing --skip-auth-check has made a claim, which is not the silent
// nothing this check exists to prevent.
func serviceCanAuthenticate(ctx context.Context, service string) error {
	if path := ConfigPath(); path != "" {
		values, err := readEnvFile(path)
		if err == nil {
			for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
				if strings.TrimSpace(values[key]) != "" {
					return nil
				}
			}
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	// Cleared, because gh reads them too: this shell's token would answer for the
	// service and hide the very gap being checked.
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN=", "GH_TOKEN=")
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	return fmt.Errorf("the %s service would have no GitHub credential: a service does not inherit this shell's GITHUB_TOKEN/GH_TOKEN. Run 'gh auth login', or put the token in %s, then install again", service, ConfigPath())
}

func (s *Service) serviceCanUseGateway(ctx context.Context, service string) error {
	if err := ghapi.ProbeServer(ctx, s.cfg.ServerURL, s.cfg.ServerToken, true); err != nil {
		return fmt.Errorf("the %s service cannot use the configured crq server: %w", service, err)
	}
	return nil
}

// autofixPath is the PATH the service runs with: this shell's, plus wherever crq
// and the agent were found.
//
// A service manager hands a unit its own minimal PATH, which on launchd is four
// system directories. The wrapper and the agent are absolute, but the session
// they start shells out to git, gh, go and crq — and a Homebrew or ~/.local/bin
// install is invisible from there, so every fix would fail at its first command.
// The installing shell's PATH is the one where those tools were just resolved.
func autofixPath(plan AutofixInstall) string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		if dir == "" || dir == "." || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	paths := []string{plan.Binary, plan.Agent}
	if self, err := os.Executable(); err == nil {
		paths = append(paths, self) // the session runs `crq resolve` itself
	}
	for _, path := range paths {
		add(filepath.Dir(path))
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		add(dir)
	}
	if len(dirs) == 0 {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

// autofixEnv is the environment the service runs with.
//
// The service manager hands a unit its own environment, not this shell's, and
// then crq reads the configuration file itself. So the unit has to carry two
// things: the path of the file the install actually read, and the effective
// value of every setting autofix cannot work without — those may have come
// from this shell alone, and there they would be lost. An install that gets this
// wrong still reports Started, and the watcher then loads a different queue, or
// none.
func (s *Service) autofixEnv(plan AutofixInstall) map[string]string {
	env := map[string]string{
		"CRQ_REPOS":        strings.Join(plan.Repos, ","),
		"CRQ_SERVER_URL":   s.cfg.ServerURL,
		"CRQ_SERVER_TOKEN": s.cfg.ServerToken,
		// The denylist travels with the allowlist. Carrying one and not the other
		// installs a service that watches a repository the operator excluded.
		"CRQ_EXCLUDE":                strings.Join(sortedRepoList(s.cfg.ExcludeRepos), ","),
		"CRQ_SCOPE":                  strings.Join(s.cfg.Scope, ","),
		"CRQ_WATCH_INTERVAL":         s.cfg.WatchInterval.String(),
		"CRQ_DISPATCH_MAX_ATTEMPTS":  fmt.Sprint(s.cfg.DispatchMaxAttempts),
		"CRQ_DISPATCH_CONCURRENCY":   fmt.Sprint(s.cfg.DispatchConcurrency),
		"CRQ_DISPATCH_FORKS":         strconv.FormatBool(s.cfg.DispatchForks),
		"CRQ_AUTOREVIEW_SKIP_MARKER": s.cfg.SkipMarker,
		// The session reads these; there is no script holding them any more.
		"CRQ_FIX_AGENT": plan.Agent,
		// Quoted, not space-joined: the session splits this back into argv, and
		// an argument the operator quoted once must not be split a second time.
		"CRQ_FIX_ARGS":              JoinArgv(plan.AgentArgs),
		"CRQ_FIX_PROMPT_FILE":       plan.Prompt,
		"CRQ_BOT":                   s.cfg.Bot,
		"CRQ_REQUIRED_BOTS":         strings.Join(s.cfg.RequiredBots, ","),
		"CRQ_FEEDBACK_BOTS":         "",
		"CRQ_REVIEW_CMD":            s.cfg.ReviewCommand,
		"CRQ_RATELIMIT_CMD":         s.cfg.RateLimitCommand,
		"CRQ_RL_MARKER":             s.cfg.RateLimitMarker,
		"CRQ_CAL_REPLY_MARKER":      s.cfg.CalibrationMarker,
		"CRQ_REVIEW_DONE_MARKER":    s.cfg.ReviewDoneMarker,
		"CRQ_COMPLETION_MARKER":     s.cfg.CompletionMarker,
		"CRQ_MIN_INTERVAL":          s.cfg.MinInterval.String(),
		"CRQ_INFLIGHT_TIMEOUT":      s.cfg.InflightTimeout.String(),
		"CRQ_POLL":                  s.cfg.PollInterval.String(),
		"CRQ_FEEDBACK_WAIT_TIMEOUT": s.cfg.FeedbackWaitTimeout.String(),
		"CRQ_SETTLE":                s.cfg.SettleWindow.String(),
		// The quota timings belong here for the same reason as every other
		// setting: the service does not inherit the shell that installed it. A
		// deliberately longer fallback set only in that shell was folded into
		// this config, written into no unit, and then silently replaced by the
		// default — so the watcher retried a review command earlier than
		// configured for every account block it could not parse a window from.
		"CRQ_CALIBRATE_TTL": s.cfg.CalibrationTTL.String(),
		"CRQ_RL_FALLBACK":   s.cfg.RateLimitFallback.String(),
		"PATH":              autofixPath(plan),
	}
	if s.cfg.FeedbackBotsExplicit {
		env["CRQ_FEEDBACK_BOTS"] = strings.Join(s.cfg.FeedbackBots, ",")
	}
	if s.cfg.RateLimitCoDegrade {
		env["CRQ_RL_CO_DEGRADE"] = "1"
	} else {
		env["CRQ_RL_CO_DEGRADE"] = "0"
	}
	coNames := make([]string, 0, len(s.cfg.CoBots))
	for _, co := range s.cfg.CoBots {
		coNames = append(coNames, co.Name)
		prefix := "CRQ_COBOT_" + strings.ToUpper(co.Name)
		env[prefix+"_CMD"] = co.Command
		env[prefix+"_TRIGGER"] = ""
		if co.TriggerExplicit {
			env[prefix+"_TRIGGER"] = string(co.Trigger)
		}
		env[prefix+"_REQUIRED"] = strconv.FormatBool(co.Required)
		env[prefix+"_GRACE"] = co.SelfHealGrace.String()
	}
	// Explicitly carry an empty set: omitting this key would re-enable every
	// default co-reviewer when the service starts.
	env["CRQ_COBOTS"] = strings.Join(coNames, ",")
	// A workspace supplied by the installing shell is already folded into cfg,
	// but the service does not inherit that shell. Preserve the effective value
	// or dispatch silently falls back to another filesystem.
	workspace := plan.Workspace
	if workspace == "" {
		workspace = s.cfg.WorkspaceRoot
	}
	if workspace != "" {
		env["CRQ_WORKSPACE"] = workspace
	}
	if path := ConfigPath(); path != "" {
		env["CRQ_CONFIG"] = path
	}
	// The queue's identity: the repository holding the state ref, the ref itself,
	// the dashboard, and the PR the account quota is probed on. Without the first
	// two, the watcher cannot even load state — or loads a queue nobody else is
	// using, which looks exactly like an idle fleet.
	if s.cfg.GateRepo != "" {
		env["CRQ_REPO"] = s.cfg.GateRepo
	}
	if s.cfg.StateRef != "" {
		env["CRQ_STATE_REF"] = s.cfg.StateRef
	}
	if s.cfg.StateGitAuthorName != "" {
		env["CRQ_STATE_GIT_AUTHOR_NAME"] = s.cfg.StateGitAuthorName
	}
	if s.cfg.StateGitAuthorEmail != "" {
		env["CRQ_STATE_GIT_AUTHOR_EMAIL"] = s.cfg.StateGitAuthorEmail
	}
	if s.cfg.DashboardIssue > 0 {
		env["CRQ_ISSUE"] = fmt.Sprint(s.cfg.DashboardIssue)
	}
	if s.cfg.CalibrationPR > 0 {
		env["CRQ_CAL_PR"] = fmt.Sprint(s.cfg.CalibrationPR)
	}
	return env
}

// autofixUnit renders the platform's service definition.
func (s *Service) autofixUnit(plan AutofixInstall) string {
	env := s.autofixEnv(plan)
	// Sorted: the unit is a file on disk that a re-install rewrites, and map order
	// would make every rewrite a different file for the same configuration.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	logDir := plan.LogDir
	if plan.Platform == "darwin" {
		var entries strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&entries, "\t\t<key>%s</key><string>%s</string>\n",
				html.EscapeString(k), html.EscapeString(env[k]))
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>no.kristofferr.crq-autofix</string>
	<key>ProgramArguments</key><array>%s</array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/autofix.log</string>
	<key>StandardErrorPath</key><string>%s/autofix.err</string>
</dict></plist>
`, plistArgv(autofixArgv(plan)), entries.String(),
			html.EscapeString(logDir), html.EscapeString(logDir))
	}
	var lines strings.Builder
	for _, k := range keys {
		// Quote the whole assignment. systemd otherwise tokenizes whitespace in
		// CRQ_CONFIG, PATH, commands, and markers into separate assignments.
		//
		// Double '%' first: systemd expands specifiers in Environment= values, so
		// a '%' in a path or a review command would be eaten or fail to load the
		// unit. Unlike ExecStart, '$' is NOT expanded here, so it stays literal.
		fmt.Fprintf(&lines, "Environment=%s\n", strconv.Quote(strings.ReplaceAll(k+"="+env[k], "%", "%%")))
	}
	return fmt.Sprintf(`[Unit]
Description=crq autofix (watch + dispatch fix sessions)
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=20
StandardOutput=append:%s/autofix.log
StandardError=append:%s/autofix.err

[Install]
WantedBy=default.target
`, lines.String(), systemdExecLine(autofixArgv(plan)), logDir, logDir)
}

// systemdExecWord makes one literal word for an ExecStart command line.
// Doubling '$' and '%' suppresses systemd's environment and specifier
// expansion; strconv.Quote handles whitespace, quotes and control characters.
func systemdExecWord(word string) string {
	word = strings.NewReplacer("$", "$$", "%", "%%").Replace(word)
	return strconv.Quote(word)
}

// shellQuote makes one literal POSIX shell word. The generated wrapper is Bash,
// so single quotes prevent parameter, command and backtick expansion; an
// embedded single quote is represented by ending and reopening the quoted word.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

// sortedRepoList renders a repo set for a unit file, in a stable order: the
// unit is a file a re-install rewrites, and map order would make every rewrite
// a different file for the same configuration.
func sortedRepoList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for repo := range set {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// plistArgv renders a command line as launchd's ProgramArguments entries.
func plistArgv(argv []string) string {
	var b strings.Builder
	for _, a := range argv {
		fmt.Fprintf(&b, "<string>%s</string>", html.EscapeString(a))
	}
	return b.String()
}

// systemdExecLine renders a command line for ExecStart, one literal word each.
func systemdExecLine(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, a := range argv {
		words = append(words, systemdExecWord(a))
	}
	return strings.Join(words, " ")
}
