package crq

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blockedReport builds the report from the CAPTURED CLI event rather than
// hand-written strings, so this test cannot keep passing against wording the
// classifier no longer recognises. dialect owns the vocabulary; this only
// consumes it.
func blockedReport(t *testing.T, wait string) PreflightReport {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "dialect", "testdata", "coderabbit", "cli-rate-limit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	report := PreflightReport{Status: "rate_limited"}
	applyPreflightEvent(&report, event)
	if !report.OrgAttributed {
		t.Fatalf("the captured fixture must be organisation-attributed: %+v", report)
	}
	if !IsCLIAccountBlock(report) {
		t.Fatalf("the captured fixture must classify as an account block: %+v", report)
	}
	if wait != "" {
		report.RetryAfter = wait
	}
	return report
}

func cliQuotaService(t *testing.T, now time.Time) (*Service, StateStore) {
	t.Helper()
	cfg := firingConfig()
	cfg.GateRepo = "kristofferR/crq-state"
	cfg.Scope = []string{"kristofferR"}
	cfg.RateLimitFallback = 15 * time.Minute
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	return svc, store
}

// The local CLI spends the same account quota the queue serializes, so a local
// block is evidence about that quota — obtained with no probe comment and no
// GitHub round trip.
func TestRecordCLIQuotaAppliesTheBlock(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "32 minutes"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied {
		t.Fatalf("the block must be recorded: %+v", got)
	}
	st, _, _ := store.Load(context.Background())
	want := now.Add(32 * time.Minute)
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(want) {
		t.Errorf("BlockedUntil = %v, want %s", st.Account.BlockedUntil, want)
	}
	if st.Account.Source != "coderabbit-cli" {
		t.Errorf("Source = %q, want coderabbit-cli", st.Account.Source)
	}
}

// The CLI can be signed in to a different CodeRabbit organisation than the one
// crq queues for. Applying that block would stall reviews for an account that is
// not limited at all, so an unknown or foreign org must be refused — and refused
// with a reason, not silently.
func TestRecordCLIQuotaRefusesAnotherAccount(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	for _, org := range []string{"someone-else", ""} {
		svc, store := cliQuotaService(t, now)
		got, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "32 minutes"), org)
		if err != nil {
			t.Fatal(err)
		}
		if got.Applied {
			t.Errorf("org %q must not apply to this account", org)
		}
		if got.Reason == "" {
			t.Errorf("org %q must be refused with a reason", org)
		}
		st, _, _ := store.Load(context.Background())
		if st.Account.BlockedUntil != nil {
			t.Errorf("org %q left a block behind: %s", org, st.Account.BlockedUntil)
		}
	}
}

// A window read from a PR comment is authoritative about the whole account; a
// local reading may be a narrower limit. Extending is safe, shortening is not.
func TestRecordCLIQuotaNeverShortensAStandingBlock(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)
	longer := now.Add(2 * time.Hour)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.BlockedUntil = &longer
		st.Account.Source = "calibrate"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "5 minutes"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied {
		t.Error("a shorter local window must not replace a longer standing block")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(longer) {
		t.Errorf("BlockedUntil = %v, want the longer standing block %s", st.Account.BlockedUntil, longer)
	}
}

// "Blocked, but I can't tell you for how long" must not read as "not blocked":
// treating an unreadable window as clear is what let the daemon re-fire every
// couple of minutes against a limit measured in tens of minutes.
func TestRecordCLIQuotaFallsBackWhenTheWindowIsUnreadable(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "soon"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied {
		t.Fatalf("an unreadable window must still block: %+v", got)
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(now.Add(15*time.Minute)) {
		t.Errorf("BlockedUntil = %v, want the conservative fallback", st.Account.BlockedUntil)
	}
}

// Anything that is not an account block leaves the shared quota alone.
func TestRecordCLIQuotaIgnoresOtherFailures(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	report := blockedReport(t, "32 minutes")
	report.ErrorType = "auth"
	if got, err := svc.RecordCLIQuota(context.Background(), report, "kristofferR"); err != nil {
		t.Fatal(err)
	} else if got.Applied {
		t.Error("a non-quota failure must not touch the account block")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil != nil {
		t.Errorf("BlockedUntil = %s, want none", st.Account.BlockedUntil)
	}
}

// A block the CLI reported is no longer the block a PR comment produced, so the
// comment identity must not survive it. Left behind, a later edit of that same
// comment is matched as a repeat of the standing block and its window is reused
// instead of read afresh.
func TestRecordCLIQuotaClearsThePRCommentIdentity(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)
	earlier := now.Add(-time.Hour)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.RLCommentID = 4242
		st.Account.RLCommentUpdated = &earlier
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "32 minutes"), "kristofferR"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.RLCommentID != 0 || st.Account.RLCommentUpdated != nil {
		t.Errorf("the pr-comment identity must not outlive a CLI block: id=%d updated=%v",
			st.Account.RLCommentID, st.Account.RLCommentUpdated)
	}
}

// The gate repo's owner is not automatically the account being queued. Accepting
// it let a personal CodeRabbit org stall an unrelated scope.
func TestRecordCLIQuotaIgnoresTheGateRepoOwner(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)
	svc.cfg.GateRepo = "alice/crq-state"
	svc.cfg.Scope = []string{"acme"}

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "32 minutes"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied {
		t.Error("alice's personal limit must not block the acme scope")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil != nil {
		t.Errorf("BlockedUntil = %s, want none", st.Account.BlockedUntil)
	}
}

// An explicit block with an unreadable window must never land at or before now,
// however CRQ_RL_FALLBACK is configured — that reads as "not blocked" and
// re-fires immediately.
func TestRecordCLIQuotaFallbackIsAlwaysInTheFuture(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	for _, configured := range []time.Duration{0, -5 * time.Minute} {
		svc, store := cliQuotaService(t, now)
		svc.cfg.RateLimitFallback = configured

		if _, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "soon"), "kristofferR"); err != nil {
			t.Fatal(err)
		}
		st, _, _ := store.Load(context.Background())
		if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.After(now) {
			t.Errorf("fallback %s produced %v, want a block in the future", configured, st.Account.BlockedUntil)
		}
	}
}

// The CLI distinguishes a limit that belongs to the organisation from one that
// belongs to this user. Sharing a personal limit would stall every repository in
// the scope over one developer's usage.
func TestRecordCLIQuotaRequiresOrgAttribution(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	report := blockedReport(t, "32 minutes")
	report.OrgAttributed = false
	got, err := svc.RecordCLIQuota(context.Background(), report, "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied {
		t.Error("a personal limit must not become a fleet-wide block")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil != nil {
		t.Errorf("BlockedUntil = %s, want none", st.Account.BlockedUntil)
	}
}

// Credentials passed on the command line cannot be reproduced by the identity
// probe, so it would report the executable's stored login instead — possibly a
// different account than the one that hit the limit.
func TestRecordCLIQuotaRefusesWhenCredentialsWerePassedIn(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	report := blockedReport(t, "32 minutes")
	report.CredentialsOverridden = true
	got, err := svc.RecordCLIQuota(context.Background(), report, "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied {
		t.Error("the account behind the block cannot be confirmed, so it must not be shared")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil != nil {
		t.Errorf("BlockedUntil = %s, want none", st.Account.BlockedUntil)
	}
}

// The flag scan has to see both spellings and the =value form, or a run that
// authenticated itself is treated as one that did not.
func TestHasExplicitCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"--api-key", "secret"}, {"--apikey", "secret"}, {"--token", "secret"},
		{"--api-key=secret"}, {"--dir", "x", "--token=y"},
	} {
		if !HasExplicitCredentials(args) {
			t.Errorf("HasExplicitCredentials(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"--light"}, {"--dir", "internal"}} {
		if HasExplicitCredentials(args) {
			t.Errorf("HasExplicitCredentials(%v) = true, want false", args)
		}
	}
}

// RefreshQuota only trusts CheckedAt while no calibration probe is outstanding,
// so a pending marker left beside a fresh CLI block makes the next pump resume the
// older calibration — and a late reply then overwrites the block with a window
// measured before it.
func TestRecordCLIQuotaClearsAPendingCalibration(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)
	asked := now.Add(-2 * time.Minute)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.CalibAskedAt = &asked
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RecordCLIQuota(context.Background(), blockedReport(t, "32 minutes"), "kristofferR"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.CalibAskedAt != nil {
		t.Errorf("a pending calibration must not outlive the block that replaced it: %s", st.Account.CalibAskedAt)
	}
}

// The fallback window is a fleet setting, so the block recorded here has to be
// the one the settings page says is in force. Read from this host's startup
// value, a fleet-lengthened window recorded the host's shorter one instead —
// resuming metered fires early, against the number the dashboard was showing.
func TestRecordCLIQuotaUsesTheFleetFallbackWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	// Built from an env map, because the fleet's generic settings are applied by
	// re-parsing the configuration crq was built from — which is what a host has.
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "kristofferR/crq-state", "CRQ_HOST": "testhost",
		"CRQ_SCOPE": "kristofferR", "CRQ_COBOTS": "", "CRQ_RL_FALLBACK": "15m",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.SetEnv(ctx, "CRQ_RL_FALLBACK", "1h", false); err != nil {
		t.Fatal(err)
	}
	got, rerr := svc.RecordCLIQuota(ctx, blockedReport(t, "soon"), "kristofferR")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !got.Applied {
		t.Fatalf("an unreadable window must still block: %+v", got)
	}
	st, _, _ := store.Load(ctx)
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(now.Add(time.Hour)) {
		t.Errorf("BlockedUntil = %v, want the fleet's hour rather than this host's 15 minutes",
			st.Account.BlockedUntil)
	}
}

func TestRecordCLIQuotaUsesTheFleetResolvedScope(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/crq-state",
		"CRQ_SCOPE": "startup-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_SCOPE": "fleet-account"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	got, err := svc.RecordCLIQuota(ctx, blockedReport(t, "32 minutes"), "fleet-account")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied {
		t.Fatalf("fleet account's block was rejected: %+v", got)
	}
	st, _, _ := store.Load(ctx)
	if st.Account.Scope != "fleet-account" {
		t.Fatalf("recorded scope = %q, want fleet-account", st.Account.Scope)
	}
}

func TestRecordCLIQuotaDropsABlockWhenFleetAccountChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/crq-state",
		"CRQ_SCOPE": "startup-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := NewMemoryStore(cfg)
	if _, err := base.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_SCOPE": "fleet-account"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &beforeUpdateStore{StateStore: base}
	store.before = func() {
		_, updateErr := base.Update(ctx, func(st *State) error {
			fd := st.Fleet
			fd.Env = map[string]string{"CRQ_SCOPE": "replacement-account"}
			st.SetFleetDefaults(fd, "other-host", now)
			return nil
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	got, err := svc.RecordCLIQuota(ctx, blockedReport(t, "32 minutes"), "fleet-account")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied || got.Reason == "" {
		t.Fatalf("block for the replaced account was not refused: %+v", got)
	}
	st, _, _ := base.Load(ctx)
	if st.Account.BlockedUntil != nil {
		t.Fatalf("block for the prior fleet account was committed: %+v", st.Account)
	}
}
