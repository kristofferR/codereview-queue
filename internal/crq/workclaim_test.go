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

func (s *cancelingWorkClaimStore) Update(ctx context.Context, _ func(*State) error) (State, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return State{}, ctx.Err()
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

	first.now = func() time.Time { return base.Add(time.Hour) }
	renewed, err := first.claimInteractiveWork(ctx, "owner/repo", 14)
	if err != nil || !renewed.acquired || !renewed.until.Equal(base.Add(time.Hour+WorkClaimTTL)) {
		t.Fatalf("renewed claim = %+v, %v", renewed, err)
	}

	other := workClaimService(t, store, cfg, "session-b", "linux:other", base.Add(time.Hour))
	blocked, err := other.claimInteractiveWork(ctx, "owner/repo", 14)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.acquired || !strings.Contains(blocked.reason, "mac:first") {
		t.Fatalf("other owner = %+v, want current owner conflict", blocked)
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
	rootOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner()
	cfg.WorkDir = nested
	nestedOwner, _ := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).workClaimOwner()
	if nestedOwner != rootOwner {
		t.Fatalf("nested owner %q, want checkout owner %q", nestedOwner, rootOwner)
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
		svc.heartbeatWorkClaim(ctx, "owner/repo", 20, ticks, errs, cancel)
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
