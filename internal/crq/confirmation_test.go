package crq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// A real review-thread payload retains the review ID when its finding is
// resolved. That distinguishes dismissal from a later clean review at the SHA.
func confirmationThread(bot, head string, reviewID int64, resolved bool) reviewThread {
	var th reviewThread
	th.ID, th.Path, th.Line = "PRRT_confirmation", "file.go", 7
	th.IsResolved = resolved
	n := reviewThreadComment{DatabaseID: 801, Body: "**Potential issue** missing test membership", Line: 7}
	n.Author.Login, n.Commit.OID = dialect.NormalizeBotName(bot), head
	n.PullRequestReview.DatabaseID = reviewID
	th.Comments.Nodes = []reviewThreadComment{n}
	th.Comments.TotalCount = 1
	return th
}

func TestConfirmationAfterThreadlessDismissal(t *testing.T) {
	f := newReplayFixture(t, time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC))
	repo, pr, head := "owner/repo", 1313, "8674e32b3"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, f.clk.now().Add(-time.Hour))
	f.setLocalWork(false, "")
	f.next(repo, pr)
	f.clk.advance(time.Minute)
	f.corpusReview(t, repo, pr, 900, head, "coderabbit/findings-outside-diff.md")
	first := f.next(repo, pr)
	f.wantAction(first, engine.ActionFix)
	id := first.Findings[0].ID
	if _, err := f.svc.Dismiss(f.ctx, repo, pr, []string{id}, "verified false positive"); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(time.Minute)
	f.wantAction(f.next(repo, pr), engine.ActionWait)
	if r := f.round(repo, pr); r.ConfirmationAfter == nil || r.IsDismissed(id) {
		t.Fatalf("confirmation retained a suppressing dismissal: %+v", r)
	}
	f.clk.advance(time.Minute)
	f.corpusReview(t, repo, pr, 901, head, "coderabbit/findings-outside-diff.md")
	again := f.next(repo, pr)
	f.wantAction(again, engine.ActionFix)
	if again.Findings[0].ID != id {
		t.Fatal("test must re-report the identical finding")
	}
	if result, err := f.svc.Dismiss(f.ctx, repo, pr, []string{id}, "still false positive"); err != nil || len(result.Dismissed) != 1 {
		t.Fatalf("fresh finding was treated as a retry: %+v %v", result, err)
	}
	f.wantAction(f.next(repo, pr), engine.ActionBlocked)
	report, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil || report.Status != "held" || report.Converged {
		t.Fatalf("feedback hid the exhausted confirmation: %+v %v", report, err)
	}
	// Changing the code starts an ordinary review and resets the allowance.
	f.clk.advance(time.Minute)
	f.setHead(repo, pr, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", f.clk.now())
	f.next(repo, pr)
	if f.round(repo, pr).ConfirmationAfter != nil {
		t.Fatal("confirmation marker leaked to a new head")
	}
}

func TestConfirmationGuards(t *testing.T) {
	for _, mode := range []string{"readonly", "dry-run", "one-pass", "withdrawn", "new-head", "local-work", "shell", "carrier", "loop"} {
		t.Run(mode, func(t *testing.T) {
			f := newReplayFixture(t, time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC))
			repo, pr, head := "owner/repo", 1313, "8674e32b3"
			f.openPull(repo, pr, head)
			f.setCommitDate(head, f.clk.now().Add(-time.Hour))
			f.setLocalWork(false, "")
			f.next(repo, pr)
			f.clk.advance(time.Minute)
			f.botReview(repo, pr, 900, head, f.clk.now())
			th := confirmationThread(f.bot, head, 900, true)
			installConfirmationThread(f, &th)
			f.clk.advance(time.Minute)
			before := len(f.gh.posted)
			switch mode {
			case "readonly":
				report, err := f.svc.FeedbackReadOnly(f.ctx, repo, pr)
				if err != nil || report.Converged {
					t.Fatalf("resolved thread converged: %+v %v", report, err)
				}
			case "dry-run":
				f.svc.cfg.DryRun = true
				f.wantAction(f.next(repo, pr), engine.ActionWait)
			case "one-pass":
				f.svc.cfg.OnePass = true
				report, err := f.svc.Feedback(f.ctx, repo, pr)
				if err != nil || !report.Converged {
					t.Fatalf("explicit one-pass mode should finish: %+v %v", report, err)
				}
			case "withdrawn":
				reply := reviewThreadComment{Body: "<review_comment_withdrawn>"}
				reply.Author.Login = f.bot
				th.Comments.Nodes = append(th.Comments.Nodes, reply)
				th.Comments.TotalCount++
				f.wantAction(f.next(repo, pr), engine.ActionDone)
			case "new-head":
				f.setHead(repo, pr, "bbbbbbbb2")
				f.setCommitDate("bbbbbbbb2", f.clk.now())
				f.botReview(repo, pr, 901, "bbbbbbbb2", f.clk.now())
				report, err := f.svc.Feedback(f.ctx, repo, pr)
				if err != nil || !report.Converged {
					t.Fatalf("old resolved thread blocked new head: %+v %v", report, err)
				}
			case "local-work":
				f.setLocalWork(true, "fix ready")
				f.wantAction(f.next(repo, pr), engine.ActionPush)
			case "shell":
				shell := ghapi.Review{ID: 901, CommitID: head, State: "COMMENTED", SubmittedAt: f.clk.now()}
				shell.User.Login = f.bot
				f.gh.reviews[fakeKey(repo, pr)] = append(f.gh.reviews[fakeKey(repo, pr)], shell)
				report, err := f.svc.Feedback(f.ctx, repo, pr)
				if err != nil || report.Converged || !report.confirmationRequired {
					t.Fatalf("empty carrier confirmed a dismissal: %+v %v", report, err)
				}
			case "carrier":
				// The inline batch was a carrier and its summary followed it.
				f.gh.reviews[fakeKey(repo, pr)][0].Body = ""
				f.botReview(repo, pr, 901, head, f.clk.now())
				report, err := f.svc.Feedback(f.ctx, repo, pr)
				if err != nil || report.Converged || !report.confirmationRequired {
					t.Fatalf("carrier finding was lost behind its summary: %+v %v", report, err)
				}
				// Another substantive review is an independent confirmation.
				f.clk.advance(time.Minute)
				f.botReview(repo, pr, 902, head, f.clk.now())
				report, err = f.svc.Feedback(f.ctx, repo, pr)
				if err != nil || !report.Converged || report.confirmationRequired {
					t.Fatalf("old carrier invalidated a later review: %+v %v", report, err)
				}
			case "loop":
				f.gh.postHook = func() {
					f.clk.advance(time.Second)
					f.botReview(repo, pr, 901, head, f.clk.now())
				}
				ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
				defer cancel()
				report, code, err := f.svc.Loop(ctx, repo, pr)
				if err != nil || code != 0 || !report.Converged {
					t.Fatalf("loop did not request and consume confirmation: %+v code=%d err=%v", report, code, err)
				}
				if len(f.gh.posted) != before+1 {
					t.Fatalf("loop posted %d commands, want 1", len(f.gh.posted)-before)
				}
				return
			}
			if len(f.gh.posted) != before || f.round(repo, pr).ConfirmationAfter != nil {
				t.Fatal("guard queued a confirmation")
			}
		})
	}
}

func installConfirmationThread(f *replayFixture, th *reviewThread) {
	f.gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return noForcePush(query, nil, out)
		}
		data, err := json.Marshal(th)
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(fmt.Sprintf(`{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s],"pageInfo":{"hasNextPage":false}}}}}`, data)), out)
	}
}

func TestConfirmationAfterResolvedFinding(t *testing.T) {
	for _, codexOnly := range []bool{false, true} {
		t.Run(fmt.Sprint("codexOnly=", codexOnly), func(t *testing.T) {
			f := newReplayFixture(t, time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC))
			if codexOnly {
				f.cfg.PrimaryOff = true
				f.cfg.RequiredBots = []string{dialect.CodexBotLogin}
				f.cfg.CoBots = codexCoBots(f.cfg.RequiredBots)
				f.svc.cfg = f.cfg
				f.bot = dialect.CodexBotLogin
			}
			repo, pr, head := "owner/repo", 1313, "8674e32b3"
			f.openPull(repo, pr, head)
			f.setCommitDate(head, f.clk.now().Add(-time.Hour))
			f.setLocalWork(false, "")
			th := confirmationThread(f.bot, head, 900, false)
			installConfirmationThread(f, &th)
			// No thread exists until its review arrives.
			th.Comments.Nodes = nil
			f.wantAction(f.next(repo, pr), engine.ActionWait)
			f.clk.advance(time.Minute)
			f.botReview(repo, pr, 900, head, f.clk.now())
			th = confirmationThread(f.bot, head, 900, false)
			f.wantAction(f.next(repo, pr), engine.ActionFix)
			th.IsResolved = true
			f.clk.advance(time.Minute)
			before := len(f.gh.posted)
			report, err := f.svc.Feedback(f.ctx, repo, pr)
			if err != nil || report.Converged {
				t.Fatalf("resolved finding converged: %+v %v", report, err)
			}
			f.wantAction(f.next(repo, pr), engine.ActionWait)
			if r := f.round(repo, pr); r.ConfirmationAfter == nil {
				t.Fatal("confirmation not queued")
			}
			// Another process holding the original snapshot cannot replace or
			// complete the pass that just fired.
			fresh := f.round(repo, pr)
			if queued, err := f.svc.queueConfirmation(f.ctx, report); err != nil || queued {
				t.Fatalf("stale caller queued again: queued=%v err=%v", queued, err)
			}
			if got := f.round(repo, pr); got.Seq != fresh.Seq || got.Phase != fresh.Phase {
				t.Fatalf("stale caller changed the new round: before=%+v after=%+v", fresh, got)
			}
			for i := 0; i < 3; i++ {
				f.wantAction(f.next(repo, pr), engine.ActionWait)
			}
			if got := len(f.gh.posted); got != before+1 {
				t.Fatalf("posted %d new commands; want exactly one; round=%+v", got-before, f.round(repo, pr))
			}
			if !codexOnly && !strings.HasSuffix(f.gh.posted[before], "@coderabbitai full review") {
				t.Fatalf("same-head confirmation must request a full review: %s", f.gh.posted[before])
			}
			// A fresh clean review finishes even though the old resolved thread remains.
			f.clk.advance(time.Minute)
			f.botReview(repo, pr, 901, head, f.clk.now())
			f.wantAction(f.next(repo, pr), engine.ActionDone)
			// A later finding on that confirmation must not buy endless re-reviews.
			th = confirmationThread(f.bot, head, 901, false)
			f.wantAction(f.next(repo, pr), engine.ActionFix)
			th.IsResolved = true
			f.wantAction(f.next(repo, pr), engine.ActionBlocked)
			if got := len(f.gh.posted); got != before+1 {
				t.Fatal("second dismissal posted another review")
			}
		})
	}
}

func TestConfirmationIgnoresOldCommentAndCheckEvidence(t *testing.T) {
	f := newReplayFixture(t, time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC))
	repo, pr, head := "owner/repo", 1313, "8674e32b3"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, f.clk.now().Add(-time.Hour))
	cfg := f.cfg
	cfg.CoBots = parseAllCoBots(nil)
	old := f.clk.now().Add(-time.Minute)
	comment := ghapi.IssueComment{ID: 100, Body: "## Codex Review\n\nDidn't find any major issues. Keep them coming!", CreatedAt: old, UpdatedAt: old}
	comment.User.Login = dialect.CodexBotLogin
	f.gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{comment}
	check := ghapi.CheckRun{ID: 101, Name: "Cursor Bugbot", HeadSHA: head, Status: "completed", Conclusion: "success", CompletedAt: old}
	check.App.Slug = "cursor"
	f.gh.setCheckRuns(head, check)
	cutoff := f.clk.now()
	round := Round{Repo: repo, PR: pr, Head: head, Phase: PhaseReviewing, ConfirmationAfter: &cutoff}
	obs, err := f.svc.observe(f.ctx, cfg, repo, pr, &round, nil, f.clk.now())
	if err != nil || len(obs.eng.Checks) != 0 || len(obs.eng.Events) != 0 || len(obs.comments) != 0 {
		t.Fatalf("old evidence leaked into confirmation: %+v %v", obs, err)
	}
	// A completed check without a timestamp cannot prove freshness either.
	check.CompletedAt = time.Time{}
	f.gh.setCheckRuns(head, check)
	obs, err = f.svc.observe(f.ctx, cfg, repo, pr, &round, nil, f.clk.now())
	if err != nil || len(obs.eng.Checks) != 0 {
		t.Fatalf("undated completed check counted: %+v %v", obs, err)
	}
	f.clk.advance(time.Second)
	comment.UpdatedAt = f.clk.now()
	f.gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{comment}
	check.CompletedAt = f.clk.now()
	f.gh.setCheckRuns(head, check)
	obs, err = f.svc.observe(f.ctx, cfg, repo, pr, &round, nil, f.clk.now())
	if err != nil || len(obs.eng.Checks) != 1 || len(obs.eng.Events) != 1 || len(obs.comments) != 1 {
		t.Fatalf("fresh evidence was dropped: %+v %v", obs, err)
	}
}
