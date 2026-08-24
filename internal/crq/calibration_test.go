package crq

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

type beforeUpdateStore struct {
	StateStore
	before func()
	fired  bool
}

func (s *beforeUpdateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	if s.before != nil && !s.fired {
		s.fired = true
		s.before()
	}
	return s.StateStore.Update(ctx, mutate)
}

func TestRefreshQuotaUsesFleetResolvedPrimary(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "o/state",
		"CRQ_CAL_PR":        "77",
		"CRQ_SCOPE":         "startup-owner",
		"CRQ_CALIBRATE_TTL": "1m",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	gh := newFakeGitHub()
	reply := ghapi.IssueComment{
		Body:      "auto-generated reply by CodeRabbit\nYou have 3 reviews remaining.",
		CreatedAt: now.Add(-10 * time.Second),
		UpdatedAt: now.Add(-10 * time.Second),
	}
	reply.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey(cfg.GateRepo, cfg.CalibrationPR)] = []ghapi.IssueComment{reply}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{
			"CRQ_BOT":   "chatgpt-codex-connector[bot]",
			"CRQ_SCOPE": "fleet-owner",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Remaining == nil || *got.Account.Remaining != 3 {
		t.Fatalf("remaining = %v, want the fleet primary's calibration reply", got.Account.Remaining)
	}
	if got.Account.Scope != "fleet-owner" {
		t.Fatalf("scope = %q, want the fleet-resolved scope", got.Account.Scope)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("a matching fleet-primary reply should not post a new probe: %v", gh.posted)
	}
}

func TestRefreshQuotaDryRunHasNoSideEffects(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CalibrationPR = 77
	cfg.DryRun = true
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("dry-run refresh posted a calibration probe: %v", gh.posted)
	}
	if got.Account.CheckedAt != nil || got.Account.CalibAskedAt != nil {
		t.Fatalf("dry-run refresh mutated quota state: %+v", got.Account)
	}
}

func TestRefreshQuotaDropsAReadingWhenFleetPolicyChanges(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":          "o/state",
		"CRQ_CAL_PR":        "77",
		"CRQ_CALIBRATE_TTL": "10m",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	gh := newFakeGitHub()
	reply := ghapi.IssueComment{
		Body:      "auto-generated reply by CodeRabbit\nYou have 3 reviews remaining.",
		CreatedAt: now.Add(-5 * time.Minute),
		UpdatedAt: now.Add(-5 * time.Minute),
	}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey(cfg.GateRepo, cfg.CalibrationPR)] = []ghapi.IssueComment{reply}
	base := NewMemoryStore(cfg)
	store := &beforeUpdateStore{StateStore: base}
	store.before = func() {
		_, updateErr := base.Update(ctx, func(st *State) error {
			fd := st.Fleet
			fd.Env = map[string]string{"CRQ_CALIBRATE_TTL": "1m"}
			st.SetFleetDefaults(fd, "other-host", now)
			return nil
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Remaining != nil || got.Account.CheckedAt != nil {
		t.Fatalf("stale in-flight calibration was committed after the TTL changed: %+v", got.Account)
	}
}

// The calibration probe is the one writer that replaced the whole quota, so it
// was also the one that could SHORTEN a standing block. That matters more now
// that a PR's own rate-limit notice records one: a probe whose reply carries no
// parseable reset would erase the window CodeRabbit had just stated, and Pump
// would fire inside it.
func TestCalibrationNeverShortensAStandingBlock(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CalibrationPR = 77
	cfg.GateRepo = "o/state"
	cfg.CalibrationTTL = time.Minute
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	// A calibration reply that says nothing about a reset — the parse miss.
	reply := ghapi.IssueComment{ID: 5, Body: "auto-generated reply by CodeRabbit\nnothing parseable here",
		CreatedAt: now.Add(-10 * time.Second), UpdatedAt: now.Add(-10 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{reply}

	// A PR's own notice already told crq the account is blocked for 40 minutes.
	until := now.Add(40 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &until
		st.Account.RLCommentID = 900
		u := now.Add(-time.Minute)
		st.Account.RLCommentUpdated = &u
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.BlockedUntil == nil {
		t.Fatal("the probe erased a block CodeRabbit had stated; Pump would fire inside the window")
	}
	if !got.Account.BlockedUntil.Equal(until) {
		t.Errorf("blocked until %s, want the standing %s", got.Account.BlockedUntil, until)
	}

	// A LONGER window from the probe is new information and still wins.
	longer := now.Add(2 * time.Hour)
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{{
		ID: 6, Body: "auto-generated reply by CodeRabbit\n> **Next review available in:** **120 minutes**",
		CreatedAt: now, UpdatedAt: now,
		User: reply.User,
	}}
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.CheckedAt = nil // force a re-read
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.BlockedUntil == nil || got.Account.BlockedUntil.Before(longer.Add(-2*time.Minute)) {
		t.Errorf("blocked until %v, want the probe's longer window near %s", got.Account.BlockedUntil, longer)
	}
}

func TestCalibrationPreservesRollingFireHistory(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CalibrationPR = 77
	cfg.GateRepo = "o/state"
	cfg.CalibrationTTL = time.Minute
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	reply := ghapi.IssueComment{ID: 5, Body: "auto-generated reply by CodeRabbit\nYou have 3 reviews remaining.",
		CreatedAt: now.Add(-10 * time.Second), UpdatedAt: now.Add(-10 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{reply}
	fired := now.Add(-time.Hour)
	coverage := now.Add(-24 * time.Hour)
	var carried AccountQuota
	if err := json.Unmarshal([]byte(`{"future_quota_policy":{"mode":"audit"}}`), &carried); err != nil {
		t.Fatal(err)
	}
	carried.Fires = []time.Time{fired}
	carried.FiresFrom = &coverage
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account = carried
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Account.Fires) != 1 || !got.Account.Fires[0].Equal(fired) {
		t.Fatalf("fires = %v, want calibration to preserve the rolling usage log", got.Account.Fires)
	}
	if got.Account.FiresFrom == nil || !got.Account.FiresFrom.Equal(coverage) {
		t.Fatalf("fires_from = %v, want calibration to preserve coverage", got.Account.FiresFrom)
	}
	raw, err := json.Marshal(got.Account)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["future_quota_policy"]; !ok {
		t.Fatalf("calibration dropped a newer binary's account member: %s", raw)
	}
}

// Not shortening a block must not become refusing to end one. A reply that
// states reviews are left is the account reporting itself available — keeping
// the old window over it leaves state saying "reviews remaining" and "blocked"
// at once, and every metered round waits out a window the bot has contradicted.
func TestCalibrationClearsABlockTheReplyContradicts(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CalibrationPR = 77
	cfg.GateRepo = "o/state"
	cfg.CalibrationTTL = time.Minute
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	reply := ghapi.IssueComment{ID: 5, Body: "auto-generated reply by CodeRabbit\nYou have 3 reviews remaining.",
		CreatedAt: now.Add(-10 * time.Second), UpdatedAt: now.Add(-10 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{reply}

	until := now.Add(40 * time.Minute)
	noticeUpdated := now.Add(-time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &until
		st.Account.RLCommentID = 900
		st.Account.RLCommentUpdated = &noticeUpdated
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.BlockedUntil != nil {
		t.Errorf("blocked until %s, want the stale window dropped: the probe read 3 reviews remaining",
			got.Account.BlockedUntil)
	}
	if got.Account.RLCommentID != 900 || got.Account.RLCommentUpdated == nil ||
		!got.Account.RLCommentUpdated.Equal(noticeUpdated) {
		t.Errorf("conclusive calibration lost the spent-notice watermark: id=%d updated=%v",
			got.Account.RLCommentID, got.Account.RLCommentUpdated)
	}
}
