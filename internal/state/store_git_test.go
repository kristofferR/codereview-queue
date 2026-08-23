package state

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
