package crq

import (
	"context"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
	"testing"
	"time"
)

// One fix session per pull request at a time. Observed live: three sessions ran
// on #54 at the SAME head, minutes apart, each pushing and each drawing a fresh
// review round — its findings climbed from 19 to 27 while they raced. State
// afterwards read `dispatch=NONE`, so nothing had refused the later passes.
//
// This is the claim doing its job or not; everything else in dispatch is
// downstream of it.
func TestSecondDispatchForOneHeadIsRefused(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	repo, pr, head := "o/r", 54, "b417a2161"
	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix",
		Findings: findingsAtHead(head, 3)}

	if ok, why, _ := svc.claimDispatch(ctx, report, "session-one", 3); !ok {
		t.Fatalf("the first dispatch was refused: %s", why)
	}

	// The next pass, while session one is still working. Nothing has changed:
	// same PR, same head, same findings.
	ok, why, byDesign := svc.claimDispatch(ctx, report, "session-two", 3)
	if ok {
		st, _, _ := store.Load(ctx)
		round := st.Round(repo, pr)
		t.Fatalf("a second session was allowed on the same head; round=%#v", round.Dispatch)
	}
	if !byDesign {
		t.Errorf("refusal %q should be a by-design skip, not a dispatcher failure", why)
	}

	// And the claim must actually be on the stored round — `dispatch=NONE` in
	// live state is what let every later pass through.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("no claim persisted for %s#%d: round=%#v", repo, pr, round)
	}
	if !round.DispatchHeld(now) {
		t.Errorf("claim is not held: %#v", round.Dispatch)
	}
}

// findingsAtHead builds n current-head findings, which is what makes a round
// look adoptable to the dispatcher.
func findingsAtHead(head string, n int) []dialect.Finding {
	out := make([]dialect.Finding, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, dialect.Finding{
			ID: string(rune('a'+i)) + "-finding", Bot: "coderabbitai[bot]",
			Commit: head, Source: "review_body", Title: "x", Body: "y",
		})
	}
	return out
}

// The claim survives one pass in isolation, so whatever loses it happens
// BETWEEN passes: every pass calls Next first, which enqueues and pumps. If any
// of that recreates the round, the claim goes with it and the next pass sees an
// unclaimed PR — which is what `dispatch=NONE` in live state meant.
func TestTheClaimSurvivesWhatAPassDoesBeforeIt(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	repo, pr, head := "o/r", 54, "b417a2161"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Number = pr
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	var commit ghapi.Commit
	commit.Committer.Date = now.Add(-time.Hour)
	gh.commits[head] = commit

	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix",
		Findings: findingsAtHead(head, 3)}
	if ok, why, _ := svc.claimDispatch(ctx, report, "session-one", 3); !ok {
		t.Fatalf("first dispatch refused: %s", why)
	}

	// What the next pass actually does before it would dispatch again: the whole
	// Next call, not a stand-in for it.
	if _, err := svc.Next(ctx, repo, pr); err != nil {
		t.Fatalf("next: %v", err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round == nil || round.Dispatch == nil {
		t.Fatalf("Next dropped the running session's claim: round=%#v", round)
	}

	if _, err := svc.Pump(ctx); err != nil {
		t.Fatalf("pump: %v", err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("pump dropped the running session's claim: round=%#v", round)
	}
	if ok, _, _ := svc.claimDispatch(ctx, report, "session-two", 3); ok {
		t.Error("a second session was allowed after a pass ran over the PR")
	}
}

// Two writers on one state ref, which is what was actually running: the autofix watcher
// and the autoreview daemon both drive this repository. A single Service cannot
// show the interaction, and every in-process reproduction above passes — so this
// is where the missing refusal has to come from.
func TestAClaimSurvivesAnotherDaemonDrivingThePR(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	now := time.Now().UTC()

	newSvc := func() *Service {
		gh := newFakeGitHub()
		gh.graphQL = noForcePush
		var pull ghapi.Pull
		pull.State = "open"
		pull.Number = 54
		pull.Head.SHA = "b417a2161"
		gh.pulls[fakeKey("o/r", 54)] = pull
		var commit ghapi.Commit
		commit.Committer.Date = now.Add(-time.Hour)
		gh.commits["b417a2161"] = commit
		s := NewService(cfg, gh, store, nil)
		s.now = func() time.Time { return now }
		return s
	}
	watcher, daemon := newSvc(), newSvc()

	repo, pr, head := "o/r", 54, "b417a2161"
	report := NextReport{Repo: repo, PR: pr, Head: head, Action: "fix",
		Findings: findingsAtHead(head, 3)}
	if ok, why, _ := watcher.claimDispatch(ctx, report, "session-one", 3); !ok {
		t.Fatalf("first dispatch refused: %s", why)
	}

	// The other daemon does its ordinary work on the same PR.
	if _, err := daemon.Enqueue(ctx, repo, pr); err != nil {
		t.Fatalf("daemon enqueue: %v", err)
	}
	if _, err := daemon.Pump(ctx); err != nil {
		t.Fatalf("daemon pump: %v", err)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("the other daemon dropped the running session's claim: round=%#v", round)
	}
	if ok, _, _ := watcher.claimDispatch(ctx, report, "session-two", 3); ok {
		t.Error("a second session was allowed after the other daemon touched the PR")
	}
}
