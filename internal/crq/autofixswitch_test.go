package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Autofix is on by default and off only where somebody said so. A repository
// nobody has ruled on gets fixed, because that is what the watcher is for.
func TestAutofixIsOnUnlessARepositorySaysOtherwise(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/one": true, "owner/two": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	settings, err := svc.AutofixSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 2 {
		t.Fatalf("settings = %+v, want one per watched repository", settings)
	}
	for _, s := range settings {
		if !s.Enabled || !s.Default {
			t.Errorf("%s = %+v, want enabled by default", s.Repo, s)
		}
	}

	if _, err := svc.SetAutofixEnabled(ctx, "Owner/One", false, "hand-tuned branch"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Case-insensitive, like every other repository key.
	if st.AutofixEnabled("owner/one") {
		t.Error("an explicit off did not take")
	}
	if !st.AutofixEnabled("owner/two") {
		t.Error("turning one repository off turned another off too")
	}

	// Back to the default is distinguishable from an explicit on, and the answer
	// it reports is the one the repository ends up with.
	if setting, cleared, err := svc.ClearAutofixEnabled(ctx, "owner/one"); err != nil || !cleared {
		t.Fatalf("clear = %v %v, want it to report the setting it removed", cleared, err)
	} else if !setting.Enabled || !setting.Default {
		t.Errorf("clear reported %+v, want the resolved default", setting)
	}
	st, _, _ = store.Load(ctx)
	if !st.AutofixEnabled("owner/one") {
		t.Error("clearing the setting did not return the repository to the default")
	}
}

// The watcher builds its targets from CRQ_REPOS AND the enrollment records, so a
// repository enrolled from the dashboard is being fixed here under the fleet
// default. Listing only what env names — and what somebody has ruled on — left
// that repository out of the one screen that says whether crq may touch it.
func TestAutofixSettingsListEnrolledRepositories(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/env": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnrollment(ctx, "owner/added", true, ""); err != nil {
		t.Fatal(err)
	}
	settings, err := svc.AutofixSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]AutofixSetting{}
	for _, s := range settings {
		listed[s.Repo] = s
	}
	added, ok := listed["owner/added"]
	if !ok {
		t.Fatalf("settings = %+v, want the enrolled repository the watcher will fix", settings)
	}
	if !added.Enabled || !added.Default {
		t.Errorf("owner/added = %+v, want it reported as on by the fleet default", added)
	}
	if _, ok := listed["owner/env"]; !ok {
		t.Errorf("settings = %+v, want this host's own repositories still listed", settings)
	}
}

func TestAutofixSettingsUseFleetResolvedAllowList(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/gate",
		"CRQ_REPOS": "startup/old",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_REPOS": "fleet/new"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := NewService(cfg, newFakeGitHub(), store, nil).AutofixSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 || settings[0].Repo != "fleet/new" {
		t.Fatalf("settings = %+v, want only the fleet-resolved repository", settings)
	}
}

func TestAutofixSwitchRejectsMalformedRepositoryNames(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, repo := range []string{
		"owner", "owner/repo/", "/repo", "owner/repo/extra", "../repo",
		"owner/bad repo", "owner/control\nname", "bad_owner/repo", "-owner/repo",
	} {
		if _, err := svc.SetAutofixEnabled(ctx, repo, false, "operator stop"); err == nil {
			t.Errorf("SetAutofixEnabled(%q) succeeded", repo)
		}
		if _, _, err := svc.ClearAutofixEnabled(ctx, repo); err == nil {
			t.Errorf("ClearAutofixEnabled(%q) succeeded", repo)
		}
	}
}

// Off stops FIXING, not watching: the pull request is still observed and still
// reviewed, so its feedback arrives for a person to act on.
func TestAutofixOffStillWatchesAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/quiet": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 5, "aaaaaaaa1"
	gh.pulls[fakeKey("owner/quiet", 5)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/quiet", 5, "aaaaaaaa1", PhaseQueued, time.Now().UTC(), 0)
	if _, err := svc.SetAutofixEnabled(ctx, "owner/quiet", false, "release branch"); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	err := svc.Watch(ctx, WatchOptions{Once: true, Command: []string{"/bin/true"}},
		func(e WatchEvent) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("a repository with autofix off stopped being watched")
	}
	for _, e := range events {
		if e.Dispatched {
			t.Errorf("a session ran for a repository with autofix off: %+v", e)
		}
	}
}

func TestSkippedDispatchStillAdvancesHeadWithCarriedFinding(t *testing.T) {
	base := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 16
	old, head := "aaaaaaaa1", "bbbbbbbb2"
	f.openPull(repo, pr, head)
	f.gh.mu.Lock()
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Number = pr
	f.gh.pulls[fakeKey(repo, pr)] = pull
	f.gh.mu.Unlock()
	f.setCommitDate(old, base.Add(-2*time.Minute))
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	f.botReview(repo, pr, 900, old, base.Add(-time.Minute))
	f.botReviewComment(repo, pr, 901, old, "internal/state/state.go", 42,
		"_⚠️ Potential issue_\n\nThis dereferences a nil round.")
	if _, err := f.svc.SetAutofixEnabled(f.ctx, repo, false, "operator stop"); err != nil {
		t.Fatal(err)
	}

	err := f.svc.Watch(f.ctx, WatchOptions{
		Repos: []string{repo}, Once: true, Dispatch: dispatchOn(), Command: []string{"/bin/true"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("current head review posts = %d, want 1 after skipped carried fix", got)
	}
	if round := f.round(repo, pr); round == nil || round.Head != head {
		t.Fatalf("current head was not advanced after the skipped dispatch: %+v", round)
	}
}
