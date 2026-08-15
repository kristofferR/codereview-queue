package crq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

func setHoldCapableLeader(t *testing.T, ctx context.Context, store StateStore, now time.Time) {
	t.Helper()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:        "current-daemon",
			Token:        "leader",
			ExpiresAt:    now.Add(time.Hour),
			UpdatedAt:    now,
			Capabilities: []string{leaderCapabilityHolds},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertOnlyHoldComment(t *testing.T, gh *fakeGitHub) {
	t.Helper()
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0], "<!-- crq:hold -->") {
		t.Fatalf("posted comments = %v, want only the hold notice", gh.posted)
	}
}

func TestParseHoldKeyRejectsMalformedNumbers(t *testing.T) {
	for _, key := range []string{
		"owner/repo#",
		"owner/repo#0",
		"owner/repo#-1",
		"owner/repo#12junk",
		"owner/repo#12#13",
	} {
		if _, _, ok := parseHoldKey(key); ok {
			t.Errorf("parseHoldKey(%q) succeeded", key)
		}
	}
	if repo, pr, ok := parseHoldKey("owner/repo#12"); !ok || repo != "owner/repo" || pr != 12 {
		t.Fatalf("valid hold key parsed as repo=%q pr=%d ok=%t", repo, pr, ok)
	}
}

func TestHoldPostsItsReasonOnThePullRequest(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	setHoldCapableLeader(t, ctx, store, now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	result, err := svc.Hold(ctx, "Owner/Repo", 12, "waiting for product approval")
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHoldComment(t, gh)
	want := "**Reason:** waiting for product approval"
	if !strings.Contains(gh.posted[0], want) || !strings.Contains(gh.posted[0], "`crq unhold owner/repo 12`") {
		t.Fatalf("hold comment = %q, want reason and resume command", gh.posted[0])
	}
	if result.Warning != "" {
		t.Fatalf("successful hold warning = %q", result.Warning)
	}
}

func TestHoldNeutralizesMentionsInPostedReason(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	setHoldCapableLeader(t, ctx, store, now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	reason := "do not run @coderabbitai review or @codex review"
	result, err := svc.Hold(ctx, "owner/repo", 12, reason)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHoldComment(t, gh)
	if strings.Contains(gh.posted[0], "@coderabbitai review") || strings.Contains(gh.posted[0], "@codex review") {
		t.Fatalf("hold comment kept a live review command: %q", gh.posted[0])
	}
	if !strings.Contains(gh.posted[0], "@\u200bcoderabbitai review") || !strings.Contains(gh.posted[0], "@\u200bcodex review") {
		t.Fatalf("hold comment did not neutralize reviewer mentions: %q", gh.posted[0])
	}
	if result.Reason != reason {
		t.Fatalf("result reason = %q, want original %q", result.Reason, reason)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hold, ok := st.HeldPR("owner/repo", 12)
	if !ok || hold.Reason != reason {
		t.Fatalf("persisted hold = %+v, want original reason %q", hold, reason)
	}
}

func TestHoldSurvivesACommentPostFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.postErrs = map[string]error{fakeKey("owner/repo", 12): errors.New("comments disabled")}
	store := NewMemoryStore(cfg)
	setHoldCapableLeader(t, ctx, store, now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	result, err := svc.Hold(ctx, "owner/repo", 12, "waiting for product approval")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "comments disabled") {
		t.Fatalf("warning = %q, want comment failure", result.Warning)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/repo", 12); !held {
		t.Fatal("comment failure rolled back the safety-critical hold")
	}
}

func TestHoldRemovesNoticeIfReleasedBeforePostCompletes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	setHoldCapableLeader(t, ctx, store, now)
	logs := &recordingLogger{}
	svc := NewService(cfg, gh, store, logs)
	svc.now = func() time.Time { return now }
	gh.postHook = func() {
		if _, err := svc.Unhold(ctx, "owner/repo", 12); err != nil {
			t.Errorf("unhold during post: %v", err)
		}
	}

	result, err := svc.Hold(ctx, "owner/repo", 12, "waiting for product approval")
	if err != nil {
		t.Fatal(err)
	}
	if result.Held {
		t.Fatalf("stale hold reported as active: %+v", result)
	}
	if result.CommentURL != "" {
		t.Fatalf("stale hold notice returned as current: %+v", result)
	}
	if len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
		t.Fatalf("delete calls = %v, want the stale hold comment", gh.deleteCalls)
	}
	if !logs.contains("owner/repo#12 hold was released before its notice completed") {
		t.Fatalf("hold race logs = %v, want the surviving released state", logs.lines)
	}
}

func TestHoldRemovesNoticeIfLegacyWriterReplacesTheHold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	setHoldCapableLeader(t, ctx, store, now)
	logs := &recordingLogger{}
	svc := NewService(cfg, gh, store, logs)
	svc.now = func() time.Time { return now }
	replacementAt := now.Add(time.Minute)
	gh.postHook = func() {
		if _, err := store.Update(ctx, func(st *State) error {
			// A tolerant older writer changes Holds but preserves the unknown
			// HoldTokens member byte-for-byte.
			st.Holds[QueueKey("owner/repo", 12)] = Hold{
				Reason: "waiting for security approval",
				By:     "old-daemon",
				At:     replacementAt,
			}
			return nil
		}); err != nil {
			t.Errorf("replace hold during post: %v", err)
		}
	}

	result, err := svc.Hold(ctx, "owner/repo", 12, "waiting for product approval")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Held || result.Reason != "waiting for security approval" || result.By != "old-daemon" ||
		result.At == nil || !result.At.Equal(replacementAt) {
		t.Fatalf("replacement hold was not reported: %+v", result)
	}
	if result.CommentURL != "" {
		t.Fatalf("stale hold notice returned as current: %+v", result)
	}
	if len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
		t.Fatalf("delete calls = %v, want the stale hold comment", gh.deleteCalls)
	}
	if !logs.contains("owner/repo#12 hold was replaced before its notice completed: waiting for security approval") {
		t.Fatalf("hold race logs = %v, want the surviving replacement hold", logs.lines)
	}
}

func TestUnholdPostsReleaseNotice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting for product approval", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	result, err := svc.Unhold(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if result.Held || result.Warning != "" {
		t.Fatalf("unexpected unhold result: %+v", result)
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0], "<!-- crq:unhold -->") ||
		!strings.Contains(gh.posted[0], "hold has been released") {
		t.Fatalf("posted comments = %v, want one release notice", gh.posted)
	}
}

func TestUnholdRemovesReleaseNoticeIfReheldBeforePostCompletes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting for product approval", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setHoldCapableLeader(t, ctx, store, now)
	svc := NewService(cfg, gh, store, nil)
	current := now
	svc.now = func() time.Time { return current }
	reheldAt := now.Add(time.Minute)
	gh.postHook = func() {
		current = reheldAt
		if _, err := svc.Hold(ctx, "owner/repo", 12, "waiting for security approval"); err != nil {
			t.Errorf("re-hold during release post: %v", err)
		}
	}

	result, err := svc.Unhold(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Held || result.Reason != "waiting for security approval" || result.By != cfg.Host ||
		result.At == nil || !result.At.Equal(reheldAt) {
		t.Fatalf("replacement hold was not reported: %+v", result)
	}
	if result.CommentURL != "" {
		t.Fatalf("stale release notice returned as current: %+v", result)
	}
	if len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
		t.Fatalf("delete calls = %v, want the stale release comment", gh.deleteCalls)
	}
}

// The race crq hold exists to close is between selecting a round and writing the
// reservation. Checking the hold only at selection leaves exactly that window
// open, so the command could return successfully while a daemon fired anyway.
func TestHoldIsRecheckedWhenTheRoundIsReserved(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	repo, pr, head := "o/r", 3, "aaaaaaaa1"

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)
	setHoldCapableLeader(t, ctx, store, now)

	// The hold lands after the round was chosen, which is the whole point.
	round := func() Round {
		st, _, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return *st.Round(repo, pr)
	}()
	if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}

	obs := engine.Observation{Open: true, Head: head}
	_, err := svc.fireRound(ctx, cfg, round, obs, true, 0, time.Time{}, "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHoldComment(t, gh)
	st, _, _ := store.Load(ctx)
	if st.FireSlot != nil {
		t.Errorf("a held round took the fire slot: %#v", st.FireSlot)
	}
}

// Quota-free co-review paths bypass NextEligible, so each one must repeat the
// hold check in the CAS that claims its trigger post.
func TestHoldIsRecheckedByQuotaFreeFirePaths(t *testing.T) {
	tests := map[string]func(*Service, context.Context, Round, time.Time) error{
		"co-only": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoOnly(ctx, s.cfg, round, []string{dialect.CodexBotLogin}, "primary already reviewed", now)
			return err
		},
		"co-deferred": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoDeferred(ctx, s.cfg, round, engine.FireDecision{
				Verdict: engine.FireCoDeferred,
				PostCo:  []string{dialect.CodexBotLogin},
				Reason:  "primary account blocked",
			}, now)
			return err
		},
		"co-review-wait": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoReviewWait(ctx, s.cfg, round, engine.Observation{
				Open:   true,
				Head:   round.Head,
				HeadAt: now.Add(-time.Minute),
			}, "waiting for automatic co-review", now)
			return err
		},
	}

	for name, fire := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
			cfg := firingConfig()
			cfg.RequiredBots = append(cfg.RequiredBots, dialect.CodexBotLogin)
			cfg.CoBots = codexCoBots(cfg.RequiredBots)
			gh := newFakeGitHub()
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, gh, store, nil)
			svc.now = func() time.Time { return now }
			repo, pr, head := "o/r", 4, "bbbbbbbb1"
			seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
			setHoldCapableLeader(t, ctx, store, now)

			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			round := *st.Round(repo, pr)
			if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
				t.Fatal(err)
			}
			if err := fire(svc, ctx, round, now); err != nil {
				t.Fatal(err)
			}
			assertOnlyHoldComment(t, gh)
			st, _, err = store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := st.Round(repo, pr)
			if got == nil || got.Phase != PhaseQueued || got.Co(dialect.CodexBotLogin).ClaimedAt != nil {
				t.Fatalf("a held round was mutated by %s: %+v", name, got)
			}
		})
	}
}

func TestHoldStopsInflightCoReviewerSelfHeal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	cfg.RequiredBots = append(cfg.RequiredBots, dialect.CodexBotLogin)
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	repo, pr, head := "o/r", 6, "cccccccc1"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
	setHoldCapableLeader(t, ctx, store, now)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round(repo, pr)
		if err := r.Reserve("token", cfg.Host, now.Add(-time.Minute)); err != nil {
			return err
		}
		if err := r.Fire(123, now.Add(-time.Minute)); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, pr)
	if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}

	svc.selfHealCoReviewers(ctx, cfg, round, engine.Observation{Open: true, Head: head}, now)
	assertOnlyHoldComment(t, gh)
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim := st.Round(repo, pr).Co(dialect.CodexBotLogin).ClaimedAt; claim != nil {
		t.Fatalf("held in-flight round acquired a co-review claim: %s", claim)
	}
}

func TestHoldRejectsTriggerClaimsAlreadyBeingPosted(t *testing.T) {
	tests := map[string]func(*State, *Round, Config, time.Time) error{
		"primary reservation": func(st *State, r *Round, cfg Config, now time.Time) error {
			if err := r.Reserve("token", cfg.Host, now); err != nil {
				return err
			}
			st.FireSlot = &FireSlot{Key: QueueKey(r.Repo, r.PR), Token: "token", Since: now}
			return nil
		},
		"co-reviewer claim": func(_ *State, r *Round, _ Config, now time.Time) error {
			r.ClaimCo(dialect.CodexBotLogin, now)
			return nil
		},
	}

	for name, claim := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
			cfg := firingConfig()
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, newFakeGitHub(), store, nil)
			svc.now = func() time.Time { return now }
			repo, pr, head := "o/r", 7, "dddddddd1"
			seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
			setHoldCapableLeader(t, ctx, store, now)
			if _, err := store.Update(ctx, func(st *State) error {
				r := st.Round(repo, pr)
				if err := claim(st, r, cfg, now); err != nil {
					return err
				}
				st.PutRound(*r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err == nil {
				t.Fatal("hold succeeded while a trigger post was already claimed")
			}
			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, held := st.HeldPR(repo, pr); held {
				t.Fatal("a rejected hold was persisted")
			}
		})
	}
}

func TestHoldRejectsExpiredTriggerClaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	repo, pr, head := "o/r", 8, "eeeeeeee1"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
	setHoldCapableLeader(t, ctx, store, now)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round(repo, pr)
		r.ClaimCo(dialect.CodexBotLogin, now.Add(-triggerClaimTTL-time.Second))
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err == nil {
		t.Fatal("hold succeeded while an expired claim's poster could still resume")
	}
}

func TestHoldAndUnholdDryRunDoNotMutateState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	cfg.DryRun = true
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("o/held", 1, "existing hold", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, revision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	held, err := svc.Hold(ctx, "o/new", 2, "simulated hold")
	if err != nil {
		t.Fatal(err)
	}
	if !held.Held || held.Reason != "simulated hold" {
		t.Fatalf("unexpected simulated hold result: %+v", held)
	}
	released, err := svc.Unhold(ctx, "o/held", 1)
	if err != nil {
		t.Fatal(err)
	}
	if released.Held {
		t.Fatalf("unexpected simulated unhold result: %+v", released)
	}

	after, afterRevision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision != revision {
		t.Fatalf("dry-run changed state revision: before=%+v after=%+v", revision, afterRevision)
	}
	if len(after.Holds) != len(before.Holds) {
		t.Fatalf("dry-run changed holds: before=%+v after=%+v", before.Holds, after.Holds)
	}
	if _, held := after.HeldPR("o/held", 1); !held {
		t.Fatal("dry-run unhold removed the existing hold")
	}
	if _, held := after.HeldPR("o/new", 2); held {
		t.Fatal("dry-run hold persisted the simulated hold")
	}
	if len(gh.posted) != 0 {
		t.Fatal("dry-run hold posted a PR comment")
	}
}

func TestClarificationHoldRequiresDispatchOwnership(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	repo, pr, head := "o/r", 9, "ffffffff1"
	report := NextReport{Repo: repo, PR: pr, Head: head}
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
	setHoldCapableLeader(t, ctx, store, now)

	if _, err := store.Update(ctx, func(st *State) error {
		round := st.Round(repo, pr)
		if ok, why := round.ClaimDispatch("other", "new-token", now, 3); !ok {
			t.Fatalf("claim: %s", why)
		}
		st.RememberDispatch(repo, pr, *round.Dispatch)
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.holdDispatch(ctx, report, "old-token", "needs clarification"); err == nil {
		t.Fatal("clarification hold succeeded after the dispatch claim changed owners")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR(repo, pr); held {
		t.Fatal("rejected clarification persisted a hold")
	}

	now = now.Add(DispatchTTL)
	if _, err := svc.holdDispatch(ctx, report, "new-token", "needs clarification"); !errors.Is(err, errDispatchClaimLost) {
		t.Fatalf("clarification hold with expired claim error = %v, want %v", err, errDispatchClaimLost)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		round := st.Round(repo, pr)
		if ok, taken := round.HeartbeatDispatch("new-token", now); !ok || taken {
			t.Fatalf("heartbeat: ok %v taken %v", ok, taken)
		}
		st.RememberDispatch(repo, pr, *round.Dispatch)
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.holdDispatch(ctx, report, "new-token", "needs clarification"); err != nil {
		t.Fatalf("claim owner could not hold the PR: %v", err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR(repo, pr); !held {
		t.Fatal("claim owner did not persist a clarification hold")
	}
}

// A rolling deployment can leave an older daemon holding the leader lease.
// Such a daemon preserves Holds in JSON but does not enforce them, so the
// command must not claim success until a capable leader owns the fleet.
func TestHoldRequiresACapableLiveLeader(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("hold succeeded without a live capable daemon")
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:     "old-daemon",
			Token:     "old",
			ExpiresAt: now.Add(time.Minute),
			UpdatedAt: now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("hold succeeded while an incompatible daemon owned the fleet")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("o/r", 5); held {
		t.Fatal("a rejected hold was persisted")
	}

	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader.Capabilities = []string{leaderCapabilityHolds}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err != nil {
		t.Fatalf("hold with capable leader: %v", err)
	}
}

func TestHoldAcceptsTopLevelCapabilityPreservedByOldWriter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:     "new-daemon",
			Token:     "current-token",
			ExpiresAt: now.Add(time.Minute),
			UpdatedAt: now,
		}
		st.LeaderCapabilities = &LeaderCapabilityLease{
			Token:        "current-token",
			Capabilities: []string{leaderCapabilityHolds},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err != nil {
		t.Fatalf("hold with preserved top-level capability: %v", err)
	}

	if _, err := store.Update(ctx, func(st *State) error {
		st.Unhold("o/r", 5)
		st.Leader.Token = "old-daemon-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("stale capabilities from a different lease token were accepted")
	}
}
