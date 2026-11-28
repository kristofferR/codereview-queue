package crq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

func autoReviewConfig() Config {
	cfg := firingConfig()
	cfg.LeaderTTL = time.Minute
	cfg.AutoReviewMaxScan = 10
	return cfg
}

func openPull(gh *fakeGitHub, repo string, pr int, sha string) {
	var pull ghapi.Pull
	pull.State = "open"
	pull.Number = pr
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.searchPRs = append(gh.searchPRs, ghapi.SearchPR{Repo: repo, Number: pr, Author: "alice"})
}

func loadState(t *testing.T, store StateStore) State {
	t.Helper()
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Searching for a renamed repository by its old name fails outright, and the
// pass used to report failure every minute for as long as the name stayed on
// record. The lookup follows the rename, and the records follow the lookup.
func TestAutoReviewFollowsARenamedRepository(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/keep": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 7, "abcdef1234567890")
	openPull(gh, "o/new", 9, "9999999912345678")
	openPull(gh, "o/keep", 1, "1111111112345678")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}
	// #7 was already reviewed at this head, under the name it had then.
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/old", PR: 7, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if _, ok := st.Enrollment("o/old"); ok {
		t.Error("the old name is still enrolled")
	}
	if !svc.EnrollmentIn(st, "o/new").Enabled {
		t.Error("the repository stopped being reviewed when it was renamed")
	}
	if r := st.Round("o/new", 7); r == nil || r.Phase != PhaseCompleted {
		t.Errorf("round for #7 = %+v, want the completed one carried over rather than a second review", r)
	}
	if st.Round("o/old", 7) != nil {
		t.Error("a round was left under the old name")
	}
	if st.Round("o/new", 9) == nil || st.Round("o/keep", 1) == nil {
		t.Errorf("open pull requests were not enqueued: %v", st.Rounds)
	}
	if gh.searches != 0 {
		t.Errorf("made %d search request(s); repositories named outright are listed in one lookup", gh.searches)
	}
}

// A name this host's env enrolls leaves no record to move. The repository must
// go on being reviewed under its new name all the same.
func TestAutoReviewEnrollsTheNewNameOfAnEnvListedRepository(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/old": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 3, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	for pass := 1; pass <= 2; pass++ {
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	st := loadState(t, store)
	if view := svc.EnrollmentIn(st, "o/new"); !view.Enabled || view.Source != "state" {
		t.Fatalf("new name = %+v, want it enrolled by record", view)
	}
	if st.Round("o/new", 3) == nil || st.Round("o/old", 3) != nil {
		t.Fatalf("rounds = %v, want #3 tracked once, under the new name", st.Rounds)
	}
}

// An off record under the old name is usually the workaround for the rename
// itself. It must not be carried over to switch off a name that is listed
// outright — but an off switch nobody has contradicted survives the rename.
func TestRenameCarriesAnOffSwitchOnlyWhereNothingContradictsIt(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		allow       map[string]bool
		wantEnabled bool
	}{
		"new name listed in CRQ_REPOS": {allow: map[string]bool{"o/new": true}, wantEnabled: true},
		"nothing names the new name":   {allow: nil, wantEnabled: false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := autoReviewConfig()
			cfg.Scope = []string{"o"}
			cfg.AllowRepos = tc.allow
			gh := newFakeGitHub()
			gh.renamedRepos = map[string]string{"o/old": "o/new"}
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, gh, store, nil)
			if _, err := svc.SetEnrollment(ctx, "o/old", false, "stopped on purpose"); err != nil {
				t.Fatal(err)
			}
			if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
				t.Fatalf("AutoReview: %v", err)
			}
			st := loadState(t, store)
			if _, ok := st.Enrollment("o/old"); ok {
				t.Error("the old name kept its record")
			}
			if got := svc.EnrollmentIn(st, "o/new").Enabled; got != tc.wantEnabled {
				t.Errorf("new name enabled = %v, want %v", got, tc.wantEnabled)
			}
		})
	}
}

// "Not found" is also what a credential that lost its access sees, so a missing
// repository is skipped at once but only forgotten after it has been confirmed
// missing on enough separate days.
func TestAutoReviewSkipsAMissingRepositoryThenForgetsIt(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/keep": true}
	gh := newFakeGitHub()
	gh.goneRepos = map[string]bool{"o/gone": true}
	openPull(gh, "o/keep", 1, "1111111112345678")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	if _, err := svc.SetEnrollment(ctx, "o/gone", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/gone", PR: 4, Head: "abcdef123", Phase: PhaseQueued, Seq: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pass := func() State {
		t.Helper()
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("AutoReview: %v", err)
		}
		return loadState(t, store)
	}

	st := pass()
	if view := svc.EnrollmentIn(st, "o/gone"); view.Enabled || view.MissingSince == "" {
		t.Fatalf("missing repository = %+v, want it skipped and marked", view)
	}
	if st.Round("o/gone", 4) != nil {
		t.Error("a queued round for a repository that is gone was left to fire")
	}
	if st.Round("o/keep", 1) == nil {
		t.Error("a missing repository kept the rest of the fleet from being enqueued")
	}

	// Back within the week: reviewed again, without anyone re-enrolling it.
	gh.goneRepos = nil
	openPull(gh, "o/gone", 4, "abcdef1234567890")
	st = pass()
	if view := svc.EnrollmentIn(st, "o/gone"); !view.Enabled || view.MissingSince != "" {
		t.Fatalf("returned repository = %+v, want it reviewed again", view)
	}
	// Its pull requests were not listed while it was missing, so they are
	// picked up by the pass after the one that noticed it was back.
	if st = pass(); st.Round("o/gone", 4) == nil {
		t.Error("the returned repository's open pull request was not enqueued")
	}

	// Gone for good. Passes on the same day count once, however many there are.
	gh.goneRepos = map[string]bool{"o/gone": true}
	for day := 1; day < repoMissingDays; day++ {
		pass()
		st = pass()
		if _, ok := st.Enrollment("o/gone"); !ok {
			t.Fatalf("forgotten on day %d, before it had been missing on %d separate days", day, repoMissingDays)
		}
		now = now.Add(24 * time.Hour)
	}
	st = pass()
	if _, ok := st.Enrollment("o/gone"); ok {
		t.Fatalf("still on record after being confirmed missing on %d separate days", repoMissingDays)
	}
	for _, repo := range st.KnownRepos() {
		if repo == "o/gone" {
			t.Fatalf("records for the forgotten repository remain: %v", st.KnownRepos())
		}
	}
}

// A repository only this host's CRQ_REPOS names has no record to carry a mark,
// and its rounds stall the queue all the same: every read of them fails.
func TestMissingRepositoryIsTakenOutOfSchedulingHoweverItWasEnrolled(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/gone": true, "o/keep": true}
	cfg.InflightTimeout = 10 * time.Minute
	gh := newFakeGitHub()
	gh.goneRepos = map[string]bool{"o/gone": true}
	openPull(gh, "o/keep", 1, "1111111112345678")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	fired := now.Add(-time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/gone", PR: 4, Head: "abcdef123", Phase: PhaseQueued, Seq: 1})
		st.PutRound(Round{Repo: "o/gone", PR: 5, Head: "abcdef456", Phase: PhaseFired, Seq: 2, Token: "tok", FiredAt: &fired})
		st.FireSlot = &FireSlot{Key: QueueKey("o/gone", 5), Token: "tok", Since: fired}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if st.Round("o/gone", 4) != nil || st.Round("o/gone", 5) != nil {
		t.Fatalf("rounds of a missing repository are still scheduled: %v", st.Rounds)
	}
	if svc.EnrollmentIn(st, "o/gone").Enabled {
		t.Error("an env-listed repository that is gone still reads as reviewed")
	}
	// The command the fired round posted may still be answered if the
	// repository is only invisible, so its slot outlives the round — but only
	// until that command's own deadline.
	if !st.SlotHeld(now) {
		t.Error("the fire slot was released while the posted command could still be in flight")
	}
	if st.SlotHeld(fired.Add(cfg.InflightTimeout + time.Second)) {
		t.Error("the fire slot stays held past the in-flight deadline: the fleet would stall behind a repository that is gone")
	}
}

func TestAutoReviewListsPastTheFirstPageWithinItsScanBudget(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/app": true}
	cfg.AutoReviewMaxScan = 3
	gh := newFakeGitHub()
	gh.pageSize = 2
	for pr := 1; pr <= 5; pr++ {
		openPull(gh, "o/app", pr, "abcdef123456789"+string(rune('0'+pr)))
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if len(st.Rounds) != 3 || st.Round("o/app", 3) == nil {
		t.Fatalf("rounds = %d, want the first three: two from the first page and one from the next", len(st.Rounds))
	}
}

func TestAutoReviewReportsALookupItCouldNotMake(t *testing.T) {
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/app": true}
	gh := newFakeGitHub()
	gh.lookupErr = errors.New("Something went wrong while executing your query")
	openPull(gh, "o/app", 1, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	err := svc.AutoReview(context.Background(), AutoOptions{Once: true, Incremental: true})
	if err == nil || err.Error() != gh.lookupErr.Error() {
		t.Fatalf("AutoReview error = %v, want the failed lookup reported", err)
	}
	if st := loadState(t, store); len(st.Rounds) != 0 {
		t.Fatalf("rounds = %v, want nothing enqueued from a listing that never arrived", st.Rounds)
	}
}

// The watcher lists pull requests by REST, which follows a rename silently. It
// has to notice from the listing itself, or it tracks every pull request under
// both names and reviews each twice.
func TestWatchFollowsARenamedRepository(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number = "open", 7
	pull.Head.SHA = "abcdef1234567890"
	pull.Base.Repo.FullName = "o/New"
	gh.pulls[fakeKey("o/old", 7)] = pull // reached through the redirect
	gh.pulls[fakeKey("o/new", 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, store)
	if st.Round("o/new", 7) == nil || st.Round("o/old", 7) != nil {
		t.Fatalf("rounds = %v, want #7 tracked once, under the new name", st.Rounds)
	}
	if _, ok := st.Enrollment("o/old"); ok {
		t.Error("the old name is still enrolled")
	}
	if len(events) != 1 || events[0].Repo != "o/new" {
		t.Fatalf("events = %+v, want one, for the repository's current name", events)
	}
}

// A merge is only noticed while a round or the archive still points at the pull
// request. One that merged after both had moved on kept its records for ever.
func TestAutoReviewRetiresRecordsOfPullRequestsThatMergedUnseen(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/app": true}
	gh := newFakeGitHub()
	var merged, closed ghapi.Pull
	merged.State, merged.Merged = "closed", true
	closed.State = "closed"
	gh.pulls[fakeKey("o/app", 1)] = merged
	gh.pulls[fakeKey("o/app", 2)] = closed
	openPull(gh, "o/app", 3, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	if _, err := store.Update(ctx, func(st *State) error {
		for pr := 1; pr <= 3; pr++ {
			st.RecordTidied("o/app", pr, PostedCommand{ID: int64(pr), Bot: "coderabbitai", At: now})
		}
		st.PutRound(Round{Repo: "o/app", PR: 3, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pass := func() State {
		t.Helper()
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("AutoReview: %v", err)
		}
		return loadState(t, store)
	}

	st := pass()
	if len(st.TidiedCommands[QueueKey("o/app", 1)]) != 0 {
		t.Error("a merged pull request kept its tombstones")
	}
	// Closed can reopen, and open is still being worked: both keep theirs.
	if len(st.TidiedCommands[QueueKey("o/app", 2)]) != 1 || len(st.TidiedCommands[QueueKey("o/app", 3)]) != 1 {
		t.Errorf("tombstones = %v, want the closed and the open pull request's kept", st.TidiedCommands)
	}
	// The open one has a round, so only the two without were asked about.
	if len(gh.mergedAsked) != 1 || gh.mergedAsked[0] != "o/app: 2" {
		t.Fatalf("merge queries = %v, want one, for the two pull requests no round covers", gh.mergedAsked)
	}

	pass()
	if len(gh.mergedAsked) != 1 {
		t.Fatalf("merge queries = %v, want the repository left alone until the sweep comes round again", gh.mergedAsked)
	}
	now = now.Add(mergedSweepEvery + time.Minute)
	pass()
	if len(gh.mergedAsked) != 2 || gh.mergedAsked[1] != "o/app: 1" {
		t.Fatalf("merge queries = %v, want the closed pull request asked about again", gh.mergedAsked)
	}
}

// A record that says off under the new name is somebody's decision about that
// repository. A rename arriving from an enabled old name must not undo it.
func TestRenameDoesNotOverrideAnOffSwitchAtTheNewName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/keep": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 7, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnrollment(ctx, "o/new", false, "not this one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/old", PR: 7, Head: "abcdef123", Phase: PhaseQueued, Seq: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if view := svc.EnrollmentIn(st, "o/new"); view.Enabled || view.Reason != "not this one" {
		t.Fatalf("new name = %+v, want its off record left exactly as it was", view)
	}
	// The queued round arrived under a name crq does not review.
	if r := st.Round("o/new", 7); r != nil && r.Active() {
		t.Fatalf("a round moved onto a switched-off repository is still waiting to fire: %+v", r)
	}
}

// A fix session or an interactive loop holds the old name in its own memory.
// Moving its claim from under it reads as "nobody is working on this".
func TestRenameWaitsForWorkInFlightUnderTheOldName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/new": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 7, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/old", PR: 7, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseCompleted})
		st.WorkClaims = map[string]WorkClaim{QueueKey("o/old", 7): {Owner: "agent", ClaimedAt: now, ExpiresAt: now.Add(time.Hour)}}
		// Already waiting under the new name, from when both names were tracked.
		st.PutRound(Round{Repo: "o/new", PR: 7, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseQueued, Seq: 9})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pass := func() State {
		t.Helper()
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("AutoReview: %v", err)
		}
		return loadState(t, store)
	}

	st := pass()
	if st.Round("o/old", 7) == nil || len(st.WorkClaims) != 1 || st.ResolveRepo("o/old") != "o/old" {
		t.Fatalf("records moved while a loop still held the old name: rounds=%v claims=%v", st.Rounds, st.WorkClaims)
	}
	// And the new name is left alone meanwhile: a review of #7 fired there would
	// be a second one of a head the old name's round says was reviewed — whether
	// the round came from this pass or was already waiting.
	if st.Round("o/new", 7) != nil {
		t.Fatal("the pull request is queued under the new name while its records are still under the old one")
	}

	now = now.Add(2 * time.Hour)
	st = pass()
	if r := st.Round("o/new", 7); r == nil || r.Phase != PhaseCompleted || st.Round("o/old", 7) != nil {
		t.Fatalf("after the claim expired, rounds = %v; want the completed round moved", st.Rounds)
	}
	// A checkout that still names the old repository is led to the records.
	if got := svc.CurrentRepo(ctx, "o/Old"); got != "o/new" {
		t.Fatalf("CurrentRepo(old) = %q, want o/new", got)
	}
}

// The kill switch is a statement about the repository. Renaming it must not be
// a way out from under a host's CRQ_EXCLUDE.
func TestExcludedRepositoryStaysExcludedUnderItsNewName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.Scope = []string{"o"}
	cfg.ExcludeRepos = map[string]bool{"o/old": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 7, "abcdef1234567890")
	openPull(gh, "o/keep", 1, "1111111112345678")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if view := svc.EnrollmentIn(st, "o/new"); view.Enabled || view.Source != "excluded" {
		t.Fatalf("new name = %+v, want it excluded like the name CRQ_EXCLUDE lists", view)
	}
	if st.Round("o/new", 7) != nil {
		t.Fatal("an excluded repository was enqueued once it had been renamed")
	}
	if st.Round("o/keep", 1) == nil {
		t.Fatal("the rest of the scope must still be reviewed")
	}
}

func TestReconcileWritesNothingOnADryRun(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/keep": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	gh.goneRepos = map[string]bool{"o/gone": true}
	store := NewMemoryStore(cfg)
	if _, err := NewService(cfg, gh, store, nil).SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(cfg, gh, store, nil).SetEnrollment(ctx, "o/gone", true, ""); err != nil {
		t.Fatal(err)
	}
	before := loadState(t, store)

	dry := cfg
	dry.DryRun = true
	svc := NewService(dry, gh, store, nil)
	looked, err := gh.LookupRepos(ctx, nil, []string{"o/old", "o/gone", "o/keep"})
	if err != nil {
		t.Fatal(err)
	}
	seen, _, err := svc.reconcileRepos(ctx, before, looked, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pass carries on with the repositories as they would be...
	if !svc.EnrollmentIn(seen, "o/new").Enabled || svc.EnrollmentIn(seen, "o/gone").Enabled {
		t.Fatalf("dry-run view: new enabled = %v, gone enabled = %v", svc.EnrollmentIn(seen, "o/new").Enabled, svc.EnrollmentIn(seen, "o/gone").Enabled)
	}
	// ...and the shared state is exactly what it was.
	after := loadState(t, store)
	if _, ok := after.Enrollment("o/old"); !ok || after.Rev != before.Rev {
		t.Fatalf("a dry run wrote state: rev %d -> %d, old name enrolled = %v", before.Rev, after.Rev, ok)
	}
	if _, marked := after.RepoMissing("o/gone"); marked {
		t.Fatal("a dry run marked a repository missing")
	}
}

// One repository GitHub could not answer for is that repository's failure. The
// others are scanned, and a one-shot run still says that this one was not.
func TestAutoReviewReportsARepositoryItCouldNotLookUp(t *testing.T) {
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/app": true, "o/odd": true}
	gh := newFakeGitHub()
	gh.lookupRepoErrs = map[string]error{"o/odd": errors.New("Resource protected by organization SAML enforcement")}
	openPull(gh, "o/app", 1, "abcdef1234567890")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Not a scan target, only a repository crq has records for: still a check
	// that did not happen.
	if _, err := svc.SetEnrollment(context.Background(), "o/parked", false, "parked"); err != nil {
		t.Fatal(err)
	}
	gh.lookupRepoErrs["o/parked"] = errors.New("Something went wrong while executing your query")

	err := svc.AutoReview(context.Background(), AutoOptions{Once: true, Incremental: true})
	if err == nil || !strings.Contains(err.Error(), "o/odd") || !strings.Contains(err.Error(), "o/parked") {
		t.Fatalf("AutoReview error = %v, want both repositories it could not look up named", err)
	}
	if st := loadState(t, store); st.Round("o/app", 1) == nil {
		t.Fatal("one unreadable repository kept another from being enqueued")
	}
}

// The lookup can sit out a throttle for longer than the lease lasts. What it
// says may only be applied by a process that is still the leader — and that is
// checked on the state the write lands on, not against this pass's own clock,
// which starts too late to vouch for a lease that ran out earlier in the pass.
func TestAutoReviewAppliesNothingAfterLosingTheLeaseDuringTheLookup(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/keep": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/keep", 1, "1111111112345678")
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}
	gh.lookupHook = func() {
		if _, err := store.Update(ctx, func(st *State) error {
			st.Leader = &LeaderLease{Owner: "standby", Token: "theirs", ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}
			return nil
		}); err != nil {
			t.Errorf("standby takeover: %v", err)
		}
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if _, ok := st.Enrollment("o/old"); !ok {
		t.Error("a pass that had lost the lease still moved a repository's records")
	}
	if len(st.Rounds) != 0 {
		t.Errorf("a pass that had lost the lease still enqueued: %v", st.Rounds)
	}
}

// With both names in its list, the watcher must move the old name's records
// before it decides the old name is a duplicate — it may be the only daemon
// running, and the current name is about to be watched with or without them.
func TestWatchMovesRecordsEvenWhenTheNewNameIsListedToo(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/new": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number = "open", 7
	pull.Head.SHA = "abcdef1234567890"
	pull.Base.Repo.FullName = "o/new"
	gh.pulls[fakeKey("o/old", 7)] = pull
	gh.pulls[fakeKey("o/new", 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/old", true, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("o/old", 7, "do not review yet", "me", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, store)
	if _, held := st.HeldPR("o/new", 7); !held {
		t.Fatal("the hold placed under the old name did not reach the pull request's current name")
	}
	if st.Round("o/old", 7) != nil {
		t.Fatalf("the pull request is tracked under the old name: %v", st.Rounds)
	}
	if len(events) != 1 || events[0].Repo != "o/new" {
		t.Fatalf("events = %+v, want the pull request handled once, under its current name", events)
	}
}

// A fleet with one repository has nothing else to resolve beside it. It is
// still taken out of scheduling when it disappears — every read of its rounds
// fails — but a lookup that resolved nothing is no evidence that it is gone for
// good, so it is never forgotten on that alone.
func TestSoleMissingRepositoryIsSkippedButNeverForgotten(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/only": true}
	gh := newFakeGitHub()
	gh.goneRepos = map[string]bool{"o/only": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	reserved := now.Add(-time.Hour) // a poster that died long ago
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/only", PR: 4, Head: "abcdef123", Phase: PhaseReserved, Seq: 1, Token: "tok", ReservedAt: &reserved})
		st.FireSlot = &FireSlot{Key: QueueKey("o/only", 4), Token: "tok", Since: reserved}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for day := 0; day <= repoMissingDays+1; day++ {
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
		st := loadState(t, store)
		missing, marked := st.RepoMissing("o/only")
		if !marked || missing.Days != 0 {
			t.Fatalf("day %d: missing record = %+v (marked %v), want it marked with no day counted", day, missing, marked)
		}
		if st.Round("o/only", 4) != nil || st.SlotHeld(now) {
			t.Fatalf("day %d: a dead poster's reserved round still holds the queue", day)
		}
		now = now.Add(24 * time.Hour)
	}
}

// Marked missing under its old name, then found again under a new one: what
// enrolled it carries over, although it read as "not reviewed" in between.
func TestRepositoryFoundAgainUnderANewNameKeepsItsEnrollment(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/old": true, "o/keep": true}
	gh := newFakeGitHub()
	gh.goneRepos = map[string]bool{"o/old": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	pass := func() State {
		t.Helper()
		if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
			t.Fatalf("AutoReview: %v", err)
		}
		return loadState(t, store)
	}
	st := pass()
	if _, marked := st.RepoMissing("o/old"); !marked {
		t.Fatal("fixture: the repository was not marked missing")
	}

	gh.goneRepos, gh.renamedRepos = nil, map[string]string{"o/old": "o/new"}
	st = pass()
	if !svc.EnrollmentIn(st, "o/new").Enabled {
		t.Fatal("a repository this host's env enrolls stopped being reviewed once it resurfaced under a new name")
	}
	if _, marked := st.RepoMissing("o/old"); marked {
		t.Fatal("the missing mark outlived the rename that explained it")
	}
}

// CRQ_EXCLUDE is this host's own kill switch. It decides what this host does;
// it says nothing about rounds the rest of the fleet still wants reviewed.
func TestRenameDoesNotAbandonSharedRoundsOverAHostLocalExclusion(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.Scope = []string{"o"}
	cfg.ExcludeRepos = map[string]bool{"o/old": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetEnrollment("o/old", RepoEnrollment{Enabled: true, By: "another-host"})
		st.PutRound(Round{Repo: "o/old", PR: 7, Head: "abcdef123", Phase: PhaseQueued, Seq: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	looked, err := gh.LookupRepos(ctx, nil, []string{"o/old", "o/keep"})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.reconcileRepos(ctx, loadState(t, store), looked, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Round("o/new", 7); r == nil || r.Phase != PhaseQueued {
		t.Fatalf("round under the new name = %+v, want it still queued for the hosts that review it", r)
	}
	if svc.EnrollmentIn(st, "o/new").Enabled {
		t.Fatal("this host's exclusion stopped applying under the new name")
	}
}

// A host may be the only one that excludes a repository, and then nobody else
// ever asks what its old name is called now.
func TestWatchKeepsAnExclusionAcrossARenameOnItsOwn(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/new": true}
	cfg.ExcludeRepos = map[string]bool{"o/old": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	var pull ghapi.Pull
	pull.State, pull.Number = "open", 7
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/new", 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := loadState(t, store); len(events) != 0 || st.Round("o/new", 7) != nil {
		t.Fatalf("an excluded repository was watched under its new name: events=%+v rounds=%v", events, st.Rounds)
	}
}

// A reviewer trigger being posted under the old name is work in flight too, and
// the same pull request must not be reviewed from the other side meanwhile.
func TestRenameWaitsForATriggerBeingPostedUnderTheOldName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/new": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	openPull(gh, "o/new", 7, "abcdef1234567890")
	gh.pulls[fakeKey("o/old", 7)] = gh.pulls[fakeKey("o/new", 7)]
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/old", PR: 7, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseReviewing,
			CoBots: map[string]CoBotRound{"cursor": {ClaimedAt: &now}}})
		st.PutRound(Round{Repo: "o/new", PR: 7, Head: dialect.ShortOID("abcdef1234567890"), Phase: PhaseQueued, Seq: 9})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	st := loadState(t, store)
	if st.Round("o/old", 7) == nil || st.ResolveRepo("o/old") != "o/old" {
		t.Fatal("records moved while a trigger was still being posted under the old name")
	}
	if r := st.Round("o/new", 7); r != nil && r.Active() {
		t.Fatalf("the pull request is still queued under the new name: %+v", r)
	}
}

// A lease that ran out before the pass got to its first heartbeat — in the
// host report, the state read or the lookup — is caught even when the lookup
// has nothing to write: the scan and the pump that follow are a leader's work.
func TestAutoReviewStopsAfterItsLeaseRanOutBeforeTheFirstHeartbeat(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.LeaderTTL = 80 * time.Millisecond
	cfg.AllowRepos = map[string]bool{"o/app": true}
	gh := newFakeGitHub()
	openPull(gh, "o/app", 1, "abcdef1234567890")
	store := &slowFirstLoadStore{StateStore: NewMemoryStore(cfg), delay: cfg.LeaderTTL}
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatalf("AutoReview: %v", err)
	}
	if st := loadState(t, store); len(st.Rounds) != 0 {
		t.Fatalf("a pass that had lost the lease still enqueued: %v", st.Rounds)
	}
}

// slowFirstLoadStore stalls the first state read past the lease, during which a
// standby takes the lease over.
type slowFirstLoadStore struct {
	StateStore
	delay time.Duration
	done  bool
}

func (s *slowFirstLoadStore) Load(ctx context.Context) (State, Revision, error) {
	st, rev, err := s.StateStore.Load(ctx)
	if s.done || err != nil {
		return st, rev, err
	}
	s.done = true
	time.Sleep(s.delay)
	_, err = s.StateStore.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{Owner: "standby", Token: "theirs", ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}
		return nil
	})
	return st, rev, err
}

// The exclusion lookup can find a rename it has to defer. The new name has no
// alias leading to the kill switch yet, so the watcher must leave it alone too.
func TestWatchLeavesADeferredRenameOfAnExcludedRepositoryAlone(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/new": true}
	cfg.ExcludeRepos = map[string]bool{"o/old": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	var pull ghapi.Pull
	pull.State, pull.Number = "open", 7
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/new", 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.WorkClaims = map[string]WorkClaim{QueueKey("o/old", 3): {Owner: "agent", ClaimedAt: now, ExpiresAt: now.Add(time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := loadState(t, store); len(events) != 0 || st.Round("o/new", 7) != nil {
		t.Fatalf("an excluded repository was watched under its new name before the rename could be recorded: events=%+v rounds=%v", events, st.Rounds)
	}
}

// A repository whose merge query keeps failing is set aside for a while, so the
// repositories sorted after it are still reached.
func TestMergedSweepSetsAsideARepositoryThatKeepsFailing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := &mergeFailingGitHub{fakeGitHub: newFakeGitHub(), failing: "o/a"}
	var merged ghapi.Pull
	merged.State, merged.Merged = "closed", true
	gh.pulls[fakeKey("o/b", 1)] = merged
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.RecordTidied("o/a", 1, PostedCommand{ID: 1, Bot: "coderabbitai", At: time.Now()})
		st.RecordTidied("o/b", 1, PostedCommand{ID: 2, Bot: "coderabbitai", At: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 2; pass++ {
		_ = svc.sweepMergedRecords(ctx, loadState(t, store))
	}
	st := loadState(t, store)
	if len(st.TidiedCommands[QueueKey("o/b", 1)]) != 0 {
		t.Fatal("a repository sorted after one whose query keeps failing was never swept")
	}
	if len(st.TidiedCommands[QueueKey("o/a", 1)]) != 1 {
		t.Fatal("the failing repository's records were touched")
	}
}

type mergeFailingGitHub struct {
	*fakeGitHub
	failing string
}

func (f *mergeFailingGitHub) MergedPRs(ctx context.Context, repo string, prs []int) (map[int]bool, error) {
	if repo == f.failing {
		return nil, errors.New("Something went wrong while executing your query")
	}
	return f.fakeGitHub.MergedPRs(ctx, repo, prs)
}

// A rename whose write failed leaves the records — a hold among them — under
// the old name, so neither name may be acted on until a write lands.
func TestReconcileHoldsOffBothNamesWhenTheMoveCannotBeWritten(t *testing.T) {
	cfg := autoReviewConfig()
	store := &upstreamFailingStore{StateStore: NewMemoryStore(cfg), failUpdate: 1}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/old": "o/new"}
	svc := NewService(cfg, gh, store, nil)
	looked, err := gh.LookupRepos(context.Background(), nil, []string{"o/old"})
	if err != nil {
		t.Fatal(err)
	}
	_, holdOff, err := svc.reconcileRepos(context.Background(), State{}, looked, nil)
	if err == nil || !holdOff["o/old"] || !holdOff["o/new"] {
		t.Fatalf("holdOff = %v, err = %v; want both names held off after a failed write", holdOff, err)
	}
}

// Two old names of one repository, one listed by this host's env and one
// switched off by a record: the explicit off is what carries over.
func TestRenameKeepsAnExplicitOffOverAnEnvListingOfAnotherOldName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.AllowRepos = map[string]bool{"o/a": true}
	gh := newFakeGitHub()
	gh.renamedRepos = map[string]string{"o/a": "o/c", "o/b": "o/c"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/b", false, "stopped on purpose"); err != nil {
		t.Fatal(err)
	}
	looked, err := gh.LookupRepos(ctx, nil, []string{"o/a", "o/b"})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.reconcileRepos(ctx, loadState(t, store), looked, nil)
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := st.Enrollment("o/c"); !ok || record.Enabled {
		t.Fatalf("enrollment at o/c = %+v (%v), want the explicit off carried over", record, ok)
	}
}

// A repository named on the command line follows its recorded rename.
func TestWatchFollowsARecordedRenameOfAnExplicitTarget(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number = "open", 7
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/new", 7)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetEnrollment("o/old", RepoEnrollment{Enabled: true})
		st.RenameRepo("o/old", "o/new")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	if err := svc.watchPass(ctx, WatchOptions{Repos: []string{"o/old"}}, newDispatchPool(0), func(event WatchEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Repo != "o/new" {
		t.Fatalf("events = %+v, want the pull request watched under the name its records moved to", events)
	}
}

func TestEnrollingANewNameRespectsAnExclusionOfItsOldName(t *testing.T) {
	ctx := context.Background()
	cfg := autoReviewConfig()
	cfg.ExcludeRepos = map[string]bool{"o/old": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.RenameRepo("o/old", "o/new")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnrollment(ctx, "o/new", true, ""); err == nil {
		t.Fatal("enrolled a repository this host's CRQ_EXCLUDE names by its old name")
	}
}

// One turn a pass, least recently served first: a repository late in the
// alphabet is not kept waiting behind earlier ones.
func TestMergedSweepServesTheLeastRecentlySweptRepositoryFirst(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var merged ghapi.Pull
	merged.State, merged.Merged = "closed", true
	gh.pulls[fakeKey("o/a", 1)] = merged
	gh.pulls[fakeKey("o/z", 1)] = merged
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	svc.mergedSweep = map[string]mergedSweepCursor{"o/a": {doneAt: now.Add(-mergedSweepEvery - time.Hour)}}
	if _, err := store.Update(ctx, func(st *State) error {
		st.RecordTidied("o/a", 1, PostedCommand{ID: 1, Bot: "coderabbitai", At: now})
		st.RecordTidied("o/z", 1, PostedCommand{ID: 2, Bot: "coderabbitai", At: now})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.sweepMergedRecords(ctx, loadState(t, store)); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, store)
	if _, ok := st.TidiedCommands[QueueKey("o/z", 1)]; ok {
		t.Fatal("the never-swept repository waited behind one swept before")
	}
	if _, ok := st.TidiedCommands[QueueKey("o/a", 1)]; !ok {
		t.Fatal("more than one repository was swept in one pass")
	}
}
