package state

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/kristofferR/coderabbit-queue/internal/gh"
)

func mustGit(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runGit(context.Background(), nil, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v (%s)", args[0], err, strings.TrimSpace(string(stderr)))
	}
	return strings.TrimSpace(string(stdout))
}

func seedGitStateRemote(t *testing.T) (string, State) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "state.git")
	work := filepath.Join(root, "seed")
	mustGit(t, "init", "--bare", "--quiet", remote)
	mustGit(t, "init", "--quiet", work)
	mustGit(t, "-C", work, "config", "user.name", "seed")
	mustGit(t, "-C", work, "config", "user.email", "seed@example.invalid")

	st := New()
	st.Account.Scope = "owner"
	st.Account.Source = "seed"
	st.Normalize(time.Now().UTC())
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, statePath), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, dashboardPath), []byte("seed dashboard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "preserve.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, "-C", work, "add", statePath, dashboardPath, "preserve.txt")
	mustGit(t, "-C", work, "commit", "--quiet", "-m", "seed state")
	mustGit(t, "-C", work, "push", "--quiet", remote, "HEAD:refs/heads/crq-state-v3")
	return remote, st
}

func newGitFallbackTestStore(t *testing.T, remote string) *GitStateStore {
	t.Helper()
	cfg := StoreConfig{
		GateRepo:       "owner/state",
		StateRef:       "crq-state-v3",
		DashboardIssue: 1,
		Scope:          []string{"owner"},
		Host:           "test-host",
	}
	store := NewGitStateStore(cfg, nil, nil)
	store.gitRemoteURL = remote
	t.Cleanup(func() {
		if store.gitDir != "" {
			_ = os.RemoveAll(store.gitDir)
		}
	})
	return store
}

func TestGitFallbackLoadsAndUpdatesStateRef(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote, seeded := seedGitStateRemote(t)
	store := newGitFallbackTestStore(t, remote)

	loaded, rev, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Account.Source != seeded.Account.Source || rev.CommitSHA == "" || rev.TreeSHA == "" {
		t.Fatalf("loaded source/revision = %q %+v, want seeded state with a git revision", loaded.Account.Source, rev)
	}

	updated, err := store.Update(context.Background(), func(st *State) error {
		st.Account.Source = "git-fallback"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Account.Source != "git-fallback" || updated.Rev != seeded.Rev+1 {
		t.Fatalf("updated state = source %q rev %d", updated.Account.Source, updated.Rev)
	}

	freshStore := newGitFallbackTestStore(t, remote)
	reloaded, _, err := freshStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Account.Source != "git-fallback" {
		t.Fatalf("reloaded source = %q, want git-fallback", reloaded.Account.Source)
	}
	if got := mustGit(t, "--git-dir", remote, "show", "refs/heads/crq-state-v3:preserve.txt"); got != "keep me" {
		t.Fatalf("unrelated tree entry = %q, want preserved", got)
	}
	if got := mustGit(t, "--git-dir", remote, "show", "-s", "--format=%an <%ae>", "refs/heads/crq-state-v3"); got != gitStateAuthorName+" <"+gitStateAuthorEmail+">" {
		t.Fatalf("state commit identity = %q, want public noreply identity", got)
	}
}

func TestGitFallbackStateRefUsesGitTransport(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote, _ := seedGitStateRemote(t)
	store := newGitFallbackTestStore(t, remote)

	got, err := store.StateRef(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := mustGit(t, "--git-dir", remote, "rev-parse", "refs/heads/crq-state-v3")
	if got != want {
		t.Fatalf("state ref = %q, want %q", got, want)
	}
}

func TestGitFallbackStateRefReportsMissingRemoteRef(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote := filepath.Join(t.TempDir(), "state.git")
	mustGit(t, "init", "--bare", "--quiet", remote)
	store := newGitFallbackTestStore(t, remote)

	got, err := store.StateRef(context.Background())
	if got != "" || !errors.Is(err, gh.ErrNotFound) {
		t.Fatalf("missing state ref = (%q, %v), want empty gh.ErrNotFound", got, err)
	}
}

func TestGitFallbackCloseRemovesPrivateCache(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote, _ := seedGitStateRemote(t)
	store := newGitFallbackTestStore(t, remote)
	if _, _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := store.gitDir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git cache still exists after Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := store.Load(context.Background()); !errors.Is(err, errGitStateStoreClosed) {
		t.Fatalf("Load after Close = %v, want the explicit closed-store error", err)
	}

	unopened := newGitFallbackTestStore(t, remote)
	if err := unopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := unopened.Load(context.Background()); !errors.Is(err, errGitStateStoreClosed) {
		t.Fatalf("first Load after an early Close = %v, want the explicit closed-store error", err)
	}
}

func TestGitFallbackRemoteCommandsInjectCurrentToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub git shim needs a POSIX shell")
	}
	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	tokenPath := filepath.Join(t.TempDir(), "token")
	script := filepath.Join(binDir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CRQ_TEST_ARGS\"\nprintf '%s' \"$CRQ_STATE_GIT_TOKEN\" > \"$CRQ_TEST_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CRQ_TEST_ARGS", argsPath)
	t.Setenv("CRQ_TEST_TOKEN", tokenPath)
	if err := exec.Command(script).Run(); err != nil {
		t.Skipf("the temporary directory cannot execute the POSIX git shim: %v", err)
	}

	store := &GitStateStore{
		cfg:    StoreConfig{TokenSource: func(context.Context) string { return "current-token" }},
		gitDir: "/tmp/private-state-cache",
	}
	if _, _, err := store.gitRemote(context.Background(), nil, nil, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "credential.helper="+gitStateCredentialHelper) {
		t.Fatalf("git args lack the state credential helper: %s", args)
	}
	if strings.Contains(string(args), "current-token") {
		t.Fatal("token leaked into git argv")
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(token) != "current-token" {
		t.Fatalf("git token env = %q, want current token", token)
	}
}

func TestGitFallbackMapsNonFastForwardToCASConflict(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote, _ := seedGitStateRemote(t)
	first := newGitFallbackTestStore(t, remote)
	second := newGitFallbackTestStore(t, remote)

	firstState, firstRev, err := first.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondState, secondRev, err := second.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstState.Rev++
	firstState.Account.Source = "first-writer"
	if err := first.compareAndSwap(context.Background(), &firstState, firstRev); err != nil {
		t.Fatal(err)
	}
	secondState.Rev++
	secondState.Account.Source = "stale-writer"
	if err := second.compareAndSwap(context.Background(), &secondState, secondRev); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale push error = %v, want ErrCASConflict", err)
	}

	check := newGitFallbackTestStore(t, remote)
	loaded, _, err := check.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Account.Source != "first-writer" {
		t.Fatalf("winning source = %q, want first-writer", loaded.Account.Source)
	}
}

func TestGitFallbackDoesNotRetryGenericPushRejections(t *testing.T) {
	generic := []byte("! [remote rejected] refs/crq/state (pre-receive hook declined)")
	if isGitNonFastForward(generic, nil) {
		t.Fatal("a generic remote rejection was classified as a retryable CAS conflict")
	}
	for _, message := range []string{"non-fast-forward", "fetch first", "stale info"} {
		if !isGitNonFastForward(nil, []byte(message)) {
			t.Errorf("lease-specific rejection %q was not classified as a CAS conflict", message)
		}
	}
}

func TestGitFallbackDeletedRefCannotBeResurrectedByStaleWriter(t *testing.T) {
	t.Setenv(gitFallbackEnv, "1")
	remote, _ := seedGitStateRemote(t)
	store := newGitFallbackTestStore(t, remote)

	stale, rev, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, "--git-dir", remote, "update-ref", "-d", "refs/heads/crq-state-v3")
	stale.Rev++
	stale.Account.Source = "stale-writer"
	if err := store.compareAndSwap(context.Background(), &stale, rev); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("deleted-ref push error = %v, want ErrCASConflict", err)
	}
	if _, _, err := runGit(context.Background(), nil, nil,
		"--git-dir", remote, "rev-parse", "--verify", "refs/heads/crq-state-v3"); err == nil {
		t.Fatal("stale writer recreated the deliberately deleted state ref")
	}
}

func TestGitFallbackIsOptInAndSkipsDashboardAPI(t *testing.T) {
	t.Setenv(gitFallbackEnv, "")
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3"}
	if NewGitStateStore(cfg, nil, nil).gitFallback {
		t.Fatal("git fallback enabled without CRQ_STATE_GIT_FALLBACK=1")
	}

	t.Setenv(gitFallbackEnv, "1")
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "REST must not be called", http.StatusForbidden)
	}))
	defer srv.Close()
	store := NewGitStateStore(cfg, gh.NewTestClient(srv.URL, srv.Client()), nil)
	if !store.gitFallback {
		t.Fatal("git fallback not enabled by CRQ_STATE_GIT_FALLBACK=1")
	}
	if err := store.SyncDashboard(context.Background(), New()); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("dashboard fallback made %d REST requests, want none", got)
	}
}
