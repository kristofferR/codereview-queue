package crq

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type retryWorkClaimStore struct {
	StateStore
	cfg Config
	now time.Time
}

type cancelingWorkClaimStore struct {
	StateStore
	started chan struct{}
	once    sync.Once
}

type delayedWorkClaimStore struct {
	StateStore
	afterFirstUpdate func()
	once             sync.Once
}

type throttledWorkClaimStore struct {
	StateStore
	err     error
	updated chan struct{}
	once    sync.Once
}

func (s *cancelingWorkClaimStore) Update(ctx context.Context, _ func(*State) error) (State, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return State{}, ctx.Err()
}

func (s *delayedWorkClaimStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	st, err := s.StateStore.Update(ctx, mutate)
	if err == nil {
		s.once.Do(s.afterFirstUpdate)
	}
	return st, err
}

func (s *throttledWorkClaimStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	if s.err != nil {
		err := s.err
		s.err = nil
		return State{}, err
	}
	st, err := s.StateStore.Update(ctx, mutate)
	if err == nil && s.updated != nil {
		s.once.Do(func() { close(s.updated) })
	}
	return st, err
}

func (s retryWorkClaimStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(s.cfg)
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(s.cfg)
	second.SetWorkClaim("owner/repo", 18, WorkClaim{
		Owner: "session-b", By: "linux:other", ClaimedAt: s.now,
		ExpiresAt: s.now.Add(WorkClaimTTL),
	})
	if err := mutate(&second); err != nil && !errors.Is(err, ErrNoChange) {
		return State{}, err
	}
	return second, nil
}

func workClaimService(t *testing.T, store StateStore, cfg Config, owner, by string, now time.Time) *Service {
	t.Helper()
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	svc.workOwnerFn = func() (string, string) { return owner, by }
	return svc
}

func TestInteractiveClaimAndAutofixDispatchAreMutuallyExclusive(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo, pr, head := "owner/repo", 12, "abcdef123"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	claimed, err := manual.claimInteractiveWork(ctx, repo, pr)
	if err != nil || !claimed.acquired {
		t.Fatalf("interactive claim = %+v, %v", claimed, err)
	}

	autofix := NewService(cfg, newFakeGitHub(), store, nil)
	ok, why, byDesign := autofix.claimDispatch(ctx,
		NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}, "dispatch", 3)
	if ok || !byDesign || !strings.Contains(why, "interactive work") {
		t.Fatalf("dispatch = ok %t, byDesign %t, reason %q", ok, byDesign, why)
	}
}

func TestInteractiveClaimWaitsWhenAutofixWonTheRace(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo, pr, head := "owner/repo", 13, "abcdef124"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	autofix := NewService(cfg, newFakeGitHub(), store, nil)
	ok, why, _ := autofix.claimDispatch(ctx,
		NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}, "dispatch", 3)
	if !ok {
		t.Fatalf("autofix claim failed: %s", why)
	}

	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	claimed, err := manual.claimInteractiveWork(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.acquired || !strings.Contains(claimed.reason, "autofix") {
		t.Fatalf("interactive claim = %+v, want autofix conflict", claimed)
	}
}

func TestInteractiveClaimReportsRemainingAutofixLease(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	now := base
	repo, pr, head := "owner/repo", 15, "abcdef125"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	autofix := NewService(cfg, newFakeGitHub(), store, nil)
	autofix.now = func() time.Time { return now }
	if ok, why, _ := autofix.claimDispatch(ctx,
		NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}, "dispatch", 3); !ok {
		t.Fatalf("autofix claim failed: %s", why)
	}
	now = base.Add(8 * time.Minute)
	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	claimed, err := manual.claimInteractiveWork(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.acquired || !claimed.until.Equal(base.Add(DispatchTTL)) {
		t.Fatalf("interactive claim = %+v, want conflict until %s", claimed, base.Add(DispatchTTL))
	}
}

func TestAutofixSessionMayUseNextUnderItsOwnDispatchClaim(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo, pr, head := "owner/repo", 16, "abcdef125"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	daemon := NewService(cfg, newFakeGitHub(), store, nil)
	if ok, why, _ := daemon.claimDispatch(ctx,
		NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}, "dispatch-token", 3); !ok {
		t.Fatalf("autofix claim failed: %s", why)
	}

	session := workClaimService(t, store, cfg, "session-a", "mac:autofix", now)
	session.dispatchTokenFn = func() string { return "dispatch-token" }
	claimed, err := session.claimInteractiveWork(ctx, repo, pr)
	if err != nil || !claimed.acquired {
		t.Fatalf("own dispatch claim = %+v, %v", claimed, err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.WorkClaim(repo, pr, now); ok {
		t.Fatal("autofix session created a second interactive claim")
	}
}

func TestInteractiveClaimRenewsForItsOwnerAndBlocksAnother(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	first := workClaimService(t, store, cfg, "session-a", "mac:first", base)
	if claim, err := first.claimInteractiveWork(ctx, "owner/repo", 14); err != nil || !claim.acquired {
		t.Fatalf("first claim = %+v, %v", claim, err)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	first.now = func() time.Time { return base.Add(time.Hour) }
	kept, err := first.claimInteractiveWork(ctx, "owner/repo", 14)
	if err != nil || !kept.acquired || !kept.until.Equal(base.Add(WorkClaimTTL)) {
		t.Fatalf("claim before renewal window = %+v, %v", kept, err)
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev != before.Rev {
		t.Fatalf("early renewal changed state revision %d -> %d", before.Rev, after.Rev)
	}

	renewAt := base.Add(WorkClaimTTL - workClaimRenewalInterval + time.Second)
	first.now = func() time.Time { return renewAt }
	renewed, err := first.claimInteractiveWork(ctx, "owner/repo", 14)
	if err != nil || !renewed.acquired || !renewed.until.Equal(renewAt.Add(WorkClaimTTL)) {
		t.Fatalf("renewed claim = %+v, %v", renewed, err)
	}

	other := workClaimService(t, store, cfg, "session-b", "linux:other", renewAt)
	blocked, err := other.claimInteractiveWork(ctx, "owner/repo", 14)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.acquired || !strings.Contains(blocked.reason, "mac:first") {
		t.Fatalf("other owner = %+v, want current owner conflict", blocked)
	}
}

func TestInteractiveClaimRefreshesBeforeReturningWork(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := workClaimService(t, store, cfg, "session-a", "mac:feature", base)
	if claim, err := svc.claimInteractiveWork(ctx, "owner/repo", 16); err != nil || !claim.acquired {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := base.Add(time.Hour)
	svc.now = func() time.Time { return now }
	renewed, err := svc.refreshInteractiveWork(ctx, "owner/repo", 16)
	if err != nil || !renewed.acquired || !renewed.until.Equal(now.Add(WorkClaimTTL)) {
		t.Fatalf("refreshed claim = %+v, %v", renewed, err)
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev == before.Rev {
		t.Fatal("refresh before returning work did not update the lease")
	}
}

func TestInteractiveClaimRefreshesLeaseAfterDelayedUpdate(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	store := &delayedWorkClaimStore{StateStore: NewMemoryStore(cfg)}
	store.afterFirstUpdate = func() { now = base.Add(WorkClaimTTL + time.Minute) }
	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", base)
	manual.now = func() time.Time { return now }

	claim, err := manual.claimInteractiveWork(ctx, "owner/repo", 21)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.acquired || !claim.until.Equal(now.Add(WorkClaimTTL)) {
		t.Fatalf("claim after delayed update = %+v, want expiry %s", claim, now.Add(WorkClaimTTL))
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := st.WorkClaim("owner/repo", 21, now)
	if !ok || persisted.Owner != "session-a" || !persisted.ExpiresAt.Equal(claim.until) {
		t.Fatalf("persisted claim = %+v, %t", persisted, ok)
	}
}

func TestFirstHeartbeatUsesExistingLeaseExpiry(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	leaseUntil := now.Add(workClaimRenewalInterval + 5*time.Second)
	if got := workClaimFirstHeartbeatDelay(now, leaseUntil); got != 5*time.Second {
		t.Fatalf("first heartbeat delay = %s, want 5s", got)
	}
}

func TestHeartbeatDoesNotRenewBeforeFirstLeaseTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := workClaimService(t, store, cfg, "session-a", "mac:feature", base)
	claim, err := svc.claimInteractiveWork(ctx, "owner/repo", 27)
	if err != nil || !claim.acquired {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now = base.Add(WorkClaimTTL - workClaimRenewalInterval + time.Second)
	svc.now = func() time.Time { return now }
	first := make(chan time.Time)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.heartbeatWorkClaim(ctx, "owner/repo", 27, claim.until, first, nil, errs, cancel)
	}()
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev != before.Rev {
		t.Fatalf("heartbeat renewed before first lease tick: revision %d -> %d", before.Rev, after.Rev)
	}

	first <- now
	deadline := time.After(time.Second)
	for {
		after, _, err = store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.Rev > before.Rev {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat did not renew after first lease tick")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	select {
	case err := <-errs:
		t.Fatalf("heartbeat error: %v", err)
	default:
	}
}

func TestInteractiveReleaseNormalizesRepo(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)

	if claim, err := manual.claimInteractiveWork(ctx, "OWNER/REPO.git", 23); err != nil || !claim.acquired {
		t.Fatalf("interactive claim = %+v, %v", claim, err)
	}
	if err := manual.releaseInteractiveWork(ctx, "OWNER/REPO.git", 23); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.WorkClaim("owner/repo", 23, now); ok {
		t.Fatal("normalized release left the interactive work claim behind")
	}
}

func TestInteractiveClaimDryRunReportsExistingOwner(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	owner := workClaimService(t, store, cfg, "session-a", "mac:first", now)
	if claim, err := owner.claimInteractiveWork(ctx, "owner/repo", 22); err != nil || !claim.acquired {
		t.Fatalf("first claim = %+v, %v", claim, err)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cfg.DryRun = true
	dry := workClaimService(t, store, cfg, "session-b", "linux:other", now)
	claim, err := dry.claimInteractiveWork(ctx, "owner/repo", 22)
	if err != nil {
		t.Fatal(err)
	}
	if claim.acquired || !strings.Contains(claim.reason, "mac:first") {
		t.Fatalf("dry-run claim = %+v, want existing-owner conflict", claim)
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev != before.Rev {
		t.Fatalf("dry-run claim changed state revision %d -> %d", before.Rev, after.Rev)
	}
}

func TestInteractiveClaimDryRunReportsAutofixDispatch(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo, pr, head := "owner/repo", 23, "abcdef126"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)
	autofix := NewService(cfg, newFakeGitHub(), store, nil)
	if ok, why, _ := autofix.claimDispatch(ctx,
		NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}, "dispatch", 3); !ok {
		t.Fatalf("autofix claim failed: %s", why)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cfg.DryRun = true
	dry := workClaimService(t, store, cfg, "session-b", "linux:other", now)
	claim, err := dry.claimInteractiveWork(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if claim.acquired || !strings.Contains(claim.reason, "autofix") {
		t.Fatalf("dry-run claim = %+v, want autofix conflict", claim)
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev != before.Rev {
		t.Fatalf("dry-run claim changed state revision %d -> %d", before.Rev, after.Rev)
	}
}

func TestInteractiveClaimResetsOutcomeOnCASRetry(t *testing.T) {
	now := time.Now().UTC()
	cfg := firingConfig()
	store := retryWorkClaimStore{cfg: cfg, now: now}
	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)

	claim, err := manual.claimInteractiveWork(context.Background(), "owner/repo", 18)
	if err != nil {
		t.Fatal(err)
	}
	if claim.acquired || !strings.Contains(claim.reason, "linux:other") {
		t.Fatalf("claim after retry = %+v, want competing owner conflict", claim)
	}
}

func TestFallbackWorkOwnerUsesCheckoutRoot(t *testing.T) {
	t.Setenv("CRQ_WORK_OWNER", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")
	root := t.TempDir()
	if _, err := gitDir(context.Background(), root, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "internal", "crq")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := firingConfig()
	cfg.Host = "test-host"
	cfg.WorkDir = root
	rootOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner(context.Background())
	cfg.WorkDir = nested
	nestedOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner(context.Background())
	if nestedOwner != rootOwner {
		t.Fatalf("nested owner %q, want checkout owner %q", nestedOwner, rootOwner)
	}
}

func TestFallbackWorkOwnerDoesNotCacheFailedCheckoutProbe(t *testing.T) {
	t.Setenv("CRQ_WORK_OWNER", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")
	root := t.TempDir()
	if _, err := gitDir(context.Background(), root, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "internal", "crq")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := firingConfig()
	cfg.Host = "test-host"
	cfg.WorkDir = nested
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	svc.workClaimOwner(cancelled)
	if svc.workOwnerCache.ready {
		t.Fatal("failed checkout probe was cached")
	}

	owner, _ := svc.workClaimOwner(context.Background())
	cfg.WorkDir = root
	want, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner(context.Background())
	if owner != want {
		t.Fatalf("owner after retry %q, want checkout owner %q", owner, want)
	}
	if !svc.workOwnerCache.ready {
		t.Fatal("successful checkout probe was not cached")
	}
}

func TestConfiguredWorkOwnerIsStableAcrossHosts(t *testing.T) {
	cfg := firingConfig()
	cfg.WorkDir = t.TempDir()
	cfg.WorkOwner = "stable-session"
	cfg.Host = "mac"
	macOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner(context.Background())
	cfg.Host = "linux"
	linuxOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner(context.Background())
	if linuxOwner != macOwner {
		t.Fatalf("owner changed across hosts: %q != %q", linuxOwner, macOwner)
	}
}

func TestHeartbeatIgnoresCancellationDuringNormalShutdown(t *testing.T) {
	cfg := firingConfig()
	store := &cancelingWorkClaimStore{
		StateStore: NewMemoryStore(cfg),
		started:    make(chan struct{}),
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.workOwnerFn = func() (string, string) { return "session-a", "mac:feature" }
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.heartbeatWorkClaim(ctx, "owner/repo", 20, time.Now().Add(WorkClaimTTL), nil, ticks, errs, cancel)
	}()

	ticks <- time.Now()
	<-store.started
	cancel()
	<-done
	select {
	case err := <-errs:
		t.Fatalf("normal shutdown published heartbeat error: %v", err)
	default:
	}
}

func TestHeartbeatRetriesThrottleWhileLeaseIsLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	baseStore := NewMemoryStore(cfg)
	owner := workClaimService(t, baseStore, cfg, "session-a", "mac:feature", base)
	claim, err := owner.claimInteractiveWork(ctx, "owner/repo", 24)
	if err != nil || !claim.acquired {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}

	now = base.Add(WorkClaimTTL - workClaimRenewalInterval + time.Second)
	store := &throttledWorkClaimStore{
		StateStore: baseStore,
		err:        &ghapi.RateLimitError{Kind: "primary"},
		updated:    make(chan struct{}),
	}
	svc := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	svc.now = func() time.Time { return now }
	var slept time.Duration
	svc.sleepFn = func(_ context.Context, delay time.Duration) error {
		slept += delay
		now = now.Add(delay)
		return nil
	}
	ticks := make(chan time.Time, 1)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.heartbeatWorkClaim(ctx, "owner/repo", 24, claim.until, nil, ticks, errs, cancel)
	}()

	ticks <- now
	<-store.updated
	cancel()
	<-done
	if slept != svc.waitTick() {
		t.Fatalf("throttled heartbeat slept %s, want %s", slept, svc.waitTick())
	}
	select {
	case err := <-errs:
		t.Fatalf("recoverable throttle published heartbeat error: %v", err)
	default:
	}
	st, _, err := baseStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := st.WorkClaim("owner/repo", 24, now)
	if !ok || !persisted.ExpiresAt.Equal(now.Add(WorkClaimTTL)) {
		t.Fatalf("renewed claim = %+v, %t", persisted, ok)
	}
}

func TestHeartbeatRetriesTransientFailureWhileLeaseIsLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	baseStore := NewMemoryStore(cfg)
	owner := workClaimService(t, baseStore, cfg, "session-a", "mac:feature", base)
	claim, err := owner.claimInteractiveWork(ctx, "owner/repo", 26)
	if err != nil || !claim.acquired {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}

	now = base.Add(WorkClaimTTL - workClaimRenewalInterval + time.Second)
	store := &throttledWorkClaimStore{
		StateStore: baseStore,
		err:        errors.New("transient state write failure"),
		updated:    make(chan struct{}),
	}
	svc := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	svc.now = func() time.Time { return now }
	var slept time.Duration
	svc.sleepFn = func(_ context.Context, delay time.Duration) error {
		slept += delay
		now = now.Add(delay)
		return nil
	}
	ticks := make(chan time.Time, 1)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.heartbeatWorkClaim(ctx, "owner/repo", 26, claim.until, nil, ticks, errs, cancel)
	}()

	ticks <- now
	<-store.updated
	cancel()
	<-done
	if slept != svc.waitTick() {
		t.Fatalf("transient failure slept %s, want %s", slept, svc.waitTick())
	}
	select {
	case err := <-errs:
		t.Fatalf("recoverable failure published heartbeat error: %v", err)
	default:
	}
}

func TestHeartbeatStopsWhenThrottleOutlivesLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := time.Now().UTC()
	cfg := firingConfig()
	baseStore := NewMemoryStore(cfg)
	owner := workClaimService(t, baseStore, cfg, "session-a", "mac:feature", base)
	claim, err := owner.claimInteractiveWork(ctx, "owner/repo", 25)
	if err != nil || !claim.acquired {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}

	now := base.Add(WorkClaimTTL - workClaimRenewalInterval + time.Second)
	store := &throttledWorkClaimStore{
		StateStore: baseStore,
		err: &ghapi.RateLimitError{
			Kind:  "primary",
			Until: time.Now().Add(2 * workClaimRenewalInterval),
		},
	}
	svc := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	ticks := make(chan time.Time, 1)
	ticks <- now
	errs := make(chan error, 1)
	svc.heartbeatWorkClaim(ctx, "owner/repo", 25, claim.until, nil, ticks, errs, cancel)

	if ctx.Err() == nil {
		t.Fatal("heartbeat left the loop running after renewal became impossible")
	}
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "before lease expires") {
			t.Fatalf("heartbeat error = %v", err)
		}
	default:
		t.Fatal("heartbeat did not publish the renewal failure")
	}
}

func TestInteractiveClaimRefusesLaggingAutofixHost(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	_, err := store.Update(ctx, func(st *State) error {
		st.SetHostReport(HostReport{
			Host: "old-linux", Caps: CapsWorkClaims - 1, Roles: []string{"autofix"},
		}, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	_, err = manual.claimInteractiveWork(ctx, "owner/repo", 15)
	if err == nil || !strings.Contains(err.Error(), "old-linux") {
		t.Fatalf("lagging host error = %v", err)
	}
}

func TestInteractiveClaimRefusesLaggingAutofixHostWhileRepositoryIsDisabled(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	_, err := store.Update(ctx, func(st *State) error {
		st.SetAutofixSwitch("owner/repo", RepoAutofixSwitch{Enabled: false})
		st.SetHostReport(HostReport{
			Host: "old-linux", Caps: CapsWorkClaims - 1, Roles: []string{"autofix"},
		}, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	_, err = manual.claimInteractiveWork(ctx, "owner/repo", 19)
	if err == nil || !strings.Contains(err.Error(), "old-linux") {
		t.Fatalf("lagging host error = %v", err)
	}
}

func TestUnclaimWorkDryRunDoesNotReleaseClaim(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	const (
		repo = "owner/repo"
		pr   = 17
	)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	manual := workClaimService(t, store, cfg, "session-a", "mac:feature", now)
	if claim, err := manual.claimInteractiveWork(ctx, repo, pr); err != nil || !claim.acquired {
		t.Fatalf("interactive claim = %+v, %v", claim, err)
	}

	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = true
	dry := workClaimService(t, store, cfg, "session-b", "linux:other", now)
	result, err := dry.UnclaimWork(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if result.Released {
		t.Fatal("dry-run unclaim reported releasing the claim")
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rev != before.Rev {
		t.Fatalf("dry-run unclaim changed state revision %d -> %d", before.Rev, after.Rev)
	}
	if _, ok := after.WorkClaim(repo, pr, now); !ok {
		t.Fatal("dry-run unclaim removed the interactive claim")
	}
}
