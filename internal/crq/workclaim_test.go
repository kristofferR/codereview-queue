package crq

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
