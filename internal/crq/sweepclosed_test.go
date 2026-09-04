package crq

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// A merged pull request must leave the queue even when the round in front of it
// cannot fire.
//
// The pump examines NextEligible — the FRONT — and nothing else, so an
// account-blocked round there hid four merged PRs in the rendered queue for an
// afternoon: every pump reported the blocked round again rather than looking
// past it. The sweep is what reaches the rest.
func TestClosedRoundsLeaveTheQueueBehindABlockedOne(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	now := time.Now().UTC()

	// The front: open, and blocked by the account quota.
	var front ghapi.Pull
	front.State, front.Number, front.Head.SHA = "open", 1, "aaaaaaaa1"
	gh.pulls[fakeKey("owner/front", 1)] = front
	// Behind it: merged, so its round is dead work.
	var merged ghapi.Pull
	merged.State, merged.Number, merged.Head.SHA = "closed", 2, "bbbbbbbb1"
	gh.pulls[fakeKey("owner/behind", 2)] = merged

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/front", 1, "aaaaaaaa1", PhaseQueued, now.Add(-time.Hour), 0)
	seedRound(t, store, cfg, "owner/behind", 2, "bbbbbbbb1", PhaseQueued, now.Add(-time.Minute), 0)
	blocked := now.Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blocked
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// One pump is enough: the sweep runs before the front is chosen.
	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Abandoning archives the round, so it leaves Rounds entirely; an entry that
	// is still there must at least not be active.
	if r := st.Round("owner/behind", 2); r != nil && r.Active() {
		t.Errorf("the merged round is still in the queue: %+v", r)
	}
	if r := st.Round("owner/front", 1); r == nil || !r.Active() {
		t.Errorf("the blocked round was dropped too: %+v", r)
	}
}

// The pass already knows which pull requests are open — it just listed them —
// so every round waiting for one that is not in that list can go at once.
//
// The per-pump sweep inspects ONE candidate, and its rotation cursor lives in
// memory, so a fresh process always re-inspects the same one. A merged PR
// therefore sat in the rendered queue behind rounds that kept winning the draw.
func TestWatchRetiresEveryClosedRoundItCanSee(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	gh := newFakeGitHub()
	var live ghapi.Pull
	live.State, live.Number, live.Head.SHA = "open", 1, "aaaaaaaa1"
	gh.pulls[fakeKey(repo, 1)] = live

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, 1, "aaaaaaaa1", PhaseQueued, now, 0)
	// Two merged PRs, neither of which ListPulls will return.
	seedRound(t, store, cfg, repo, 2, "bbbbbbbb1", PhaseQueued, now, 0)
	seedRound(t, store, cfg, repo, 3, "cccccccc1", PhaseQueued, now, 0)
	gh.pulls[fakeKey(repo, 2)] = ghapi.Pull{State: "closed", Merged: true}
	gh.pulls[fakeKey(repo, 3)] = ghapi.Pull{State: "closed", Merged: true}

	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), nil); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int{2, 3} {
		if r := st.Round(repo, pr); r != nil && r.Active() {
			t.Errorf("#%d is still in the queue after its PR closed: %+v", pr, r)
		}
	}
	if r := st.Round(repo, 1); r == nil || !r.Active() {
		t.Errorf("the open PR's round was retired too: %+v", r)
	}
}

func TestRetireClosedRoundsBoundsHistoricalPullReads(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	for pr := 1; pr <= 3; pr++ {
		seedRound(t, store, cfg, repo, pr, "abcdef123", PhaseCompleted, now, int64(pr))
		gh.pulls[fakeKey(repo, pr)] = ghapi.Pull{State: "closed"}
	}

	if err := svc.retireClosedRounds(ctx, repo, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for pr := 1; pr <= 3; pr++ {
		reads += gh.pullReads[fakeKey(repo, pr)]
	}
	if reads != 1 {
		t.Fatalf("historical pull reads in one pass = %d, want 1", reads)
	}

	for range 2 {
		if err := svc.retireClosedRounds(ctx, repo, map[int]bool{}); err != nil {
			t.Fatal(err)
		}
	}
	for pr := 1; pr <= 3; pr++ {
		if got := gh.pullReads[fakeKey(repo, pr)]; got != 1 {
			t.Errorf("PR %d reads after one rotation = %d, want 1", pr, got)
		}
	}
}

func TestRetireClosedRoundsIncludesIndexOnlyMergedPRs(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	const pr = 4
	key := QueueKey(repo, pr)
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey(repo, pr)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		// This is the state left when an older rolling-upgrade peer resurrects
		// evidence from a merged archive entry and that entry is later evicted.
		st.CoActivity = map[string]map[string]time.Time{}
		st.CoAnswers = map[string]map[string]time.Time{}
		st.CoActivity[key] = map[string]time.Time{"cursor": now}
		st.CoAnswers[key] = map[string]time.Time{"cursor": now}
		st.ReviewedHeads[key] = []string{"abcdef123"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.retireClosedRounds(ctx, repo, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.CoActivity[key]; ok {
		t.Fatal("merged PR kept its index-only activity evidence")
	}
	if _, ok := st.CoAnswers[key]; ok {
		t.Fatal("merged PR kept its index-only answer evidence")
	}
	if _, ok := st.ReviewedHeads[key]; ok {
		t.Fatal("merged PR kept its index-only review ledger")
	}
	if got := gh.pullReads[fakeKey(repo, pr)]; got != 1 {
		t.Fatalf("index-only PR reads = %d, want 1", got)
	}
}

func TestAutoReviewRetiresMergedEvidenceWithoutWatcher(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	const pr = 4
	key := QueueKey(repo, pr)
	cfg := firingConfig()
	cfg.Scope = []string{"owner"}
	cfg.LeaderTTL = time.Minute
	cfg.AutoReviewMaxScan = 10
	gh := newFakeGitHub()
	gh.pulls[fakeKey(repo, pr)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, "abcdef123", PhaseCompleted, now, 0)
	if _, err := store.Update(ctx, func(st *State) error {
		st.CoActivity = map[string]map[string]time.Time{}
		st.CoAnswers = map[string]map[string]time.Time{}
		st.CoActivity[key] = map[string]time.Time{"cursor": now}
		st.CoAnswers[key] = map[string]time.Time{"cursor": now}
		st.ReviewedHeads[key] = []string{"abcdef123"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Round(repo, pr) != nil {
		t.Fatal("autoreview kept the merged PR's completed round")
	}
	if _, ok := st.CoActivity[key]; ok {
		t.Fatal("autoreview kept the merged PR's activity index")
	}
	if _, ok := st.CoAnswers[key]; ok {
		t.Fatal("autoreview kept the merged PR's answer index")
	}
	if _, ok := st.ReviewedHeads[key]; ok {
		t.Fatal("autoreview kept the merged PR's review ledger")
	}
}

func TestRetireClosedRoundsPreservesIndexOnlyClosedPRs(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	const pr = 5
	key := QueueKey(repo, pr)
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey(repo, pr)] = ghapi.Pull{State: "closed"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.CoActivity = map[string]map[string]time.Time{}
		st.CoActivity[key] = map[string]time.Time{"cursor": now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.retireClosedRounds(ctx, repo, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.CoActivity[key]["cursor"]; !got.Equal(now) {
		t.Fatalf("closed PR activity = %v, want %v", got, now)
	}
}

func TestRetireClosedRoundsContinuesAfterUnreadablePR(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pullErrs[fakeKey(repo, 1)] = errors.New("repository unavailable")
	gh.pulls[fakeKey(repo, 2)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, 1, "aaaaaaaaa", PhaseQueued, now, 0)
	seedRound(t, store, cfg, repo, 2, "bbbbbbbbb", PhaseQueued, now, 0)
	if _, err := store.Update(ctx, func(st *State) error {
		st.MarkOnePassReady(repo, 3, "ccccccccc", "basebase1", "test", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := svc.retireClosedRounds(ctx, repo, map[int]bool{})
	if err == nil || !errors.Is(err, gh.pullErrs[fakeKey(repo, 1)]) {
		t.Fatalf("retire error = %v, want the unreadable PR reported", err)
	}
	st, _, loadErr := store.Load(ctx)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st.Round(repo, 1) == nil {
		t.Fatal("unreadable PR was retired without evidence")
	}
	if st.Round(repo, 2) != nil {
		t.Fatal("merged PR after the unreadable one was not retired")
	}
	if progress, ok := st.OnePassProgressFor(repo, 3); !ok || progress.ReadyHead != "" {
		t.Fatalf("closed one-pass hand-off was not invalidated after the pull-read failure: %+v", progress)
	}
}

func TestWatchRotatesPastUnreadableTerminalPR(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	gh := newFakeGitHub()
	gh.pullErrs[fakeKey(repo, 1)] = errors.New("repository unavailable")
	gh.pulls[fakeKey(repo, 2)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, 1, "aaaaaaaaa", PhaseCompleted, now, 0)
	seedRound(t, store, cfg, repo, 2, "bbbbbbbbb", PhaseCompleted, now, 0)

	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), nil); err != nil {
		t.Fatalf("continuous watch stopped at unreadable PR: %v", err)
	}
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), nil); err != nil {
		t.Fatalf("continuous watch stopped before rotating: %v", err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Round(repo, 2) != nil {
		t.Fatal("watch did not rotate to the merged PR after a partial read failure")
	}

	err = svc.watchPass(ctx, WatchOptions{Once: true}, newDispatchPool(0), nil)
	if err == nil || !strings.Contains(err.Error(), repo+"#1") {
		t.Fatalf("one-shot watch error = %v, want unreadable terminal PR", err)
	}
}

// The per-head attempt budget stops a fix that keeps not working from looping.
// A session the WATCHER stopped never got to fail, so it must not spend one:
// two redeploys spent two of one head's three attempts, and a third would have
// left the pull request unfixable at that commit while `crq next` kept asking
// for a fix.
func TestAStoppedSessionKeepsItsAttempt(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 4, sha
	gh.pulls[fakeKey(repo, 4)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 4, sha, PhaseQueued, time.Now().UTC(), 0)

	// A session that outlives the watcher, which is cancelled under it.
	script := filepath.Join(t.TempDir(), "session.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	pool := newDispatchPool(0)
	report := NextReport{Repo: repo, PR: 4, Head: sha, Action: "fix"}
	ok, why, _ := svc.startDispatchResult(ctx, WatchOptions{
		Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3,
	}, pool, report)
	if !ok {
		t.Fatalf("dispatch did not start: %s", why)
	}
	time.Sleep(150 * time.Millisecond)
	stop()
	pool.wait()

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 4)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("no claim recorded: %+v", round)
	}
	if round.Dispatch.Attempts != 0 {
		t.Errorf("attempts = %d, want the attempt returned when the watcher stopped the session",
			round.Dispatch.Attempts)
	}
}
