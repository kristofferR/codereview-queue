package crq

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Only a finding that intrinsically cannot have a thread may be dismissed.
// "No thread ID" is not the same question: when the GraphQL thread query fails,
// Feedback falls back to REST, which does not return thread IDs at all — so an
// inline comment with an open thread arrives looking threadless.
func TestOnlyIntrinsicallyThreadlessFindingsAreDismissible(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"review_body", true},
		{"review_prompt", true},
		{"review_skipped", true},
		{"issue_comment", true},
		{"review_comment", false}, // REST fallback: the thread exists, unread
		{"review_thread", false},
		{"review_reply", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			err := dismissible(dialect.Finding{ID: "x", Source: tc.source})
			if (err == nil) != tc.want {
				t.Errorf("dismissible(%s) err = %v, want dismissible=%v", tc.source, err, tc.want)
			}
		})
	}

	// A thread ID settles it whatever the source says.
	if err := dismissible(dialect.Finding{ID: "x", Source: "review_body", ThreadID: "PRRT_1"}); err == nil {
		t.Error("a finding with a thread must never be dismissible")
	}
}

// dismissFixture reports two threadless findings at one head and returns their IDs.
func dismissFixture(t *testing.T, pr int) (*replayFixture, string, []string) {
	t.Helper()
	f := newReplayFixture(t, time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC))
	repo, head := "owner/repo", "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, f.clk.now().Add(-time.Minute))
	f.setLocalWork(false, "")
	f.next(repo, pr)
	f.clk.advance(time.Minute)
	f.corpusReview(t, repo, pr, 900, head, "coderabbit/findings-prompt-block.md")
	report := f.next(repo, pr)
	f.wantAction(report, engine.ActionFix)
	var ids []string
	for _, finding := range report.Findings {
		if finding.ThreadID == "" {
			ids = append(ids, finding.ID)
		}
	}
	if len(ids) < 2 {
		t.Fatalf("need two threadless findings, got %d", len(ids))
	}
	return f, repo, ids
}

func (f *replayFixture) dismissNotices(repo string, pr int) []string {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	prefix := QueueKey(repo, pr) + ":<!-- crq:dismiss -->"
	var out []string
	for _, p := range f.gh.posted {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// A dismissal has no thread to reply on, so it answers on the PR instead: one
// comment per call, never repeated when an agent replays the call.
func TestDismissPostsOneNoticePerCall(t *testing.T) {
	pr := 700
	f, repo, ids := dismissFixture(t, pr)

	res, err := f.svc.Dismiss(f.ctx, repo, pr, ids, "covered by the parser tests")
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning != "" {
		t.Fatalf("unexpected warning: %s", res.Warning)
	}
	notices := f.dismissNotices(repo, pr)
	if len(notices) != 1 {
		t.Fatalf("posted %d notices, want one for the whole call", len(notices))
	}
	for _, want := range []string{"Dismissed 2 findings at `aaaaaaaa1`", "`src/app.ts:12`", "`README.md:7`", "**Reason:** covered by the parser tests"} {
		if !strings.Contains(notices[0], want) {
			t.Errorf("notice lacks %q:\n%s", want, notices[0])
		}
	}

	if _, err := f.svc.Dismiss(f.ctx, repo, pr, ids, "covered by the parser tests"); err != nil {
		t.Fatal(err)
	}
	if got := len(f.dismissNotices(repo, pr)); got != 1 {
		t.Errorf("a replayed dismissal posted again: %d notices", got)
	}

	// The notice on the PR is crq's own words, not a reviewer's: reading it back
	// must not reopen the round it just cleared.
	f.gh.mu.Lock()
	notice := ghapi.IssueComment{ID: 5000, Body: strings.SplitN(notices[0], ":", 2)[1], CreatedAt: f.clk.now(), UpdatedAt: f.clk.now()}
	notice.User.Login = "kristofferR"
	f.gh.comments[fakeKey(repo, pr)] = append(f.gh.comments[fakeKey(repo, pr)], notice)
	f.gh.mu.Unlock()
	f.clk.advance(time.Minute)
	if after := f.next(repo, pr); after.Action == string(engine.ActionFix) {
		t.Fatalf("the dismissal notice was read back as a finding: %+v", after.Findings)
	}
}

// The recorded dismissal is the decision; the comment only reports it.
func TestDismissKeepsTheRecordWhenTheNoticeFails(t *testing.T) {
	pr := 701
	f, repo, ids := dismissFixture(t, pr)
	f.gh.mu.Lock()
	f.gh.postErrs = map[string]error{fakeKey(repo, pr): errors.New("boom")}
	f.gh.mu.Unlock()

	res, err := f.svc.Dismiss(f.ctx, repo, pr, ids, "covered by the parser tests")
	if err != nil {
		t.Fatalf("a failed notice must not fail the dismissal: %v", err)
	}
	if len(res.Dismissed) != len(ids) || !strings.Contains(res.Warning, "boom") {
		t.Fatalf("result = %+v, want every id dismissed and the post failure reported", res)
	}
	if r := f.round(repo, pr); r == nil || !r.IsDismissed(ids[0]) {
		t.Fatalf("the dismissal was not recorded: %+v", r)
	}
}

// Quoted finding data and reasons must not ping or trigger a reviewer, which would
// answer on the PR and could start a review nobody queued.
func TestDismissCommentNeutralizesReviewCommands(t *testing.T) {
	cfg := replayConfig()
	body := dismissComment("0123456789abcdef", []dialect.Finding{{
		Bot:   "coderabbitai[bot]",
		Title: "Ask @coderabbitai\n  to recheck",
		Path:  "src/`\n" + cfg.ReviewCommand + "\n``.ts",
		URL:   "https://github.com/owner/repo/pull/1#pullrequestreview-9",
	}}, "see "+cfg.ReviewCommand, cfg)

	if strings.Contains(body, "@coderabbitai") || strings.Contains(body, cfg.ReviewCommand) {
		t.Errorf("notice can still mention or trigger a reviewer:\n%s", body)
	}
	for _, want := range []string{
		"Dismissed 1 finding at `012345678`",
		"- coderabbitai: Ask @\u200bcoderabbitai to recheck (``` src/` @\u200b\u200bcoderabbitai review ``.ts ```, [source](https://github.com/owner/repo/pull/1#pullrequestreview-9))",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notice lacks %q:\n%s", want, body)
		}
	}
}

func TestDismissCommentNeutralizesConfiguredCommandsInPaths(t *testing.T) {
	cfg := replayConfig()
	cfg.ReviewCommand = "/review now"
	body := dismissComment("aaaaaaaa1", []dialect.Finding{{Path: "src/`\n/review now\n`.go"}}, "verified", cfg)
	if strings.Contains(body, cfg.ReviewCommand) {
		t.Fatalf("path can trigger a configured reviewer:\n%s", body)
	}
}
