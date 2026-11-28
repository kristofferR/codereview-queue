package state

import (
	"reflect"
	"testing"
	"time"
)

// A renamed repository keeps its pull request numbers, so everything recorded
// per PR is still about the same PR and has to follow the name.
func TestRenameRepoMovesEveryRecordAndTheNewNameWins(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	st := New()
	st.SetEnrollment("o/old", RepoEnrollment{Enabled: true, By: "host"})
	st.Repos = map[string]RepoReviewers{"o/old": {}}
	st.RepoAutofix = map[string]RepoAutofixSwitch{"o/old": {}, "o/new": {}}
	st.PutRound(Round{Repo: "o/old", PR: 7, Head: "aaa", Phase: PhaseCompleted})
	st.PutRound(Round{Repo: "o/old", PR: 8, Head: "old-head", Phase: PhaseCompleted})
	st.PutRound(Round{Repo: "o/new", PR: 8, Head: "new-head", Phase: PhaseQueued})
	st.PutRound(Round{Repo: "o/older", PR: 7, Head: "bbb", Phase: PhaseCompleted}) // shares a prefix, not a name
	st.FireSlot = &FireSlot{Key: Key("o/old", 7)}
	st.Hold("o/old", 7, "wait", "me", now)
	st.ReviewedHeads = map[string][]string{Key("o/old", 7): {"aaa"}}
	st.TidiedCommands = map[string][]PostedCommand{Key("o/old", 7): {{ID: 1}}, Key("o/new", 7): {{ID: 2}}}
	st.Archive = []Round{{Repo: "o/old", PR: 3}}
	st.Fleet.Env = map[string]string{"CRQ_REPOS": "o/keep,o/old", "CRQ_EXCLUDE": "o/old,o/new"}

	if !st.RenameRepo("O/Old", "o/new") {
		t.Fatal("RenameRepo reported no change")
	}
	if _, ok := st.Enrollment("o/old"); ok {
		t.Error("the old name kept its enrollment")
	}
	if rec, ok := st.Enrollment("o/new"); !ok || !rec.Enabled {
		t.Errorf("enrollment under the new name = %+v, %v", rec, ok)
	}
	if _, ok := st.Repos["o/new"]; !ok || len(st.Repos) != 1 {
		t.Errorf("reviewer overrides = %v", st.Repos)
	}
	if len(st.RepoAutofix) != 1 {
		t.Errorf("autofix switches = %v, want only the new name's", st.RepoAutofix)
	}
	if r := st.Round("o/new", 7); r == nil || r.Repo != "o/new" || r.Head != "aaa" {
		t.Errorf("round 7 under the new name = %+v", r)
	}
	if r := st.Round("o/new", 8); r == nil || r.Head != "new-head" {
		t.Errorf("round 8 = %+v, want the one already under the new name", r)
	}
	if st.Round("o/old", 7) != nil || st.Round("o/old", 8) != nil {
		t.Error("rounds were left under the old name")
	}
	if st.Round("o/older", 7) == nil {
		t.Error("a repository whose name merely starts the same was renamed too")
	}
	if st.FireSlot.Key != Key("o/new", 7) {
		t.Errorf("fire slot = %q, want it to follow the round", st.FireSlot.Key)
	}
	if _, held := st.HeldPR("o/new", 7); !held {
		t.Error("the hold did not follow the pull request")
	}
	if got := st.ReviewedHeads[Key("o/new", 7)]; !reflect.DeepEqual(got, []string{"aaa"}) {
		t.Errorf("reviewed heads = %v", got)
	}
	if got := st.TidiedCommands[Key("o/new", 7)]; len(got) != 1 || got[0].ID != 2 || len(st.TidiedCommands) != 1 {
		t.Errorf("tombstones = %v, want the new name's kept and the old key gone", st.TidiedCommands)
	}
	if st.Archive[0].Repo != "o/new" {
		t.Errorf("archived round still names %q", st.Archive[0].Repo)
	}
	if got := st.Fleet.Env["CRQ_REPOS"]; got != "o/keep,o/new" {
		t.Errorf("fleet CRQ_REPOS = %q", got)
	}
	if got := st.Fleet.Env["CRQ_EXCLUDE"]; got != "o/new" {
		t.Errorf("fleet CRQ_EXCLUDE = %q, want the old name dropped beside the new one", got)
	}
	if st.RenameRepo("o/old", "o/new") {
		t.Error("a second rename found something left to move")
	}
	// The rename is remembered, so the old name still leads to the records.
	if got := st.ResolveRepo("O/Old"); got != "o/new" {
		t.Errorf("ResolveRepo(old) = %q, want the new name", got)
	}
	if got := st.ResolveRepo("o/new"); got != "o/new" {
		t.Errorf("ResolveRepo(new) = %q, want itself", got)
	}
}

func TestRenameRepoKeepsEarlierNamesPointingAtTheCurrentOne(t *testing.T) {
	st := New()
	st.RenameRepo("o/first", "o/second")
	st.RenameRepo("o/second", "o/third")
	for _, old := range []string{"o/first", "o/second"} {
		if got := st.ResolveRepo(old); got != "o/third" {
			t.Errorf("ResolveRepo(%s) = %q, want o/third", old, got)
		}
	}
	// Renamed back: the name that is current again leads nowhere.
	st.RenameRepo("o/third", "o/first")
	if got := st.ResolveRepo("o/first"); got != "o/first" {
		t.Errorf("ResolveRepo(o/first) = %q after renaming back, want itself", got)
	}
	if got := st.ResolveRepo("o/third"); got != "o/first" {
		t.Errorf("ResolveRepo(o/third) = %q, want o/first", got)
	}
	if !st.ForgetAlias("o/third") || st.ForgetAlias("o/third") || st.ResolveRepo("o/third") != "o/third" {
		t.Error("ForgetAlias must drop the alias exactly once")
	}
}

// Something holding the repository's name in its own memory must be able to
// find its claim again.
func TestBusyPullsWhileASessionOrALoopHoldsTheName(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	for name, hold := range map[string]func(*State){
		"dispatch claim": func(st *State) {
			st.Dispatches = map[string]DispatchClaim{Key("o/app", 1): {Token: "t", Heartbeat: now}}
		},
		"work claim": func(st *State) {
			st.WorkClaims = map[string]WorkClaim{Key("o/app", 1): {Owner: "me", ExpiresAt: now.Add(time.Hour)}}
		},
	} {
		st := New()
		if got := st.BusyPulls("o/app", now); len(got) != 0 {
			t.Fatalf("%s: an idle repository reads as busy: %v", name, got)
		}
		hold(&st)
		if got := st.BusyPulls("O/App", now); !reflect.DeepEqual(got, []int{1}) {
			t.Errorf("%s: busy pull requests = %v, want #1", name, got)
		}
		if got := st.BusyPulls("o/app", now.Add(48*time.Hour)); len(got) != 0 {
			t.Errorf("%s: an expired claim still makes the repository busy: %v", name, got)
		}
		if got := st.BusyPulls("o/application", now); len(got) != 0 {
			t.Errorf("%s: a repository whose name merely starts the same reads as busy: %v", name, got)
		}
	}
}

// A fired round's posted command holds the fire slot. Moving records while it
// does could leave the slot naming a round with another token, which reads as
// free while the command is still unanswered.
func TestBusyPullsIncludesTheRoundHoldingTheFireSlot(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	st := New()
	st.PutRound(Round{Repo: "o/app", PR: 5, Phase: PhaseFired, Token: "tok"})
	st.FireSlot = &FireSlot{Key: Key("o/app", 5), Token: "tok"}
	if got := st.BusyPulls("o/app", now); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("busy pull requests = %v, want the slot's round", got)
	}
}

func TestDropRepoForgetsARepositoryAndNothingElse(t *testing.T) {
	st := New()
	st.SetEnrollment("o/gone", RepoEnrollment{Enabled: true})
	st.SetEnrollment("o/kept", RepoEnrollment{Enabled: true})
	st.PutRound(Round{Repo: "o/gone", PR: 1, Phase: PhaseFired, Token: "t"})
	st.PutRound(Round{Repo: "o/kept", PR: 1, Phase: PhaseQueued})
	st.FireSlot = &FireSlot{Key: Key("o/gone", 1), Token: "t"}
	st.WrittenCommands = map[string][]PostedCommand{Key("o/gone", 1): {{ID: 1}}, Key("o/kept", 1): {{ID: 2}}}
	st.Archive = []Round{{Repo: "o/gone", PR: 2}, {Repo: "o/kept", PR: 2}}
	st.NoteRepoMissing("o/gone", time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC), true)
	st.RepoAliases = map[string]string{"o/before": "o/gone"}

	if !st.DropRepo("o/gone") {
		t.Fatal("DropRepo reported no change")
	}
	if got := st.KnownRepos(); !reflect.DeepEqual(got, []string{"o/kept"}) {
		t.Fatalf("known repositories = %v, want only o/kept", got)
	}
	if st.SlotRound() != nil {
		t.Error("the fire slot still names a round that no longer exists")
	}
	if _, marked := st.RepoMissing("o/gone"); marked || st.ResolveRepo("o/before") != "o/before" {
		t.Error("the missing mark or an alias outlived the repository")
	}
	if st.DropRepo("o/gone") {
		t.Error("a second drop found something left")
	}
}

// Forgetting waits on days a lookup actually said "missing", so a week in which
// nobody looked counts for nothing.
func TestNoteRepoMissingCountsSeparateDaysOnly(t *testing.T) {
	day := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	st := New()
	first, changed := st.NoteRepoMissing("O/App", day, true)
	if !changed || first.Days != 1 || !first.Since.Equal(day) {
		t.Fatalf("first confirmation = %+v, %v", first, changed)
	}
	if again, changed := st.NoteRepoMissing("o/app", day.Add(8*time.Hour), true); changed || again.Days != 1 {
		t.Fatalf("a second confirmation the same day = %+v, %v; want it not counted", again, changed)
	}
	// Nine days later, having looked on none of the days between.
	later, changed := st.NoteRepoMissing("o/app", day.Add(9*24*time.Hour), true)
	if !changed || later.Days != 2 || !later.Since.Equal(day) {
		t.Fatalf("after a gap = %+v, %v; want two days counted, not nine", later, changed)
	}
	if !st.ClearRepoMissing("o/app") || st.ClearRepoMissing("o/app") {
		t.Fatal("clearing must change the record once")
	}
	if _, marked := st.RepoMissing("o/app"); marked {
		t.Fatal("still marked after clearing")
	}

	// Unconfirmed: marked, so that it is skipped, but no day counts towards
	// forgetting it.
	unsure, changed := st.NoteRepoMissing("o/app", day, false)
	if !changed || unsure.Days != 0 {
		t.Fatalf("an unconfirmed mark = %+v, %v; want it marked with no days counted", unsure, changed)
	}
	if _, changed := st.NoteRepoMissing("o/app", day.Add(24*time.Hour), false); changed {
		t.Fatal("an unconfirmed lookup on a later day changed the record")
	}
}

// A confirmation from a lookup that began before midnight, landing on a record
// a later one already counted, is not a new day.
func TestNoteRepoMissingNeverCountsAnEarlierDay(t *testing.T) {
	st := New()
	day := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	st.NoteRepoMissing("o/app", day, true)
	if late, changed := st.NoteRepoMissing("o/app", day.Add(-12*time.Hour), true); changed || late.Days != 1 || late.LastDay != "2026-11-28" {
		t.Fatalf("an earlier day = %+v, %v; want nothing counted and LastDay unchanged", late, changed)
	}
}

// The name GitHub just answered with is plainly not missing.
func TestRenameRepoClearsTheMissingMarkOfTheNewNameToo(t *testing.T) {
	st := New()
	day := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	st.NoteRepoMissing("o/new", day, true)
	st.RenameRepo("o/old", "o/new")
	if _, marked := st.RepoMissing("o/new"); marked {
		t.Fatal("the name a rename resolved to is still marked missing")
	}
}
