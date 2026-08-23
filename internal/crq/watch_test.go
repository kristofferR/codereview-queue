package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
	"github.com/kristofferR/coderabbit-queue/internal/workspace"
)

// Dispatch is the one place crq starts something that writes code, so what it
// hands the session has to be right: the PR's own worktree at the head the
// findings are about, and the findings themselves.
func TestWatchDispatchesAFixSessionWithItsContext(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	t.Setenv("GITHUB_TOKEN", "ghp_session_token")
	t.Setenv("GH_TOKEN", "stale_higher_precedence_token")
	t.Setenv(workspace.TokenEnv, "stale_git_token")

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	if realRoot, err := filepath.EvalSymlinks(cfg.WorkspaceRoot); err != nil {
		t.Fatal(err)
	} else {
		cfg.WorkspaceRoot = realRoot
	}
	cfg.AllowRepos = map[string]bool{repo: true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	var pull ghapi.Pull
	pull.State = "open"
	pull.Number = 4
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 4)] = pull

	// A record of what the session was given, written by the "session" itself.
	record := filepath.Join(t.TempDir(), "record.json")
	script := filepath.Join(t.TempDir(), "session.sh")
	body := "#!/bin/sh\n" +
		"printf '{\"repo\":\"%s\",\"pr\":\"%s\",\"head\":\"%s\",\"cwd\":\"%s\",\"findings\":\"%s\",\"token\":\"%s\",\"github_token\":\"%s\",\"gh_token\":\"%s\",\"git_token\":\"%s\"}' " +
		"\"$CRQ_DISPATCH_REPO\" \"$CRQ_DISPATCH_PR\" \"$CRQ_DISPATCH_HEAD\" \"$(pwd)\" \"$CRQ_DISPATCH_FINDINGS\" \"$CRQ_DISPATCH_TOKEN\" \"$GITHUB_TOKEN\" \"$GH_TOKEN\" \"$CRQ_GIT_TOKEN\" > " + record + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	report := NextReport{Repo: repo, PR: 4, Head: sha, Action: "fix"}
	seedRound(t, store, cfg, repo, 4, sha, PhaseQueued, time.Now().UTC(), 0)

	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{
		Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3,
	}, pool, report)
	if !ok {
		t.Fatalf("dispatch did not run: %s", why)
	}
	pool.wait()

	var got struct {
		Repo        string `json:"repo"`
		PR          string `json:"pr"`
		Head        string `json:"head"`
		Cwd         string `json:"cwd"`
		Findings    string `json:"findings"`
		Token       string `json:"token"`
		GitHubToken string `json:"github_token"`
		GHToken     string `json:"gh_token"`
		GitToken    string `json:"git_token"`
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the session did not run: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Repo != repo || got.PR != "4" || got.Head != sha {
		t.Errorf("session context = %+v, want %s#4@%s", got, repo, sha)
	}
	// It ran in a checkout of the PR, not in whatever directory crq was started
	// from — the entire reason the workspace exists.
	if !strings.HasPrefix(got.Cwd, cfg.WorkspaceRoot) {
		t.Errorf("session ran in %q, want a worktree under %q", got.Cwd, cfg.WorkspaceRoot)
	}
	if got.Findings == "" {
		t.Fatal("the session was given no findings file")
	}
	if got.Token == "" {
		t.Fatal("the session was given no dispatch token for its crq next calls")
	}
	if got.GitHubToken != "ghp_session_token" || got.GHToken != "ghp_session_token" || got.GitToken != "ghp_session_token" {
		t.Fatalf("session credentials = github:%q gh:%q git:%q, want the daemon's current token", got.GitHubToken, got.GHToken, got.GitToken)
	}
	// OUTSIDE the worktree: at the repository root it is an untracked file, and
	// a session following the documented `git add -A` push would commit crq's
	// review payload into the PR.
	if strings.HasPrefix(got.Findings, got.Cwd) {
		t.Errorf("findings at %q are inside the worktree %q", got.Findings, got.Cwd)
	}

	// The claim is released afterwards, so the next round is not blocked by a
	// session that already finished.
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 4)
	if round == nil || round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("claim still held after the session finished: %#v", round.Dispatch)
	}
	if round.Dispatch == nil || round.Dispatch.Attempts != 1 {
		t.Errorf("attempts = %#v, want 1 recorded", round.Dispatch)
	}
	// And the worktree is cleaned up rather than left to accumulate. Empty
	// parent directories are fine; a checkout still holding a repository is not.
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == ".git" {
			t.Errorf("a worktree was left behind at %s", filepath.Dir(path))
		}
		return nil
	})
}

func TestDispatchHeartbeatLosesToWorkClaimAfterExpiry(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	repo, pr, head := "owner/thing", 4, "abcdef123"
	st := DefaultState(firingConfig())
	round := Round{Repo: repo, PR: pr, Head: head, Phase: PhaseQueued}
	if ok, why := round.ClaimDispatch("autofix", "old-token", now, 3); !ok {
		t.Fatalf("claim dispatch: %s", why)
	}
	st.PutRound(round)

	reconnected := now.Add(DispatchTTL + time.Minute)
	st.SetWorkClaim(repo, pr, WorkClaim{
		Owner: "interactive", By: "mac:feature", ClaimedAt: reconnected,
		ExpiresAt: reconnected.Add(WorkClaimTTL),
	})
	updated, taken, gone := refreshDispatch(&st, NextReport{
		Repo: repo, PR: pr, Head: head,
	}, "old-token", reconnected)
	if updated || !taken || gone {
		t.Fatalf("heartbeat = updated %v, taken %v, gone %v; want loss to work claim", updated, taken, gone)
	}
	if got := st.Round(repo, pr).Dispatch.Heartbeat; !got.Equal(now) {
		t.Fatalf("heartbeat restored expired dispatch at %s, want %s", got, now)
	}
}

func TestDispatchHeartbeatStopsAtKnownLeaseExpiry(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	report := NextReport{
		Repo: "owner/thing", PR: 4, Head: "abcdef123",
		dispatchUntil: time.Now().Add(20 * time.Millisecond),
	}
	lost := svc.beatDispatch(ctx, report, "old-token", func() {
		cancel()
		close(stopped)
	})
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("dispatch continued past its locally known lease expiry")
	}
	if !lost() {
		t.Fatal("expired dispatch was not reported as lost")
	}
}

func TestClaimDispatchRefreshesLeaseAfterDelayedUpdate(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	baseStore := NewMemoryStore(cfg)
	repo, pr, head := "owner/thing", 4, "abcdef123"
	seedRound(t, baseStore, cfg, repo, pr, head, PhaseQueued, base, 0)
	store := &delayedWorkClaimStore{StateStore: baseStore}
	store.afterFirstUpdate = func() { now = base.Add(DispatchTTL + time.Minute) }
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}

	ok, why, _, _ := svc.claimDispatchModels(ctx, &report, "dispatch-token", 3)
	if !ok {
		t.Fatalf("claim after delayed update failed: %s", why)
	}
	st, _, err := baseStore.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Dispatch == nil || !round.Dispatch.Heartbeat.Equal(now) {
		t.Fatalf("dispatch after delayed update = %#v, want heartbeat %s", round, now)
	}
	if round.Dispatch.Attempts != 1 {
		t.Fatalf("dispatch attempts = %d, want renewal not a second attempt", round.Dispatch.Attempts)
	}
	if !report.dispatchUntil.Equal(now.Add(DispatchTTL)) {
		t.Fatalf("dispatch expiry = %s, want %s", report.dispatchUntil, now.Add(DispatchTTL))
	}
}

func TestClaimDispatchRenewsLeaseBeforeFirstHeartbeat(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	now := base
	cfg := firingConfig()
	baseStore := NewMemoryStore(cfg)
	repo, pr, head := "owner/thing", 6, "abcdef125"
	seedRound(t, baseStore, cfg, repo, pr, head, PhaseQueued, base, 0)
	store := &delayedWorkClaimStore{StateStore: baseStore}
	store.afterFirstUpdate = func() { now = base.Add(2*DispatchTTL/3 + time.Minute) }
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}

	ok, why, _, _ := svc.claimDispatchModels(ctx, &report, "dispatch-token", 3)
	if !ok {
		t.Fatalf("claim after delayed update failed: %s", why)
	}
	st, _, err := baseStore.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Dispatch == nil || !round.Dispatch.Heartbeat.Equal(now) {
		t.Fatalf("dispatch after delayed update = %#v, want heartbeat %s", round, now)
	}
	if round.Dispatch.Attempts != 1 {
		t.Fatalf("dispatch attempts = %d, want renewal not a second attempt", round.Dispatch.Attempts)
	}
	if !report.dispatchUntil.Equal(now.Add(DispatchTTL)) {
		t.Fatalf("dispatch expiry = %s, want %s", report.dispatchUntil, now.Add(DispatchTTL))
	}
}

func TestStandaloneDispatchKeepsClarificationTerminal(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 5, sha
	gh.pulls[fakeKey(repo, 5)] = pull
	svc := NewService(cfg, gh, store, nil)
	report := NextReport{Repo: repo, PR: 5, Head: sha, Action: "fix"}
	seedRound(t, store, cfg, repo, 5, sha, PhaseQueued, time.Now().UTC(), 0)

	script := filepath.Join(t.TempDir(), "session.sh")
	body := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"" +
		clarificationMarker + " Which behavior should remain?\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	pool := newDispatchPool(0)
	if ok, why := svc.startDispatch(context.Background(), WatchOptions{
		Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3,
	}, pool, report); !ok {
		t.Fatalf("dispatch did not run: %s", why)
	}
	pool.wait()

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 5)
	if round == nil || round.Dispatch == nil ||
		round.Dispatch.Clarification != "Which behavior should remain?" {
		t.Fatalf("clarification marker = %#v, want the question retained on this head", round)
	}
	if ok, why, byDesign := svc.claimDispatch(context.Background(), report, "again", 3); ok ||
		!byDesign || !strings.Contains(why, "Which behavior should remain?") {
		t.Fatalf("repeat dispatch = ok %v byDesign %v reason %q", ok, byDesign, why)
	}
}

func TestProviderOutageUsesFallbackWithoutSpendingAnAttempt(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.AllowRepos = map[string]bool{repo: true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 18, sha
	gh.pulls[fakeKey(repo, 18)] = pull
	store := NewMemoryStore(cfg)
	switchable := &loadSwitchStore{StateStore: store}
	svc := NewService(cfg, gh, switchable, nil)
	if _, err := svc.SetSolver(context.Background(), repo, SolverChange{
		Models: []string{"opus", "sonnet"},
	}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, 18, sha, PhaseQueued, time.Now().UTC(), 0)

	record := filepath.Join(t.TempDir(), "models")
	script := filepath.Join(t.TempDir(), "agent.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$CRQ_FIX_MODEL\" >> " + record + "\n" +
		"if test \"$CRQ_FIX_MODEL\" = opus; then\n" +
		"  printf '%s\\n' '{\"type\":\"result\",\"error\":\"rate_limit\",\"resetsAt\":4102444800}'\n" +
		"  exit 1\n" +
		"fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	report := NextReport{Repo: repo, PR: 18, Head: sha, Action: "fix"}
	opts := WatchOptions{Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 5}

	var claimedModel string
	if ok, why, _, model := svc.claimDispatchModels(context.Background(), &report, "first", 5); !ok {
		t.Fatalf("first claim: %s", why)
	} else if model != "opus" {
		t.Fatalf("first claim returned model %q, want opus", model)
	} else {
		claimedModel = model
	}
	// The selected model belongs to the successful claim. A later state-ref
	// outage must not replace it with this host's startup default.
	switchable.unreadable = true
	if ok, why := svc.dispatchWithStart(context.Background(), opts, report, "first", claimedModel, nil); ok || !strings.Contains(why, "temporarily unavailable") {
		t.Fatalf("provider outage = ok %v reason %q", ok, why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Round(repo, 18).Dispatch.Attempts; got != 0 {
		t.Fatalf("provider outage spent %d attempts, want 0", got)
	}

	if ok, why, _, model := svc.claimDispatchModels(context.Background(), &report, "second", 5); !ok {
		t.Fatalf("fallback claim: %s", why)
	} else if model != "sonnet" {
		t.Fatalf("fallback claim returned model %q, want sonnet", model)
	} else {
		claimedModel = model
	}
	if ok, why := svc.dispatchWithStart(context.Background(), opts, report, "second", claimedModel, nil); !ok {
		t.Fatalf("fallback did not run: %s", why)
	}
	models, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(models)); got != "opus\nsonnet" {
		t.Fatalf("models tried = %q, want ranked fallback", got)
	}
	st, _, err = store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Round(repo, 18).Dispatch.Attempts; got != 1 {
		t.Fatalf("successful fallback left %d attempts, want only its real attempt", got)
	}
}

func TestProviderOutageReleasesAnArchivedDispatchClaim(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	const (
		repo  = "o/repo"
		pr    = 18
		token = "session"
	)
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound(repo, pr, "aaaaaaaa1", now)
		if err != nil {
			return err
		}
		if ok, why := round.ClaimDispatchModels("host", token, now, 5, []string{"opus"}); !ok {
			return errors.New(why)
		}
		st.RememberDispatch(repo, pr, *round.Dispatch)
		st.PutRound(*round)
		_, err = st.Supersede(repo, pr, "bbbbbbbb2", now.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	retryAt := now.Add(time.Hour)
	svc.releaseDispatchUnavailable(ctx, NextReport{Repo: repo, PR: pr}, token, dialect.AgentFailure{
		RetryAt: retryAt, Reason: "provider unavailable",
	})
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ArchivedDispatchHeld(repo, pr, now.Add(2*time.Minute)) {
		t.Fatal("provider outage left the archived claim live")
	}
	for _, round := range st.Archive {
		if round.Repo != repo || round.PR != pr || round.Dispatch == nil {
			continue
		}
		if got := round.Dispatch.UnavailableModels["opus"]; !got.Equal(retryAt) {
			t.Fatalf("archived outage = %s, want %s", got, retryAt)
		}
		if round.Dispatch.Attempts != 0 {
			t.Fatalf("archived outage spent %d attempts, want 0", round.Dispatch.Attempts)
		}
		return
	}
	t.Fatal("no archived dispatch claim found")
}

// Dispatching is the default, so a machine with no fix agent configured must
// still be able to watch: refusing to start would make the default setting
// break the plain command. It observes instead — and says so, because an autofix watcher
// that quietly does nothing is the failure this whole area is about.
func TestWatchObservesWhenNoFixCommandIsConfigured(t *testing.T) {
	cfg := firingConfig()
	cfg.DispatchCommand = nil
	cfg.AllowRepos = map[string]bool{"owner/thing": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 3, "aaaaaaaa1"
	gh.pulls[fakeKey("owner/thing", 3)] = pull
	said := &recordingLogger{}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, said)

	var events []WatchEvent
	err := svc.Watch(context.Background(), WatchOptions{Once: true}, func(e WatchEvent) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("watch refused to observe without a fix command: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("nothing was observed")
	}
	for _, e := range events {
		if e.Dispatched {
			t.Errorf("a session was dispatched with no command configured: %+v", e)
		}
	}
	if !said.contains("observing only") {
		t.Errorf("the watcher did not say it was observing only: %q", said.lines)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report, ok := st.HostReports[cfg.Host]; ok && report.RolesFresh([]string{"autofix"}, time.Now().UTC(), HostReportTTL) {
		t.Fatal("observation-only watcher reported the autofix role")
	}
}

// recordingLogger keeps what the service said, so a test can assert on the one
// line that explains a silent-looking mode.
type recordingLogger struct{ lines []string }

func (r *recordingLogger) Printf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) contains(want string) bool {
	for _, line := range r.lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func TestWatchEmitsAnEventForASkipMarkedPullRequest(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	cfg.SkipMarker = "<!-- crq:skip-autoreview -->"
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Number = 9
	pull.Body = cfg.SkipMarker
	gh.pulls[fakeKey("o/r", 9)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	var events []WatchEvent
	if err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "skipped" || events[0].Reason == "" {
		t.Fatalf("events = %#v, want one explained skip", events)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Round("o/r", 9) != nil {
		t.Fatal("skip-marked PR was passed to the mutating Next oracle")
	}
}

func TestOneShotDispatchPoolWaitsForCapacity(t *testing.T) {
	pool := newDispatchPool(1)
	if ok, why := pool.acquire(); !ok {
		t.Fatal(why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan bool)
	go func() {
		ok, _ := pool.acquireContext(ctx)
		result <- ok
	}()
	select {
	case <-result:
		t.Fatal("one-shot acquisition did not wait for the occupied slot")
	case <-time.After(20 * time.Millisecond):
	}
	pool.release()
	if ok := <-result; !ok {
		t.Fatal("one-shot acquisition did not take capacity when it became available")
	}
	pool.release()

	if ok, why := pool.acquire(); !ok {
		t.Fatal(why)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if ok, why := pool.acquireContext(cancelled); ok || !strings.Contains(why, context.Canceled.Error()) {
		t.Fatalf("cancelled acquisition = ok %v reason %q", ok, why)
	}
	pool.release()
}

// DryRun means crq writes nothing and posts nothing. Claiming shared state and
// running a code-writing command is the largest possible violation of that.
func TestDispatchHonoursDryRun(t *testing.T) {
	cfg := firingConfig()
	cfg.DryRun = true
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	ran := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: dispatchOn(), Command: []string{script}},
		pool, NextReport{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Action: "fix"})
	pool.wait()
	if ok {
		t.Error("a dry run dispatched a session")
	}
	if !strings.Contains(why, "dry run") {
		t.Errorf("reason = %q, want it to say why", why)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Error("the fix command ran under a dry run")
	}
	// Health is a shared CAS write like any other, so a dry run must not record
	// one — let alone raise a dispatcher alarm nothing caused.
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Autofix != nil {
		t.Errorf("a dry run wrote dispatch health: %+v", st.Autofix)
	}
}

func TestWatchClaimsCarriedFeedbackBeforeAdvancingTheQueue(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 12
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", pr, sha
	pull.Base.SHA, pull.Base.Ref = sha, "main"
	gh.pulls[fakeKey(repo, pr)] = pull
	created := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			payload := `{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},` +
				`"nodes":[{"id":"THREAD1","isResolved":false,"isOutdated":false,"path":"a.go","line":1,` +
				`"comments":{"nodes":[{"databaseId":55,"body":"Carried-over finding","url":"http://x","path":"a.go","line":1,` +
				`"createdAt":"` + created + `","author":{"login":"coderabbitai[bot]"},"commit":{"oid":"oldhead123456"}}]}}]}}}}`
			return json.Unmarshal([]byte(payload), out)
		}
		return noForcePush(query, nil, out)
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	script := filepath.Join(t.TempDir(), "session.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.Watch(context.Background(), WatchOptions{
		Repos: []string{repo}, Once: true, Dispatch: dispatchOn(),
		Command: []string{script}, MaxAttempts: 3,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("watch fired a review before claiming the carried-feedback fix: %v", gh.posted)
	}
}

func TestOneShotWatchReportsDispatchFailure(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 13
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", pr, sha
	gh.pulls[fakeKey(repo, pr)] = pull
	created := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			payload := `{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},` +
				`"nodes":[{"id":"THREAD1","isResolved":false,"isOutdated":false,"path":"a.go","line":1,` +
				`"comments":{"nodes":[{"databaseId":55,"body":"Finding","url":"http://x","path":"a.go","line":1,` +
				`"createdAt":"` + created + `","author":{"login":"coderabbitai[bot]"},"commit":{"oid":"oldhead123456"}}]}}]}}}}`
			return json.Unmarshal([]byte(payload), out)
		}
		return noForcePush(query, nil, out)
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	script := filepath.Join(t.TempDir(), "session.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 17\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	err := svc.Watch(context.Background(), WatchOptions{
		Repos: []string{repo}, Once: true, Dispatch: dispatchOn(),
		Command: []string{script}, MaxAttempts: 3,
	}, func(event WatchEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "fix session failed") {
		t.Fatalf("Watch error = %v, want the asynchronous dispatch failure", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want the PR's actual dispatch result", events)
	}
	if events[0].Dispatched || !strings.Contains(events[0].Skipped, "fix session failed") {
		t.Errorf("event = %#v, want dispatched=false with the session failure", events[0])
	}
}

func TestOneShotDispatchReportsImmediateOnePassMergeFailure(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 131
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	mergeable := true
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", pr, sha
	pull.Base.SHA, pull.Base.Ref = sha, "main"
	pull.Mergeable, pull.MergeableState = &mergeable, "clean"
	gh.pulls[fakeKey(repo, pr)] = pull
	mergeErr := errors.New("merge endpoint unavailable")
	gh.mergeErrs[fakeKey(repo, pr)] = mergeErr
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	on, method, one := true, "squash", 1
	if _, err := svc.SetSolver(context.Background(), repo, SolverChange{OnePass: &on, MergeMethod: &method, MaxAttempts: &one}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, sha, PhaseCompleted, time.Now().UTC(), 0)
	report := NextReport{
		Repo: repo, PR: pr, Head: sha, Action: string(engine.ActionFix),
		Findings: []dialect.Finding{{ID: onePassFinalizeSource, Source: onePassFinalizeSource, Commit: sha, Severity: "major"}},
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report.onePassCampaign = st.EffectiveSolver(repo).OnePassCampaign
	token := "one-pass-token"
	if ok, why, _ := svc.claimDispatch(context.Background(), report, token, 1); !ok {
		t.Fatalf("claimDispatch refused: %s", why)
	}

	ok, why := svc.dispatch(context.Background(), WatchOptions{Once: true, Command: []string{"/usr/bin/true"}}, report, token)
	if ok || !strings.Contains(why, mergeErr.Error()) {
		t.Fatalf("one-shot dispatch = (%t, %q), want immediate merge failure", ok, why)
	}
}

func TestOnePassFinalizerBindsTheBaseRefreshedAfterItsSession(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 132
	repoDir := filepath.Join(base, repo)
	initial := originRepo(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "BASE.md"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(context.Background(), repoDir, "add", "BASE.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(context.Background(), repoDir, "commit", "-m", "advance base"); err != nil {
		t.Fatal(err)
	}
	advanced, err := gitDir(context.Background(), repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var before, after ghapi.Pull
	before.State, before.Number, before.Head.SHA = "open", pr, initial
	before.Base.SHA, before.Base.Ref = initial, "main"
	after = before
	after.Head.SHA, after.Base.SHA = advanced, advanced
	key := fakeKey(repo, pr)
	gh.pulls[key] = after
	gh.pullResults = map[string]map[int]ghapi.Pull{key: {1: before}}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	on, one := true, 1
	if _, err := svc.SetSolver(context.Background(), repo, SolverChange{OnePass: &on, MaxAttempts: &one}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, initial, PhaseCompleted, time.Now().UTC(), 0)
	report := NextReport{
		Repo: repo, PR: pr, Head: initial, Action: string(engine.ActionFix),
		Findings: []dialect.Finding{{ID: onePassFinalizeSource, Source: onePassFinalizeSource, Commit: initial, Severity: "major"}},
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report.onePassCampaign = st.EffectiveSolver(repo).OnePassCampaign
	token := "moving-base-token"
	if ok, why, _ := svc.claimDispatch(context.Background(), report, token, 1); !ok {
		t.Fatalf("claimDispatch refused: %s", why)
	}
	script := filepath.Join(t.TempDir(), "finalize.sh")
	body := fmt.Sprintf("#!/bin/sh\nset -eu\ngit merge --ff-only %s\ngit push origin HEAD:refs/pull/%d/head\n", advanced, pr)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, why := svc.dispatch(context.Background(), WatchOptions{
		Command: []string{script},
	}, report, token)
	if !ok {
		t.Fatalf("one-pass dispatch failed: %s", why)
	}
	st, _, err = store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	progress, ok := st.OnePassProgressFor(repo, pr)
	if !ok || progress.ReadyHead != advanced || progress.ReadyBase != advanced {
		t.Fatalf("ready hand-off = %+v, ok=%t; want refreshed base %s", progress, ok, advanced)
	}
}

func TestOnePassFinalizerKeepsSuccessfulHandoffWhenPostSessionPullReadFails(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 133
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", pr, sha
	pull.Base.SHA, pull.Base.Ref = sha, "main"
	key := fakeKey(repo, pr)
	gh.pulls[key] = pull
	gh.pullErrOnRead[key] = 2
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	on := true
	if _, err := svc.SetSolver(context.Background(), repo, SolverChange{OnePass: &on}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, sha, PhaseCompleted, time.Now().UTC(), 0)
	report := NextReport{
		Repo: repo, PR: pr, Head: sha, Action: string(engine.ActionFix),
		Findings: []dialect.Finding{{
			ID: onePassFinalizeSource, Source: onePassFinalizeSource, Commit: sha, Severity: "major",
		}},
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report.onePassCampaign = st.EffectiveSolver(repo).OnePassCampaign
	token := "post-session-read-failure"
	if ok, why, _ := svc.claimDispatch(context.Background(), report, token, 3); !ok {
		t.Fatalf("claimDispatch refused: %s", why)
	}

	ok, why := svc.dispatch(context.Background(), WatchOptions{
		Command: []string{"/usr/bin/true"},
	}, report, token)
	if !ok {
		t.Fatalf("successful finalizer was terminalized after a transient pull read: %s", why)
	}
	st, _, err = store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	progress, present := st.OnePassProgressFor(repo, pr)
	if !present || progress.ReadyHead != sha || progress.ReadyBase != sha || !progress.VerificationPending {
		t.Fatalf("retryable hand-off = %+v, present=%t; want exact head/base pending verification", progress, present)
	}
}

func TestDispatchReportsAZeroExitSessionWithUnlandedWork(t *testing.T) {
	base := t.TempDir()
	repo, pr := "owner/thing", 13
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", pr, sha
	gh.pulls[fakeKey(repo, pr)] = pull
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	report := NextReport{
		Repo: repo, PR: pr, Head: sha, Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: sha}},
	}
	token := "dispatch-token"
	if ok, why, _ := svc.claimDispatch(context.Background(), report, token, 3); !ok {
		t.Fatalf("claimDispatch refused: %s", why)
	}
	script := filepath.Join(t.TempDir(), "session.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch unfinished.go\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, why := svc.dispatch(context.Background(), WatchOptions{Command: []string{script}}, report, token)
	if ok || why != "the session left uncommitted work" {
		t.Fatalf("dispatch = (%t, %q), want an unlanded-work failure", ok, why)
	}
}

// One repository renamed, deleted, or unreadable by this token used to abort the
// whole pass. The service restarts into the same list and hits it again, so every
// healthy repository after it gets no events and no fix sessions — indefinitely.
func TestWatchPassOutlivesAnUnreadableRepository(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/gone": true, "o/fine": true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	gh.listPullErrs = map[string]error{"o/gone": errors.New("404 Not Found")}
	var p ghapi.Pull
	p.State = "open"
	p.Number = 3
	p.Head.SHA = "aaaaaaaa1"
	gh.pulls[fakeKey("o/fine", 3)] = p
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	var seen []int
	if err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(0), func(e WatchEvent) error {
		seen = append(seen, e.PR)
		return nil
	}); err != nil {
		t.Fatalf("a daemon pass must survive one bad repository: %v", err)
	}
	if len(seen) != 1 || seen[0] != 3 {
		t.Errorf("visited %v, want the healthy repository's PR", seen)
	}

	// A --once run is somebody's cron job, and reporting success after skipping a
	// repository it could not read makes a broken scan look like a clean one.
	err := svc.watchPass(context.Background(), WatchOptions{Once: true}, newDispatchPool(0), nil)
	if err == nil || !strings.Contains(err.Error(), "o/gone") {
		t.Errorf("err = %v, want a one-shot run to name the repository it could not check", err)
	}
}

// A session that fixes files without pushing must not have that work deleted:
// removing the worktree discards fixes that were made but not landed.
func TestDispatchKeepsAWorktreeWithUnpushedWork(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 8)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 8, sha, PhaseQueued, time.Now().UTC(), 0)

	// A session that edits a file and stops there, which is the ordinary shape.
	script := filepath.Join(t.TempDir(), "fix.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho fixed >> README.md\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 8, Head: sha, Action: "fix"})
	if !ok {
		t.Fatalf("dispatch failed: %s", why)
	}
	pool.wait()
	found := false
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "README.md" {
			if body, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(body), "fixed") {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("the session's uncommitted fix was deleted with the worktree")
	}
}

// With three dispatch slots and four PRs needing fixes, a fixed pass order gives
// the same three the slots every time and tells the fourth "at dispatch
// capacity" forever. One PR sat five hours that way while its findings grew from
// 15 to 25, so every PR has to reach the front eventually.
func TestWatchPassRotatesSoNoPRIsStarved(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	for _, pr := range []int{1, 2, 3, 4} {
		var p ghapi.Pull
		p.State = "open"
		p.Number = pr
		p.Head.SHA = "aaaaaaaa1"
		gh.pulls[fakeKey("o/r", pr)] = p
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	// Record the order each pass visits PRs in. Only the ORDER matters here, so
	// the pool takes every candidate.
	seen := map[int]int{}
	firstOf := []int{}
	for pass := 0; pass < 4; pass++ {
		var order []int
		err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(4), func(e WatchEvent) error {
			order = append(order, e.PR)
			seen[e.PR]++
			return nil
		})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(order) == 0 {
			t.Fatalf("pass %d visited nothing", pass)
		}
		firstOf = append(firstOf, order[0])
	}

	// Every PR was looked at every pass...
	for pr, n := range seen {
		if n != 4 {
			t.Errorf("pr %d seen %d times, want every pass", pr, n)
		}
	}
	// ...and the front of the queue moved, so the tail is not starved of slots.
	distinct := map[int]bool{}
	for _, pr := range firstOf {
		distinct[pr] = true
	}
	if len(distinct) < 2 {
		t.Errorf("the same PR led every pass (%v); the tail can never get a dispatch slot", firstOf)
	}
}

func TestWatchPassVisitsPrioritizedPRBeforeFairnessRotation(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	for _, pr := range []int{1, 2, 3, 4} {
		var p ghapi.Pull
		p.State = "open"
		p.Number = pr
		p.Head.SHA = "aaaaaaaa1"
		gh.pulls[fakeKey("o/r", pr)] = p
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "o/r", 4, "aaaaaaaa1", PhaseQueued, time.Now().UTC(), 0)
	if err := svc.Prioritize(context.Background(), "o/r", 4); err != nil {
		t.Fatal(err)
	}

	var order []int
	if err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(4), func(e WatchEvent) error {
		order = append(order, e.PR)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(order) == 0 || order[0] != 4 {
		t.Fatalf("visit order = %v, want prioritized PR 4 first", order)
	}
}

func TestPrioritizeDryRunDoesNotReportAMutation(t *testing.T) {
	cfg := firingConfig()
	cfg.DryRun = true
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	err := svc.Prioritize(context.Background(), "o/r", 4)
	if err == nil || !strings.Contains(err.Error(), "dry run") {
		t.Fatalf("dry-run prioritize error = %v, want an explicit non-success outcome", err)
	}
}

// A session that commits its fixes but does not push them leaves a clean working
// tree, and deleting that worktree destroys the only copy of the fix.
func TestDispatchKeepsACommittedButUnpushedFix(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 11)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 11, sha, PhaseQueued, time.Now().UTC(), 0)

	script := filepath.Join(t.TempDir(), "fix.sh")
	body := "#!/bin/sh\necho fixed >> README.md\n" +
		"git -c user.email=t@example.invalid -c user.name=t commit -qam 'fix the finding'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 11, Head: sha, Action: "fix"})
	if !ok {
		t.Fatalf("dispatch failed: %s", why)
	}
	pool.wait()

	found := false
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "README.md" {
			if content, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(content), "fixed") {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("the session's committed but unpushed fix was deleted with the worktree")
	}
}

// A command that never reached a process did not use up the per-head budget: a
// mistyped fix agent would otherwise spend every attempt without a session ever
// running, and correcting it would come too late for that head.
func TestDispatchRefundsTheAttemptWhenTheCommandCannotStart(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 6)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 6, sha, PhaseQueued, time.Now().UTC(), 0)

	pool := newDispatchPool(0)
	missing := filepath.Join(t.TempDir(), "no-such-agent")
	if ok, why := svc.startDispatch(context.Background(),
		WatchOptions{Dispatch: dispatchOn(), Command: []string{missing}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 6, Head: sha, Action: "fix"}); ok {
		t.Fatal("a command that never started was reported as dispatched")
	} else if !strings.Contains(why, "could not start") {
		t.Fatalf("dispatch refusal = %q, want the startup failure", why)
	}
	pool.wait()

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 6)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("round = %#v, want the claim released", round)
	}
	if round.Dispatch.Attempts != 0 {
		t.Errorf("attempts = %d, want the budget intact for a session that never started", round.Dispatch.Attempts)
	}
}

// A watch pass lists open PRs once, but checkout preparation happens later and
// can overlap another merge. Never spend CPU on an agent after that snapshot is
// stale, and refund the claim because no fix attempt actually ran.
func TestDispatchRechecksThePullBeforeStartingTheAgent(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 7, sha, PhaseQueued, time.Now().UTC(), 0)

	marker := filepath.Join(t.TempDir(), "agent-started")
	script := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pool := newDispatchPool(0)
	if ok, why := svc.startDispatch(context.Background(), WatchOptions{
		Dispatch: dispatchOn(), Command: []string{script, marker}, MaxAttempts: 3,
	}, pool, NextReport{Repo: repo, PR: 7, Head: sha, Action: "fix"}); ok {
		t.Fatal("a stale merged pull request started a fix session")
	} else if !strings.Contains(why, "no longer open") {
		t.Fatalf("dispatch refusal = %q, want the stale-open explanation", why)
	}
	pool.wait()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("agent marker stat = %v, want the agent never started", err)
	}

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 7)
	if round == nil || round.Dispatch == nil || round.Dispatch.Attempts != 0 {
		t.Fatalf("round after stale skip = %#v, want the unspent attempt refunded", round)
	}
}

func TestWatchQueuesCheckoutWithoutBlockingThePass(t *testing.T) {
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CRQ_REMOTE_BASE", t.TempDir())

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{Repo: "owner/thing", PR: 16, Head: "aaaaaaaa1", Action: "fix"}
	seedRound(t, store, cfg, report.Repo, report.PR, report.Head, PhaseQueued, time.Now().UTC(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	pool := newDispatchPool(0)
	before := time.Now()
	queued, why, result := svc.queueDispatchResult(ctx, WatchOptions{
		Dispatch: dispatchOn(), Command: []string{"/bin/true"}, MaxAttempts: 3,
	}, pool, report)
	if elapsed := time.Since(before); elapsed > time.Second {
		t.Fatalf("queueing waited %s for checkout; the serial pass would be blocked", elapsed)
	}
	if !queued {
		t.Fatalf("dispatch was not queued: %s", why)
	}

	cancel()
	pool.wait()
	if outcome := <-result; outcome.ok || !strings.Contains(outcome.reason, "checkout failed") {
		t.Fatalf("canceled queued dispatch = %+v, want checkout cancellation", outcome)
	}
}

func TestDispatchHealthRecordsProcessStartBeforeItsExit(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 15, sha
	gh.pulls[fakeKey(repo, 15)] = pull
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 15, sha, PhaseQueued, time.Now().UTC(), 0)
	script := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 17\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	pool := newDispatchPool(0)
	report := NextReport{Repo: repo, PR: 15, Head: sha, Action: "fix"}
	if ok, why := svc.startDispatch(context.Background(), WatchOptions{
		Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 3,
	}, pool, report); !ok {
		t.Fatalf("dispatch was not claimed: %s", why)
	}
	pool.wait()

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Autofix == nil || st.Autofix.LastSuccessAt == nil || st.Autofix.ConsecutiveFailures != 0 {
		t.Errorf("a process that started but exited nonzero was recorded as a start failure: %+v", st.Autofix)
	}
}

type dashboardCountingStore struct {
	StateStore
	syncs []State
}

func (s *dashboardCountingStore) SyncDashboard(_ context.Context, state State) error {
	s.syncs = append(s.syncs, cloneState(state))
	return nil
}

func TestDispatchHealthSyncsEveryVisibleAlertChange(t *testing.T) {
	cfg := firingConfig()
	cfg.DashboardIssue = 1
	store := &dashboardCountingStore{StateStore: NewMemoryStore(cfg)}
	svc := NewService(cfg, newFakeGitHub(), store, &recordingLogger{})
	ctx := context.Background()

	for i := 1; i <= AutofixUnhealthyAfter+1; i++ {
		svc.noteDispatchHealth(ctx, false, fmt.Sprintf("failure %d", i))
	}
	if got := len(store.syncs); got != 2 {
		t.Fatalf("dashboard syncs = %d, want threshold and later unhealthy update", got)
	}
	last := store.syncs[len(store.syncs)-1].Autofix
	if last == nil || last.ConsecutiveFailures != AutofixUnhealthyAfter+1 || last.LastError != "failure 4" {
		t.Fatalf("last synced health = %+v", last)
	}

	svc.noteDispatchHealth(ctx, true, "")
	if got := len(store.syncs); got != 3 {
		t.Fatalf("dashboard syncs after recovery = %d, want alert removal synced", got)
	}
}

// Findings on a head crq never queued — a review somebody triggered by hand, or
// feedback that predates the autofix watcher — used to be undispatchable forever, because
// `Next` returns fix before enqueueing and the claim had nowhere to live.
func TestClaimDispatchAdoptsAHeadTheQueueNeverSaw(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}

	if ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3); !ok {
		t.Fatalf("claim refused: %s — these findings can never be cleared", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Head != report.Head {
		t.Fatalf("round = %#v, want one tracking the observed head", round)
	}
	// Adopting the head must not buy a review: the findings are already in hand.
	if round.FireEligible(time.Now().UTC()) {
		t.Error("the adopted round is fire-eligible; dispatching would cost an account-metered review")
	}
	if !round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("dispatch = %#v, want the claim held", round.Dispatch)
	}
}

// Feedback carried from an older commit is not evidence that anybody reviewed
// the current head. Marking it reviewed to hold the claim left a completed round
// for a head no reviewer had looked at: the review is deduped away and the
// caller waits for one that can no longer be requested.
func TestClaimDispatchDoesNotMarkACarriedHeadReviewed(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "bbbbbbbb2", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1", ThreadID: "PRRT_1"}},
	}

	if ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3); !ok {
		t.Fatalf("claim refused: %s", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Phase == PhaseCompleted {
		t.Fatalf("round = %#v, want one that still needs a review of this head", round)
	}
	if !round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("dispatch = %#v, want the claim held", round.Dispatch)
	}
}

func TestClaimDispatchRechecksRepositoryAutofixSwitch(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}
	if _, err := svc.SetAutofixEnabled(ctx, report.Repo, false, "operator stop"); err != nil {
		t.Fatal(err)
	}

	ok, why, byDesign := svc.claimDispatch(ctx, report, "tok", 3)
	if ok {
		t.Fatal("claim bypassed a repository autofix switch that was turned off after the pass snapshot")
	}
	if !byDesign {
		t.Errorf("autofix refusal %q counted as a dispatcher failure", why)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(report.Repo, report.PR); round != nil && round.Dispatch != nil {
		t.Errorf("refused claim mutated the round: %+v", round)
	}
}

// A head that moved on while its previous round still stood: `Next` reports fix
// without enqueueing, so nothing else supersedes the stale round. Refusing the
// claim over the mismatch left the new head's findings undispatchable on every
// pass — and counted each refusal as the dispatcher failing.
func TestClaimDispatchSupersedesAStaleRound(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	seedRound(t, store, cfg, "owner/thing", 12, "aaaaaaaa1", PhaseCompleted, time.Now().UTC(), 0)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "bbbbbbbb2", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "bbbbbbbb2"}},
	}

	ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3)
	if !ok {
		t.Fatalf("claim refused: %s — the new head's findings can never be cleared", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Head != report.Head {
		t.Fatalf("round = %#v, want the stale one superseded by the observed head", round)
	}

	// A session already running for an earlier head keeps the PR to itself: its
	// own push is what moved the head, and it is still landing that work.
	live := NextReport{Repo: report.Repo, PR: report.PR, Head: "cccccccc3", Action: "fix"}
	if ok, why, byDesign := svc.claimDispatch(context.Background(), live, "tok2", 3); ok {
		t.Error("a second session was started against a PR somebody is already fixing")
	} else if !byDesign {
		t.Errorf("reason %q counted as a dispatcher failure", why)
	}
}

// A session's own push moves the head, and the next pass enqueues it — which
// supersedes the round the session is holding and archives its claim. Reading
// only the current round then answers "nobody is fixing this" about a pull
// request somebody is still resolving threads on, and a second session starts.
func TestClaimDispatchSeesAClaimItsOwnPushArchived(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	repo, pr := "owner/thing", 12
	first := NextReport{
		Repo: repo, PR: pr, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}
	if ok, why, _ := svc.claimDispatch(ctx, first, "tok1", 3); !ok {
		t.Fatalf("claim refused: %s", why)
	}

	// That session pushes; the watcher enqueues the new head, which supersedes.
	if _, err := store.Update(ctx, func(st *State) error {
		_, err := st.Supersede(repo, pr, "bbbbbbbb2", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	second := NextReport{Repo: repo, PR: pr, Head: "bbbbbbbb2", Action: "fix"}
	ok, why, byDesign := svc.claimDispatch(ctx, second, "tok2", 3)
	if ok {
		t.Fatal("a second session was started against the PR the first is still finishing")
	}
	if !byDesign {
		t.Errorf("reason %q counted as a dispatcher failure; another session holding the PR is the design", why)
	}

	// And when that session finishes, its archived claim must stop holding the
	// next one out for the rest of the TTL.
	svc.releaseDispatch(ctx, first, "tok1", true)
	if ok, why, _ := svc.claimDispatch(ctx, second, "tok3", 3); !ok {
		t.Errorf("claim refused after the session released it: %s", why)
	}
}

// The attempt bound is crq obeying its own configuration, not fix sessions
// failing to start. Counted as autofix health, a correctly bounded head raised the
// "fix sessions are not starting" alert after three passes — and every pass
// after that, forever.
func TestExhaustedAttemptsAreNotADispatcherFailure(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}

	for attempt := 1; attempt <= 2; attempt++ {
		ok, why, _ := svc.claimDispatch(context.Background(), report, fmt.Sprintf("tok%d", attempt), 2)
		if !ok {
			t.Fatalf("attempt %d refused: %s", attempt, why)
		}
		svc.releaseDispatch(context.Background(), report, fmt.Sprintf("tok%d", attempt), true)
	}
	ok, why, byDesign := svc.claimDispatch(context.Background(), report, "tok3", 2)
	if ok {
		t.Fatal("the attempt bound let a third dispatch through")
	}
	if !byDesign {
		t.Errorf("reason %q counted as a dispatcher failure; the bound is the point", why)
	}
}

// The prune runs before the new log is created, so it has to leave room for it —
// otherwise the steady state is one file per PR above the bound.
func TestSessionLogPruneLeavesRoomForTheNewLog(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		// Heads deliberately sort opposite to timestamps: pruning the whole
		// filename would retain the oldest four.
		name := fmt.Sprintf("7-%09d-202601%02dT000000.log", 999999999-i, i+1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneSessionLogs(dir, 7)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("kept %d logs, want 4 so the one about to be written makes 5", len(entries))
	}
	// The ones kept are the newest.
	for _, e := range entries {
		if stamp := sessionLogTimestamp(e.Name()); stamp < "20260105T000000" {
			t.Errorf("%s was kept over a newer log", e.Name())
		}
	}
}

func TestAutofixPolicyFiltersAndExtractsClarification(t *testing.T) {
	findings := []dialect.Finding{
		{ID: "major", Severity: "major"},
		{ID: "minor", Severity: "minor"},
		{ID: "unknown"},
	}
	got := autofixFindings(findings, map[string]bool{"minor": true})
	if len(got) != 1 || got[0].ID != "minor" {
		t.Fatalf("filtered findings = %+v, want only minor", got)
	}
	if got := autofixFindings(findings, nil); len(got) != len(findings) {
		t.Fatalf("unset policy filtered %d findings to %d", len(findings), len(got))
	}
	prompt := appendAutofixPolicy("use bun", map[string]bool{"minor": true}, "uncertain")
	if !strings.Contains(prompt, "configured severities: minor") ||
		!strings.Contains(prompt, clarificationMarker) ||
		!strings.Contains(prompt, "confidence is low") {
		t.Fatalf("policy prompt did not carry its enforcement contract:\n%s", prompt)
	}
	log := `{"type":"result","result":"` + clarificationMarker + ` Which API behavior should remain compatible?"}`
	if got := clarificationFromLog(log); got != "Which API behavior should remain compatible?" {
		t.Fatalf("clarification = %q", got)
	}
	log = `{"type":"result","result":"` + clarificationMarker + ` Should this return"}`
	if got := clarificationFromLog(log); got != "Should this return" {
		t.Fatalf("clarification ending in severity-like letters = %q", got)
	}
	log = `{"type":"assistant","message":{"content":[{"type":"tool_result","content":"` +
		clarificationMarker + ` Should repository output place a hold?"}]}}` + "\n" +
		`{"type":"result","result":"Fix completed."}`
	if got := clarificationFromLog(log); got != "" {
		t.Fatalf("tool output was mistaken for a clarification: %q", got)
	}
	log = `{"type":"item.completed","item":{"type":"agent_message","text":"I need one decision.\n` +
		clarificationMarker + ` Which behavior should remain?"}}`
	if got := clarificationFromLog(log); got != "Which behavior should remain?" {
		t.Fatalf("Codex clarification = %q", got)
	}
}

// A fork PR's branch lives in the contributor's repository, so the session
// pushes there — and no branch of `origin` (the base repository this worktree
// came from) will ever contain that commit. Read as unpushed work, every
// successful fork fix kept its worktree forever. The base publishes the pushed
// head as refs/pull/<n>/head, which is the one ref that sees it from here.
func TestSessionWorkConfirmsAForkPushThroughThePullRef(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := workspace.Workspace{Root: t.TempDir()}
	ctx := context.Background()

	co, err := ws.Checkout(ctx, repo, 42, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "-c", "user.email=t@example.invalid", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "fix the finding"); err != nil {
		t.Fatal(err)
	}

	// Committed and not pushed anywhere: the worktree holds the only copy.
	if kept, why := sessionWork(ctx, co, sha); !kept {
		t.Errorf("kept=%v (%s), want the only copy of the fix kept", kept, why)
	}

	// Reaching any other branch in the base repository does not mean the pull
	// request moved. The checkout is still the only safe recovery point.
	if _, err := co.Git(ctx, "push", "origin", "HEAD:refs/heads/wrong-branch"); err != nil {
		t.Fatal(err)
	}
	if kept, why := sessionWork(ctx, co, sha); !kept {
		t.Errorf("kept=%v (%s), want a commit absent from the PR kept", kept, why)
	}

	// The contributor's branch is not in the base repository, but the PR ref is:
	// this is what GitHub publishes once the fork branch moves.
	if _, err := co.Git(ctx, "push", "origin", "HEAD:refs/pull/42/head"); err != nil {
		t.Fatal(err)
	}
	if kept, why := sessionWork(ctx, co, sha); kept {
		t.Errorf("kept=%v (%s), want a landed fork push recognised", kept, why)
	}
}

// A fix session checks the head out and runs an agent over it with approvals
// bypassed, holding a token that can write to the repository. On a fork PR that
// code belongs to a stranger, so it is not run unless the operator says so.
func TestDispatchSkipsAForkUnlessAllowed(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	var own, fork ghapi.Pull
	own.Head.Repo.FullName = "Owner/Thing" // case differs; the repository does not
	fork.Head.Repo.FullName = "contributor/thing"

	if !svc.mayDispatch(svc.cfg, "owner/thing", own) {
		t.Error("a branch in the repository itself must be dispatchable")
	}
	if svc.mayDispatch(svc.cfg, "owner/thing", fork) {
		t.Error("a fork was dispatched without CRQ_DISPATCH_FORKS")
	}
	// A deleted fork answers with no repository at all. Reading that as "ours"
	// would grant exactly the untrusted case the permission it lacks.
	if svc.mayDispatch(svc.cfg, "owner/thing", ghapi.Pull{}) {
		t.Error("an unreadable head repository was treated as our own")
	}

	allowed := cfg
	allowed.DispatchForks = true
	if !NewService(allowed, newFakeGitHub(), NewMemoryStore(allowed), nil).mayDispatch(allowed, "owner/thing", fork) {
		t.Error("CRQ_DISPATCH_FORKS did not allow a fork")
	}
}

func TestForkPolicyIsRecheckedInsideTheClaim(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/thing": true}
	cfg.DispatchForks = true // the pass-level snapshot was permissive
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	ctx := context.Background()
	report := NextReport{
		Repo: "owner/thing", PR: 9, Head: "aaaaaaaa1", Action: "fix", Fork: true,
	}
	seedRound(t, store, cfg, report.Repo, report.PR, report.Head, PhaseQueued, time.Now().UTC(), 0)

	off := false
	if _, err := svc.SetSolver(ctx, report.Repo, SolverChange{Forks: &off}); err != nil {
		t.Fatal(err)
	}
	if ok, why, byDesign := svc.claimDispatch(ctx, report, "token", 5); ok {
		t.Fatal("a stale permissive fork decision started a session after policy was turned off")
	} else if !byDesign || !strings.Contains(why, "current solver policy") {
		t.Fatalf("claim refusal = %q byDesign=%v", why, byDesign)
	}
}

// The watcher must not write into the caller's slices.
//
// `crq watch -- <cmd>` splits argv at "--": the flag half is args[:i] and the
// command half is args[i+1:], so the flag half keeps CAPACITY reaching into the
// command. fs.Args() is a sub-slice of it, and filling an empty repo list with
// append then overwrote the fix command in place — every dispatch tried to exec
// a repository slug ("fork/exec kristofferr/coderabbit-queue: no such file or
// directory") and no pull request in the fleet was fixed.
func TestWatchDoesNotOverwriteTheCommandItWasGiven(t *testing.T) {
	argv := []string{"--dispatch", "--", "/bin/true", "exec"}
	flagArgs, command := argv[:1], argv[2:]
	repos := flagArgs[1:] // what fs.Args() hands back: empty, but aliasing command

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/one": true, "owner/two": true, "owner/three": true}
	gh := newFakeGitHub()
	gh.listPullErrs = map[string]error{
		"owner/one": errors.New("unreadable"), "owner/two": errors.New("unreadable"),
		"owner/three": errors.New("unreadable"),
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	_ = svc.watchPass(context.Background(), WatchOptions{
		Repos: repos, Command: command, Dispatch: dispatchOn(),
	}, newDispatchPool(0), nil)

	if command[0] != "/bin/true" || command[1] != "exec" {
		t.Fatalf("the fix command was overwritten: %q", command)
	}
}

// dispatchOn is the explicit "yes" a test needs now that WatchOptions.Dispatch
// distinguishes unset (default on) from an answer.
func dispatchOn() *bool {
	on := true
	return &on
}

// CRQ_EXCLUDE means "crq does not go here", to every path that acts on a
// repository. autoReviewPass has always honoured it; the watcher did not, so
// the one setting that reads like a fleet-wide opt-out covered only half of
// what crq does — reviews stopped and the watcher carried on.
func TestWatchHonoursTheExcludedRepositories(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/kept": true, "owner/gone": true}
	cfg.ExcludeRepos = map[string]bool{"owner/gone": true}
	gh := newFakeGitHub()
	for _, repo := range []string{"owner/kept", "owner/gone"} {
		var pull ghapi.Pull
		pull.State, pull.Number, pull.Head.SHA = "open", 1, "aaaaaaaa1"
		gh.pulls[fakeKey(repo, 1)] = pull
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	var seen []string
	if err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(0),
		func(e WatchEvent) error { seen = append(seen, e.Repo); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, repo := range seen {
		if NormalizeRepo(repo) == "owner/gone" {
			t.Errorf("an excluded repository was watched: %v", seen)
		}
	}
	if len(seen) == 0 {
		t.Error("excluding one repository stopped the rest being watched")
	}
}

// `crq watch --max-attempts N` has to mean N. The per-repository budget is
// resolved from the RECORD, not from the merged configuration: that always
// carries a positive default, so reading it there accepted the flag and then
// silently replaced it with 3 on every ordinary setup.
func TestDispatchAttemptBudgetPrefersTheRecordedValue(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.DispatchMaxAttempts = 3
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 9)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 9, sha, PhaseQueued, time.Now().UTC(), 0)
	spendAttempts(t, store, repo, 9, 3)

	report := NextReport{Repo: repo, PR: 9, Head: sha, Action: "fix"}
	script := filepath.Join(t.TempDir(), "fix.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Nothing recorded: this run's own limit is the one in force.
	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(ctx, WatchOptions{Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 5}, pool, report)
	pool.wait()
	if !ok {
		t.Fatalf("dispatch refused with --max-attempts 5 after 3 attempts: %s", why)
	}

	// A recorded value still outranks it: the budget belongs to the project.
	spendAttempts(t, store, repo, 9, 1)
	if _, err := svc.SetSolver(ctx, repo, SolverChange{MaxAttempts: intptr(2)}); err != nil {
		t.Fatal(err)
	}
	pool = newDispatchPool(0)
	ok, why = svc.startDispatch(ctx, WatchOptions{Dispatch: dispatchOn(), Command: []string{script}, MaxAttempts: 5}, pool, report)
	pool.wait()
	if ok {
		t.Fatal("dispatch ran past the repository's recorded budget of 2")
	}
	if !strings.Contains(why, "dispatch attempts already made") {
		t.Errorf("refusal = %q, want the attempt bound to be the reason", why)
	}
}

// spendAttempts records n finished dispatch attempts on a round, the way a
// session that ran and released its claim leaves it.
func spendAttempts(t *testing.T, store StateStore, repo string, pr, n int) {
	t.Helper()
	_, err := store.Update(context.Background(), func(st *State) error {
		r := st.Round(repo, pr)
		if r == nil {
			t.Fatalf("no round for %s#%d", repo, pr)
		}
		for i := 0; i < n; i++ {
			if ok, why := r.ClaimDispatch("host", "tok", time.Now().UTC(), 0); !ok {
				t.Fatalf("seeding attempt %d: %s", i, why)
			}
			r.ReleaseDispatch("tok")
		}
		st.PutRound(*r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "Stop reviewing" has to stop a fix session too, and it has to stop one that
// is being granted right now. The pass reads enrollment once and best-effort,
// so a click landing after that read — or a read that failed — would otherwise
// let a coding agent start on a repository somebody just turned off.
func TestDispatchRefusesADisabledRepositoryUnderTheClaim(t *testing.T) {
	ctx := context.Background()
	repo := "owner/thing"
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{repo: true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	seedRound(t, store, cfg, repo, 9, "abcdef123", PhaseQueued, time.Now().UTC(), 0)

	// Disabled AFTER the pass would have read enrollment, which is the window.
	if _, err := svc.SetEnrollment(ctx, repo, false, "stopped from the dashboard"); err != nil {
		t.Fatal(err)
	}
	claimed, why, byDesign := svc.claimDispatch(ctx,
		NextReport{Repo: repo, PR: 9, Head: "abcdef123", Action: "fix"}, "tok", 3)
	if claimed {
		t.Fatal("a fix session was granted for a repository crq is not reviewing")
	}
	if !byDesign {
		t.Errorf("refusal %q must count as the gate working, not as a dispatcher failure", why)
	}
	if !strings.Contains(why, "not reviewing") {
		t.Errorf("refusal = %q, want enrollment named as the reason", why)
	}
}

// Enrollment is a fleet-wide record and autoreview already builds its scan list
// from it. The watcher read only CRQ_REPOS, so a repository enrolled from the
// dashboard was reviewed but never watched — its findings arrived and no fix
// session ever started — until somebody edited an env file on this host and
// reinstalled the service.
func TestWatchTargetsIncludeRepositoriesEnrolledFromTheDashboard(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/known": true}
	gh := newFakeGitHub()
	for _, repo := range []string{"owner/known", "owner/enrolled"} {
		var pull ghapi.Pull
		pull.State, pull.Number, pull.Head.SHA = "open", 1, "aaaaaaaa1"
		gh.pulls[fakeKey(repo, 1)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "owner/enrolled", true, ""); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0),
		func(e WatchEvent) error { seen[NormalizeRepo(e.Repo)] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !seen["owner/enrolled"] {
		t.Errorf("watched %v, want the repository the record enrolled", seen)
	}
	if !seen["owner/known"] {
		t.Errorf("watched %v, want this host's own list still honoured", seen)
	}
}

func TestWatchTargetsIncludeMigratedFleetAllowList(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/gate",
		"CRQ_REPOS": "owner/startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	for _, repo := range []string{"owner/startup", "owner/migrated"} {
		var pull ghapi.Pull
		pull.State, pull.Number, pull.Head.SHA = "open", 1, "aaaaaaaa1"
		gh.pulls[fakeKey(repo, 1)] = pull
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_REPOS": "owner/migrated"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	seen := map[string]bool{}
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0),
		func(e WatchEvent) error { seen[NormalizeRepo(e.Repo)] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !seen["owner/migrated"] {
		t.Errorf("watched %v, want the repository from the migrated fleet allow-list", seen)
	}
	if seen["owner/startup"] {
		t.Errorf("watched %v, startup allow-list should be replaced by the migrated fleet policy", seen)
	}
}

// The fork switch is not a refinement of a setting; it is the line between
// running an agent over the operator's own code and over a stranger's. When the
// shared record cannot be read, falling back to a permissive host value would
// re-enable fork dispatches at precisely the moment the safety policy is
// unavailable, so the fallback denies them instead.
func TestForkDispatchFallsBackClosedWhenTheRecordCannotBeRead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DispatchForks = true // this host's env says yes
	svc := NewService(cfg, newFakeGitHub(), unreadableStore{NewMemoryStore(cfg)}, nil)

	var fork ghapi.Pull
	fork.Head.Repo.FullName = "contributor/thing"
	if svc.mayDispatch(svc.repoCfg(ctx, "owner/thing"), "owner/thing", fork) {
		t.Error("a fork was dispatched on a permissive env value while the shared policy was unreadable")
	}
	// Everything else still falls back to the host's configuration: an
	// unreadable setting must not stop crq fixing its own branches.
	var own ghapi.Pull
	own.Head.Repo.FullName = "owner/thing"
	if !svc.mayDispatch(svc.repoCfg(ctx, "owner/thing"), "owner/thing", own) {
		t.Error("an own-repository pull request must still be dispatchable")
	}
}

// unreadableStore is a state ref that cannot be read — a transient outage, a
// revoked token, a ref that has not been created yet.
type unreadableStore struct{ StateStore }

func (unreadableStore) Load(context.Context) (State, Revision, error) {
	return State{}, Revision{}, errors.New("state ref unreadable")
}

type loadSwitchStore struct {
	StateStore
	unreadable bool
}

func (s *loadSwitchStore) Load(ctx context.Context) (State, Revision, error) {
	if s.unreadable {
		return State{}, Revision{}, errors.New("state ref unreadable")
	}
	return s.StateStore.Load(ctx)
}

func TestWatchPassStopsWhenSharedStateCannotBeRead(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/r", Number: 1}}
	svc := NewService(cfg, gh, unreadableStore{NewMemoryStore(cfg)}, nil)

	err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(0), nil)
	if err == nil || !strings.Contains(err.Error(), "loading shared state") {
		t.Fatalf("watchPass error = %v, want shared-state load failure", err)
	}
}

// A solver record that names an author to skip has to reach the watcher too.
// Next is a MUTATING oracle — it enqueues, it can fire, and it can start a fix
// session that runs an agent with approvals bypassed — so a watcher that
// checked only the skip marker reviewed and executed against pull requests the
// per-repository setting had just excluded.
func TestWatchHonoursTheSkipAuthorList(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	gh := newFakeGitHub()
	var bot ghapi.Pull
	bot.State = "open"
	bot.Number = 9
	bot.Head.SHA = "abcdef1234567890"
	bot.User.Login = "dependabot[bot]"
	gh.pulls[fakeKey("o/r", 9)] = bot
	var human ghapi.Pull
	human.State = "open"
	human.Number = 10
	human.Head.SHA = "beefbeef12345678"
	human.User.Login = "kristofferR"
	gh.pulls[fakeKey("o/r", 10)] = human
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetSolver(ctx, "o/r", SolverChange{SkipAuthors: []string{"dependabot[bot]"}}); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var skipped *WatchEvent
	for i := range events {
		if events[i].PR == 9 {
			skipped = &events[i]
		}
	}
	if skipped == nil || skipped.Action != "skipped" || skipped.Reason == "" {
		t.Fatalf("events = %#v, want the skipped author explained", events)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Round("o/r", 9) != nil {
		t.Error("a skipped author's pull request was passed to the mutating Next oracle")
	}
	if st.Round("o/r", 10) == nil {
		t.Error("the rest of the repository must still be watched")
	}
}

// The attempt budget is fleet-settable, and the settings editor saves it into
// the generic fleet map. A claim that consulted only the typed solver record
// went on enforcing this host's number while the dashboard reported the
// fleet's — with no later state read able to change it.
func TestRecordedMaxAttemptsReadsBothRecords(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	// Nothing recorded: this run's own flag stands, which is the whole reason a
	// RECORDED value is asked for rather than a resolved one.
	if got := svc.recordedMaxAttempts(ctx, "o/r", 7); got != 7 {
		t.Errorf("attempts = %d, want this run's own 7", got)
	}
	if _, err := svc.SetEnv(ctx, "CRQ_DISPATCH_MAX_ATTEMPTS", "9", false); err != nil {
		t.Fatal(err)
	}
	if got := svc.recordedMaxAttempts(ctx, "o/r", 7); got != 9 {
		t.Errorf("attempts = %d, want the fleet's recorded 9", got)
	}
	// The per-repository record is more specific and wins outright.
	if _, err := svc.SetSolver(ctx, "o/r", SolverChange{MaxAttempts: intptr(2)}); err != nil {
		t.Fatal(err)
	}
	if got := svc.recordedMaxAttempts(ctx, "o/r", 7); got != 2 {
		t.Errorf("attempts = %d, want the repository's 2", got)
	}
	if got := svc.recordedMaxAttempts(ctx, "o/other", 7); got != 9 {
		t.Errorf("attempts = %d, want another repository still on the fleet's 9", got)
	}
}

func TestDispatchClaimResolvesCurrentSolverSettingsInsideCAS(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.FixModels = []string{"old-model"}
	cfg.FixModel = "old-model"
	store := NewMemoryStore(cfg)
	repo, pr, head := "o/r", 19, "abcdef123"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, time.Now().UTC(), 0)

	hooked := &hookedStore{StateStore: store}
	hooked.hook = func() {
		if _, err := store.Update(ctx, func(st *State) error {
			sv := st.EffectiveSolver(repo)
			sv.Models, sv.SetModels, sv.Model = []string{"new-model"}, true, "new-model"
			sv.MaxAttempts = intptr(1)
			st.SetSolver(repo, sv, "operator", time.Now().UTC())
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	svc := NewService(cfg, newFakeGitHub(), hooked, nil)
	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix"}

	ok, why, _, model := svc.claimDispatchModels(ctx, &report, "first", 5)
	if !ok {
		t.Fatalf("first claim: %s", why)
	}
	if model != "new-model" {
		t.Fatalf("selected model = %q, want the ranking written immediately before the CAS", model)
	}
	if report.dispatchUntil.IsZero() {
		t.Fatal("dispatch claim did not retain its locally known lease expiry")
	}
	svc.releaseDispatch(ctx, report, "first", true)

	if ok, why, byDesign, _ := svc.claimDispatchModels(ctx, &report, "second", 5); ok {
		t.Fatal("a second claim exceeded the current one-attempt limit")
	} else if !byDesign || !strings.Contains(why, "attempt") {
		t.Fatalf("second claim = byDesign %v reason %q, want the current attempt limit", byDesign, why)
	}
}

func TestDispatchClaimFiltersFindingsWithCurrentSeverityPolicy(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	repo, pr, head := "o/r", 29, "abcdef123"
	if _, err := svc.SetSolver(ctx, repo, SolverChange{Severities: []string{"minor"}}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, time.Now().UTC(), 0)
	report := NextReport{
		Repo: repo, PR: pr, Head: head, Action: "fix",
		Findings: []dialect.Finding{
			{ID: "major", Severity: "major"},
			{ID: "minor", Severity: "minor"},
		},
	}

	ok, why, _, _ := svc.claimDispatchModels(ctx, &report, "severity-token", 5)
	if !ok {
		t.Fatalf("claim refused: %s", why)
	}
	if len(report.Findings) != 1 || report.Findings[0].Severity != "minor" {
		t.Fatalf("claimed findings = %+v, want only the severity allowed by the claiming state revision", report.Findings)
	}
}
