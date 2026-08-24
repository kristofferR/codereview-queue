package crq

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Tidy deletes, so what it must NOT touch matters more than what it does. A PR
// with a spent command from a superseded round, the live round's own command,
// and a person's request to review.
func TestTidyRemovesOnlySpentCommandsCrqPosted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 12
	head := "bbbbbbbb2"

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits[head] = commitAt(now.Add(-10 * time.Minute))

	add := func(id int64, login, body string, at time.Time) {
		c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		c.User.Login = login
		gh.comments[fakeKey(repo, pr)] = append(gh.comments[fakeKey(repo, pr)], c)
	}
	add(100, "kristofferR", cfg.ReviewCommand, now.Add(-2*time.Hour)) // spent: old round
	add(200, "kristofferR", cfg.ReviewCommand, now.Add(-time.Minute)) // the live round's
	// A person's request that a round ADOPTED as its own fire. The round records
	// it in exactly the same CommandID as one crq posted, so only "did crq write
	// it" keeps this from being deleted out from under whoever asked.
	add(300, "someone-else", cfg.ReviewCommand, now.Add(-4*time.Hour))
	// The bot's own answer. It must survive: an auto-reply can be a rate-limit or
	// skip notice, which crq reads as evidence and surfaces as a finding.
	add(400, cfg.Bot, "<!-- auto-generated reply by CodeRabbit -->\nReview in progress", now.Add(-2*time.Hour))

	// The bot reviewed after the old command, which is the evidence that it read
	// it — and reviewed at the CURRENT head, so the round is answered.
	review := ghapi.Review{ID: 900, CommitID: head, State: "COMMENTED", SubmittedAt: now.Add(-90 * time.Minute), Body: "looks fine"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Three rounds: one that adopted the person's command, one holding the spent
	// command crq posted, and the live round holding 200.
	if _, err := store.Update(ctx, func(st *State) error {
		adopting, err := st.NewRound(repo, pr, "aaaaaaaa0", now.Add(-5*time.Hour))
		if err != nil {
			return err
		}
		if err := adopting.Fire(300, now.Add(-4*time.Hour)); err != nil {
			return err
		}
		st.PutRound(*adopting)
		if _, err := st.Supersede(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		old := st.Round(repo, pr)
		if err := old.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := old.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		old.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		st.PutRound(*old)
		if _, err := st.Supersede(repo, pr, head, now.Add(-time.Hour)); err != nil {
			return err
		}
		live := st.Round(repo, pr)
		if err := live.Reserve("t2", "h", now.Add(-2*time.Minute)); err != nil {
			return err
		}
		if err := live.Fire(200, now.Add(-time.Minute)); err != nil {
			return err
		}
		live.RecordPosted(cfg.Bot, 200, now.Add(-time.Minute))
		st.PutRound(*live)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != 100 {
		t.Fatalf("deleted = %v, want only the spent command from the superseded round", result.Deleted)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 1 || got[0].ID != 100 {
		t.Fatalf("tidied commands = %v, want a durable tombstone for comment 100", got)
	}

	left := map[int64]bool{}
	for _, c := range gh.comments[fakeKey(repo, pr)] {
		left[c.ID] = true
	}
	if !left[200] {
		t.Error("the live round's command was deleted; the next pump will post a duplicate")
	}
	if !left[300] {
		t.Error("a person's review request was deleted; adopting one is not writing it")
	}
	if !left[400] {
		t.Error("a bot comment was deleted; those carry rate-limit and skip notices crq reads as findings")
	}
}

// Dry run must report the same decision and change nothing.
func TestTidyDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 13

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		r, err := st.NewRound(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour))
		if err != nil {
			return err
		}
		if err := r.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		if err := r.Complete(); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || len(result.Deleted) != 1 {
		t.Fatalf("result = %+v, want the same decision, reported not applied", result)
	}
	if len(gh.comments[fakeKey(repo, pr)]) != 1 {
		t.Error("a dry run deleted a comment")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 0 {
		t.Errorf("a dry run recorded tidied commands: %v", got)
	}
	if got := st.TidyReactionCursors[QueueKey(repo, pr)]; got != 0 {
		t.Errorf("a dry run advanced the reaction cursor to %d", got)
	}
}

// A command may disappear before crq sees it. It must not be deleted again, but
// its FIFO position must survive after the round falls out of the archive.
func TestTidySkipsCommandsAlreadyGone(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 14

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	// The round remembers posting command 100; the PR no longer has it.
	if _, err := store.Update(ctx, func(st *State) error {
		r, err := st.NewRound(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour))
		if err != nil {
			return err
		}
		if err := r.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		if err := r.Complete(); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Errorf("deleted = %v, want nothing: the comment is already gone", result.Deleted)
	}
	if len(gh.deleted) != 0 {
		t.Errorf("issued %d delete(s) for a comment that is not on the pr", len(gh.deleted))
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 1 || got[0].ID != 100 {
		t.Fatalf("tidied commands = %v, want a durable tombstone for absent comment 100", got)
	}
}

// A recorded ID proves crq wrote the comment, not that it is still the one-line
// command crq wrote. crq posts as the operator's own account, so anyone who can
// edit that comment can turn it into something they meant to keep — and the body
// is the only thing that still says which it is.
func TestTidyKeepsATriggerCommentEditedIntoSomethingElse(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 15

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: "Asked for a re-review here because the auth change needs a second pair of eyes.", CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 || len(gh.deleted) != 0 {
		t.Fatalf("deleted %v: an edited comment is someone's words, not a spent request", result.Deleted)
	}
}

// The initial issue-comment page is only a snapshot. Recheck the exact comment
// after the slower force-push and reaction reads, immediately before DELETE.
func TestTidyKeepsATriggerEditedDuringThePass(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 24

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	original := ghapi.IssueComment{
		ID: 100, Body: cfg.ReviewCommand,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	original.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{original}
	edited := original
	edited.Body = cfg.ReviewCommand + "\n\nKeep this context."
	edited.UpdatedAt = now.Add(-time.Minute)
	gh.getComment = func(_ string, _ int64) (ghapi.IssueComment, error) {
		return edited, nil
	}
	review := ghapi.Review{
		ID: 900, CommitID: "bbbbbbbb2", State: "COMMENTED", Body: "looks fine",
		SubmittedAt: now.Add(-90 * time.Minute),
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(original.ID, original.CreatedAt); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, original.ID, original.CreatedAt)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 || len(gh.deleted) != 0 {
		t.Fatalf("edited comment was deleted: result=%#v deleted=%v", result, gh.deleted)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 0 {
		t.Fatalf("kept comment retained a deletion tombstone: %v", got)
	}
}

// A delete GitHub refuses must reach the caller. An empty Deleted otherwise
// reads as "nothing was spent" when it means "the token may not delete".
func TestTidyReportsDeletionFailures(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	gh.deleteErrs[100] = &ghapi.APIError{Method: "DELETE", Status: 403, Body: "resource not accessible by integration"}
	now := time.Now().UTC()
	repo, pr := "o/r", 16

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing: the delete was refused", result.Deleted)
	}
	if len(result.Failed) != 1 || result.Failed[0].ID != 100 || result.Failed[0].Error == "" {
		t.Fatalf("failed = %+v, want the refused comment and why", result.Failed)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 0 {
		t.Fatalf("definitively refused delete retained tombstones: %v", got)
	}

	// Ordinary cleanup failures stay best-effort, but a GitHub throttle must
	// reach autoreview so it sleeps through the reset window.
	throttle := &ghapi.RateLimitError{Kind: "primary"}
	delete(gh.deleteErrs, 100)
	gh.getComment = func(string, int64) (ghapi.IssueComment, error) {
		return ghapi.IssueComment{}, throttle
	}
	if err := svc.tidyProgressed(ctx, st, repo, pr); !ghapi.IsThrottled(err) {
		t.Fatalf("tidyProgressed error = %v, want the pre-delete read throttle", err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 0 {
		t.Fatalf("failed pre-delete read retained tombstones for present comments: %v", got)
	}

	// The force-push cutoff is another housekeeping lookup; its throttle must
	// take the same backoff path.
	gh.getComment = nil
	gh.graphQL = func(string, map[string]any, any) error { return throttle }
	if _, err := svc.Tidy(ctx, repo, pr, false); !ghapi.IsThrottled(err) {
		t.Fatalf("Tidy error = %v, want the force-push lookup throttle", err)
	}
}

func TestTidyRetainsTombstoneWhenDeleteOutcomeIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	gh.deleteAfterErrs[100] = context.DeadlineExceeded
	now := time.Now().UTC()
	repo, pr := "o/r", 29

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failed) != 1 || result.Failed[0].ID != 100 {
		t.Fatalf("failed = %+v, want the ambiguous delete failure", result.Failed)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidiedCommands[QueueKey(repo, pr)]; len(got) != 1 || got[0].ID != 100 {
		t.Fatalf("tidied commands = %v, want tombstone for ambiguously deleted comment 100", got)
	}
}

func TestTidyBoundsAndRotatesUnansweredCodexReactionReads(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = []CoBotConfig{{Login: dialect.CodexBotLogin, Name: "codex", Command: "@codex review"}}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 25

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	round := Round{
		Repo: repo, PR: pr, Head: "aaaaaaaa1", Phase: PhaseCompleted,
		EnqueuedAt: now.Add(-3 * time.Hour),
	}
	for i := int64(1); i <= 10; i++ {
		at := now.Add(time.Duration(-130+i) * time.Minute)
		comment := ghapi.IssueComment{ID: i, Body: "@codex review", CreatedAt: at, UpdatedAt: at}
		comment.User.Login = "kristofferR"
		gh.comments[fakeKey(repo, pr)] = append(gh.comments[fakeKey(repo, pr)], comment)
		round.RecordPosted(dialect.CodexBotLogin, i, at)
	}

	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Archive = append(st.Archive, round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Tidy(ctx, repo, pr, false); err != nil {
		t.Fatal(err)
	}
	first := append([]int64(nil), gh.reactionReads...)
	if len(first) != maxTidyReactionCandidates {
		t.Fatalf("first pass read reactions for %v, want %d candidates", first, maxTidyReactionCandidates)
	}
	for i, id := range first {
		if want := int64(i + 1); id != want {
			t.Fatalf("first pass = %v, want commands 1 through %d", first, maxTidyReactionCandidates)
		}
	}
	if _, err := svc.Tidy(ctx, repo, pr, false); err != nil {
		t.Fatal(err)
	}
	second := gh.reactionReads[len(first):]
	if len(second) != maxTidyReactionCandidates {
		t.Fatalf("second pass read reactions for %v, want %d candidates", second, maxTidyReactionCandidates)
	}
	for i, id := range second {
		if want := int64(maxTidyReactionCandidates + i + 1); id != want {
			t.Fatalf("persisted cursor did not rotate the scan: first=%v second=%v", first, second)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.TidyReactionCursors[QueueKey(repo, pr)]; got != second[len(second)-1] {
		t.Fatalf("cursor = %d, want last inspected command %d", got, second[len(second)-1])
	}
}

// Codex can answer a trigger with nothing but its thumbs-up, and deleting the
// target deletes that approval too. Keep the target so subsequent observations
// still see the completed gate.
func TestTidyRetainsCodexThumbsUpReactionTarget(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = []CoBotConfig{{Login: dialect.CodexBotLogin, Name: "codex", Command: "@codex review"}}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 17

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "aaaaaaaa1"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["aaaaaaaa1"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: "@codex review", CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	// The whole answer: a +1 on the trigger, no review, comment or check run.
	thumb := ghapi.Reaction{ID: 1, Content: "+1", CreatedAt: now.Add(-100 * time.Minute)}
	thumb.User.Login = dialect.CodexBotLogin
	gh.reactions[100] = []ghapi.Reaction{thumb}
	review := ghapi.Review{
		ID: 101, CommitID: pull.Head.SHA, State: "COMMENTED",
		SubmittedAt: now.Add(-90 * time.Minute), Body: "review complete",
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	// A co-only round, which is how a Codex trigger becomes a round's own
	// command: the comment anchors the round and is recorded against Codex.
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.CoOnly = true
			r.SetCoCommand(dialect.CodexBotLogin, 100, now.Add(-2*time.Hour))
			r.RecordPosted(dialect.CodexBotLogin, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("deleted = %v, want the reaction target retained", result.Deleted)
	}
	if report, err := svc.Feedback(ctx, repo, pr); err != nil {
		t.Fatal(err)
	} else if !report.Converged {
		t.Fatalf("feedback lost the Codex approval after tidy: %#v", report)
	}
}

// A force-push can point the PR at a commit object OLDER than the command it
// answered, and adoption knows it: its cutoff is the force-push, not the commit
// date. Tidying reads the same rule backwards, so it must use the same cutoff —
// or a command no round can adopt again is kept for ever.
func TestTidyUsesTheForcePushCutoffNotJustTheCommitDate(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	now := time.Now().UTC()
	repo, pr := "o/r", 18
	forcePushAt := now.Add(-time.Hour)
	gh.graphQL = func(_ string, _ map[string]any, out any) error {
		payload := `{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"createdAt":"` + forcePushAt.Format(time.RFC3339) + `"}]}}}}`
		return json.Unmarshal([]byte(payload), out)
	}

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	// The head was force-pushed to a commit made before the command.
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-3 * time.Hour))

	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", State: "COMMENTED", Body: "looks fine", SubmittedAt: now.Add(-90 * time.Minute)}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != 100 {
		t.Fatalf("deleted = %v (kept: %v), want the command the force-push put out of adoption's reach", result.Deleted, result.Kept)
	}
}

// Codex answers a command in the PR description by reacting to the PR itself,
// and observe() accepts that as the round's completion — so tidying has to read
// the same source, or the trigger it completed is kept for ever.
func TestTidyCountsACodexThumbsUpOnThePR(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = []CoBotConfig{{Login: dialect.CodexBotLogin, Name: "codex", Command: "@codex review"}}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 19

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: "@codex review", CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	// The whole answer, and it is nowhere near the trigger comment.
	thumb := ghapi.Reaction{ID: 1, Content: "+1", CreatedAt: now.Add(-100 * time.Minute)}
	thumb.User.Login = dialect.CodexBotLogin
	gh.issueReactions[fakeKey(repo, pr)] = []ghapi.Reaction{thumb}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.CoOnly = true
			r.SetCoCommand(dialect.CodexBotLogin, 100, now.Add(-2*time.Hour))
			r.RecordPosted(dialect.CodexBotLogin, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != 100 {
		t.Fatalf("deleted = %v (kept: %v), want the trigger the PR reaction answered", result.Deleted, result.Kept)
	}
}

// The third place observe() reads a Codex thumbs-up from is the command the
// round fired on, which for a Codex-gated round is CodeRabbit's — not the
// "@codex review" crq posted beside it. A round that completed from one of those
// leaves its own trigger looking like nobody ever read it.
func TestTidyCountsACodexThumbsUpOnTheFiredCommand(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = []CoBotConfig{{Login: dialect.CodexBotLogin, Name: "codex", Command: "@codex review"}}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 21

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	add := func(id int64, body string) {
		c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: now.Add(-2 * time.Hour)}
		c.User.Login = "kristofferR"
		gh.comments[fakeKey(repo, pr)] = append(gh.comments[fakeKey(repo, pr)], c)
	}
	add(100, cfg.ReviewCommand) // the round's own fire, posted by crq
	add(101, "@codex review")   // and its co-reviewer trigger
	// Codex's whole answer, left on the command that fired the round.
	thumb := ghapi.Reaction{ID: 1, Content: "+1", CreatedAt: now.Add(-100 * time.Minute)}
	thumb.User.Login = dialect.CodexBotLogin
	gh.reactions[100] = []ghapi.Reaction{thumb}
	review := ghapi.Review{
		ID: 900, CommitID: pull.Head.SHA, State: "COMMENTED",
		SubmittedAt: now.Add(-90 * time.Minute), Body: "review complete",
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.SetCoCommand(dialect.CodexBotLogin, 101, now.Add(-2*time.Hour))
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			r.RecordPosted(dialect.CodexBotLogin, 101, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != 101 {
		t.Fatalf("deleted = %v (kept: %v), want the co trigger removed and its reaction target retained", result.Deleted, result.Kept)
	}
}

// A pump that changed nothing must not buy a second observation of the PR it
// just observed: a long review is polled for hours, and housekeeping that
// re-reads it every poll spends the REST quota the queue runs on.
func TestTidyAfterPumpSkipsAPumpThatChangedNothing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 20

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))
	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.tidyAfterPump(ctx, st, PumpResult{Action: "waiting", Repo: repo, PR: pr, Reason: "review in progress"}); err != nil {
		t.Fatal(err)
	}
	if reads := gh.reviewPolls(); reads != 0 {
		t.Fatalf("a no-progress pump observed the pr %d time(s); the pump had just read it", reads)
	}
	// The same PR, once something actually moved.
	if err := svc.tidyAfterPump(ctx, st, PumpResult{Action: "cleared", Repo: repo, PR: pr}); err != nil {
		t.Fatal(err)
	}
	if gh.reviewPolls() == 0 {
		t.Fatal("a round that progressed was never tidied")
	}
}

// Pump can progress the slot holder and then return a different PR's
// quota-free result. The caller can only tidy the returned result, so Pump must
// tidy the progressed slot before replacing it.
func TestPumpTidiesProgressedSlotBeforeReturningQuotaFreeResult(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	holderRepo, holderPR := "o/holder", 21
	holderSHA := "1111111111111111"
	holderPull := ghapi.Pull{State: "open"}
	holderPull.Head.SHA = holderSHA
	gh.pulls[fakeKey(holderRepo, holderPR)] = holderPull
	gh.commits[holderSHA] = commitAt(now.Add(-10 * time.Minute))
	holderCommand := ghapi.IssueComment{
		ID:        100,
		Body:      cfg.ReviewCommand,
		CreatedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-2 * time.Hour),
	}
	holderCommand.User.Login = "kristofferR"
	gh.comments[fakeKey(holderRepo, holderPR)] = []ghapi.IssueComment{holderCommand}
	holderReview := ghapi.Review{
		ID:          101,
		CommitID:    holderSHA,
		State:       "COMMENTED",
		SubmittedAt: now.Add(-time.Minute),
		Body:        "review complete",
	}
	holderReview.User.Login = cfg.Bot
	gh.reviews[fakeKey(holderRepo, holderPR)] = []ghapi.Review{holderReview}

	backRepo, backPR := "o/back", 22
	backSHA := "2222222222222222"
	backPull := ghapi.Pull{State: "open"}
	backPull.Head.SHA = backSHA
	gh.pulls[fakeKey(backRepo, backPR)] = backPull
	backReview := ghapi.Review{
		ID:          201,
		CommitID:    backSHA,
		State:       "COMMENTED",
		SubmittedAt: now.Add(-time.Minute),
		Body:        "already reviewed",
	}
	backReview.User.Login = cfg.Bot
	gh.reviews[fakeKey(backRepo, backPR)] = []ghapi.Review{backReview}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	seedRound(t, store, cfg, holderRepo, holderPR, holderSHA[:9], PhaseFired, now.Add(-2*time.Hour), holderCommand.ID)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round(holderRepo, holderPR)
		r.RecordPosted(cfg.Bot, holderCommand.ID, holderCommand.CreatedAt)
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, backRepo, backPR); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Repo != backRepo || result.PR != backPR {
		t.Fatalf("the quota-free result should replace the progressed slot result, got %#v", result)
	}
	if len(gh.deleted) != 1 || gh.deleted[0] != holderCommand.ID {
		t.Fatalf("the replaced slot result was not tidied, deleted=%v", gh.deleted)
	}
}

// A completed round remains the dedupe marker after its trigger is tidied.
// Feedback must not ask GitHub for reactions on that now-deleted comment.
func TestFeedbackSkipsReactionsForTidiedCommand(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 23
	head := "bbbbbbbb22222222"

	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits[head] = commitAt(now.Add(-10 * time.Minute))
	command := ghapi.IssueComment{
		ID:        100,
		Body:      cfg.ReviewCommand,
		CreatedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-2 * time.Hour),
	}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{command}
	review := ghapi.Review{
		ID:          101,
		CommitID:    head,
		State:       "COMMENTED",
		SubmittedAt: now.Add(-time.Minute),
		Body:        "review complete",
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(command.ID, command.CreatedAt); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, command.ID, command.CreatedAt)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Tidy(ctx, repo, pr, false); err != nil {
		t.Fatal(err)
	} else if len(result.Deleted) != 1 {
		t.Fatalf("tidy result = %#v, want the spent trigger deleted", result)
	}
	gh.reactionErrs[command.ID] = ghapi.ErrNotFound

	if _, err := svc.Feedback(ctx, repo, pr); err != nil {
		t.Fatalf("feedback read reactions from the deleted command: %v", err)
	}
}

func TestTidyUsesFleetResolvedConfiguration(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
		"CRQ_REVIEW_CMD": "@host review", "CRQ_TIDY": "0",
	})
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr, head := "o/r", 24, "cccccccc33333333"
	const commandBody = "@fleet review"

	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits[head] = commitAt(now.Add(-10 * time.Minute))
	command := ghapi.IssueComment{
		ID: 200, Body: commandBody,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{command}
	review := ghapi.Review{
		ID: 201, CommitID: head, State: "COMMENTED",
		SubmittedAt: now.Add(-time.Minute), Body: "review complete",
	}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	st, err := store.Update(ctx, func(st *State) error {
		st.Fleet = fleetEnvSet(st.Fleet, "CRQ_TIDY", "1", false)
		st.Fleet = fleetEnvSet(st.Fleet, "CRQ_REVIEW_CMD", commandBody, false)
		return completedRound(st, repo, pr, now, func(r *Round) error {
			if err := r.Fire(command.ID, command.CreatedAt); err != nil {
				return err
			}
			r.RecordPosted(cfg.Bot, command.ID, command.CreatedAt)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.tidyProgressed(ctx, st, repo, pr); err != nil {
		t.Fatal(err)
	}
	if len(gh.deleted) != 1 || gh.deleted[0] != command.ID {
		t.Fatalf("deleted = %v, want the command recognized under fleet settings", gh.deleted)
	}
}

// completedRound builds a round that fired (via fire) and then completed, which
// is the "has progressed" precondition every tidy candidate needs.
func completedRound(st *State, repo string, pr int, now time.Time, fire func(*Round) error) error {
	r, err := st.NewRound(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour))
	if err != nil {
		return err
	}
	if err := r.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
		return err
	}
	if err := fire(r); err != nil {
		return err
	}
	if err := r.Complete(); err != nil {
		return err
	}
	st.PutRound(*r)
	return nil
}

// commitAt is a commit object with a committer date, which is the cutoff
// adoption uses and therefore the cutoff tidying respects.
func commitAt(at time.Time) ghapi.Commit {
	var c ghapi.Commit
	c.Committer.Date = at
	return c
}
