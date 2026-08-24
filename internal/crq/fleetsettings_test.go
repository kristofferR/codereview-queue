package crq

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

func TestFleetUnsetInstructionsWinOverValues(t *testing.T) {
	minimum := "10m"
	savedWeekly := 60
	weekly := 80
	service := &Service{}
	got, err := service.applyFleetChange(State{Fleet: FleetDefaults{
		MinInterval: "5m",
		WeeklyLimit: &savedWeekly,
	}}, FleetChange{
		MinInterval:      &minimum,
		UnsetMinInterval: true,
		WeeklyLimit:      &weekly,
		UnsetWeeklyLimit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.MinInterval != "" || got.WeeklyLimit != nil {
		t.Fatalf("unset did not take precedence: %+v", got)
	}
}

func TestFleetRequiredReviewersResolveAgainstTheEffectivePrimary(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	const current = "replacement-reviewer[bot]"
	st := DefaultState(cfg)
	st.Fleet = fleetEnvSet(st.Fleet, "CRQ_BOT", current, false)
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	next, err := svc.applyFleetChange(st, FleetChange{Required: []string{current}})
	if err != nil {
		t.Fatalf("current fleet primary was rejected: %v", err)
	}
	if got := cfg.WithFleet(next).RequiredBots; len(got) != 1 || !sameBot(got[0], current) {
		t.Fatalf("required = %v, want the effective primary", got)
	}
	if _, err := svc.applyFleetChange(st, FleetChange{Required: []string{cfg.Bot}}); err == nil {
		t.Fatal("the retired startup primary was accepted")
	}
}

func TestFleetReviewerEditPreservesExistingCustomRequiredBot(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot],sonar[bot]",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: cfg}
	next, err := svc.applyFleetChange(DefaultState(cfg), FleetChange{
		Required: []string{"coderabbitai[bot]", "sonar[bot]", "codex"},
	})
	if err != nil {
		t.Fatalf("unrelated fleet reviewer edit rejected the existing custom gate: %v", err)
	}
	if got := strings.Join(next.Required, ","); !strings.Contains(got, "sonar[bot]") {
		t.Fatalf("required = %q, want the existing custom gate preserved", got)
	}
}

func TestMigratedFleetScopeAndRepoPolicyStillApplies(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":    "owner/gate",
		"CRQ_SCOPE":   "local",
		"CRQ_REPOS":   "local/repo",
		"CRQ_EXCLUDE": "local/paused",
	})
	if err != nil {
		t.Fatal(err)
	}
	effective := cfg.WithFleet(FleetDefaults{Env: map[string]string{
		"CRQ_SCOPE":   "owner,friend",
		"CRQ_REPOS":   "owner/repo,friend/private",
		"CRQ_EXCLUDE": "owner/paused",
	}})
	if strings.Join(effective.Scope, ",") != "owner,friend" ||
		!effective.AllowRepos["owner/repo"] || !effective.AllowRepos["friend/private"] ||
		!effective.ExcludeRepos["owner/paused"] {
		t.Fatalf("migrated v4 fleet policy was not applied: %+v", effective)
	}
}

func TestEnvSettingsRenderEffectiveDefaults(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{"CRQ_REPO": "owner/gate"})
	if err != nil {
		t.Fatal(err)
	}
	settings := (&Service{cfg: cfg}).EnvSettings(State{})
	foundCalibration := false
	foundDegrade := false
	foundPreflightSkip := false
	for _, setting := range settings {
		if setting.Key == "CRQ_CALIBRATE_TTL" {
			foundCalibration = true
			if setting.Value != "2m0s" || setting.Source != "default" {
				t.Fatalf("calibration setting = %+v, want the effective default", setting)
			}
		}
		switch setting.Key {
		case "CRQ_RL_CO_DEGRADE":
			foundDegrade = true
			if setting.Value != "1" || setting.Source != "default" {
				t.Fatalf("degrade setting = %+v, want the effective on-by-default value", setting)
			}
		case "CRQ_PREFLIGHT_SKIP_BLOCKED":
			foundPreflightSkip = true
			if setting.Value != "1" || setting.Source != "default" {
				t.Fatalf("preflight skip setting = %+v, want the effective on-by-default value", setting)
			}
		}
	}
	if !foundCalibration {
		t.Fatal("CRQ_CALIBRATE_TTL was not listed")
	}
	if !foundDegrade {
		t.Fatal("CRQ_RL_CO_DEGRADE was not listed")
	}
	if !foundPreflightSkip {
		t.Fatal("CRQ_PREFLIGHT_SKIP_BLOCKED was not listed")
	}
}

func TestEnvSettingsUseOnlyTheTypedRequiredReviewerControl(t *testing.T) {
	for _, setting := range EnvKeys() {
		if strings.HasPrefix(setting.Key, "CRQ_COBOT_") && strings.HasSuffix(setting.Key, "_REQUIRED") {
			t.Fatalf("%s exposes a shadowed per-bot required control; use CRQ_REQUIRED_BOTS", setting.Key)
		}
	}
}

func TestFleetViewRecognizesGenericFleetProvenance(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{"CRQ_REPO": "owner/gate"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: cfg}
	view := svc.fleetViewOf(State{Fleet: FleetDefaults{Env: map[string]string{
		"CRQ_MIN_INTERVAL": "4m",
		"CRQ_WEEKLY_LIMIT": "90",
		"CRQ_COBOTS":       "codex",
	}}})
	for _, key := range []string{"min_interval", "weekly_limit", "reviewers"} {
		if view.Sources[key] != "fleet" {
			t.Errorf("source[%s] = %q, want fleet for a generic fleet record", key, view.Sources[key])
		}
	}
}

// The three layers are the whole feature, so this pins their order and — just
// as importantly — that an absent fleet setting changes nothing at all. A fleet
// that never writes a record must behave exactly as it did before the record
// existed.
func TestFleetDefaultsLayering(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	cfg.MinInterval = 90 * time.Second
	cfg.WeeklyReviewLimit = 60
	cfg.AllowRepos = map[string]bool{"o/plain": true, "o/opinionated": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// No record: every value is this host's env, and .sources says so.
	view, err := svc.FleetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Recorded || view.MinInterval != "1m30s" || view.WeeklyLimit != 60 {
		t.Fatalf("view = %+v, want the env values with no record", view)
	}
	// "default" and "env" are different answers: firingConfig sets none of
	// these, so nothing here comes from a file and saying "env" would send a
	// reader looking for a line that does not exist.
	for key, src := range view.Sources {
		if src != "default" {
			t.Errorf("source[%s] = %q, want default when nothing sets it", key, src)
		}
	}
	if !view.AutofixDefault {
		t.Error("autofix defaults on, as it always has")
	}

	// Inheritance is per SETTING. A repository that answers the reviewer
	// question itself is still paced by the fleet — there is one queue and one
	// fire slot — so a pacing change reaches it, and a preview that called it
	// "unaffected" would be understating where the change lands.
	if _, err := svc.SetReviewers(ctx, "o/opinionated", []string{"codex"}, []string{"codex"}, nil); err != nil {
		t.Fatal(err)
	}
	impact, err := svc.PreviewFleet(ctx, FleetChange{MinInterval: strptr("3m")})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Repos != 2 || impact.Overridden != 0 {
		t.Errorf("repos/overridden = %d/%d, want pacing to reach both repositories", impact.Repos, impact.Overridden)
	}
	if len(impact.Changes) != 1 {
		t.Errorf("changes = %v, want the pacing change named", impact.Changes)
	}
	// Same for the autofix default: what stops it is a repository's own autofix
	// switch, and neither repository has one.
	autofix, err := svc.PreviewFleet(ctx, FleetChange{AutofixDefault: boolptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if autofix.Repos != 2 || autofix.Overridden != 0 {
		t.Errorf("repos/overridden = %d/%d, want the autofix default to reach both repositories",
			autofix.Repos, autofix.Overridden)
	}
	// A preview must not write.
	if v, _ := svc.FleetSettings(ctx); v.Recorded {
		t.Fatal("a preview wrote a record")
	}

	// Recording it changes the effective config for repositories that follow.
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{
		MinInterval: strptr("3m"), WeeklyLimit: intptr(90), AutofixDefault: boolptr(false),
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/plain").MinInterval; got != 3*time.Minute {
		t.Errorf("min interval = %s, want the fleet record to win over env", got)
	}
	if got := svc.cfgFor(st, "o/plain").WeeklyReviewLimit; got != 90 {
		t.Errorf("weekly limit = %d, want 90", got)
	}
	if st.AutofixEnabled("o/never-ruled-on") {
		t.Error("a repository with no switch must follow the fleet default, which is now off")
	}
	view, _ = svc.FleetSettings(ctx)
	if view.Sources["min_interval"] != "fleet" || view.Sources["reviewers"] != "default" {
		t.Errorf("sources = %v, want only the recorded settings sourced from the fleet", view.Sources)
	}

	// The override still wins over the record.
	if got := svc.cfgFor(st, "o/opinionated").RequiredBots; len(got) != 1 {
		t.Errorf("required = %v, want the repository's own answer to survive a fleet default", got)
	}

	// Gating on nobody is refused here for the same reason it is per repo.
	if _, err := svc.PreviewFleet(ctx, FleetChange{Required: []string{}}); err == nil {
		t.Error("an empty required set must be refused")
	}
	// So is pacing fast enough to be meaningless.
	if _, err := svc.PreviewFleet(ctx, FleetChange{MinInterval: strptr("1s")}); err == nil {
		t.Error("a sub-5s pacing floor must be refused")
	}

	// Clearing returns every setting to env.
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{Clear: true}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	if got := svc.cfgFor(st, "o/plain").MinInterval; got != 90*time.Second {
		t.Errorf("min interval = %s, want the env value back", got)
	}
	if !st.AutofixEnabled("o/never-ruled-on") {
		t.Error("clearing must restore the default-on answer")
	}
}

func TestClearFleetSettingsClearsTimestampLessMigratedPolicy(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/gate",
		"CRQ_SCOPE": "startup-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_SCOPE": "migrated-owner"}
		st.Fleet.UpdatedAt = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if view, err := svc.FleetSettings(ctx); err != nil {
		t.Fatal(err)
	} else if !view.Recorded {
		t.Fatal("timestamp-less migrated policy was reported as absent")
	}

	view, _, err := svc.SetFleetSettings(ctx, FleetChange{Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.Recorded {
		t.Fatal("cleared migrated policy still reports a fleet record")
	}
	st, _, _ := store.Load(ctx)
	if !st.Fleet.Empty() {
		t.Fatalf("fleet policy was not cleared: %+v", st.Fleet)
	}
}

func TestFleetViewRecognizesExplicitEmptyEnvironmentOverrides(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":   "owner/gate",
		"CRQ_HOST":   "testhost",
		"CRQ_COBOTS": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).
		FleetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Sources["reviewers"]; got != "env" {
		t.Fatalf("reviewer source = %q, want env for a present empty override", got)
	}
}

func TestFleetViewRecognizesPrimaryEnvironmentOverride(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate",
		"CRQ_HOST": "testhost",
		"CRQ_BOT":  "replacement-reviewer[bot]",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).
		FleetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Sources["reviewers"]; got != "env" {
		t.Fatalf("reviewer source = %q, want env for CRQ_BOT", got)
	}
}

func TestSetEnvNormalizesStoredValues(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate",
		"CRQ_HOST": "testhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnv(ctx, "CRQ_INFLIGHT_TIMEOUT", " 5m ", false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Fleet.Env["CRQ_INFLIGHT_TIMEOUT"]; got != "5m" {
		t.Fatalf("stored timeout = %q, want normalized value", got)
	}
	if got := svc.cfgFor(st, "o/repo").InflightTimeout; got != 5*time.Minute {
		t.Fatalf("effective timeout = %s, want 5m", got)
	}
}

func TestConfigValueOfIncludesCalibrationTTL(t *testing.T) {
	cfg := firingConfig()
	cfg.CalibrationTTL = 7 * time.Minute
	if got := configValueOf(cfg, "CRQ_CALIBRATE_TTL"); got != "7m0s" {
		t.Fatalf("calibration TTL = %q, want 7m0s", got)
	}
}

func TestDashboardReviewerSummaryUsesFleetState(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":   "owner/gate",
		"CRQ_HOST":   "testhost",
		"CRQ_COBOTS": "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := DefaultState(cfg)
	st.Fleet.CoBots = []string{"cursor[bot]"}
	st.Fleet.SetCoBots = true

	body := renderDashboard(st, cfg)
	if !strings.Contains(body, "| **Co-reviewers** | bugbot") {
		t.Fatalf("dashboard did not render the fleet's reviewer set:\n%s", body)
	}
	if strings.Contains(body, "| **Co-reviewers** | codex") {
		t.Fatalf("dashboard rendered the host's stale startup reviewers:\n%s", body)
	}
}

// Unsetting a fleet setting has to actually unset it. The typed fields — the
// two lists and the weekly limit — are the ones where "leave this alone" and
// "the fleet has no answer" look identical in JSON, so this is where a clear
// could report success and change nothing.
func TestSetEnvClearsTypedFleetSettings(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.WeeklyReviewLimit = 90 // this host's env, which a clear must hand back
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, set := range []struct{ key, value string }{
		{"CRQ_COBOTS", "codex"},
		{"CRQ_REQUIRED_BOTS", "coderabbitai[bot]"},
		{"CRQ_WEEKLY_LIMIT", "60"},
	} {
		if _, err := svc.SetEnv(ctx, set.key, set.value, false); err != nil {
			t.Fatalf("recording %s: %v", set.key, err)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Fleet.SetCoBots || !st.Fleet.SetRequired || st.Fleet.WeeklyLimit == nil {
		t.Fatalf("fleet = %+v, want all three recorded before the clear", st.Fleet)
	}

	for _, key := range []string{"CRQ_COBOTS", "CRQ_REQUIRED_BOTS", "CRQ_WEEKLY_LIMIT"} {
		if _, err := svc.SetEnv(ctx, key, "", true); err != nil {
			t.Fatalf("clearing %s: %v", key, err)
		}
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.SetCoBots || st.Fleet.CoBots != nil {
		t.Errorf("cobots = %v (set=%v), want the record gone", st.Fleet.CoBots, st.Fleet.SetCoBots)
	}
	if st.Fleet.SetRequired || st.Fleet.Required != nil {
		t.Errorf("required = %v (set=%v), want the record gone", st.Fleet.Required, st.Fleet.SetRequired)
	}
	if st.Fleet.WeeklyLimit != nil {
		t.Errorf("weekly limit = %d, want the pointer removed rather than a default written",
			*st.Fleet.WeeklyLimit)
	}
	// The point of removing it: this host's own 90 answers again.
	if got := svc.cfgFor(st, "o/plain").WeeklyReviewLimit; got != 90 {
		t.Errorf("weekly limit = %d, want this host's env value back", got)
	}
}

func TestSetEnvBlankClearsGenericFleetSetting(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost",
		"CRQ_REVIEW_CMD": "@host review", "CRQ_COBOTS": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnv(ctx, "CRQ_REVIEW_CMD", "@fleet review", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnv(ctx, "CRQ_REVIEW_CMD", "", false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Fleet.Env["CRQ_REVIEW_CMD"]; ok {
		t.Fatalf("fleet env = %v, want the blank value removed", st.Fleet.Env)
	}
	if got := svc.cfgFor(st, "owner/repo").ReviewCommand; got != "@host review" {
		t.Fatalf("review command = %q, want host inheritance restored", got)
	}
}

func TestSetEnvPreservesMeaningfulEmptyFleetSettings(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost",
		"CRQ_AUTOREVIEW_SKIP_AUTHORS": "dependabot[bot]",
		"CRQ_AUTOREVIEW_SKIP_MARKER":  "<!-- host marker -->",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, key := range []string{"CRQ_AUTOREVIEW_SKIP_AUTHORS", "CRQ_AUTOREVIEW_SKIP_MARKER"} {
		if _, err := svc.SetEnv(ctx, key, "", false); err != nil {
			t.Fatalf("recording empty %s: %v", key, err)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CRQ_AUTOREVIEW_SKIP_AUTHORS", "CRQ_AUTOREVIEW_SKIP_MARKER"} {
		if value, ok := st.Fleet.Env[key]; !ok || value != "" {
			t.Errorf("fleet[%s] = %q (present=%v), want an explicit empty value", key, value, ok)
		}
	}
	effective := svc.cfgFor(st, "owner/repo")
	if len(effective.SkipAuthors) != 0 || effective.SkipMarker != "" {
		t.Fatalf("effective skip policy = authors %v marker %q, want both disabled",
			effective.SkipAuthors, effective.SkipMarker)
	}
}

func TestFleetSettingsNamesLaggingAutofixWatchers(t *testing.T) {
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	svc.now = func() time.Time { return now }
	st := DefaultState(cfg)
	st.SetFleetDefaults(FleetDefaults{AutofixDefault: boolptr(false)}, "operator", now)
	st.SetHostReport(HostReport{
		Host: "old-watcher", Caps: CapsFleetDefaults - 1, Roles: []string{"autofix"},
	}, now)

	view := svc.fleetViewOf(st)
	if len(view.Lagging) != 1 || view.Lagging[0] != "old-watcher" {
		t.Fatalf("lagging = %v, want the autofix watcher that ignores fleet defaults", view.Lagging)
	}
}

func TestFleetSettingsNamesHostsThatIgnoreBlockedPreflightPolicy(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg, err := BuildConfig(map[string]string{"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	svc.now = func() time.Time { return now }
	st := DefaultState(cfg)
	st.SetFleetDefaults(FleetDefaults{Env: map[string]string{"CRQ_PREFLIGHT_SKIP_BLOCKED": "1"}}, "operator", now)
	st.SetHostReport(HostReport{
		Host: "old-watcher", Caps: CapsPreflightSkipBlocked - 1, Roles: []string{"autofix"},
	}, now)

	view := svc.fleetViewOf(st)
	if len(view.Lagging) != 1 || view.Lagging[0] != "old-watcher" {
		t.Fatalf("lagging = %v, want the autofix watcher that ignores the blocked-preflight policy", view.Lagging)
	}
}

func TestFleetSettingsDoesNotNameHostsWhenBlockedPreflightSkippingIsOff(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg, err := BuildConfig(map[string]string{"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	svc.now = func() time.Time { return now }
	st := DefaultState(cfg)
	st.SetFleetDefaults(FleetDefaults{Env: map[string]string{"CRQ_PREFLIGHT_SKIP_BLOCKED": "0"}}, "operator", now)
	st.SetHostReport(HostReport{
		Host: "old-watcher", Caps: CapsPreflightSkipBlocked - 1, Roles: []string{"autofix"},
	}, now)

	view := svc.fleetViewOf(st)
	if len(view.Lagging) != 0 {
		t.Fatalf("lagging = %v, want no warning when blocked-preflight skipping is off", view.Lagging)
	}
}

// A fleet default reaches the repositories that follow the fleet, and a
// repository somebody turned off follows nothing. It has both an enrollment
// record and completed rounds, so it used to reach the requeue set twice over —
// and saving an unrelated default then spent quota there.
func TestReposFollowingFleetExcludesDisabledRepositories(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/on": true, "o/off": true, "o/excluded": true}
	cfg.ExcludeRepos = map[string]bool{"o/excluded": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/off", false, "not reviewing this one"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range svc.reposFollowingFleet(st) {
		if repo == "o/off" || repo == "o/excluded" {
			t.Fatalf("following = %v, want disabled and host-excluded repositories omitted", svc.reposFollowingFleet(st))
		}
	}
}

// Naming a different primary is a reviewer change, whichever door it comes
// through. It arrives as a generic env setting rather than one of the four
// typed ones, and that path recorded the value and stopped — so every completed
// round stayed a "this head was reviewed" marker and the bot somebody had just
// configured was never asked for one.
func TestSetEnvPrimaryReopensCompletedRounds(t *testing.T) {
	ctx := context.Background()
	// From an env map, because the generic fleet settings are applied by
	// re-parsing the configuration this host was built from.
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "", "CRQ_MIN_INTERVAL": "0s", "CRQ_POLL": "1ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head := "o/r", 4, "aaaaaaaa1"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head + "bcdef1234"
	gh.pulls[fakeKey(repo, pr)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 11)

	impact, err := svc.PreviewEnv(ctx, "CRQ_BOT", dialect.CodexBotLogin, false)
	if err != nil {
		t.Fatal(err)
	}
	if impact.Reopened != 1 || !strings.Contains(strings.Join(impact.Changes, "\n"), "primary reviewer") {
		t.Fatalf("impact = %+v, want the primary change and one reopened round", impact)
	}

	// A confirmation is tied to exactly what it priced. An unrelated writer
	// moving state still makes the operator preview again.
	if _, err := store.Update(ctx, func(st *State) error {
		st.CalibrationIssue++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SetEnvAt(ctx, "CRQ_BOT", dialect.CodexBotLogin, false, &impact.Rev); err == nil ||
		!strings.Contains(err.Error(), "preview the change again") {
		t.Fatalf("stale save error = %v, want a revision mismatch", err)
	}
	impact, err = svc.PreviewEnv(ctx, "CRQ_BOT", dialect.CodexBotLogin, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SetEnvAt(ctx, "CRQ_BOT", dialect.CodexBotLogin, false, &impact.Rev); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, repo).Bot; got != dialect.CodexBotLogin {
		t.Fatalf("primary = %q, want the fleet record applied", got)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseQueued || round.Head != head {
		t.Fatalf("round = %#v, want the completed round requeued at the same head", round)
	}

	// And a setting that decides nothing about reviewers still reopens nothing:
	// requeueing on every save would spend the account's quota on a timezone.
	if _, err := svc.SetEnv(ctx, "CRQ_TZ", "Europe/Oslo", false); err != nil {
		t.Fatal(err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Round(repo, pr); r == nil || r.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want an unrelated setting to leave it exactly as it was", r)
	}
}

func TestSetEnvPrimaryReopensFullyOverriddenRepository(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "", "CRQ_MIN_INTERVAL": "0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head := "o/r", 14, "cccccccc3"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head+"def1234"
	gh.pulls[fakeKey(repo, pr)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Both repository-level reviewer questions are overridden. The primary is
	// still inherited unless PrimaryOff is set.
	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{cfg.Bot}, nil); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 21)

	if _, err := svc.SetEnv(ctx, "CRQ_BOT", "replacement-reviewer[bot]", false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want a primary change to requeue the fully overridden repository", round)
	}
}

func TestSetEnvTriggerPolicyReopensCompletedRounds(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "codex", "CRQ_COBOT_CODEX_TRIGGER": "never",
		"CRQ_COBOT_CODEX_CMD": "@codex review", "CRQ_MIN_INTERVAL": "0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head := "o/r", 5, "bbbbbbbb2"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head+"cdef1234"
	gh.pulls[fakeKey(repo, pr)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 12)

	impact, err := svc.PreviewEnv(ctx, "CRQ_COBOT_CODEX_TRIGGER", "always", false)
	if err != nil {
		t.Fatal(err)
	}
	if impact.Reopened != 1 || !strings.Contains(strings.Join(impact.Changes, "\n"), "trigger policy") {
		t.Fatalf("impact = %+v, want the trigger change and one reopened round", impact)
	}
	if _, _, err := svc.SetEnvAt(ctx, "CRQ_COBOT_CODEX_TRIGGER", "always", false, &impact.Rev); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want the trigger policy change to requeue it", round)
	}
}

func TestFleetReviewerChangeRefusesAClaimedCoTrigger(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "codex", "CRQ_REQUIRED_BOTS": "coderabbitai[bot],codex",
		"CRQ_COBOT_CODEX_TRIGGER": "always", "CRQ_COBOT_CODEX_CMD": "@codex review",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/r", 9, "abcdef123", time.Now())
		if err != nil {
			return err
		}
		round.ClaimCo(dialect.CodexBotLogin, time.Now())
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	impact, err := svc.PreviewFleet(ctx, FleetChange{
		CoBots: []string{}, Required: []string{cfg.Bot},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.SetFleetSettings(ctx, FleetChange{
		CoBots: []string{}, Required: []string{cfg.Bot}, ExpectedRev: &impact.Rev,
	})
	if err == nil || !strings.Contains(err.Error(), "trigger is already being posted") {
		t.Fatalf("fleet save error = %v, want the claimed post to block reviewer removal", err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.SetCoBots || st.Fleet.SetRequired {
		t.Fatalf("fleet = %+v, want the rejected reviewer edit not to land", st.Fleet)
	}
}

func TestFleetCommandChangeRefusesAClaimedCoTrigger(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "codex", "CRQ_COBOT_CODEX_TRIGGER": "always",
		"CRQ_COBOT_CODEX_CMD": "@codex review",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/r", 9, "abcdef123", time.Now())
		if err != nil {
			return err
		}
		round.ClaimCo(dialect.CodexBotLogin, time.Now())
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	impact, err := svc.PreviewEnv(ctx, "CRQ_COBOT_CODEX_CMD", "@codex full review", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(impact.Changes, "\n"), "command") {
		t.Fatalf("impact = %+v, want the command change reported", impact)
	}
	if _, _, err := svc.SetEnvAt(
		ctx, "CRQ_COBOT_CODEX_CMD", "@codex full review", false, &impact.Rev,
	); err == nil || !strings.Contains(err.Error(), "trigger is already being posted") {
		t.Fatalf("command save error = %v, want the claimed post to block it", err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Fleet.Env["CRQ_COBOT_CODEX_CMD"]; ok {
		t.Fatalf("fleet env = %+v, want the rejected command edit not to land", st.Fleet.Env)
	}
}

func TestSetEnvRecomputesFleetFollowersInsideCAS(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_REPOS": "o/r",
		"CRQ_COBOTS": "", "CRQ_MIN_INTERVAL": "0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head := "o/r", 6, "cccccccc3"
	store := NewMemoryStore(cfg)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetRepoOverride(repo, RepoReviewers{
			CoBots: []string{}, SetCoBots: true,
			Required: []string{cfg.Bot}, SetRequired: true,
			UpdatedAt: &now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, now, 13)

	hooked := &hookedStore{StateStore: store}
	hooked.hook = func() {
		if _, err := store.Update(ctx, func(st *State) error {
			st.ClearRepoOverride(repo)
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	svc := NewService(cfg, newFakeGitHub(), hooked, nil)
	if _, err := svc.SetEnv(ctx, "CRQ_BOT", "chatgpt-codex-connector[bot]", false); err != nil {
		t.Fatal(err)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseCompleted || !round.ReviewersChanged {
		t.Fatalf("round = %#v, want a conservative reopen marker after the repository began following the fleet", round)
	}
}

func strptr(s string) *string { return &s }
func intptr(n int) *int       { return &n }
func boolptr(b bool) *bool    { return &b }

// A fleet-recorded primary has to be the one crq actually asks. The decision
// resolves the record; the apply path used to post this host's startup value —
// so the daemon spent the account's quota reserving a round for one bot and
// then sent the other bot's command, and waited for a review nobody requested.
func TestFireUsesTheFleetResolvedPrimary(t *testing.T) {
	ctx := context.Background()
	// Built from an env map rather than a literal: the fleet's generic settings
	// are applied by re-parsing the configuration crq was built from, which is
	// what a host actually has.
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost",
		"CRQ_COBOTS": "", "CRQ_MIN_INTERVAL": "0s", "CRQ_POLL": "1ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnv(ctx, "CRQ_REVIEW_CMD", "@coderabbitai full review", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	if pumped, err := svc.Pump(ctx); err != nil || pumped.Action != "fired" {
		t.Fatalf("pump = %+v, err = %v, want a fired round", pumped, err)
	}
	if len(gh.posted) != 1 || !strings.HasSuffix(gh.posted[0], "@coderabbitai full review") {
		t.Fatalf("posted %q, want the fleet's recorded review command", gh.posted)
	}
	// And the round records the command as the primary's, so the wait is for a
	// bot that was actually asked.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round("owner/repo", 12)
	if round == nil || len(round.PostedCommands) != 1 || round.PostedCommands[0].Bot != cfg.Bot {
		t.Errorf("posted commands = %+v, want one attributed to %s", round.PostedCommands, cfg.Bot)
	}
}

// A repository that states ONE half of its reviewers still inherits the other
// from the fleet. Treating either half as a complete override dropped it from
// the impact preview and from the requeue: cfgFor handed it the newly required
// reviewer, its completed round stayed a "this head was reviewed" marker, and
// the reviewer somebody had just required was never asked there.
func TestFleetReachesRepositoriesThatOverrideOnlyOneHalf(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	cfg.AllowRepos = map[string]bool{"o/half": true, "o/whole": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// o/half names its co-reviewers and inherits the required set; o/whole
	// answers both questions itself.
	if _, err := svc.SetReviewers(ctx, "o/half", []string{"codex"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetReviewers(ctx, "o/whole", []string{"codex"}, []string{"codex"}, nil); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	following := map[string]bool{}
	for _, repo := range svc.reposFollowingFleet(st) {
		following[repo] = true
	}
	if !following["o/half"] {
		t.Errorf("following = %v, want the half-overridden repository included: it still inherits the required set",
			svc.reposFollowingFleet(st))
	}
	if following["o/whole"] {
		t.Errorf("following = %v, want the fully-overridden repository excluded", svc.reposFollowingFleet(st))
	}

	// And the preview counts as "unaffected" only the one that really is.
	impact, err := svc.PreviewFleet(ctx, FleetChange{Required: []string{"coderabbitai[bot]", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Overridden != 1 {
		t.Errorf("overridden = %d, want only the repository that answers both questions itself", impact.Overridden)
	}
}

func TestFleetImpactCountsOnlyOpenCompletedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = nil
	cfg.RequiredBots = []string{cfg.Bot}
	cfg.AllowRepos = map[string]bool{"o/repo": true}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/repo", Number: 1}}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		for _, pr := range []int{1, 2} {
			round, err := st.NewRound("o/repo", pr, fmt.Sprintf("head-%d", pr), time.Now())
			if err != nil {
				return err
			}
			round.Phase = PhaseCompleted
			st.PutRound(*round)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	impact, err := svc.PreviewFleet(ctx, FleetChange{CoBots: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Reopened != 1 {
		t.Fatalf("reopened = %d, want only the completed round whose PR is still open", impact.Reopened)
	}
}

func TestFleetSaveRejectsAStalePreviewRevision(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	impact, err := svc.PreviewFleet(ctx, FleetChange{MinInterval: strptr("3m")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.HostReports = map[string]HostReport{"other": {Host: "other"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{
		MinInterval: strptr("3m"), ExpectedRev: &impact.Rev,
	}); err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("stale preview save error = %v, want a preview-again refusal", err)
	}
}

func TestClearFleetSettingsReopensFullyOverriddenRepoForNewPrimary(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_REPOS":         "o/repo",
		"CRQ_BOT":           "env-primary[bot]",
		"CRQ_REVIEW_CMD":    "@env-primary review",
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "env-primary[bot],codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/repo", Number: 1}}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{
			"CRQ_BOT":           "fleet-primary[bot]",
			"CRQ_REVIEW_CMD":    "@fleet-primary review",
			"CRQ_REQUIRED_BOTS": "fleet-primary[bot],codex",
		}
		st.SetRepoOverride("o/repo", RepoReviewers{
			CoBots: []string{"codex"}, SetCoBots: true,
			Required: []string{"codex"}, SetRequired: true,
		})
		round, err := st.NewRound("o/repo", 1, "head-1", now)
		if err != nil {
			return err
		}
		round.Phase = PhaseCompleted
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	impact, err := svc.PreviewFleet(ctx, FleetChange{Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Reopened != 1 || !strings.Contains(strings.Join(impact.Changes, " "), "primary reviewer") {
		t.Fatalf("clear impact = %+v, want the inherited primary change and one reopened round", impact)
	}
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{Clear: true, ExpectedRev: &impact.Rev}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/repo", 1); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round after clear = %+v, want it reopened for the env primary", round)
	}
}

// A save the fleet will ignore is worse than a refused one: the settings page
// then reports a value that no daemon is running. Every setting below is read
// back through a "> 0" guard, so a zero — or a negative that Atoi happily
// parses — leaves each host on its own startup value while the page says
// otherwise.
func TestEnvValidationRejectsValuesTheFleetWouldIgnore(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		wantErr    bool
	}{
		{"CRQ_AUTOREVIEW_MAX_SCAN", "-1", true},
		{"CRQ_AUTOREVIEW_MAX_SCAN", "0", true},
		{"CRQ_AUTOREVIEW_MAX_SCAN", "200", false},
		{"CRQ_WATCH_INTERVAL", "0s", true},
		{"CRQ_WATCH_INTERVAL", "5m", false},
		{"CRQ_INFLIGHT_TIMEOUT", "0s", true},
		{"CRQ_DISPATCH_MAX_ATTEMPTS", "0", true},
		{"CRQ_WEEKLY_LIMIT", "-1", true},
		// Zero is a stated answer for these two — no pacing, no settling — so
		// they stay accepted. A blanket "positive only" would take a documented
		// setting away.
		{"CRQ_MIN_INTERVAL", "0s", false},
		{"CRQ_SETTLE", "0s", false},
		{"CRQ_WEEKLY_LIMIT", "0", false},
		{"CRQ_TZ", "Europe/Oslo", false},
		{"CRQ_TZ", "Local", true},
		{"CRQ_TZ", "Europe/Not_A_Real_Place", true},
		// Empty is "unset here", which every key allows.
		{"CRQ_WATCH_INTERVAL", "", false},
	} {
		err := validateEnvValue(tc.key, tc.value)
		if tc.wantErr && err == nil {
			t.Errorf("%s=%q was accepted; the fleet would ignore it", tc.key, tc.value)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s=%q was refused: %v", tc.key, tc.value, err)
		}
	}
}

func TestAdoptEnvSkipsInvalidValues(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_INFLIGHT_TIMEOUT": "not-a-duration",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	adopted, err := svc.AdoptEnv(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := findAdopted(adopted, "CRQ_INFLIGHT_TIMEOUT")
	if !ok || !strings.Contains(got.Skipped, "invalid") {
		t.Fatalf("invalid setting report = %+v, want an explicit skip", got)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := st.Fleet.Env["CRQ_INFLIGHT_TIMEOUT"]; recorded {
		t.Fatal("invalid environment value was persisted into fleet state")
	}
}

// Adopting is a report as much as a write, and the CAS behind it deliberately
// leaves an existing record alone. Saying "adopted" for a value shared state
// never took is the one way this command can mislead: an operator reads their
// number back and believes the fleet now runs on it.
func TestAdoptEnvReportsWhatStateActuallyTook(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
		"CRQ_MIN_INTERVAL": "90s", "CRQ_SETTLE": "45s",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// The fleet has already ruled on both, in each of the two places a setting
	// can live: the typed field and the generic map.
	if _, err := svc.SetEnv(ctx, "CRQ_MIN_INTERVAL", "5m", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnv(ctx, "CRQ_SETTLE", "2m", false); err != nil {
		t.Fatal(err)
	}

	adopted, err := svc.AdoptEnv(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CRQ_MIN_INTERVAL", "CRQ_SETTLE"} {
		got, ok := findAdopted(adopted, key)
		if !ok {
			t.Fatalf("%s missing from the report", key)
		}
		if got.Skipped == "" {
			t.Errorf("%s reported as adopted, but state kept its own value", key)
		}
	}
	// And it really did keep them.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.MinInterval != "5m0s" || st.Fleet.Env["CRQ_SETTLE"] != "2m" {
		t.Errorf("fleet = %q / %q, want the recorded values untouched",
			st.Fleet.MinInterval, st.Fleet.Env["CRQ_SETTLE"])
	}
}

func TestAdoptEnvSkipsMalformedValues(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
		"CRQ_MIN_INTERVAL": "not-a-duration",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	adopted, err := svc.AdoptEnv(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	setting, ok := findAdopted(adopted, "CRQ_MIN_INTERVAL")
	if !ok || !strings.Contains(setting.Skipped, "invalid") {
		t.Fatalf("adoption = %+v, want malformed interval skipped", setting)
	}
}

func TestFleetViewNamesGenericFleetValuesAsFleet(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	st := State{}
	st.Fleet.Env = map[string]string{"CRQ_MIN_INTERVAL": "2m"}

	if got := svc.fleetViewOf(st).Sources["min_interval"]; got != "fleet" {
		t.Fatalf("min interval source = %q, want generic fleet record", got)
	}
}

func TestAdoptEnvResolvesRequiredBotsAgainstTheFleetPrimary(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_HOST":          "testhost",
		"CRQ_COBOTS":        "",
		"CRQ_REQUIRED_BOTS": "replacement-reviewer[bot]",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		fd := st.Fleet
		fd.Env = map[string]string{"CRQ_BOT": "replacement-reviewer[bot]"}
		st.SetFleetDefaults(fd, "other-host", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.AdoptEnv(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Fleet.Required, ","); got != "replacement-reviewer[bot]" {
		t.Fatalf("required = %q, want the fleet-effective primary", got)
	}
}

func TestAdoptEnvAppliesPrimaryBeforeRequiredBotsInOneWrite(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_HOST":          "testhost",
		"CRQ_COBOTS":        "",
		"CRQ_BOT":           "replacement-reviewer[bot]",
		"CRQ_REQUIRED_BOTS": "replacement-reviewer[bot]",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.AdoptEnv(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.Env["CRQ_BOT"] != "replacement-reviewer[bot]" {
		t.Fatalf("primary = %q, want replacement reviewer", st.Fleet.Env["CRQ_BOT"])
	}
	if got := strings.Join(st.Fleet.Required, ","); got != "replacement-reviewer[bot]" {
		t.Fatalf("required = %q, want newly adopted primary", got)
	}
}

func TestAdoptEnvIncludesPerBotRequiredSettings(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost",
		"CRQ_COBOTS": "", "CRQ_COBOT_BUGBOT_REQUIRED": "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.AdoptEnv(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Fleet.SetRequired || !hasLogin(st.Fleet.Required, "cursor[bot]") {
		t.Fatalf("fleet required = %v (set=%v), want the compatibility alias folded into the typed required set",
			st.Fleet.Required, st.Fleet.SetRequired)
	}
	if _, ok := st.Fleet.Env["CRQ_COBOT_BUGBOT_REQUIRED"]; ok {
		t.Fatalf("fleet env = %+v, want no shadowed per-bot required setting", st.Fleet.Env)
	}
	neutral, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "other-host", "CRQ_COBOTS": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	effective := neutral.WithFleet(st.Fleet)
	if !hasLogin(effective.RequiredBots, "cursor[bot]") {
		t.Fatalf("required reviewers = %v, want adopted bugbot requirement on another host", effective.RequiredBots)
	}
}

func TestAdoptEnvReopensCompletedRoundsForNewReviewers(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_HOST":          "testhost",
		"CRQ_REPOS":         "o/r",
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai,codex",
		"CRQ_MIN_INTERVAL":  "0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head := "o/r", 7, "dddddddd4"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 14)

	if _, err := svc.AdoptEnv(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want adoption of a required reviewer to requeue it", round)
	}
}

func findAdopted(list []AdoptedSetting, key string) (AdoptedSetting, bool) {
	for _, a := range list {
		if a.Key == key {
			return a, true
		}
	}
	return AdoptedSetting{}, false
}
