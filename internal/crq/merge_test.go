package crq

import (
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

func (f *replayFixture) setMergeableState(repo string, pr int, state string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	p := f.gh.pulls[fakeKey(repo, pr)]
	p.MergeableState = state
	f.gh.pulls[fakeKey(repo, pr)] = p
}

func (f *replayFixture) setCheckRuns(ref string, runs ...ghapi.CheckRun) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	if f.gh.checkRuns == nil {
		f.gh.checkRuns = map[string][]ghapi.CheckRun{}
	}
	f.gh.checkRuns[ref] = runs
}

func ciRun(name, status, conclusion string) ghapi.CheckRun {
	run := ghapi.CheckRun{Name: name, Status: status, Conclusion: conclusion, HTMLURL: "https://ci/" + name}
	run.App.Slug = "github-actions"
	return run
}

// The reviewers converging is not enough for `done`: CI has to finish green,
// a red check is work that never holds a review back, and setting an unrelated
// failure aside asks no reviewer for a confirmation pass.
func TestNextDoneWaitsForGreenCI(t *testing.T) {
	base := time.Now().UTC()
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/repo", 700, "aaaaaaaa1234567890123456789012345678901234"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	f.setMergeableState(repo, pr, "unstable")
	f.setCheckRuns(head, ciRun("test", "in_progress", ""))
	// CodeRabbit's own status is the reviewer's business, not CI's.
	f.gh.statuses = map[string][]ghapi.CommitStatus{head: {{Context: "CodeRabbit", State: "pending"}}}

	f.wantAction(f.next(repo, pr), engine.ActionWait)
	if f.reviewsPosted(repo, pr) == 0 {
		t.Fatal("running CI must not hold the review back")
	}
	f.clk.advance(time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())

	running := f.next(repo, pr)
	f.wantAction(running, engine.ActionWait)
	if running.Reason != "waiting for CI: test" {
		t.Fatalf("reason = %q, want the running check named", running.Reason)
	}

	f.setCheckRuns(head, ciRun("test", "completed", "failure"))
	red := f.next(repo, pr)
	f.wantAction(red, engine.ActionFix)
	if len(red.Findings) != 1 || red.Findings[0].Source != engine.SourceCICheck || red.Findings[0].URL != "https://ci/test" {
		t.Fatalf("findings = %+v, want the failed check with its link", red.Findings)
	}

	if _, err := f.svc.Dismiss(f.ctx, repo, pr, []string{red.Findings[0].ID}, "flaky upstream mirror, unrelated to this PR"); err != nil {
		t.Fatal(err)
	}
	// The fake's GraphQL fails, so nobody's open thread can be ruled out yet.
	unread := f.next(repo, pr)
	f.wantAction(unread, engine.ActionWait)
	if !strings.Contains(unread.Reason, "review threads") {
		t.Fatalf("reason = %q, want the unreadable threads named", unread.Reason)
	}
	f.threadsGraphQL(nil)
	done := f.next(repo, pr)
	f.wantAction(done, engine.ActionDone)
	if done.HeadOID != head || done.Head != head[:9] {
		t.Fatalf("decision head = %q (%q), want full %q and abbreviated head", done.HeadOID, done.Head, head)
	}
}

func TestNextMergeGateBlocksOnPeopleAndWaitsForRequiredChecks(t *testing.T) {
	base := time.Now().UTC()
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/repo", 701, "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	f.setMergeableState(repo, pr, "blocked")
	f.setCheckRuns(head, ciRun("test", "completed", "success"))
	// A required reviewer status is the reviewer's business; it must not read
	// as a required check that never reports.
	f.gh.requiredChecks = map[string][]string{repo + "#main": {"PR model disclosure", "CodeRabbit"}}
	f.next(repo, pr)
	f.clk.advance(time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())

	missing := f.next(repo, pr)
	f.wantAction(missing, engine.ActionWait)
	if missing.Reason != "waiting for CI: PR model disclosure" {
		t.Fatalf("reason = %q, want the required check that has not reported", missing.Reason)
	}

	f.gh.statuses = map[string][]ghapi.CommitStatus{head: {{Context: "PR model disclosure", State: "success"}}}
	review := ghapi.Review{ID: 950, State: "CHANGES_REQUESTED", CommitID: head, SubmittedAt: f.clk.now()}
	review.User.Login, review.User.Type = "alice", "User"
	f.gh.mu.Lock()
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], review)
	f.gh.mu.Unlock()
	blocked := f.next(repo, pr)
	f.wantAction(blocked, engine.ActionBlocked)
	if blocked.Reason != "changes requested by alice" {
		t.Fatalf("reason = %q, want the person named", blocked.Reason)
	}
}

func TestMergeGatePeopleExcludeBotsAndSupersededVerdicts(t *testing.T) {
	cfg := firingConfig()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	review := func(login, kind, state string, minutes int) ghapi.Review {
		r := ghapi.Review{State: state, SubmittedAt: at.Add(time.Duration(minutes) * time.Minute)}
		r.User.Login, r.User.Type = login, kind
		return r
	}
	got := changesRequested(cfg, []ghapi.Review{
		review("alice", "User", "APPROVED", 3), // listed out of order on purpose
		review("alice", "User", "CHANGES_REQUESTED", 1),
		review("bob", "User", "CHANGES_REQUESTED", 2),
		review("bob", "User", "COMMENTED", 4), // a comment is not a new verdict
		review("coderabbitai[bot]", "Bot", "CHANGES_REQUESTED", 5),
	})
	if len(got) != 1 || got[0] != "bob" {
		t.Fatalf("changesRequested = %v, want only bob", got)
	}

	thread := func(resolved bool, login, kind, url string) reviewThread {
		var th reviewThread
		th.IsResolved = resolved
		var c reviewThreadComment
		c.Author.Login, c.Author.Typename, c.URL = login, kind, url
		th.Comments.Nodes = []reviewThreadComment{c}
		return th
	}
	urls := personThreads(cfg, []reviewThread{
		thread(false, "alice", "User", "https://x/person"),
		thread(true, "alice", "User", "https://x/resolved"),
		thread(false, "coderabbitai", "Bot", "https://x/bot"),
	})
	if len(urls) != 1 || urls[0] != "https://x/person" {
		t.Fatalf("personThreads = %v, want only the open thread a person started", urls)
	}
}

// Outside a campaign, --merge acts on done alone: the exact head the decision
// was made on, never a fork, never a head the listing has not caught up with.
func TestMergeDoneMergesTheDecidedHeadOnly(t *testing.T) {
	base := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "owner/repo", 702, "dddddddd1234567890123456789012345678901234"
	f.openPull(repo, pr, head)
	squash := "squash"
	if _, err := f.svc.SetSolver(f.ctx, repo, SolverChange{MergeMethod: &squash}); err != nil {
		t.Fatal(err)
	}
	pull := f.gh.pulls[fakeKey(repo, pr)]
	pull.Number = pr
	pull.Head.Repo.FullName = repo
	done := NextReport{Repo: repo, PR: pr, Head: head[:9], HeadOID: head, Action: string(engine.ActionDone)}

	fork := pull
	fork.Head.Repo.FullName = "someone/repo"
	if _, merged, reason, err := f.svc.mergeDone(f.ctx, done, fork); err != nil || merged || !strings.Contains(reason, "another repository") {
		t.Fatalf("fork merge = merged %t reason %q err %v; want it refused", merged, reason, err)
	}
	for _, oid := range []string{"", head[:9], "eeeeeeee1234567890123456789012345678901234", head[:9] + strings.Repeat("e", 31)} {
		moved := done
		moved.HeadOID = oid
		if _, merged, _, err := f.svc.mergeDone(f.ctx, moved, pull); err != nil || merged {
			t.Fatalf("a decision with head OID %q merged: %v", oid, err)
		}
	}

	// A live work claim means unpushed changes this watcher cannot see.
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.SetWorkClaim(repo, pr, WorkClaim{Owner: "session-a", By: "mac:feature", ClaimedAt: f.clk.now(), ExpiresAt: f.clk.now().Add(WorkClaimTTL)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, merged, reason, err := f.svc.mergeDone(f.ctx, done, pull); err != nil || merged || !strings.Contains(reason, "mac:feature") {
		t.Fatalf("claimed merge = merged %t reason %q err %v; want it to wait for the owner", merged, reason, err)
	}
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.ReleaseWorkClaim(repo, pr, "session-a", false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	eligible, merged, reason, err := f.svc.mergeDone(f.ctx, done, pull)
	if err != nil || !eligible || !merged {
		t.Fatalf("merge = eligible %t merged %t reason %q err %v", eligible, merged, reason, err)
	}
	if len(f.gh.merged) != 1 || !strings.Contains(f.gh.merged[0], "@"+head+":squash") {
		t.Fatalf("merge calls = %v, want one exact-head squash", f.gh.merged)
	}
}
