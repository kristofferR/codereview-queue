package crq

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ServeInstall is the plan for keeping a crq service running across a logout
// and a reboot. It covers both the dashboard and the review daemon: they are
// the same shape of thing — one long-running crq subcommand — and the only
// differences are the unit name and the arguments.
//
// Deliberately much thinner than the autofix install beside it. The dashboard
// reads the same config file every other command reads, so editing that file
// changes it after a restart. Only CRQ_* overrides from the installing shell
// are also carried: otherwise those values disappear under a service manager.
type ServeInstall struct {
	// Service is the crq subcommand this unit runs: "serve" or "autoreview".
	Service  string `json:"service"`
	Platform string `json:"platform"`
	Unit     string `json:"unit"`
	LogDir   string `json:"log_dir"`
	Binary   string `json:"binary"`
	Addr     string `json:"addr"`
	Poll     string `json:"poll,omitempty"`
	// AllowHosts are the extra names the dashboard accepts actions on. Part of
	// the unit rather than of the config file because it belongs to the address
	// this instance is served at, which is also where --addr lives.
	AllowHosts []string `json:"allow_hosts,omitempty"`
	Config     string   `json:"config,omitempty"`
	// ReadOnly installs a dashboard that refuses every write, for pointing at a
	// fleet you do not administer.
	ReadOnly bool `json:"read_only,omitempty"`
	// SkipAuthCheck installs without proving the service can authenticate. Same
	// escape hatch as the autofix install: a macOS host reached over SSH cannot
	// read the GUI session's keychain, so an expired token and a perfectly good
	// one look identical from there.
	SkipAuthCheck bool     `json:"skip_auth_check,omitempty"`
	Commands      []string `json:"commands"`
	DryRun        bool     `json:"dry_run,omitempty"`
	Started       bool     `json:"started,omitempty"`
	home          string
	account       string
	environment   map[string]string
}

// InstallServe writes the service definition for `crq serve` and starts it.
func (s *Service) InstallServe(ctx context.Context, addr string, allowHosts []string, readOnly bool, poll time.Duration, dryRun, skipAuth bool) (ServeInstall, error) {
	return s.installUnit(ctx, "serve", addr, allowHosts, readOnly, poll, dryRun || s.cfg.DryRun, skipAuth)
}

// InstallAutoReview writes the service definition for `crq autoreview` and
// starts it — the daemon that finds pull requests needing a review and fires
// the queue.
//
// Which host runs it is a real choice: it takes the leader lease, so the fleet
// only fires while that machine is awake. A laptop that sleeps is the wrong
// host for it, and nothing about the queue says so until reviews quietly stop.
func (s *Service) InstallAutoReview(ctx context.Context, dryRun, skipAuth bool) (ServeInstall, error) {
	return s.installUnit(ctx, "autoreview", "", nil, false, 0, dryRun || s.cfg.DryRun, skipAuth)
}

func (s *Service) installUnit(ctx context.Context, service, addr string, allowHosts []string, readOnly bool, poll time.Duration, dryRun, skipAuth bool) (ServeInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ServeInstall{}, fmt.Errorf("resolving home directory: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return ServeInstall{}, fmt.Errorf("resolving the crq binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if service == "serve" && strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:7777"
	}
	if service == "serve" && poll <= 0 {
		poll = 5 * time.Second
	}
	pollText := ""
	if service == "serve" {
		pollText = poll.String()
	}

	// systemd refuses to start a unit whose StandardOutput path cannot be
	// opened, so the directory has to exist before the unit does.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := ServeInstall{
		Service:       service,
		Platform:      runtime.GOOS,
		LogDir:        logDir,
		Binary:        self,
		Addr:          addr,
		Poll:          pollText,
		AllowHosts:    allowHosts,
		Config:        ConfigPath(),
		ReadOnly:      readOnly,
		SkipAuthCheck: skipAuth,
		DryRun:        dryRun,
		home:          home,
		environment:   serveEnvironment(),
	}
	if current, uerr := osuser.Current(); uerr == nil {
		plan.account = current.Username
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-"+service+".plist")
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-"+service+".service")
	}
	commands := serveCommands(plan)
	for _, command := range commands {
		plan.Commands = append(plan.Commands, command.display)
	}
	if dryRun {
		return plan, nil
	}

	// Both units read GitHub on every pass and neither inherits this shell's
	// variables, so the same check the autofix install makes belongs here.
	// Without it `systemctl restart` succeeds, the install prints Started, and
	// the process then fails its state reads for ever — no reviews, no
	// dashboard, and nothing that says why.
	if !skipAuth {
		if err := serviceCanAuthenticate(ctx, service); err != nil {
			return plan, err
		}
	}

	for _, dir := range []string{logDir, filepath.Dir(plan.Unit)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return plan, err
		}
	}
	if err := writeAutofixFile(plan.Unit, serveUnitBody(plan), 0o600); err != nil {
		return plan, err
	}

	var failed []string
	for _, command := range commands {
		line, args := command.display, command.argv
		if len(args) == 0 {
			return plan, fmt.Errorf("serve start command %q has no executable", line)
		}
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil && launchdJobAbsent(line, output) {
			continue
		}
		if err != nil {
			detail := err.Error()
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += ": " + text
			}
			failed = append(failed, fmt.Sprintf("%s: %s", line, detail))
		}
	}
	if len(failed) > 0 {
		// The unit file is the durable part and is already written, so say what
		// to run rather than pretending the dashboard is up.
		return plan, fmt.Errorf("%s is written, but starting it failed — run these by hand: %s",
			plan.Unit, strings.Join(failed, "; "))
	}
	plan.Started = true
	return plan, nil
}

type serveCommand struct {
	display string
	argv    []string
}

func serveCommands(plan ServeInstall) []serveCommand {
	if plan.Platform == "darwin" {
		domain := "gui/" + currentUID()
		return []serveCommand{
			{
				display: "launchctl bootout " + shellQuote(domain+"/no.kristofferr.crq-"+plan.Service),
				argv:    []string{"launchctl", "bootout", domain + "/no.kristofferr.crq-" + plan.Service},
			},
			{
				display: "launchctl bootstrap " + shellQuote(domain) + " " + shellQuote(plan.Unit),
				argv:    []string{"launchctl", "bootstrap", domain, plan.Unit},
			},
		}
	}
	out := []serveCommand{}
	if plan.account != "" {
		out = append(out, serveCommand{
			display: "loginctl enable-linger " + shellQuote(plan.account),
			argv:    []string{"loginctl", "enable-linger", plan.account},
		})
	}
	return append(out,
		serveCommand{display: "systemctl --user daemon-reload", argv: []string{"systemctl", "--user", "daemon-reload"}},
		serveCommand{
			display: "systemctl --user enable crq-" + plan.Service,
			argv:    []string{"systemctl", "--user", "enable", "crq-" + plan.Service},
		},
		// restart rather than "enable --now", which does nothing to a unit that
		// is already running after an address change.
		serveCommand{
			display: "systemctl --user restart crq-" + plan.Service,
			argv:    []string{"systemctl", "--user", "restart", "crq-" + plan.Service},
		},
	)
}

// serveArgv is the command the service runs.
func serveArgv(plan ServeInstall) []string {
	if plan.Service != "serve" {
		return []string{plan.Binary, plan.Service}
	}
	argv := []string{plan.Binary, "serve", "--addr", plan.Addr}
	if plan.Poll != "" {
		argv = append(argv, "--poll", plan.Poll)
	}
	if len(plan.AllowHosts) > 0 {
		argv = append(argv, "--allow-host", strings.Join(plan.AllowHosts, ","))
	}
	if plan.ReadOnly {
		argv = append(argv, "--read-only")
	}
	return argv
}

// unitDescription is what the service manager and `systemctl status` show.
func unitDescription(service string) string {
	if service == "autoreview" {
		return "crq autoreview (find pull requests needing a review and fire the queue)"
	}
	return "crq GitHub control plane and dashboard (crq serve)"
}

func serveUnitBody(plan ServeInstall) string {
	home := plan.home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	env := make(map[string]string, len(plan.environment)+3)
	for key, value := range plan.environment {
		env[key] = value
	}
	env["HOME"] = home
	if plan.Config != "" {
		env["CRQ_CONFIG"] = plan.Config
	}
	// A service manager hands a unit its own minimal PATH, and the dashboard
	// shells out to git for the state ref and to gh's credential helper.
	if path := os.Getenv("PATH"); path != "" {
		env["PATH"] = path
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if plan.Platform == "darwin" {
		var entries strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&entries, "\t\t<key>%s</key><string>%s</string>\n",
				html.EscapeString(k), html.EscapeString(env[k]))
		}
		var argv strings.Builder
		for _, a := range serveArgv(plan) {
			fmt.Fprintf(&argv, "<string>%s</string>", html.EscapeString(a))
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>no.kristofferr.crq-%s</string>
	<key>ProgramArguments</key><array>%s</array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/%s.log</string>
	<key>StandardErrorPath</key><string>%s/%s.err</string>
</dict></plist>
`, html.EscapeString(plan.Service), argv.String(), entries.String(),
			html.EscapeString(plan.LogDir), html.EscapeString(plan.Service),
			html.EscapeString(plan.LogDir), html.EscapeString(plan.Service))
	}

	var lines strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&lines, "Environment=%s\n", systemdEnvironment(strings.ReplaceAll(k+"="+env[k], "%", "%%")))
	}
	words := make([]string, 0, 4)
	for _, a := range serveArgv(plan) {
		words = append(words, systemdExecWord(a))
	}
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=5
StandardOutput=append:%s/%s.log
StandardError=append:%s/%s.err

[Install]
WantedBy=default.target
`, unitDescription(plan.Service), lines.String(), strings.Join(words, " "),
		plan.LogDir, plan.Service, plan.LogDir, plan.Service)
}

// systemdEnvironment quotes one Environment= assignment using systemd.syntax
// escapes while leaving printable UTF-8 intact. strconv.Quote can emit Go-only
// escapes for arbitrary shell values that older systemd versions reject.
func systemdEnvironment(value string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&quoted, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		switch r {
		case '\a':
			quoted.WriteString(`\a`)
		case '\b':
			quoted.WriteString(`\b`)
		case '\f':
			quoted.WriteString(`\f`)
		case '\n':
			quoted.WriteString(`\n`)
		case '\r':
			quoted.WriteString(`\r`)
		case '\t':
			quoted.WriteString(`\t`)
		case '\v':
			quoted.WriteString(`\v`)
		case '\\', '"', '\'':
			quoted.WriteByte('\\')
			quoted.WriteRune(r)
		default:
			quoted.WriteRune(r)
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}

// serveEnvironment carries configuration that may have reached the install
// through its invoking shell. A service manager does not inherit that shell,
// so pointing at the same config file alone is insufficient.
func serveEnvironment() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, "CRQ_") {
			continue
		}
		switch key {
		case "CRQ_CONFIG", "CRQ_DRY_RUN", "CRQ_NO_OPEN",
			"CRQ_DISPATCH_REPO", "CRQ_DISPATCH_PR", "CRQ_DISPATCH_HEAD", "CRQ_DISPATCH_FINDINGS":
			continue
		}
		env[key] = value
	}
	return env
}
