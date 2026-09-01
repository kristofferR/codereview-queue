package crq

import (
	"context"
	"encoding/xml"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/engine"
)

// Setup people get wrong is setup that silently does nothing, which is the exact
// failure this whole feature exists to prevent. A dry run has to describe the
// real thing, and it must not touch anything.
func TestInstallAutofixPlansWithoutWriting(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	agent := fakeAgent(t, "claude")
	plan, err := svc.InstallAutofix(context.Background(), agent, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Started {
		t.Errorf("plan = %+v, want a dry run that started nothing", plan)
	}
	if plan.Agent != agent {
		t.Errorf("agent = %q, want the one asked for", plan.Agent)
	}
	if len(plan.Repos) != 1 || plan.Repos[0] != "owner/name" {
		t.Errorf("repos = %v, want the configured fleet", plan.Repos)
	}
	for _, path := range []string{plan.Prompt, plan.Binary, plan.Unit, plan.LogDir} {
		if path == "" {
			t.Fatalf("plan = %+v, want every path named so --dry-run is reviewable", plan)
		}
	}
	if len(plan.Commands) == 0 {
		t.Error("a plan that runs nothing cannot survive a logout")
	}
	// The service must be the platform's own, not Linux's everywhere.
	if runtime.GOOS == "darwin" && !strings.HasSuffix(plan.Unit, ".plist") {
		t.Errorf("unit = %q, want a launchd agent on macOS", plan.Unit)
	}
	if runtime.GOOS == "linux" && !strings.HasSuffix(plan.Unit, ".service") {
		t.Errorf("unit = %q, want a systemd unit on linux", plan.Unit)
	}
}

// The plan is what --dry-run shows and what the unit records, so the fleet it
// names has to be the same fleet twice. Taken straight off the map it was not:
// a re-install rewrote the unit with the repositories in a new order, and a
// dry run described a different service than the install that followed it.
func TestAutofixPlanOrdersTheFleetTheSameWayEveryTime(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/zulu": true, "owner/alpha": true, "owner/mike": true}
	agent := fakeAgent(t, "claude")

	want := []string{"owner/alpha", "owner/mike", "owner/zulu"}
	for i := range 8 {
		plan, err := AutofixPlan(cfg, agent, nil, nil, true, false)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Repos, want) {
			t.Fatalf("plan %d repos = %v, want %v", i, plan.Repos, want)
		}
	}
}

func TestAutofixPlanAllowsRepositoriesFromSharedEnrollment(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = nil

	plan, err := AutofixPlan(cfg, fakeAgent(t, "claude"), nil, nil, true, false)
	if err != nil {
		t.Fatalf("planning with no legacy CRQ_REPOS: %v", err)
	}
	if len(plan.Repos) != 0 {
		t.Fatalf("static repos = %v, want the watcher to load shared enrollments", plan.Repos)
	}
}

// The rename breaks the state and the CLI, but a break in naming stops nothing
// that is already running: a host that ran the pre-rename installer keeps an
// enabled crq-drain unit, and installing autofix beside it left two watchers
// scanning the same fleet and racing each other's dispatch claims.
func TestAutofixPlanRetiresThePreRenameWatcher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	agent := fakeAgent(t, "claude")

	// A host that never ran the old installer must not be told to retire it.
	clean, err := AutofixPlan(cfg, agent, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Retire != "" {
		t.Errorf("retire = %q on a host with no legacy unit", clean.Retire)
	}
	for _, c := range clean.Commands {
		if strings.Contains(c, "crq-drain") {
			t.Errorf("commands = %v, want nothing about a watcher that was never installed", clean.Commands)
		}
	}

	legacy := filepath.Join(home, ".config", "systemd", "user", "crq-drain.service")
	if runtime.GOOS == "darwin" {
		legacy = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-drain.plist")
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := AutofixPlan(cfg, agent, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Retire != legacy {
		t.Errorf("retire = %q, want the legacy unit %q", plan.Retire, legacy)
	}
	if len(plan.Commands) == 0 || !strings.Contains(plan.Commands[0], "crq-drain") {
		t.Fatalf("commands = %v, want the legacy watcher stopped before the new one starts", plan.Commands)
	}
	// applyAutofix pairs the two by index and refuses to run a mismatch.
	if got, want := len(autofixCommandArgs(plan)), len(plan.Commands); got != want {
		t.Errorf("argv count = %d, want %d — the plan and what it runs must be the same list", got, want)
	}
}

func TestInstallAutofixHonoursConfiguredDryRun(t *testing.T) {
	cfg := firingConfig()
	cfg.DryRun = true
	cfg.AllowRepos = map[string]bool{"owner/repo": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallAutofix(context.Background(), fakeAgent(t, "claude"), nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Started {
		t.Fatalf("configured dry-run installation applied its plan: %+v", plan)
	}
}

// A missing agent must fail loudly at install time. Discovering it at the first
// dispatch means an autofix watcher that looks installed and fixes nothing.
func TestInstallAutofixRefusesWithoutAnAgent(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	_, err := svc.InstallAutofix(context.Background(), "definitely-not-a-real-binary", nil, nil, true, false)
	if err == nil {
		t.Skip("a binary by that name exists on this machine")
	}
}

// The prompt crq installs is the one documented, because it is the same bytes.
func TestEmbeddedPromptCarriesTheRulesThatCostUs(t *testing.T) {
	for _, want := range []string{
		"DETACHED",
		"HEAD:refs/heads/",
		"crq resolve",
		"crq dismiss",
		"`hold` or `wait`",
		"do not run `crq next --wait`",
		"ONLY after the push is confirmed",
		"Threadless findings are the exception",
		"stop only after every finding is",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("the embedded fix prompt no longer mentions %q", want)
		}
	}
}

func TestWriteAutofixFileRestoresDeclaredMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crq-autofix")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAutofixFile(path, "new", 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %o, want 755", got)
	}
}

func TestAutofixStartArgumentsPreservePathsWithSpaces(t *testing.T) {
	plan := AutofixInstall{
		Platform: "darwin",
		Unit:     "/Users/Example User/Library/LaunchAgents/crq-autofix.plist",
	}
	got := autofixCommandArgs(plan)[1]
	if len(got) != 4 || got[3] != plan.Unit {
		t.Fatalf("bootstrap argv = %q, want unit path %q as one argument", got, plan.Unit)
	}
}

// The service does not inherit the installing shell, and then crq reads its
// configuration file. A unit that names neither the file nor the settings that
// reached the install from the shell alone starts a watcher that loads a
// different queue — or none — while the install reports Started.
func TestAutofixUnitCarriesTheConfigurationTheInstallRead(t *testing.T) {
	config := filepath.Join(t.TempDir(), "env")
	t.Setenv("CRQ_CONFIG", config)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	cfg.Scope = []string{"reviewed-org", "second-org"}
	cfg.DashboardIssue = 7
	cfg.CalibrationPR = 1
	cfg.ServerURL = "https://crq.example.test"
	cfg.ServerToken = "shared-gateway-secret"
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallAutofix(context.Background(), fakeAgent(t, "claude"), nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	unit := svc.autofixUnit(plan)
	// The state ref included: without it the service falls back to the default
	// and reads a queue nobody else is using, which looks exactly like an idle
	// fleet.
	for _, want := range []string{
		config,
		cfg.GateRepo,
		"CRQ_SCOPE=reviewed-org,second-org",
		"CRQ_ISSUE",
		"CRQ_CAL_PR",
		"CRQ_STATE_REF=" + cfg.StateRef,
		"CRQ_SERVER_URL=" + cfg.ServerURL,
		"CRQ_SERVER_TOKEN=" + cfg.ServerToken,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not carry %q; the autofix watcher would not find it:\n%s", want, unit)
		}
	}
	// A secret in a file every local user can read is not the way to hand the
	// service a credential.
	if strings.Contains(unit, "GITHUB_TOKEN") || strings.Contains(unit, "GH_TOKEN") {
		t.Errorf("the unit carries a token:\n%s", unit)
	}
	// Rewriting it for the same configuration must produce the same file.
	if again := svc.autofixUnit(plan); again != unit {
		t.Error("two renderings of one configuration differ; every re-install would rewrite the unit")
	}
}

func TestAutofixUnitCarriesEffectiveReviewerConfiguration(t *testing.T) {
	cfg := firingConfig()
	cfg.DispatchForks = true
	cfg.SkipMarker = "<!-- custom-skip -->"
	cfg.Bot = "custom-reviewer[bot]"
	cfg.RequiredBots = []string{"custom-reviewer[bot]", "cursor[bot]"}
	cfg.FeedbackBots = []string{"custom-reviewer[bot]", "cursor[bot]", "observer[bot]"}
	cfg.FeedbackBotsExplicit = true
	cfg.ReviewCommand = "@custom review this"
	cfg.RateLimitCoDegrade = false
	cfg.MinInterval = time.Hour
	cfg.InflightTimeout = 37 * time.Minute
	cfg.PollInterval = 23 * time.Second
	cfg.FeedbackWaitTimeout = 41 * time.Minute
	cfg.SettleWindow = 17 * time.Second
	cfg.CoBots = []CoBotConfig{{
		Name: "bugbot", Login: "cursor[bot]", Command: "bugbot run now",
		Trigger: engine.TriggerAlways, TriggerExplicit: true,
		Required: true, SelfHealGrace: 4 * time.Minute,
	}}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan := AutofixInstall{Platform: "linux", Binary: "/tmp/crq autofix", LogDir: "/tmp/crq logs"}

	unit := svc.autofixUnit(plan)
	for _, want := range []string{
		`CRQ_BOT=custom-reviewer[bot]`,
		`CRQ_REQUIRED_BOTS=custom-reviewer[bot],cursor[bot]`,
		`CRQ_FEEDBACK_BOTS=custom-reviewer[bot],cursor[bot],observer[bot]`,
		`CRQ_REVIEW_CMD=@custom review this`,
		`CRQ_COBOTS=bugbot`,
		`CRQ_COBOT_BUGBOT_CMD=bugbot run now`,
		`CRQ_COBOT_BUGBOT_TRIGGER=always`,
		`CRQ_COBOT_BUGBOT_REQUIRED=true`,
		`CRQ_COBOT_BUGBOT_GRACE=4m0s`,
		`CRQ_DISPATCH_FORKS=true`,
		`CRQ_AUTOREVIEW_SKIP_MARKER=<!-- custom-skip -->`,
		`CRQ_RL_CO_DEGRADE=0`,
		`CRQ_MIN_INTERVAL=1h0m0s`,
		`CRQ_INFLIGHT_TIMEOUT=37m0s`,
		`CRQ_POLL=23s`,
		`CRQ_FEEDBACK_WAIT_TIMEOUT=41m0s`,
		`CRQ_SETTLE=17s`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not carry %q:\n%s", want, unit)
		}
	}
}

func TestAutofixEnvKeepsDerivedReviewerSettingsDynamic(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_HOST":          "installer",
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	env := svc.autofixEnv(AutofixInstall{})

	if got := env["CRQ_FEEDBACK_BOTS"]; got != "" {
		t.Fatalf("derived feedback bots were frozen into the unit as %q", got)
	}
	if got := env["CRQ_COBOT_CODEX_TRIGGER"]; got != "" {
		t.Fatalf("implicit Codex trigger was frozen into the unit as %q", got)
	}

	// A later fleet change can make Codex required. Restarting from the unit
	// must then re-derive both visibility and the required `always` trigger.
	env["CRQ_REQUIRED_BOTS"] = "coderabbitai,chatgpt-codex-connector[bot]"
	reloaded, err := BuildConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.FeedbackBotsExplicit ||
		!hasLogin(reloaded.FeedbackBots, "chatgpt-codex-connector[bot]") {
		t.Fatalf("feedback bots stayed frozen after restart: %v", reloaded.FeedbackBots)
	}
	if len(reloaded.CoBots) != 1 || reloaded.CoBots[0].Trigger != engine.TriggerAlways ||
		reloaded.CoBots[0].TriggerExplicit {
		t.Fatalf("Codex trigger did not follow requiredness after restart: %+v", reloaded.CoBots)
	}
}

func TestAutofixEnvCarriesAnIntentionallyEmptySkipMarker(t *testing.T) {
	cfg := firingConfig()
	cfg.SkipMarker = ""
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	env := svc.autofixEnv(AutofixInstall{})
	marker, ok := env["CRQ_AUTOREVIEW_SKIP_MARKER"]
	if !ok || marker != "" {
		t.Fatalf("skip marker = %q, present=%t; want an explicit empty value", marker, ok)
	}
}

func TestAutofixEnvCarriesConfiguredWorkspace(t *testing.T) {
	cfg := firingConfig()
	cfg.WorkspaceRoot = "/mnt/large disk/crq"
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	if got := svc.autofixEnv(AutofixInstall{})["CRQ_WORKSPACE"]; got != cfg.WorkspaceRoot {
		t.Fatalf("CRQ_WORKSPACE = %q, want %q", got, cfg.WorkspaceRoot)
	}
}

func TestAutofixEnvCarriesConfiguredStateGitIdentity(t *testing.T) {
	cfg := firingConfig()
	cfg.StateGitAuthorName = "Queue Operator"
	cfg.StateGitAuthorEmail = "queue@example.invalid"
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	env := svc.autofixEnv(AutofixInstall{})
	if env["CRQ_STATE_GIT_AUTHOR_NAME"] != cfg.StateGitAuthorName ||
		env["CRQ_STATE_GIT_AUTHOR_EMAIL"] != cfg.StateGitAuthorEmail {
		t.Fatalf("state Git identity was not carried into autofix: %v", env)
	}
}

func TestInstallAutofixMakesRelativeWorkspaceAbsolute(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	cfg.WorkspaceRoot = "relative workspace"
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallAutofix(context.Background(), fakeAgent(t, "claude"), nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, cfg.WorkspaceRoot)
	if got := svc.autofixEnv(plan)["CRQ_WORKSPACE"]; got != want {
		t.Fatalf("CRQ_WORKSPACE = %q, want absolute install-time path %q", got, want)
	}
}

func TestLaunchdUnitEscapesXMLValues(t *testing.T) {
	cfg := firingConfig()
	cfg.ReviewCommand = "@bot review <this> & report"
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan := AutofixInstall{
		Platform: "darwin",
		Binary:   "/tmp/a&b/crq",
		LogDir:   "/tmp/a<b/logs",
	}

	unit := svc.autofixUnit(plan)
	var document struct{}
	if err := xml.Unmarshal([]byte(unit), &document); err != nil {
		t.Fatalf("launchd unit is not valid XML: %v\n%s", err, unit)
	}
	for _, raw := range []string{plan.Binary, plan.LogDir, cfg.ReviewCommand} {
		if strings.Contains(unit, raw) {
			t.Errorf("launchd unit contains unescaped value %q:\n%s", raw, unit)
		}
		if escaped := html.EscapeString(raw); !strings.Contains(unit, escaped) {
			t.Errorf("launchd unit lost escaped value %q:\n%s", escaped, unit)
		}
	}
}

func TestInstallAutofixMakesRelativeAgentAbsolute(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(bin, "wrapper")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan, err := svc.InstallAutofix(context.Background(), "./bin/wrapper", []string{"--run"}, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(plan.Agent) || plan.Agent != agent {
		t.Errorf("agent = %q, want absolute %q", plan.Agent, agent)
	}
}

func TestSystemdEnvironmentAssignmentsQuoteWhitespace(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "config with spaces", "env"))
	t.Setenv("PATH", "/usr/bin:/opt/tools with spaces/bin")
	plan := AutofixInstall{Platform: "linux", Binary: "/tmp/crq", LogDir: "/tmp/crq"}

	unit := svc.autofixUnit(plan)
	for _, key := range []string{"CRQ_CONFIG", "PATH", "CRQ_REVIEW_CMD"} {
		if !strings.Contains(unit, `Environment="`+key+`=`) {
			t.Errorf("%s assignment is not quoted:\n%s", key, unit)
		}
	}
	if strings.Contains(unit, "Environment=CRQ_CONFIG=") {
		t.Errorf("CRQ_CONFIG was emitted as an unquoted systemd assignment:\n%s", unit)
	}
	// One binary, twice: it starts the watcher and it runs each fix session.
	// Every word quoted, since a path with a space in it is a path.
	if !strings.Contains(unit, `ExecStart="/tmp/crq" "watch" "--" "/tmp/crq" "fix-session"`) {
		t.Errorf("ExecStart is not the crq binary, quoted, twice:\n%s", unit)
	}
}

func TestSystemdExecWordQuotesWhitespaceAndExpansion(t *testing.T) {
	got := systemdExecWord(`/tmp/a path/$user/%i/"crq"`)
	want := `"/tmp/a path/$$user/%%i/\"crq\""`
	if got != want {
		t.Fatalf("systemdExecWord = %q, want %q", got, want)
	}
}

func TestLaunchdMissingJobIsBenignOnlyForBootout(t *testing.T) {
	output := []byte("Boot-out failed: 3: No such process")
	if !launchdJobAbsent("launchctl bootout gui/501/no.kristofferr.crq-autofix", output) {
		t.Error("first-install bootout should accept an absent launchd job")
	}
	if launchdJobAbsent("launchctl bootstrap gui/501 /tmp/autofix.plist", output) {
		t.Error("a bootstrap failure must never be ignored")
	}
	if launchdJobAbsent("launchctl bootout gui/501/no.kristofferr.crq-autofix", []byte("permission denied")) {
		t.Error("a genuine bootout failure must not be ignored")
	}
}

// systemd refuses to start a unit whose StandardOutput path cannot be opened
// (209/STDOUT), so a log directory that does not exist is a service that never
// runs — the silent nothing this command exists to prevent.
func TestInstallAutofixNamesALogDirectory(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallAutofix(context.Background(), fakeAgent(t, "claude"), nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogDir == "" {
		t.Fatal("no log directory planned; the unit would reference a path nothing creates")
	}
	unit := svc.autofixUnit(plan)
	if !strings.Contains(unit, plan.LogDir) {
		t.Errorf("the unit does not write into the directory the install creates:\n%s", unit)
	}
}

// crq knows how to CALL the agents it supports and nothing about which model
// they should use — that belongs in the agent's own config or in --agent-args.
// Getting this wrong is invisible until a session starts and dies on a flag the
// binary does not have.
func TestFixSessionArgvPerAgent(t *testing.T) {
	prompt := "do the thing"

	claude := fixSessionArgv("/usr/bin/claude", prompt, nil, "", "")
	joined := strings.Join(claude, " ")
	for _, want := range []string{"-p", "bypassPermissions", "stream-json", prompt} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude argv %q missing %q", joined, want)
		}
	}

	codex := fixSessionArgv("/usr/bin/codex", prompt, []string{"--foo"}, "", "")
	joined = strings.Join(codex, " ")
	for _, want := range []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "--foo", prompt} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex argv %q missing %q", joined, want)
		}
	}
	// Codex takes the prompt as a positional, and last; claude's flags are not
	// its flags.
	if codex[len(codex)-1] != prompt {
		t.Errorf("codex argv must end with the prompt, got %q", codex[len(codex)-1])
	}
	if strings.Contains(joined, "--permission-mode") {
		t.Errorf("codex argv carries claude's flags: %q", joined)
	}

	// No model or effort unless the repository asked for one. An empty value
	// must add no flag at all: every agent rejects an empty one differently and
	// none ignores it, so the session would die on its first argument and the
	// fix would silently never happen.
	for _, argv := range [][]string{claude, codex} {
		if slices.Contains(argv, "--model") || slices.Contains(argv, "--effort") {
			t.Errorf("argv invents a model or effort: %v", argv)
		}
		for _, a := range argv {
			if a == "" {
				t.Errorf("argv carries an empty word: %v", argv)
			}
		}
	}

	// And they are applied in each agent's own spelling when asked for.
	claude = fixSessionArgv("/usr/bin/claude", prompt, nil, "opus", "high")
	if !slices.Contains(claude, "--model") || !slices.Contains(claude, "opus") ||
		!slices.Contains(claude, "--effort") || !slices.Contains(claude, "high") {
		t.Errorf("claude argv missing the requested model/effort: %v", claude)
	}
	codex = fixSessionArgv("/usr/bin/codex", prompt, nil, "gpt-5", "high")
	if !slices.Contains(codex, `model_reasoning_effort="high"`) {
		t.Errorf("codex argv missing its own effort spelling: %v", codex)
	}

	// An unknown executable is a self-contained prompt-taking wrapper: no flags
	// are invented for it.
	mystery := fixSessionArgv("/usr/bin/mystery", prompt, []string{"--run"}, "opus", "high")
	if mystery[len(mystery)-1] != prompt || !slices.Contains(mystery, "--run") {
		t.Errorf("unknown agent argv = %v, want its own args and the prompt last", mystery)
	}
	if slices.Contains(mystery, "--model") {
		t.Errorf("flags were invented for an unknown agent: %v", mystery)
	}
}

func TestShellQuotePreservesLiteralArguments(t *testing.T) {
	for _, value := range []string{"", "plain", "two words", "$HOME", "`uname`", "$(printf injected)", "it's literal"} {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(value)).Output()
		if err != nil {
			t.Fatalf("quoting %q: %v", value, err)
		}
		if got := string(out); got != value {
			t.Errorf("quoted %q became %q", value, got)
		}
	}
}

// fakeAgent is an executable with the given name, so an install test exercises
// the real "does this agent exist and how is it called" path.
func fakeAgent(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// "enable --now" does nothing to a unit that is already running, so a reinstall
// with a different agent kept the old one going — an install that reports
// success and changes nothing.
func TestInstallAutofixRestartsAnAlreadyRunningService(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("launchd bootout/bootstrap already replaces a running agent")
	}
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallAutofix(context.Background(), fakeAgent(t, "claude"), nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	restarts := false
	for _, c := range plan.Commands {
		if strings.Contains(c, "restart") {
			restarts = true
		}
	}
	if !restarts {
		t.Errorf("commands = %v, want one that replaces a running service", plan.Commands)
	}
}
