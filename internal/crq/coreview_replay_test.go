package crq

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// End-to-end scenarios for the co-reviewer abstraction (Bugbot, Macroscope):
// check-run convergence, BUG_ID dedupe, resolved-edit settlement, selfheal
// triggers, empty-shell retention, informational verdicts, and always-mode
// fire-time triggers. Bot wording comes from the dialect golden corpus so a
// rewording that breaks classification breaks these replays too.

const (
	bugbotLogin = dialect.BugbotLogin
	macroLogin  = dialect.MacroscopeLogin
)

// defaultCoBots returns the production defaults by running the real parser on
// an empty environment. Duplicating the defaults here let them drift silently:
// a new co-reviewer, or a changed trigger default, would leave replay coverage
// asserting a world that no longer exists.
// defaultCoBots is every registry co-reviewer with its own defaults. Named
// explicitly because a co-reviewer is opt-in: these replays are ABOUT the
// co-reviewers, so they enable them the way an operator would.
func defaultCoBots(t *testing.T) []CoBotConfig {
	t.Helper()
	co, err := parseCoBots(map[string]string{"CRQ_COBOTS": "codex,bugbot,macroscope"}, nil)
	if err != nil {
		t.Fatalf("parseCoBots defaults: %v", err)
	}
	return co
}

func newCoReplayFixture(t *testing.T, base time.Time, mutate func(*Config)) *replayFixture {
	t.Helper()
	clk := newReplayClock(base)
	cfg := replayConfig()
	cfg.CoBots = defaultCoBots(t)
	cfg.FeedbackBots = unionBots(cfg.RequiredBots, []string{dialect.CodexBotLogin, bugbotLogin, macroLogin})
	if mutate != nil {
		mutate(&cfg)
	}
	gh := newFakeGitHub()
	gh.now = clk.now
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = clk.now
	return &replayFixture{t: t, ctx: t.Context(), clk: clk, gh: gh, store: store, svc: svc, cfg: cfg, bot: cfg.Bot}
}

// requireBugbot marks Bugbot configured-required (the CoBots entry and the
// RequiredBots fold LoadConfig would produce).
func requireBugbot(cfg *Config) {
	cfg.RequiredBots = append(cfg.RequiredBots, bugbotLogin)
	for i := range cfg.CoBots {
		if cfg.CoBots[i].Name == "bugbot" {
			cfg.CoBots[i].Required = true
		}
	}
}

// macroscopeSettled appends Macroscope's settled marker to a finding body,
// taking the wording from the golden corpus rather than a literal — this
// file's invariant is that a reword which breaks the classifier breaks the
// replay too, and a hardcoded marker would silently survive one.
func macroscopeSettled(t *testing.T, body, sha string) string {
	t.Helper()
	settled := corpusMessage(t, "macroscope/inline-finding-resolved.md")
	marker := dialect.MacroscopeResolvedInSHA(settled)
	if marker == "" {
		t.Fatal("corpus resolved-finding no longer carries a settled marker")
	}
	idx := strings.Index(settled, "✅")
	if idx < 0 {
		t.Fatal("corpus resolved-finding no longer uses the ✅ settled form")
	}
	line := strings.SplitN(settled[idx:], "\n", 2)[0]
	return body + "\n\n" + strings.Replace(line, marker, sha, 1)
}

// corpusCheckRun loads one captured check-run object from the dialect corpus.
func corpusCheckRun(t *testing.T, name string) ghapi.CheckRun {
	t.Helper()
	var run ghapi.CheckRun
	if err := json.Unmarshal([]byte(corpusMessage(t, name)), &run); err != nil {
		t.Fatalf("corpus check run %s: %v", name, err)
	}
	return run
}

// coPostedBody counts posted comments whose body equals body.
func (f *replayFixture) coPostedBody(repo string, pr int, body string) int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	want := QueueKey(repo, pr) + ":" + body
	n := 0
	for _, p := range f.gh.posted {
		if p == want {
			n++
		}
	}
	return n
}

// threadsGraphQL installs a GraphQL handler serving the given review-thread
// nodes (and an empty force-push timeline).
func (f *replayFixture) threadsGraphQL(nodes []map[string]any) {
	payload := map[string]any{
		"repository": map[string]any{
			"pullRequest": map[string]any{
				"reviewThreads": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					"nodes":    nodes,
				},
			},
		},
	}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	f.gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, out)
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"timelineItems":{"nodes":[]}}}}`), out)
	}
}

func threadNode(id string, resolved bool, login, path string, line int, commentID int64, body string, at time.Time) map[string]any {
	return map[string]any{
		"id": id, "isResolved": resolved, "isOutdated": false, "path": path, "line": line,
		"comments": map[string]any{
			"totalCount": 1,
			"nodes": []map[string]any{{
				"databaseId": commentID, "body": body, "url": "https://example.test/c/" + id,
				"path": path, "line": line, "originalLine": line,
				"createdAt": at.UTC().Format(time.RFC3339),
				"author":    map[string]any{"login": login},
			}},
		},
	}
}

// --- 1. required Bugbot, silent-clean round ---------------------------------

// TestCoReplayBugbotSilentCleanConvergesOnCheckRun: Bugbot posts NOTHING on
// the timeline for a clean round — only its check run. A required Bugbot
// round must wait while the check runs and converge once it completes clean.
func TestCoReplayBugbotSilentCleanConvergesOnCheckRun(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 31, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}
	// Bugbot's selfheal trigger must NOT post at fire time.
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
		t.Fatalf("selfheal must not trigger at fire time, got %d posts", got)
	}

	// CodeRabbit reviews the head; Bugbot's check is still running.
	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 500, sha, f.clk.now())
	running := corpusCheckRun(t, "bugbot/check-in-progress.json")
	f.gh.setCheckRuns(sha, running)
	f.pump() // review submitted; bugbot still required → reviewing, not complete
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must keep waiting on the running check, got %+v", r)
	}
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converged {
		t.Fatalf("an in-progress check must not converge the round: %+v", rep)
	}
	if st := rep.CoReviewers[dialect.NormalizeBotName(bugbotLogin)]; st.CheckState != "in_progress" || st.Reviewed {
		t.Fatalf("co_reviewers must report the running check, got %+v", st)
	}

	// The check completes clean (conclusion success, "no issues found").
	f.clk.advance(3 * time.Minute)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = f.clk.now()
	f.gh.setCheckRuns(sha, clean)
	rep, err = f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Converged || len(rep.Findings) != 0 {
		t.Fatalf("the clean check run must converge the silent-clean round: %+v", rep)
	}
	if st := rep.CoReviewers[dialect.NormalizeBotName(bugbotLogin)]; st.CheckState != "clean" || !st.Reviewed {
		t.Fatalf("co_reviewers must report the clean check, got %+v", st)
	}
	f.pump() // the reviewing sweep completes the round
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete, got %+v", r)
	}
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
		t.Fatalf("no trigger may ever post for an auto-reviewing clean round, got %d", got)
	}
}

func TestFeedbackPersistsCoReviewerActivity(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 40, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}

	f.clk.advance(time.Minute)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = f.clk.now()
	f.gh.setCheckRuns(sha, clean)
	if _, err := f.svc.Feedback(f.ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	if r := f.round(repo, pr); r == nil || r.Co(bugbotLogin).SeenActiveAt == nil {
		t.Fatalf("Feedback did not persist observed Bugbot activity: %+v", r)
	}
}

func TestNoteCoAnswersCarriesActivityThroughConcurrentSupersede(t *testing.T) {
	base := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, oldHead, newHead := "o/r", 45, "abcdef1234567890", "fedcba9876543210"
	f.openPull(repo, pr, oldHead)
	f.enqueue(repo, pr)
	round := *f.round(repo, pr)

	f.svc.store = &supersedeBeforeUpdateStore{
		StateStore: f.store,
		repo:       repo,
		pr:         pr,
		head:       newHead[:9],
		now:        base.Add(time.Minute),
	}
	obs := engine.Observation{Head: round.Head, Open: true,
		Checks: []engine.CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean}}}
	if err := f.svc.noteCoAnswers(f.ctx, f.cfg, round, obs, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	carried := f.round(repo, pr)
	if carried == nil || carried.Head != newHead[:9] {
		t.Fatalf("round was not superseded: %+v", carried)
	}
	co := carried.Co(bugbotLogin)
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("superseding round lost observed reviewer activity: %+v", co)
	}
	if co.AnsweredAt != nil {
		t.Fatalf("old-head evidence must not answer the replacement round: %+v", co)
	}
}

func TestNoteCoAnswersPersistsArchivedActivityBeyondEviction(t *testing.T) {
	base := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, head := "o/r", 46, "abcdef1234567890"
	f.openPull(repo, pr, head)
	f.enqueue(repo, pr)
	round := *f.round(repo, pr)

	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.EndRound(repo, pr, "pr closed")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	obs := engine.Observation{Head: round.Head, Open: true,
		Checks: []engine.CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean}}}
	if err := f.svc.noteCoAnswers(f.ctx, f.cfg, round, obs, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.Update(f.ctx, func(st *State) error {
		for otherPR := 100; otherPR < 100+ArchiveMax; otherPR++ {
			other, err := st.NewRound("o/other", otherPR, "123456789", base)
			if err != nil {
				return err
			}
			st.PutRound(*other)
			st.EndRound("o/other", otherPR, "pr closed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if activity := st.CoActivity[QueueKey(repo, pr)]["cursor"]; !activity.Equal(base.Add(time.Minute)) {
		t.Fatalf("archived reviewer activity was not indexed: %v", activity)
	}
	reopened, err := st.NewRound(repo, pr, "fedcba9876543210", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if co := reopened.Co(bugbotLogin); co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("reopened round lost evicted archived activity: %+v", co)
	}
}

func TestNoteCoAnswersPersistsActivityAfterConcurrentArchiveEviction(t *testing.T) {
	base := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, head := "o/r", 47, "abcdef1234567890"
	f.openPull(repo, pr, head)
	f.enqueue(repo, pr)
	round := *f.round(repo, pr)

	f.svc.store = &evictBeforeUpdateStore{
		StateStore: f.store,
		repo:       repo,
		pr:         pr,
		now:        base.Add(time.Minute),
	}
	obs := engine.Observation{Head: round.Head, Open: true,
		Checks: []engine.CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean}}}
	if err := f.svc.noteCoAnswers(f.ctx, f.cfg, round, obs, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if activity := st.CoActivity[QueueKey(repo, pr)]["cursor"]; !activity.Equal(base.Add(time.Minute)) {
		t.Fatalf("evicted reviewer activity was not indexed: %v", activity)
	}
	reopened, err := st.NewRound(repo, pr, "fedcba9876543210", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if co := reopened.Co(bugbotLogin); co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("reopened round lost concurrently evicted activity: %+v", co)
	}
}

func TestFeedbackReturnsCoReviewerActivityPersistenceFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 41, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}

	f.clk.advance(time.Minute)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = f.clk.now()
	f.gh.setCheckRuns(sha, clean)
	writeErr := errors.New("transient state write failure")
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 1, err: writeErr}

	if _, err := f.svc.Feedback(f.ctx, repo, pr); !errors.Is(err, writeErr) {
		t.Fatalf("Feedback error = %v, want the reviewer-activity persistence failure", err)
	}
}

func TestPumpReturnsCoReviewerActivityPersistenceFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 42, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = base
	f.gh.setCheckRuns(sha, clean)
	writeErr := errors.New("transient state write failure")
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 1, err: writeErr}

	if _, err := f.svc.Pump(f.ctx); !errors.Is(err, writeErr) {
		t.Fatalf("Pump error = %v, want the reviewer-activity persistence failure", err)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseQueued {
		t.Fatalf("Pump advanced the round despite the persistence failure: %+v", r)
	}
}

func TestProgressSlotRoundReturnsCoReviewerActivityPersistenceFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 43, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = base
	f.gh.setCheckRuns(sha, clean)
	writeErr := errors.New("transient state write failure")
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 1, err: writeErr}

	if _, err := f.svc.progressSlotRound(f.ctx, *f.round(repo, pr)); !errors.Is(err, writeErr) {
		t.Fatalf("progressSlotRound error = %v, want the reviewer-activity persistence failure", err)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseFired {
		t.Fatalf("progressSlotRound advanced the round despite the persistence failure: %+v", r)
	}
}

func TestSweepReviewingReturnsCoReviewerActivityPersistenceFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 44, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, sha[:9], PhaseReviewing, base.Add(-time.Minute), 400)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = base
	f.gh.setCheckRuns(sha, clean)
	writeErr := errors.New("transient state write failure")
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 1, err: writeErr}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.sweepReviewing(f.ctx, st, base); !errors.Is(err, writeErr) {
		t.Fatalf("sweepReviewing error = %v, want the reviewer-activity persistence failure", err)
	}
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("sweepReviewing advanced the round despite the persistence failure: %+v", r)
	}
}

func TestSweepReviewingRecordsAccountBlockBeforeActivityFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 45, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, sha[:9], PhaseReviewing, base.Add(-time.Minute), 400)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = base
	f.gh.setCheckRuns(sha, clean)
	f.botComment(repo, pr, 901, replayFairUsage(t, 40), base)
	writeErr := errors.New("transient state write failure")
	// The account-block write succeeds; the following activity write fails.
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 2, err: writeErr}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.sweepReviewing(f.ctx, st, base); !errors.Is(err, writeErr) {
		t.Fatalf("sweepReviewing error = %v, want the reviewer-activity persistence failure", err)
	}
	st, _, err = f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(base.Add(40*time.Minute)) {
		t.Fatalf("account block = %v, want %s despite the later activity failure", st.Account.BlockedUntil, base.Add(40*time.Minute))
	}
	if r := st.Round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("sweepReviewing advanced the round despite the persistence failure: %+v", r)
	}
}

func TestPumpRecordsAccountBlockBeforeActivityFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 47, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = base
	f.gh.setCheckRuns(sha, clean)
	f.botComment(repo, pr, 901, replayFairUsage(t, 40), base)
	writeErr := errors.New("transient state write failure")
	// The account-block write succeeds; the following activity write fails.
	f.svc.store = &failNthUpdateStore{StateStore: f.store, n: 2, err: writeErr}

	if _, err := f.svc.Pump(f.ctx); !errors.Is(err, writeErr) {
		t.Fatalf("Pump error = %v, want the reviewer-activity persistence failure", err)
	}
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(base.Add(40*time.Minute)) {
		t.Fatalf("account block = %v, want %s despite the later activity failure", st.Account.BlockedUntil, base.Add(40*time.Minute))
	}
	if r := st.Round(repo, pr); r == nil || r.Phase != PhaseQueued {
		t.Fatalf("Pump advanced the round despite the persistence failure: %+v", r)
	}
}

func TestQuotaFreePathsPersistActivityBeforeCompletion(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		run  func(*replayFixture, string, int) (bool, error)
	}{
		{
			name: "sweep",
			run: func(f *replayFixture, repo string, pr int) (bool, error) {
				st, _, err := f.store.Load(f.ctx)
				if err != nil {
					return false, err
				}
				_, handled, err := f.svc.sweepQuotaFree(f.ctx, st, f.clk.now(), "", 0)
				return handled, err
			},
		},
		{
			name: "advance",
			run: func(f *replayFixture, repo string, pr int) (bool, error) {
				_, handled, err := f.svc.advanceQuotaFree(f.ctx, repo, pr)
				return handled, err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoReplayFixture(t, base, func(cfg *Config) {
				requireBugbot(cfg)
				for i := range cfg.CoBots {
					if cfg.CoBots[i].Name == "bugbot" {
						cfg.CoBots = []CoBotConfig{cfg.CoBots[i]}
						break
					}
				}
				cfg.FeedbackBots = cfg.RequiredBots
			})
			repo, pr, sha := "o/private", 48, "abcdef1234567890"
			f.openPull(repo, pr, sha)
			f.setCommitDate(sha, base.Add(-time.Hour))
			f.botComment(repo, pr, 900, corpusMessage(t, "coderabbit/summary-only-free-plan.md"), base.Add(-30*time.Minute))
			clean := corpusCheckRun(t, "bugbot/check-clean.json")
			clean.CompletedAt = base
			f.gh.setCheckRuns(sha, clean)
			f.enqueue(repo, pr)

			handled, err := tc.run(f, repo, pr)
			if err != nil {
				t.Fatal(err)
			}
			if !handled {
				t.Fatal("quota-free path did not handle the completed round")
			}
			r := f.round(repo, pr)
			if r == nil || r.Phase != PhaseCompleted || r.Co(bugbotLogin).SeenActiveAt == nil {
				t.Fatalf("quota-free completion lost reviewer activity: %+v", r)
			}
		})
	}
}

func TestParkedCoReviewWaitPreservesSelfHealGraceForOldCommit(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	f.gh.graphQL = noForcePush
	repo, pr, head := "o/r", 46, "abcdef123"
	seenAt := base.Add(-time.Hour)
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseQueued, base.Add(-time.Minute), 0)
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		r := st.Round(repo, pr)
		r.NoteCoActivity(bugbotLogin, seenAt)
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	round := *f.round(repo, pr)
	obs := engine.Observation{
		Open:   true,
		Head:   head,
		HeadAt: base.AddDate(0, -1, 0),
		Reviews: []engine.ReviewSeen{
			{Bot: f.cfg.Bot, Commit: head, SubmittedAt: base},
			{Bot: bugbotLogin, Commit: "111111111", SubmittedAt: base.Add(-30 * time.Minute)},
		},
		Events: []dialect.BotEvent{{
			Kind: dialect.EvCoCommand, For: bugbotLogin, CommentID: 700,
			CreatedAt: base.Add(-time.Hour),
		}},
	}
	if _, err := f.svc.fireCoReviewWait(f.ctx, f.cfg, round, obs, "awaiting co-review", base); err != nil {
		t.Fatal(err)
	}
	parked := f.round(repo, pr)
	if parked == nil || parked.FiredAt == nil || !parked.FiredAt.Equal(obs.HeadAt) {
		t.Fatalf("co-review wait lost its old evidence floor: %+v", parked)
	}

	f.svc.selfHealCoReviewers(f.ctx, f.cfg, *parked, obs, base)
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
		t.Fatalf("self-heal fired before grace from enqueue elapsed, got %d posts", got)
	}
	f.svc.selfHealCoReviewers(f.ctx, f.cfg, *f.round(repo, pr), obs, base.Add(11*time.Minute))
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("self-heal did not fire after grace from enqueue elapsed, got %d posts", got)
	}
}

func TestCarriedCoReviewWaitRejectsOldSHALessSummaryAfterReset(t *testing.T) {
	base := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr, head := "o/r", 49, "abcdef123"
	seenAt := base.Add(-time.Hour)
	resetAt := base.Add(-time.Minute)
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseQueued, base, 0)
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		r := st.Round(repo, pr)
		r.NoteCoActivity(bugbotLogin, seenAt)
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.gh.graphQL = func(_ string, _ map[string]any, out any) error {
		payload := `{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"createdAt":"` + resetAt.Format(time.RFC3339) + `"}]}}}}`
		return json.Unmarshal([]byte(payload), out)
	}
	oldSummary := dialect.BotEvent{Kind: dialect.EvCoClean, Bot: bugbotLogin,
		CreatedAt: base.Add(-30 * time.Minute)}
	obs := engine.Observation{
		Open: true, Head: head, HeadAt: base.AddDate(0, -1, 0),
		Reviews: []engine.ReviewSeen{{Bot: f.cfg.Bot, Commit: head, SubmittedAt: base.Add(-2 * time.Minute)}},
		Events:  []dialect.BotEvent{oldSummary},
	}

	if _, err := f.svc.fireCoReviewWait(f.ctx, f.cfg, *f.round(repo, pr), obs, "awaiting co-review", base); err != nil {
		t.Fatal(err)
	}
	parked := f.round(repo, pr)
	if parked == nil || parked.FiredAt == nil || !parked.FiredAt.Equal(resetAt) {
		t.Fatalf("co-review wait did not use the reset boundary: %+v", parked)
	}
	if got := engine.Completion(*parked, obs, f.cfg.policy()); got.ReviewedBy[bugbotLogin] {
		t.Fatalf("old SHA-less summary satisfied the reset head: %+v", got)
	}
	obs.Events = append(obs.Events, dialect.BotEvent{Kind: dialect.EvCoClean, Bot: bugbotLogin,
		CreatedAt: base.Add(time.Minute)})
	if got := engine.Completion(*parked, obs, f.cfg.policy()); !got.ReviewedBy[bugbotLogin] {
		t.Fatalf("new SHA-less summary did not satisfy the reset head: %+v", got)
	}
}

// A silent clean Bugbot round leaves no timeline evidence, and observe fetches
// checks only for the current head. The durable activity proof carried by
// Supersede is therefore the only signal self-heal can use when Bugbot misses
// the next head.
func TestCoReplaySelfHealRemembersSilentCleanPreviousHead(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr := "o/r", 39
	first, second := "1111222233334444", "5555666677778888"
	f.openPull(repo, pr, first)
	f.setCommitDate(first, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the first CodeRabbit fire, got %+v", res)
	}
	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 500, first, f.clk.now())
	clean := corpusCheckRun(t, "bugbot/check-clean.json")
	clean.CompletedAt = f.clk.now()
	f.gh.setCheckRuns(first, clean)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted || r.Co(bugbotLogin).SeenActiveAt == nil {
		t.Fatalf("silent clean round did not persist Bugbot activity: %+v", r)
	}

	// The next head has no Bugbot review, comment, or check run at all.
	f.openPull(repo, pr, second)
	f.setCommitDate(second, f.clk.now())
	f.enqueue(repo, pr)
	carried := f.round(repo, pr)
	if carried == nil || carried.Co(bugbotLogin).SeenActiveAt == nil || carried.Co(bugbotLogin).AnsweredAt != nil {
		t.Fatalf("new head did not carry only the durable activity proof: %+v", carried)
	}
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the second CodeRabbit fire, got %+v", res)
	}
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
		t.Fatalf("self-heal fired before its grace period, got %d posts", got)
	}

	f.clk.advance(11 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("silent-clean activity did not recover the missed next head, got %d posts", got)
	}
}

// --- 2. Bugbot findings + BUG_ID dedupe -------------------------------------

// TestCoReplayBugbotFindingsDedupeOnBugID: Bugbot re-reports the same bug in a
// new thread after a push; the BUG_ID marker must collapse both threads to ONE
// finding, and resolving them (with the completed check) converges.
func TestCoReplayBugbotFindingsDedupeOnBugID(t *testing.T) {
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 32, "abcdef1234567890"
	head := sha[:9]
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	firedAt := base.Add(-10 * time.Minute)
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseReviewing, firedAt, 400)

	f.botReview(repo, pr, 500, sha, base.Add(-5*time.Minute))
	issues := corpusCheckRun(t, "bugbot/check-issues.json")
	issues.CompletedAt = base.Add(-4 * time.Minute)
	f.gh.setCheckRuns(sha, issues)
	// The carrier review summary (<!-- BUGBOT_REVIEW -->) must not add findings.
	f.gh.mu.Lock()
	carrier := ghapi.Review{ID: 501, CommitID: sha, State: "COMMENTED", SubmittedAt: base.Add(-4 * time.Minute), Body: corpusMessage(t, "bugbot/review-summary-issues.md")}
	carrier.User.Login = bugbotLogin
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], carrier)
	f.gh.mu.Unlock()

	finding := corpusMessage(t, "bugbot/inline-finding-high.md")
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", false, "cursor", "apps/server/src/orchestration/projector.ts", 447, 901, finding, base.Add(-4*time.Minute)),
		threadNode("PRRT_2", false, "cursor", "apps/server/src/orchestration/projector.ts", 451, 902, finding, base.Add(-2*time.Minute)),
	})

	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	bugbotFindings := 0
	for _, fd := range rep.Findings {
		if dialect.NormalizeBotName(fd.Bot) == "cursor" {
			bugbotFindings++
		}
	}
	if bugbotFindings != 1 {
		t.Fatalf("the shared BUG_ID must dedupe to one finding, got %d: %+v", bugbotFindings, rep.Findings)
	}
	if rep.Converged {
		t.Fatal("open findings must block convergence")
	}
	if st := rep.CoReviewers[dialect.NormalizeBotName(bugbotLogin)]; st.CheckState != "issues" || !st.Reviewed {
		t.Fatalf("co_reviewers must report the completed findings check, got %+v", st)
	}

	// Resolving the threads clears the findings; the completed check + the
	// CodeRabbit review converge the round.
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", true, "cursor", "apps/server/src/orchestration/projector.ts", 447, 901, finding, base.Add(-4*time.Minute)),
		threadNode("PRRT_2", true, "cursor", "apps/server/src/orchestration/projector.ts", 451, 902, finding, base.Add(-2*time.Minute)),
	})
	rep, err = f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Converged || len(rep.Findings) != 0 {
		t.Fatalf("resolved threads + completed check must converge: %+v", rep)
	}
}

// --- 3. Macroscope resolved-edit --------------------------------------------

// TestCoReplayMacroscopeResolvedEditSettlesFinding: Macroscope never resolves
// threads — it EDITS the finding comment to append its settled marker. The
// edited finding must vanish (both wordings) while an untouched finding still
// surfaces.
func TestCoReplayMacroscopeResolvedEditSettlesFinding(t *testing.T) {
	base := time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr, sha := "o/r", 33, "abcdef1234567890"
	head := sha[:9]
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseReviewing, base.Add(-10*time.Minute), 400)
	f.botReview(repo, pr, 500, sha, base.Add(-5*time.Minute))

	open := corpusMessage(t, "macroscope/inline-finding-high.md")
	resolved := corpusMessage(t, "macroscope/inline-finding-resolved.md")
	noLonger := corpusMessage(t, "macroscope/inline-finding-no-longer-relevant.md")
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", false, "macroscopeapp", "preview/handlers.ts", 57, 901, open, base.Add(-4*time.Minute)),
		threadNode("PRRT_2", false, "macroscopeapp", "Layers/ProjectionPipeline.ts", 1554, 902, resolved, base.Add(-4*time.Minute)),
		threadNode("PRRT_3", false, "macroscopeapp", "home/homeThreadList.ts", 105, 903, noLonger, base.Add(-4*time.Minute)),
	})

	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	var macro []dialect.Finding
	for _, fd := range rep.Findings {
		if dialect.NormalizeBotName(fd.Bot) == "macroscopeapp" {
			macro = append(macro, fd)
		}
	}
	if len(macro) != 1 || macro[0].Line != 57 {
		t.Fatalf("only the un-settled finding may surface, got %+v", macro)
	}

	// The open finding gets the settled edit too → nothing left, convergence
	// clears naturally (thread rebuttal machinery untouched: nothing surfaces
	// from these single-comment threads).
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_1", false, "macroscopeapp", "preview/handlers.ts", 57, 901, macroscopeSettled(t, open, sha), base.Add(-time.Minute)),
		threadNode("PRRT_2", false, "macroscopeapp", "Layers/ProjectionPipeline.ts", 1554, 902, resolved, base.Add(-4*time.Minute)),
		threadNode("PRRT_3", false, "macroscopeapp", "home/homeThreadList.ts", 105, 903, noLonger, base.Add(-4*time.Minute)),
	})
	rep, err = f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range rep.Findings {
		if dialect.NormalizeBotName(fd.Bot) == "macroscopeapp" {
			t.Fatalf("a settled-edit finding must not surface: %+v", fd)
		}
	}
	if !rep.Converged {
		t.Fatalf("with every finding settled the round must converge: %+v", rep)
	}
}

// --- 4. selfheal trigger ----------------------------------------------------

// TestCoReplaySelfHealTriggersOnceAfterGrace: an auto-active Bugbot that
// missed the head gets exactly one `bugbot run` after the grace period — no
// repost, no post while its check runs, no post over a human trigger.
func TestCoReplaySelfHealTriggersOnceAfterGrace(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, requireBugbot)
	repo, pr, sha := "o/r", 34, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-2*time.Hour))
	// Bugbot auto-reviewed an OLDER head (uncommanded evidence → AutoActive).
	f.gh.mu.Lock()
	old := ghapi.Review{ID: 400, CommitID: "0123456789aaaaaa", State: "COMMENTED", SubmittedAt: base.Add(-90 * time.Minute), Body: "### Old finding\n\n**High Severity**"}
	old.User.Login = bugbotLogin
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], old)
	f.gh.mu.Unlock()

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the CodeRabbit fire, got %+v", res)
	}
	f.botReview(repo, pr, 500, sha, f.clk.now().Add(time.Minute))

	// Inside the grace period: no trigger.
	f.clk.advance(5 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
		t.Fatalf("selfheal must wait out the grace period, got %d posts", got)
	}
	// Past the grace period: exactly one trigger, recorded on the round.
	f.clk.advance(6 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("selfheal must post exactly once, got %d", got)
	}
	if r := f.round(repo, pr); r == nil || r.Co(bugbotLogin).CommandID == 0 {
		t.Fatalf("the trigger must be recorded on the round, got %+v", r)
	}
	f.clk.advance(20 * time.Minute)
	f.pump()
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("a recorded command must never repost, got %d", got)
	}
}

// TestCoReplaySelfHealSuppressedByHumanTriggerAndRunningCheck: a live human
// `bugbot run` (or `cursor review`) after the fire, or an in-progress Bugbot
// check, suppresses the selfheal post.
func TestCoReplaySelfHealSuppressedByHumanTriggerAndRunningCheck(t *testing.T) {
	base := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		setup func(f *replayFixture, repo string, pr int, sha string)
	}{
		{name: "human trigger (alt spelling)", setup: func(f *replayFixture, repo string, pr int, sha string) {
			f.humanComment(repo, pr, 700, "cursor review", f.clk.now())
		}},
		{name: "running check", setup: func(f *replayFixture, repo string, pr int, sha string) {
			f.gh.setCheckRuns(sha, corpusCheckRun(f.t, "bugbot/check-in-progress.json"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoReplayFixture(t, base, requireBugbot)
			repo, pr, sha := "o/r", 35, "abcdef1234567890"
			f.openPull(repo, pr, sha)
			f.setCommitDate(sha, base.Add(-2*time.Hour))
			f.gh.mu.Lock()
			old := ghapi.Review{ID: 400, CommitID: "0123456789aaaaaa", State: "COMMENTED", SubmittedAt: base.Add(-90 * time.Minute), Body: "### Old finding\n\n**High Severity**"}
			old.User.Login = bugbotLogin
			f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], old)
			f.gh.mu.Unlock()

			f.enqueue(repo, pr)
			if res := f.pump(); res.Action != "fired" {
				t.Fatalf("expected the fire, got %+v", res)
			}
			f.botReview(repo, pr, 500, sha, f.clk.now().Add(time.Minute))
			tc.setup(f, repo, pr, sha)
			f.clk.advance(11 * time.Minute)
			f.pump()
			f.pump()
			if got := f.coPostedBody(repo, pr, "bugbot run"); got != 0 {
				t.Fatalf("selfheal must stay quiet (%s), got %d posts", tc.name, got)
			}
		})
	}
}

// --- 5. empty-shell regression ----------------------------------------------

// TestCoReplayEmptyShellScopedToConfiguredBot: both new bots deliver findings
// on empty-body COMMENTED reviews — those carriers ARE review evidence and
// must be retained, while the configured bot's empty shell is still dropped.
func TestCoReplayEmptyShellScopedToConfiguredBot(t *testing.T) {
	base := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr, sha := "o/r", 36, "abcdef1234567890"
	head := sha[:9]
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseReviewing, base.Add(-10*time.Minute), 400)

	// A CodeRabbit empty COMMENTED shell (dropped) and a Macroscope empty
	// COMMENTED carrier (retained as its head review evidence).
	f.shellReview(repo, pr, 500, sha, base.Add(-5*time.Minute))
	f.gh.mu.Lock()
	carrier := ghapi.Review{ID: 501, CommitID: sha, State: "COMMENTED", SubmittedAt: base.Add(-4 * time.Minute)}
	carrier.User.Login = macroLogin
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], carrier)
	f.gh.mu.Unlock()

	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy[f.bot] {
		t.Fatalf("the configured bot's empty shell must not count as review evidence: %+v", rep.ReviewedBy)
	}
	if st := rep.CoReviewers[dialect.NormalizeBotName(macroLogin)]; !st.Reviewed {
		t.Fatalf("macroscope's empty carrier review must count as head evidence: %+v", rep.CoReviewers)
	}
}

// --- 6. wanted-only Macroscope verdict --------------------------------------

// TestCoReplayWantedOnlyMacroscopeVerdictNeverGates: a wanted (non-required)
// Macroscope surfaces its status and verdict, and a "Needs human review"
// verdict never blocks convergence or changes exit semantics.
func TestCoReplayWantedOnlyMacroscopeVerdictNeverGates(t *testing.T) {
	base := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr, sha := "o/r", 37, "abcdef1234567890"
	head := sha[:9]
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	firedAt := base.Add(-10 * time.Minute)
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseReviewing, firedAt, 400)
	f.botReview(repo, pr, 500, sha, base.Add(-5*time.Minute))

	// Macroscope participates: an inline-carrier review, its checks, and the
	// per-round approvability verdict comment.
	f.gh.mu.Lock()
	carrier := ghapi.Review{ID: 501, CommitID: sha, State: "COMMENTED", SubmittedAt: base.Add(-4 * time.Minute)}
	carrier.User.Login = macroLogin
	f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], carrier)
	verdict := ghapi.IssueComment{ID: 800, Body: corpusMessage(t, "macroscope/approvability-needs-human.md"), CreatedAt: base.Add(-3 * time.Minute), UpdatedAt: base.Add(-3 * time.Minute)}
	verdict.User.Login = macroLogin
	f.gh.comments[fakeKey(repo, pr)] = append(f.gh.comments[fakeKey(repo, pr)], verdict)
	f.gh.mu.Unlock()
	clean := corpusCheckRun(t, "macroscope/check-correctness-clean.json")
	clean.CompletedAt = base.Add(-3 * time.Minute)
	f.gh.setCheckRuns(sha, clean)

	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Converged || len(rep.Findings) != 0 {
		t.Fatalf("a needs-human verdict must not gate convergence: %+v", rep)
	}
	st := rep.CoReviewers[dialect.NormalizeBotName(macroLogin)]
	if st.Verdict != "needs_human_review" || !st.Reviewed || st.CheckState != "clean" {
		t.Fatalf("co_reviewers must carry the informational verdict, got %+v", st)
	}
	// The verdict comment itself must never surface as a finding.
	for _, fd := range rep.Findings {
		if fd.CommentID == 800 {
			t.Fatalf("the approvability comment surfaced as a finding: %+v", fd)
		}
	}
}

// --- 7. always-mode fire-time trigger ---------------------------------------

// TestCoReplayAlwaysModePostsWithFireAndHealsAfterFailure: an always-mode
// Bugbot trigger posts atomically with the CodeRabbit fire; when the trigger
// post fails, the fire still stands and a later pump heals it exactly once.
func TestCoReplayAlwaysModePostsWithFireAndHealsAfterFailure(t *testing.T) {
	base := time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC)
	alwaysBugbot := func(cfg *Config) {
		requireBugbot(cfg)
		for i := range cfg.CoBots {
			if cfg.CoBots[i].Name == "bugbot" {
				cfg.CoBots[i].Trigger = engine.TriggerAlways
			}
		}
	}

	f := newCoReplayFixture(t, base, alwaysBugbot)
	repo, pr, sha := "o/r", 38, "abcdef1234567890"
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected the fire, got %+v", res)
	}
	if got := f.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("always-mode must post the trigger with the fire, got %d", got)
	}
	if r := f.round(repo, pr); r == nil || r.Co(bugbotLogin).CommandID == 0 {
		t.Fatalf("the trigger id must land in the fire write, got %+v", r)
	}

	// Failure path: the trigger post fails while the fire succeeds.
	f2 := newCoReplayFixture(t, base, alwaysBugbot)
	f2.openPull(repo, pr, sha)
	f2.setCommitDate(sha, base.Add(-time.Hour))
	f2.gh.mu.Lock()
	f2.gh.postBodyErrs = map[string]error{"bugbot run": errors.New("boom")}
	f2.gh.mu.Unlock()
	f2.enqueue(repo, pr)
	if res := f2.pump(); res.Action != "fired" {
		t.Fatalf("the CodeRabbit fire must survive a failed trigger post, got %+v", res)
	}
	if r := f2.round(repo, pr); r == nil || r.Co(bugbotLogin).CommandID != 0 {
		t.Fatalf("a failed post must leave the command unset for the heal, got %+v", r)
	}
	f2.gh.mu.Lock()
	f2.gh.postBodyErrs = nil
	f2.gh.mu.Unlock()
	f2.clk.advance(time.Minute)
	f2.pump() // progress/sweep path heals the missing trigger
	if got := f2.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("the heal must post exactly once, got %d", got)
	}
	if r := f2.round(repo, pr); r == nil || r.Co(bugbotLogin).CommandID == 0 {
		t.Fatalf("the healed trigger must be recorded, got %+v", r)
	}
	f2.clk.advance(time.Minute)
	f2.pump()
	if got := f2.coPostedBody(repo, pr, "bugbot run"); got != 1 {
		t.Fatalf("no repost after the heal, got %d", got)
	}
}

// --- 8. the summary-only co-review wait must be satisfiable ------------------

// TestCoReplaySummaryOnlyWaitAcceptsExistingAnswer reproduces a repeatable
// 20-minute timeout hit while dogfooding krisHQ#1021.
//
// On a summary-only PR there is no CodeRabbit review to anchor the co-review
// wait on, and crq posts no trigger because the co-reviewer auto-reviews. The
// anchor therefore fell through to "now", i.e. the moment crq happened to look
// — which lands AFTER the co-reviewer's existing answer for this head. The wait
// could then never be satisfied: the bot had already spoken and would not speak
// again for an unchanged head, so every round timed out identically.
//
// The floor must be when the HEAD appeared, not when crq noticed it.
func TestCoReplaySummaryOnlyWaitAcceptsExistingAnswer(t *testing.T) {
	base := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = append(cfg.RequiredBots, dialect.CodexBotLogin)
	})
	repo, pr, sha := "o/private", 1021, "23179e9aa1234567"
	f.openPull(repo, pr, sha)
	// The head was pushed well before this round is observed.
	f.setCommitDate(sha, base.Add(-30*time.Minute))

	// CodeRabbit's Free-plan walkthrough: no review is ever coming.
	f.botComment(repo, pr, 900, corpusMessage(t, "coderabbit/summary-only-free-plan.md"), base.Add(-25*time.Minute))
	// Codex auto-reviewed and found nothing, before crq looked. Its clean
	// summary names no SHA (the legacy shape), so the round's evidence floor is
	// the ONLY thing that can bind it to this head — which is precisely why a
	// floor set at observation time strands it.
	f.codexComment(repo, pr, 901, corpusMessage(t, "codex/clean-summary-legacy.md"), base.Add(-20*time.Minute))

	f.enqueue(repo, pr)
	f.pump()

	// crq must not have asked CodeRabbit for anything.
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("a summary-only round must never post the review command, got %d", got)
	}
	rep, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.PrimaryUnavailable {
		t.Fatalf("precondition: the round must read as primary-unavailable, got %+v", rep)
	}
	// The answer that already exists must satisfy the round rather than being
	// stranded below a floor set at observation time.
	if !rep.ReviewedBy[dialect.CodexBotLogin] {
		t.Fatalf("the co-reviewer's existing answer for this head must count: %#v", rep.ReviewedBy)
	}
	if !rep.Converged {
		t.Fatalf("the round must converge instead of waiting out the deadline: %+v", rep)
	}
}

// --- 8. Bugbot sibling threads ----------------------------------------------

// bugbotAtSHA rewrites a corpus finding's "for commit <sha>" footer, so the same
// BUG_ID can be replayed as reported on an earlier head.
func bugbotAtSHA(t *testing.T, body, sha string) string {
	t.Helper()
	old := dialect.BugbotReviewedCommitSHA(body)
	if old == "" {
		t.Fatal("precondition: the corpus finding must name the commit it reviewed")
	}
	return strings.Replace(body, old, sha, 1)
}

// TestCoReplayBugbotSiblingSettlementUsesHead: one BUG_ID can sit in a settled
// thread and an open one at the same time, and the two readings pull opposite
// ways — a leftover duplicate of the thread just resolved, or a genuine
// re-report after a regression. Comment timestamps cannot tell them apart:
// resolving a thread does not restamp its comment, so the settled occurrence
// stays OLDER than a sibling filed minutes before it and every duplicate reads
// as a regression. The commit each occurrence names is what decides.
func TestCoReplayBugbotSiblingSettlementUsesHead(t *testing.T) {
	base := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	f := newCoReplayFixture(t, base, nil)
	repo, pr := "o/r", 44
	finding := corpusMessage(t, "bugbot/inline-finding-high.md")
	sha := dialect.BugbotReviewedCommitSHA(finding)
	head := sha[:9]
	f.openPull(repo, pr, sha)
	f.setCommitDate(sha, base.Add(-time.Hour))
	seedRound(t, f.store, f.cfg, repo, pr, head, PhaseReviewing, base.Add(-10*time.Minute), 400)
	f.botReview(repo, pr, 500, sha, base.Add(-5*time.Minute))

	bugbotFindings := func(t *testing.T) int {
		t.Helper()
		rep, err := f.svc.Feedback(f.ctx, repo, pr)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, fd := range rep.Findings {
			if dialect.NormalizeBotName(fd.Bot) == "cursor" {
				n++
			}
		}
		return n
	}

	// Two same-head siblings, the OLDER one resolved. The open one is a leftover
	// duplicate, not a regression — surfacing it forces the user to resolve every
	// copy by hand, which is the whole reason family suppression exists.
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_settled", true, "cursor", "projector.ts", 447, 901, finding, base.Add(-9*time.Minute)),
		threadNode("PRRT_dup", false, "cursor", "projector.ts", 451, 902, finding, base.Add(-8*time.Minute)),
	})
	if n := bugbotFindings(t); n != 0 {
		t.Fatalf("a duplicate of a settled same-head finding must not resurface, got %d", n)
	}

	// The regression shape: the settled occurrence names an EARLIER commit and
	// the open one names the head. Bugbot found it again on the current code, so
	// it must surface even though a thread for that id is resolved.
	f.threadsGraphQL([]map[string]any{
		threadNode("PRRT_settled", true, "cursor", "projector.ts", 447, 901,
			bugbotAtSHA(t, finding, "1111111111111111111111111111111111111111"), base.Add(-9*time.Minute)),
		threadNode("PRRT_again", false, "cursor", "projector.ts", 451, 902, finding, base.Add(-8*time.Minute)),
	})
	if n := bugbotFindings(t); n != 1 {
		t.Fatalf("a re-report on the current head is a regression and must surface, got %d", n)
	}
}
