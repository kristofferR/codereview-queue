package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// This suite re-enacts the 2026-07-16 spam incident (ha-adjustable-bed#448: crq
// posted `@coderabbitai review` ~19× in one day, 10+ against a single head) and
// proves the v3 round-state architecture makes it structurally impossible. Each
// scenario drives the real Service.Pump/Feedback/Loop against the fakeGitHub +
// MemoryStore harness with an injected clock, so the whole day is replayed in
// microseconds with no sleeps and no wall-clock dependence.
//
// The invariant every scenario asserts: a review command is posted at most once
// per (head, retry window). Forgetting "already requested at this head" would
// require destroying the round record, and no v3 transition does that.

// replayClock is a controllable UTC clock shared by the Service (scheduling
// decisions) and the fakeGitHub (posted-comment timestamps), so a fire's
// recorded FiredAt tracks the same time the test advances.
type replayClock struct {
	mu sync.Mutex
	t  time.Time
}

func newReplayClock(t time.Time) *replayClock { return &replayClock{t: t.UTC()} }

func (c *replayClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *replayClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *replayClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}

// replayConfig is firingConfig with the timeouts relaxed so an in-flight round
// does not spuriously time out (and re-fire) mid-replay: the scenarios control
// every retry explicitly.
func replayConfig() Config {
	cfg := firingConfig()
	cfg.MinInterval = 0
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	cfg.RateLimitFallback = 15 * time.Minute
	cfg.SettleWindow = 0 // scenarios assert immediate verdicts; the settle test opts in
	return cfg
}

// corpusMessage loads a bot message from the dialect golden corpus, so the
// replays and the classifiers share ONE source of truth for bot wording — a
// rewording that breaks classification breaks these replays too.
func corpusMessage(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dialect", "testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("corpus message: %v", err)
	}
	return strings.TrimRight(string(data), "\n")
}

// replayFairUsage renders the Fair Usage rate-limit reply with a chosen
// "available in N minutes" window, templated from the corpus message.
func replayFairUsage(t *testing.T, minutes int) string {
	msg := corpusMessage(t, "coderabbit/rate-limit-fair-usage.md")
	out := strings.Replace(msg, "48 minutes", fmt.Sprintf("%d minutes", minutes), 1)
	if out == msg {
		t.Fatal("fair-usage corpus message no longer carries the 48-minute window")
	}
	return out
}

// replayFixture bundles the harness for one scenario.
type replayFixture struct {
	t     *testing.T
	ctx   context.Context
	clk   *replayClock
	gh    *fakeGitHub
	store StateStore
	svc   *Service
	cfg   Config
	bot   string
}

func newReplayFixture(t *testing.T, base time.Time) *replayFixture {
	t.Helper()
	clk := newReplayClock(base)
	cfg := replayConfig()
	gh := newFakeGitHub()
	gh.now = clk.now
	store := NewMemoryStore(cfg)
	store.SetClock(clk.now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = clk.now
	return &replayFixture{t: t, ctx: context.Background(), clk: clk, gh: gh, store: store, svc: svc, cfg: cfg, bot: cfg.Bot}
}

// --- harness helpers -------------------------------------------------------

func (f *replayFixture) openPull(repo string, pr int, sha string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	var p ghapi.Pull
	p.State = "open"
	p.Head.SHA = sha
	p.Base.SHA = sha
	p.Base.Ref = "main"
	f.gh.pulls[fakeKey(repo, pr)] = p
}

func (f *replayFixture) setHead(repo string, pr int, sha string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	p := f.gh.pulls[fakeKey(repo, pr)]
	p.Head.SHA = sha
	f.gh.pulls[fakeKey(repo, pr)] = p
}

func (f *replayFixture) setCommitDate(sha string, date time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.Commit{SHA: sha}
	c.Committer.Date = date.UTC()
	f.gh.commits[sha] = c
}

// botComment appends a CodeRabbit issue comment (created == updated == at).
func (f *replayFixture) botComment(repo string, pr int, id int64, body string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	c.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.comments[key] = append(f.gh.comments[key], c)
}

// humanComment appends a comment authored by the review requester (e.g. the
// trigger command comment that is left on the PR after crq fires).
func (f *replayFixture) humanComment(repo string, pr int, id int64, body string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	c.User.Login = "kristofferR"
	key := fakeKey(repo, pr)
	f.gh.comments[key] = append(f.gh.comments[key], c)
}

// editComment rewrites an existing comment in place and bumps its UpdatedAt,
// modelling how CodeRabbit edits its single rate-limit / top-summary comment
// rather than posting a new one.
func (f *replayFixture) editComment(repo string, pr int, id int64, body string, updated time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	key := fakeKey(repo, pr)
	for i := range f.gh.comments[key] {
		if f.gh.comments[key][i].ID == id {
			if body != "" {
				f.gh.comments[key][i].Body = body
			}
			f.gh.comments[key][i].UpdatedAt = updated.UTC()
			return
		}
	}
	f.t.Fatalf("editComment: no comment %d on %s#%d", id, repo, pr)
}

func (f *replayFixture) botReview(repo string, pr int, id int64, commitSHA string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	// A neutral non-empty body: the shell filter keys only on empty-vs-not, and
	// real bot wording belongs in the dialect corpus, not a test fixture.
	r := ghapi.Review{ID: id, CommitID: commitSHA, State: "COMMENTED", SubmittedAt: at.UTC(),
		Body: "[review body]"}
	r.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
}

// corpusReview posts a bot review whose body is a captured bot message, for the
// reviews that carry their findings in the body itself.
func (f *replayFixture) corpusReview(t *testing.T, repo string, pr int, id int64, commitSHA, corpus string) {
	t.Helper()
	body := corpusMessage(t, corpus)
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	r := ghapi.Review{ID: id, CommitID: commitSHA, State: "COMMENTED", SubmittedAt: f.clk.now().UTC(), Body: body}
	r.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
}

func (f *replayFixture) botReviewComment(repo string, pr int, id int64, commitSHA, path string, line int, body string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.ReviewComment{ID: id, Body: body, Path: path, Line: line, CommitID: commitSHA}
	c.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.reviewComments[key] = append(f.gh.reviewComments[key], c)
}

func (f *replayFixture) enqueue(repo string, pr int) {
	f.t.Helper()
	if _, err := f.svc.Enqueue(f.ctx, repo, pr); err != nil {
		f.t.Fatalf("enqueue %s#%d: %v", repo, pr, err)
	}
}

func (f *replayFixture) pump() PumpResult {
	f.t.Helper()
	res, err := f.svc.Pump(f.ctx)
	if err != nil {
		f.t.Fatalf("pump: %v", err)
	}
	return res
}

// autoReviewEnqueue runs the autoreview daemon's per-PR gate: needsReview, and
// enqueueBatch when it says a review is needed. It returns whether it enqueued —
// the spam-relevant question ("did the daemon decide to request another
// review?").
func (f *replayFixture) autoReviewEnqueue(repo string, pr int) bool {
	f.t.Helper()
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		f.t.Fatalf("load: %v", err)
	}
	need, head, err := f.svc.needsReview(f.ctx, st, repo, pr, true)
	if err != nil {
		f.t.Fatalf("needsReview %s#%d: %v", repo, pr, err)
	}
	if !need {
		return false
	}
	if err := f.svc.enqueueBatch(f.ctx, []queueCandidate{{Repo: repo, PR: pr, Head: head}}); err != nil {
		f.t.Fatalf("enqueueBatch %s#%d: %v", repo, pr, err)
	}
	return true
}

func (f *replayFixture) round(repo string, pr int) *Round {
	f.t.Helper()
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		f.t.Fatalf("load: %v", err)
	}
	return st.Round(repo, pr)
}

func (f *replayFixture) archived(repo string, pr int, head string) *Round {
	f.t.Helper()
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		f.t.Fatalf("load: %v", err)
	}
	for i := range st.Archive {
		r := st.Archive[i]
		if r.PR == pr && r.Head == head && NormalizeRepo(r.Repo) == NormalizeRepo(repo) {
			return &r
		}
	}
	return nil
}

// reviewsPosted counts how many `@coderabbitai review` commands crq actually
// posted for repo#pr — the spam meter.
func (f *replayFixture) reviewsPosted(repo string, pr int) int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	want := QueueKey(repo, pr) + ":" + f.cfg.ReviewCommand
	n := 0
	for _, p := range f.gh.posted {
		if p == want {
			n++
		}
	}
	return n
}

// --- (a) a rate-limit bounce cannot re-fire ---------------------------------

// TestReplayRateLimitBounceFiresOncePerWindow is the core #448 defence: a fired
// review comes back rate-limited ("available in 40 minutes"), CodeRabbit edits
// that one comment in place repeatedly, and the daemon pumps + re-scans every
// 60s for the whole window. crq must post exactly ONE command through the
// window, park in awaiting_retry with a fixed RetryAt, then fire exactly one
// retry when the window passes — never the ~19 the incident produced.
func TestReplayRateLimitBounceFiresOncePerWindow(t *testing.T) {
	base := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "o/ha-adjustable-bed", 448
	headSHA, head := "abcdef1234567890", "abcdef123"
	f.openPull(repo, pr, headSHA)
	f.setCommitDate(headSHA, base.Add(-time.Hour))

	// Fire once at head H.
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" || res.Head != head {
		t.Fatalf("first pump should fire at the head, got %#v", res)
	}
	if f.reviewsPosted(repo, pr) != 1 {
		t.Fatalf("expected exactly one command after the first fire, got %d", f.reviewsPosted(repo, pr))
	}

	// CodeRabbit answers with the Fair Usage rate limit, window 40 minutes.
	const rlID = 9001
	f.botComment(repo, pr, rlID, replayFairUsage(t, 40), base)
	expectedRetry := base.Add(40 * time.Minute) // parsed from the comment's UpdatedAt (base)

	// Simulate the daemon for the full window: advance 60s each step, editing the
	// SAME comment in place every 5 minutes (bumping its UpdatedAt), pumping and
	// re-scanning at each. The round must stay parked and the count must stay 1.
	for m := 1; m < 40; m++ {
		f.clk.advance(time.Minute)
		if m%5 == 0 {
			f.editComment(repo, pr, rlID, "", f.clk.now()) // CodeRabbit edits it in place
		}
		f.pump()
		if f.autoReviewEnqueue(repo, pr) {
			t.Fatalf("minute %d: autoreview must not enqueue a duplicate for a head it already requested", m)
		}
		if got := f.reviewsPosted(repo, pr); got != 1 {
			t.Fatalf("minute %d: expected still one command through the window, got %d", m, got)
		}
		r := f.round(repo, pr)
		if r == nil || r.Phase != PhaseAwaitingRetry {
			t.Fatalf("minute %d: round must be parked awaiting retry, got %#v", m, r)
		}
		if r.RetryAt == nil || !r.RetryAt.Equal(expectedRetry) {
			t.Fatalf("minute %d: RetryAt must stay fixed at %s despite in-place edits, got %v", m, expectedRetry, r.RetryAt)
		}
	}

	// The window passes: exactly one retry fires.
	f.clk.set(expectedRetry.Add(time.Second))
	if res := f.pump(); res.Action != "fired" || res.Head != head {
		t.Fatalf("the retry must fire once the window passes, got %#v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 2 {
		t.Fatalf("expected exactly two commands total (fire + one retry), got %d", got)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseFired {
		t.Fatalf("the retry should leave the round in flight, got %#v", r)
	}

	// And never more: keep pumping past the retry — the stale rate-limit comment
	// (last edited before the retry) must not re-fire it.
	for i := 0; i < 5; i++ {
		f.clk.advance(time.Minute)
		f.pump()
		if got := f.reviewsPosted(repo, pr); got != 2 {
			t.Fatalf("no further command may post after the single retry, got %d", got)
		}
	}
}

// --- (b) instant completion ack on a first-ever command ---------------------

// TestReplayInstantAckDoesNotConvergeOrDoubleFire covers the case where
// CodeRabbit answers the very first review command with an immediate "Review
// finished." while the real review is still queued on its side and no review
// object exists yet. That ack must not converge the loop or complete/re-fire the
// round; the real review's findings surface once it lands (exit path 10).
func TestReplayInstantAckDoesNotConvergeOrDoubleFire(t *testing.T) {
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "o/ha-adjustable-bed", 448
	headSHA := "abcdef1234567890"
	f.openPull(repo, pr, headSHA)
	f.setCommitDate(headSHA, base.Add(-time.Hour))

	// First-ever command on the PR — the bot has NO submitted reviews.
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("first pump should fire, got %#v", res)
	}

	// The auto-reply completion ack lands 5s later, no review object with it.
	f.clk.advance(5 * time.Second)
	f.botComment(repo, pr, 100, corpusMessage(t, "coderabbit/completion-reply.md"), f.clk.now())

	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("an instant ack on a never-reviewed PR must keep the round open (reviewing), got %#v", r)
	}
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	if rep.Converged {
		t.Fatal("a completion ack with no submitted review must not converge")
	}
	if f.autoReviewEnqueue(repo, pr) {
		t.Fatal("autoreview must not enqueue a duplicate while the review is still running")
	}
	f.pump()
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("the instant ack must not trigger a second command, got %d", got)
	}

	// The real review with findings lands minutes later.
	f.clk.advance(5 * time.Minute)
	f.botReview(repo, pr, 200, headSHA, f.clk.now())
	f.botReviewComment(repo, pr, 201, headSHA, "custom_components/adjustable_bed/bed.py", 42,
		"**Potential issue.**\n\nThis dereferences a nil handle and will crash on disconnect.")

	rep, code, err := f.svc.Loop(f.ctx, repo, pr)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if code != 10 {
		t.Fatalf("the loop must surface the real review's findings (exit 10), got code %d (%#v)", code, rep)
	}
	if len(rep.Findings) == 0 {
		t.Fatalf("expected the landed review's findings, got none")
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("no re-fire may happen across the whole scenario, got %d commands", got)
	}
}

// --- (c) in-progress summary releases the slot but keeps the round open ------

// TestReplayInProgressSummaryReleasesSlotButKeepsRound proves the slot-release
// semantics: CodeRabbit's "Currently processing…" top summary acknowledges the
// command (freeing the global fire slot so another PR can fire) without
// completing the round or converging, and without ever re-firing the first PR.
func TestReplayInProgressSummaryReleasesSlotButKeepsRound(t *testing.T) {
	base := time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo := "o/ha-adjustable-bed"
	pr1, pr2 := 448, 449
	head1SHA := "111111112222aaaa"
	head2SHA := "aaaabbbbccccdddd"
	f.openPull(repo, pr1, head1SHA)
	f.openPull(repo, pr2, head2SHA)
	f.setCommitDate(head1SHA, base.Add(-time.Hour))
	f.setCommitDate(head2SHA, base.Add(-time.Hour))

	f.enqueue(repo, pr1)
	f.enqueue(repo, pr2)
	if res := f.pump(); res.Action != "fired" || res.PR != pr1 {
		t.Fatalf("first pump should fire PR1, got %#v", res)
	}

	// CodeRabbit posts the in-progress top summary for PR1 (edited to now).
	f.clk.advance(time.Minute)
	f.botComment(repo, pr1, 300, corpusMessage(t, "coderabbit/review-in-progress.md"), f.clk.now())

	f.pump()
	if r := f.round(repo, pr1); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("the in-progress summary must move PR1 to reviewing, got %#v", r)
	}
	st, _, _ := f.store.Load(f.ctx)
	if st.FireSlot != nil {
		t.Fatalf("the in-progress summary must release the fire slot, got %#v", st.FireSlot)
	}
	rep, err := f.svc.Feedback(f.ctx, repo, pr1)
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	if rep.Converged {
		t.Fatal("a still-processing round must not converge")
	}

	// The freed slot lets PR2 fire.
	if res := f.pump(); res.Action != "fired" || res.PR != pr2 {
		t.Fatalf("with the slot released the next pump should fire PR2, got %#v", res)
	}
	if f.reviewsPosted(repo, pr1) != 1 || f.reviewsPosted(repo, pr2) != 1 {
		t.Fatalf("each PR should have exactly one command, got PR1=%d PR2=%d", f.reviewsPosted(repo, pr1), f.reviewsPosted(repo, pr2))
	}

	// PR1 keeps reviewing and never re-fires while PR2's review runs.
	for i := 0; i < 4; i++ {
		f.clk.advance(time.Minute)
		f.pump()
		if r := f.round(repo, pr1); r == nil || r.Phase != PhaseReviewing {
			t.Fatalf("PR1 must stay reviewing, got %#v", r)
		}
		if f.reviewsPosted(repo, pr1) != 1 {
			t.Fatalf("PR1 must never get a second command, got %d", f.reviewsPosted(repo, pr1))
		}
	}
}

// --- (d) force-push mid-round supersedes without stale adoption --------------

// TestReplayForcePushSupersedesWithoutStaleAdoption checks that when the head
// force-pushes mid-round, the old round is abandoned and a fresh round fires its
// OWN command for the new head — the stale command comment left on the PR for
// the old head must NOT be adopted (which would mark the new head reviewed
// without a review). Total commands: exactly one per head.
func TestReplayForcePushSupersedesWithoutStaleAdoption(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "o/ha-adjustable-bed", 448
	head1SHA, head1 := "1111aaaa2222bbbb", "1111aaaa2"
	head2SHA, head2 := "3333cccc4444dddd", "3333cccc4"

	f.openPull(repo, pr, head1SHA)
	f.setCommitDate(head1SHA, base.Add(-time.Hour))

	// Fire at H1.
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" || res.Head != head1 {
		t.Fatalf("first pump should fire at H1, got %#v", res)
	}
	cmd1 := f.round(repo, pr).CommandID
	// The fired command comment is now on the PR (the fake records only the post,
	// so mirror it as the comment CodeRabbit would show for the old head).
	f.humanComment(repo, pr, cmd1, f.cfg.ReviewCommand, base)

	// A force-push retargets the PR at H2. Its commit object predates the H1
	// command (a rebase onto an older base), so ONLY the force-push timeline —
	// not the commit date — keeps the stale command from being adopted.
	forcePushAt := base.Add(10 * time.Minute)
	f.setHead(repo, pr, head2SHA)
	f.setCommitDate(head2SHA, base.Add(-30*time.Minute))
	f.gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			return errors.New("graphql unavailable")
		}
		payload := `{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"createdAt":"` + forcePushAt.Format(time.RFC3339) + `"}]}}}}`
		return json.Unmarshal([]byte(payload), out)
	}

	// The daemon notices the head moved and supersedes.
	f.clk.set(base.Add(15 * time.Minute))
	if !f.autoReviewEnqueue(repo, pr) {
		t.Fatal("the moved head must be enqueued (superseded) by autoreview")
	}
	if a := f.archived(repo, pr, head1); a == nil || a.Phase != PhaseAbandoned {
		t.Fatalf("the H1 round must be archived abandoned, got %#v", a)
	}

	// The fresh H2 round fires its own command, not the adopted stale one.
	res := f.pump()
	if res.Action != "fired" || res.Head != head2 {
		t.Fatalf("a fresh H2 round should fire, got %#v", res)
	}
	if res.Reason == "review command already posted" {
		t.Fatal("the stale H1 command must NOT be adopted for the force-pushed head")
	}
	r := f.round(repo, pr)
	if r == nil || r.Head != head2 || r.Phase != PhaseFired {
		t.Fatalf("expected a fired H2 round, got %#v", r)
	}
	if r.CommandID == cmd1 {
		t.Fatalf("the H2 round adopted the stale H1 command %d instead of firing its own", cmd1)
	}
	if got := f.reviewsPosted(repo, pr); got != 2 {
		t.Fatalf("expected exactly two commands (one per head), got %d", got)
	}
}

// --- (e) the whole 19×-day sequence -----------------------------------------

// TestReplay448DaySequenceFiresThreeTimes scripts the actual incident shape end
// to end: fire → rate-limited (unparseable window → 15m fallback) → window
// expiry → retry → rate-limited again (parseable 40m, same comment edited in
// place) → head moves mid-window → the new head fires once after the block
// clears → a real review completes it. Where the old code posted ~19 commands,
// v3 posts exactly THREE (H1, the H1 retry, H2) and converges.
func TestReplay448DaySequenceFiresThreeTimes(t *testing.T) {
	base := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "o/ha-adjustable-bed", 448
	head1SHA, head1 := "abcdef1234567890", "abcdef123"
	head2SHA, head2 := "fedcba0987654321", "fedcba098"
	f.openPull(repo, pr, head1SHA)
	f.setCommitDate(head1SHA, base.Add(-time.Hour))

	// 1. Fire at H1.
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" || res.Head != head1 {
		t.Fatalf("first pump should fire at H1, got %#v", res)
	}

	// 2. Rate-limited with an unparseable window → fixed 15m fallback.
	const rlID = 7001
	f.botComment(repo, pr, rlID, corpusMessage(t, "coderabbit/rate-limit-no-window.md"), base)
	f.clk.advance(time.Minute) // base+1m
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseAwaitingRetry ||
		r.RetryAt == nil || !r.RetryAt.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("an unparseable rate limit must park for 15m from the notice (base+15m), got %#v", r)
	}
	if f.reviewsPosted(repo, pr) != 1 {
		t.Fatalf("still one command after the first rate limit, got %d", f.reviewsPosted(repo, pr))
	}

	// 3. Window expires → one retry fires.
	f.clk.set(base.Add(16*time.Minute + time.Second))
	if res := f.pump(); res.Action != "fired" || res.Head != head1 {
		t.Fatalf("the retry must fire once the fallback window passes, got %#v", res)
	}
	if f.reviewsPosted(repo, pr) != 2 {
		t.Fatalf("expected two commands after the retry, got %d", f.reviewsPosted(repo, pr))
	}

	// 4. Rate-limited again — CodeRabbit edits the SAME comment in place, now with
	//    a parseable 40-minute window.
	f.clk.set(base.Add(17 * time.Minute))
	f.editComment(repo, pr, rlID, replayFairUsage(t, 40), f.clk.now())
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseAwaitingRetry ||
		r.RetryAt == nil || !r.RetryAt.Equal(base.Add(57*time.Minute)) {
		t.Fatalf("the parseable 40m window must park until base+57m, got %#v", r)
	}

	// 5. The head moves mid-window (still inside the 40m block).
	f.clk.set(base.Add(30 * time.Minute))
	f.setHead(repo, pr, head2SHA)
	f.setCommitDate(head2SHA, base.Add(50*time.Minute))
	if !f.autoReviewEnqueue(repo, pr) {
		t.Fatal("the moved head must be superseded by autoreview")
	}
	if a := f.archived(repo, pr, head1); a == nil || a.Phase != PhaseAbandoned {
		t.Fatalf("the H1 round must be archived abandoned after the head move, got %#v", a)
	}
	// The account block still stands, so the new head cannot fire yet.
	if res := f.pump(); res.Action != "blocked" {
		t.Fatalf("the account block must prevent the new head from firing early, got %#v", res)
	}
	if f.reviewsPosted(repo, pr) != 2 {
		t.Fatalf("the blocked new head must not post yet, got %d commands", f.reviewsPosted(repo, pr))
	}

	// 6. The block clears → the new head fires exactly once.
	f.clk.set(base.Add(57*time.Minute + time.Second))
	if res := f.pump(); res.Action != "fired" || res.Head != head2 {
		t.Fatalf("the new head should fire once the block clears, got %#v", res)
	}
	if f.reviewsPosted(repo, pr) != 3 {
		t.Fatalf("expected exactly three commands over the whole day (H1, H1 retry, H2), got %d", f.reviewsPosted(repo, pr))
	}

	// 7. A real review lands and completes the round.
	f.clk.advance(time.Minute)
	f.botReview(repo, pr, 800, head2SHA, f.clk.now())
	if res := f.pump(); res.Action != "cleared" {
		t.Fatalf("the real review should complete the round, got %#v", res)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted || r.Head != head2 {
		t.Fatalf("the final round must be completed at H2, got %#v", r)
	}
	if f.reviewsPosted(repo, pr) != 3 {
		t.Fatalf("no command may post after convergence, got %d", f.reviewsPosted(repo, pr))
	}
}

// shellReview appends an empty-bodied COMMENTED review object — the carrier
// CodeRabbit submits for an inline-comment batch before its real review.
func (f *replayFixture) shellReview(repo string, pr int, id int64, commitSHA string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	r := ghapi.Review{ID: id, CommitID: commitSHA, State: "COMMENTED", SubmittedAt: at.UTC()}
	r.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
}

// TestReplayEmptyReviewShellDoesNotConverge pins the 17:26-vs-17:32 incident:
// CodeRabbit submitted five empty review shells at the head minutes before the
// real review; a loop polling in that window must NOT converge on them.
func TestReplayEmptyReviewShellDoesNotConverge(t *testing.T) {
	base := time.Date(2026, 7, 17, 17, 20, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr, head := "o/r", 30, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}

	// Comment-batch shells arrive at the fired head.
	f.shellReview(repo, pr, 601, head, f.clk.now().Add(90*time.Second))
	f.shellReview(repo, pr, 602, head, f.clk.now().Add(91*time.Second))
	f.clk.advance(2 * time.Minute)
	f.pump()
	report, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged {
		t.Fatalf("empty review shells must not converge the round: %+v", report.ReviewedBy)
	}
	if r := f.round(repo, pr); r == nil || r.Phase == PhaseCompleted {
		t.Fatalf("round must stay open on shells, got %+v", r)
	}

	// The real review lands six minutes later — now it converges.
	f.botReview(repo, pr, 603, head, f.clk.now().Add(6*time.Minute))
	f.clk.advance(7 * time.Minute)
	f.pump()
	report, err = f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged {
		t.Fatalf("real review must converge: %+v", report.ReviewedBy)
	}
}

// --- a refusal is not a rate limit -----------------------------------------

// TestReplaySkippedRefusalNeverBlocksTheAccount pins the fleet-wide property
// behind CodeRabbit's "Review skipped" refusal.
//
// That notice ships with the rate-limit marker embedded, and reading it as a
// timed block was catastrophic rather than merely wrong: the block is
// account-WIDE, nothing about waiting clears "this PR has 119 files", and every
// re-fire re-read the same notice and re-blocked. One oversized PR stalled review
// for every repository.
//
// The classifier discriminates them (TestGoldenClassification), but the cost of
// a regression lands here, so assert the consequence and not just the reading:
// after observing the refusal, no account block exists and an unrelated PR still
// fires.
func TestReplaySkippedRefusalNeverBlocksTheAccount(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	oversized, other := 700, 701
	f.openPull("owner/repo", oversized, "aaaaaaaa1")
	f.setCommitDate("aaaaaaaa1", base.Add(-time.Minute))
	f.openPull("owner/repo", other, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", base.Add(-time.Minute))

	f.enqueue("owner/repo", oversized)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("the oversized PR should fire once, got %#v", res)
	}

	// CodeRabbit refuses the head. The body carries the rate-limit marker.
	f.clk.advance(time.Minute)
	f.botComment("owner/repo", oversized, 900,
		corpusMessage(t, "coderabbit/review-skipped-too-many-files.md"), f.clk.now())
	f.pump()

	// Prove the refusal was actually observed. Without this the assertions below
	// would also pass if nothing had happened at all — and "no block" is only
	// meaningful once the notice that used to create one has been read.
	if r := f.round("owner/repo", oversized); r == nil || r.Phase == PhaseFired {
		t.Fatalf("the refusal must move the round off fired, got %#v", r)
	}

	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil != nil {
		t.Fatalf("a refusal must not block the account: BlockedUntil = %s", st.Account.BlockedUntil)
	}

	// And the block being absent has to mean something: an unrelated PR fires.
	f.clk.advance(2 * time.Minute)
	f.enqueue("owner/repo", other)
	for i := 0; i < 4; i++ {
		if f.reviewsPosted("owner/repo", other) > 0 {
			break
		}
		f.clk.advance(time.Minute)
		f.pump()
	}
	if got := f.reviewsPosted("owner/repo", other); got == 0 {
		t.Fatal("an unrelated PR must still fire: one oversized PR cannot stall the fleet")
	}
}

// CodeRabbit may answer a changed head by declining to review commits it says
// an earlier round already covered. Once that response is paired to this
// command and a prior review proves it really is a re-review, the round is
// complete: retrying cannot produce different evidence and eventually left
// `crq next` at a terminal blocked verdict.
func TestReplayDeclinedRereviewCompletesChangedHead(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 702
	oldHead, head := "1111222233334444", "5555666677778888"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.botReview(repo, pr, 700, oldHead, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("changed head should request its re-review, got %#v", res)
	}
	r := f.round(repo, pr)
	if r == nil || r.FiredAt == nil || r.CommandID == 0 {
		t.Fatalf("fired round missing its command anchor: %#v", r)
	}
	f.humanComment(repo, pr, r.CommandID, f.cfg.ReviewCommand, *r.FiredAt)
	f.clk.advance(time.Minute)
	f.botComment(repo, pr, 701, corpusMessage(t, "coderabbit/already-reviewed.md"), f.clk.now())
	feedback, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !feedback.Converged {
		t.Fatalf("declined re-review still reports the primary pending: %#v", feedback.ReviewedBy)
	}

	if res := f.pump(); res.Action != "cleared" || res.Reason != "re-review declined" {
		t.Fatalf("declined re-review should clear the round, got %#v", res)
	}
	if r = f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("declined re-review left the head waiting for a retry: %#v", r)
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("declined re-review was re-fired %d times", got)
	}
}
