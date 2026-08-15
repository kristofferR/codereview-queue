package state

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

var t0 = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func newFired(t *testing.T, s *State) Round {
	t.Helper()
	r, err := s.NewRound("owner/repo", 7, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(101, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.PutRound(*r)
	return *r
}

func TestHappyPathTransitions(t *testing.T) {
	s := New()
	r := newFired(t, &s)
	if r.Phase != PhaseFired || r.Attempts != 1 || r.CommandID != 101 {
		t.Fatalf("after fire: %+v", r)
	}
	if err := r.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if err := r.Acknowledge(); err != nil {
		t.Fatalf("acknowledge must be idempotent: %v", err)
	}
	if err := r.Complete(); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseCompleted {
		t.Fatalf("phase = %s", r.Phase)
	}
}

// TestFiredHeadCannotRefire encodes the #448 invariant: once a head has
// fired, no transition path leads back to another Fire without an explicit
// retry window or a new head.
func TestFiredHeadCannotRefire(t *testing.T) {
	s := New()
	r := newFired(t, &s)

	var te *TransitionError
	if err := r.Fire(102, t0.Add(time.Minute)); !errors.As(err, &te) {
		t.Fatalf("double fire must be illegal, got %v", err)
	}
	if err := r.Reserve("tok2", "host", t0.Add(time.Minute)); !errors.As(err, &te) {
		t.Fatalf("re-reserve of a fired round must be illegal, got %v", err)
	}
	if r.FireEligible(t0.Add(time.Hour)) {
		t.Fatal("a fired round is never fire-eligible")
	}

	// The rate-limited path parks the round; it stays ineligible until the
	// window passes, and its history survives.
	retryAt := t0.Add(15 * time.Minute)
	if err := r.AwaitRetry(retryAt, "account rate limited", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.FireEligible(retryAt.Add(-time.Second)) {
		t.Fatal("must not be eligible before RetryAt")
	}
	if !r.FireEligible(retryAt) {
		t.Fatal("must be eligible once RetryAt passes")
	}
	if r.Attempts != 1 || r.CommandID != 101 || r.Head != "abcdef123" {
		t.Fatalf("retry must keep fire history: %+v", r)
	}
	// Re-reserving for the retry keeps counting attempts.
	if err := r.Reserve("tok3", "host", retryAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(103, retryAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", r.Attempts)
	}
}

// TestDedupeCompletesFromQueued covers the "bot already reviewed the head"
// path: a queued round is marked complete without recording a fictitious fire,
// leaving it as the dedup marker.
func TestDedupeCompletesFromQueued(t *testing.T) {
	s := New()
	r, err := s.NewRound("owner/repo", 10, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Dedupe(t0); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseCompleted || r.FiredAt != nil || r.Attempts != 0 {
		t.Fatalf("dedupe must complete without a fire: %+v", r)
	}
	// A fired round cannot be deduped — it goes through Complete.
	fired := newFired(t, &s)
	var te *TransitionError
	if err := fired.Dedupe(t0); !errors.As(err, &te) {
		t.Fatalf("dedupe of a fired round must be illegal, got %v", err)
	}
}

func TestIllegalCompletions(t *testing.T) {
	s := New()
	r, err := s.NewRound("owner/repo", 8, "cafebabe1", t0)
	if err != nil {
		t.Fatal(err)
	}
	var te *TransitionError
	if err := r.Complete(); !errors.As(err, &te) {
		t.Fatalf("completing a queued round must be illegal, got %v", err)
	}
	if err := r.Acknowledge(); !errors.As(err, &te) {
		t.Fatalf("acknowledging a queued round must be illegal, got %v", err)
	}
}

func TestReleaseToQueueKeepsAdoptionCutoff(t *testing.T) {
	s := New()
	r, _ := s.NewRound("owner/repo", 9, "abc123def", t0)
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseToQueue("post failed", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseQueued || r.Attempts != 1 {
		t.Fatalf("after release: %+v", r)
	}
	if r.LastAttemptAt == nil || !r.LastAttemptAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("adoption cutoff must advance: %+v", r.LastAttemptAt)
	}
}

// TestAwaitCoReviewBoundsTheWait covers the co-review wait: a queued round with
// CodeRabbit's review already standing moves to reviewing with a deadline and a
// fire anchor (so the wait is bounded), and the transition is illegal from a
// finished round.
func TestAwaitCoReviewBoundsTheWait(t *testing.T) {
	s := New()
	r, err := s.NewRound("owner/repo", 14, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	deadline := t0.Add(time.Hour)
	if err := r.AwaitCoReview(deadline, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseReviewing {
		t.Fatalf("await co-review must move to reviewing, got %s", r.Phase)
	}
	if r.WaitDeadline == nil || !r.WaitDeadline.Equal(deadline) {
		t.Fatalf("wait deadline must be set, got %+v", r.WaitDeadline)
	}
	if r.FiredAt == nil || !r.FiredAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("no prior fire must anchor FiredAt at now, got %+v", r.FiredAt)
	}
	if r.Token != "" || r.ReservedAt != nil {
		t.Fatalf("co-review wait holds no slot: token=%q reserved=%+v", r.Token, r.ReservedAt)
	}

	// Illegal from a finished round.
	s2 := New()
	completed := newFired(t, &s2)
	if err := completed.Complete(); err != nil {
		t.Fatal(err)
	}
	var te *TransitionError
	if err := completed.AwaitCoReview(deadline, t0); !errors.As(err, &te) {
		t.Fatalf("await co-review from completed must be illegal, got %v", err)
	}
	s3 := New()
	abandoned := newFired(t, &s3)
	abandoned.Abandon("cancelled")
	if err := abandoned.AwaitCoReview(deadline, t0); !errors.As(err, &te) {
		t.Fatalf("await co-review from abandoned must be illegal, got %v", err)
	}
}

func TestOneRoundPerPR(t *testing.T) {
	s := New()
	first, err := s.NewRound("Owner/Repo", 7, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	first.NoteCoAnswer("cursor[bot]", t0.Add(time.Second))
	first.SetCoCommand("cursor[bot]", 42, t0.Add(2*time.Second))
	s.PutRound(*first)
	if _, err := s.NewRound("owner/repo", 7, "00fedcba9", t0); err == nil {
		t.Fatal("second round for the same PR must be refused (case-insensitive key)")
	}
	r, err := s.Supersede("owner/repo", 7, "00fedcba9", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if r.Head != "00fedcba9" || r.Phase != PhaseQueued || r.Seq != 2 {
		t.Fatalf("superseded round: %+v", r)
	}
	co := r.Co("cursor[bot]")
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("superseded round lost prior reviewer activity: %+v", co)
	}
	if co.AnsweredAt != nil || co.CommandID != 0 || co.CommandedAt != nil {
		t.Fatalf("supersede carried head-scoped reviewer state: %+v", co)
	}
	if len(s.Archive) != 1 || s.Archive[0].Phase != PhaseAbandoned || s.Archive[0].Head != "abcdef123" {
		t.Fatalf("old round must be archived abandoned: %+v", s.Archive)
	}
}

func TestSupersedeMigratesLegacyCoAnswerAsActivity(t *testing.T) {
	s := New()
	first, err := s.NewRound("owner/repo", 8, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	answered := t0.Add(time.Second)
	first.setCo("cursor[bot]", CoBotRound{AnsweredAt: &answered})
	s.PutRound(*first)

	next, err := s.Supersede("owner/repo", 8, "00fedcba9", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	co := next.Co("cursor[bot]")
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(answered) {
		t.Fatalf("supersede did not migrate legacy answer as durable activity: %+v", co)
	}
	if co.AnsweredAt != nil {
		t.Fatalf("supersede carried head-scoped legacy answer: %+v", co)
	}
}

func TestNewRoundRestoresArchivedCoActivityAfterReopen(t *testing.T) {
	s := New()
	first, err := s.NewRound("owner/repo", 9, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	answered := t0.Add(time.Second)
	first.NoteCoAnswer("cursor[bot]", answered)
	s.PutRound(*first)
	s.EndRound("owner/repo", 9, "pr closed")

	reopened, err := s.NewRound("owner/repo", 9, "00fedcba9", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	co := reopened.Co("cursor[bot]")
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(answered) {
		t.Fatalf("reopened round lost archived reviewer activity: %+v", co)
	}
	if co.AnsweredAt != nil {
		t.Fatalf("reopened round carried head-scoped reviewer state: %+v", co)
	}
}

func TestNormalizeRestoresArchivedCoActivityAfterLegacySupersede(t *testing.T) {
	seen := t0.Add(time.Second)
	commanded := t0.Add(time.Minute)
	current := Round{Repo: "owner/repo", PR: 10, Head: "00fedcba9", Phase: PhaseQueued,
		CoBots: map[string]CoBotRound{"cursor": {CommandID: 42, CommandedAt: &commanded}}}
	s := State{
		Rounds: map[string]Round{Key(current.Repo, current.PR): current},
		Archive: []Round{{Repo: current.Repo, PR: current.PR, Head: "abcdef123", Phase: PhaseAbandoned,
			CoBots: map[string]CoBotRound{"cursor": {SeenActiveAt: &seen}}}},
	}

	s.Normalize(t0.Add(2 * time.Minute))

	co := s.Round(current.Repo, current.PR).Co("cursor[bot]")
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(seen) {
		t.Fatalf("Normalize lost activity preserved by the archived round: %+v", co)
	}
	if co.CommandID != 42 || co.CommandedAt == nil || !co.CommandedAt.Equal(commanded) {
		t.Fatalf("Normalize overwrote current-round reviewer state: %+v", co)
	}
}

func TestNormalizeRestoresCoActivityBeyondEmptyArchivedRound(t *testing.T) {
	seen := t0.Add(time.Second)
	current := Round{Repo: "owner/repo", PR: 11, Head: "00fedcba9", Phase: PhaseQueued}
	s := State{
		Rounds: map[string]Round{Key(current.Repo, current.PR): current},
		Archive: []Round{
			{Repo: current.Repo, PR: current.PR, Head: "abcdef123", Phase: PhaseAbandoned,
				CoBots: map[string]CoBotRound{"cursor": {SeenActiveAt: &seen}}},
			{Repo: current.Repo, PR: current.PR, Head: "123456789", Phase: PhaseAbandoned},
		},
	}

	s.Normalize(t0.Add(2 * time.Minute))

	co := s.Round(current.Repo, current.PR).Co("cursor[bot]")
	if co.SeenActiveAt == nil || !co.SeenActiveAt.Equal(seen) {
		t.Fatalf("Normalize lost activity beyond an empty archived round: %+v", co)
	}
}

func TestSlotRoundStaleness(t *testing.T) {
	s := New()
	r := newFired(t, &s)
	s.FireSlot = &FireSlot{Key: Key(r.Repo, r.PR), Token: "tok", Since: t0}
	if s.SlotRound() == nil {
		t.Fatal("slot round must resolve")
	}
	s.FireSlot.Token = "stolen"
	if s.SlotRound() != nil {
		t.Fatal("token mismatch must read as stale")
	}
	s.Normalize(t0)
	if s.FireSlot != nil {
		t.Fatal("Normalize must clear a stale slot")
	}
}

func TestNextEligibleOrdersBySeq(t *testing.T) {
	s := New()
	a, _ := s.NewRound("owner/repo", 1, "aaaaaaaa1", t0)
	s.PutRound(*a)
	b, _ := s.NewRound("owner/repo", 2, "bbbbbbbb2", t0)
	s.PutRound(*b)
	// Round a parks awaiting retry; b becomes the eligible head of queue.
	ra := s.Round("owner/repo", 1)
	if err := ra.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := ra.Fire(1, t0); err != nil {
		t.Fatal(err)
	}
	if err := ra.AwaitRetry(t0.Add(10*time.Minute), "rate limited", t0); err != nil {
		t.Fatal(err)
	}
	s.PutRound(*ra)

	if got := s.NextEligible(t0.Add(time.Minute)); got == nil || got.PR != 2 {
		t.Fatalf("expected PR 2 eligible, got %+v", got)
	}
	// Once the window passes, the older round wins again by Seq.
	if got := s.NextEligible(t0.Add(11 * time.Minute)); got == nil || got.PR != 1 {
		t.Fatalf("expected PR 1 eligible after retry window, got %+v", got)
	}
}

func TestArchiveBounded(t *testing.T) {
	s := New()
	for i := 0; i < ArchiveMax+10; i++ {
		if _, err := s.NewRound("owner/repo", i, "abcdef123", t0); err != nil {
			t.Fatal(err)
		}
		s.EndRound("owner/repo", i, "closed")
	}
	if len(s.Archive) != ArchiveMax {
		t.Fatalf("archive = %d, want %d", len(s.Archive), ArchiveMax)
	}
}

// TestCoBotKeyMatchesDialect pins state's local login normalization (and the
// Codex fold key) to dialect's, since state stays stdlib-only by design.
func TestCoBotKeyMatchesDialect(t *testing.T) {
	if got := coBotKey(dialect.CodexBotLogin); got != codexCoBotKey {
		t.Fatalf("coBotKey(CodexBotLogin) = %q, want %q", got, codexCoBotKey)
	}
	if got := coBotKey("cursor[bot]"); got != dialect.NormalizeBotName("cursor[bot]") {
		t.Fatalf("coBotKey = %q, want dialect normalization %q", got, dialect.NormalizeBotName("cursor[bot]"))
	}
}

// TestCoBotsDualWrite: Codex writes through the accessors must mirror into the
// legacy per-round fields (old binaries read codex_command_id from the shared
// state ref); other bots live only in the map.
func TestCoBotsDualWrite(t *testing.T) {
	var r Round
	r.ClaimCo(dialect.CodexBotLogin, t0)
	if r.CodexClaimedAt == nil || !r.CodexClaimedAt.Equal(t0) {
		t.Fatalf("ClaimCo did not mirror into CodexClaimedAt: %+v", r)
	}
	r.SetCoCommand(dialect.CodexBotLogin, 42, t0.Add(time.Second))
	if r.CodexCommandID != 42 || r.CodexCommandedAt == nil || r.CodexClaimedAt != nil {
		t.Fatalf("SetCoCommand did not mirror legacy fields: %+v", r)
	}
	if co := r.Co("chatgpt-codex-connector"); co.CommandID != 42 || co.ClaimedAt != nil {
		t.Fatalf("map entry = %+v, want command 42, no claim", co)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"codex_command_id":42`, `"cobots"`, `"chatgpt-codex-connector"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("marshaled round missing %s: %s", want, data)
		}
	}

	var other Round
	other.SetCoCommand("cursor[bot]", 7, t0)
	if other.CodexCommandID != 0 || other.CodexCommandedAt != nil {
		t.Fatalf("bugbot write leaked into legacy Codex fields: %+v", other)
	}
	if co := other.Co("cursor"); co.CommandID != 7 {
		t.Fatalf("Co(cursor) = %+v, want command 7", co)
	}
}

// TestNormalizeFoldsLegacyCodex: a round written by an old binary (legacy
// fields only) gains its CoBots mirror on load, and legacy values win over a
// stale mirror (old binaries only write the legacy fields).
func TestNormalizeFoldsLegacyCodex(t *testing.T) {
	payload := `{"v":3,"rounds":{"owner/repo#1":{"repo":"owner/repo","pr":1,"head":"abcdef123",` +
		`"seq":1,"phase":"fired","enqueued_at":"2026-07-17T12:00:00Z",` +
		`"codex_command_id":11,"codex_commanded_at":"2026-07-17T12:01:00Z"}}}`
	var s State
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatal(err)
	}
	s.Normalize(t0)
	r := s.Round("owner/repo", 1)
	co := r.Co(dialect.CodexBotLogin)
	if co.CommandID != 11 || co.CommandedAt == nil {
		t.Fatalf("fold produced %+v, want command 11", co)
	}

	// Stale mirror: an old binary moved the legacy command on; fold overwrites.
	stale := *r
	stale.CoBots = map[string]CoBotRound{codexCoBotKey: {CommandID: 5}}
	stale.foldLegacyCodex()
	if got := stale.Co(dialect.CodexBotLogin).CommandID; got != 11 {
		t.Fatalf("fold kept stale mirror %d, want legacy 11", got)
	}

	// CoBots-only rounds (new bots) survive a marshal/unmarshal round-trip.
	var b Round
	b.SetCoCommand("cursor[bot]", 9, t0)
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back Round
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Co("cursor[bot]").CommandID; got != 9 {
		t.Fatalf("round-trip lost bugbot entry: %+v", back)
	}
}

// TestClearCoClaimPrunesTheEntry covers setCo's empty() branch — the subtlest
// path in this file: it must delete the key, nil the map when it was the last
// entry, and zero the legacy Codex mirror. A regression that kept an all-zero
// entry would leave "cobots" in every serialized round and, worse, leave a
// stale legacy claim visible to old binaries.
func TestClearCoClaimPrunesTheEntry(t *testing.T) {
	var r Round
	r.ClaimCo(dialect.CodexBotLogin, t0)
	if r.CodexClaimedAt == nil || len(r.CoBots) != 1 {
		t.Fatalf("precondition: claim must be recorded, got %+v", r)
	}
	r.ClearCoClaim(dialect.CodexBotLogin)
	if r.CoBots != nil {
		t.Fatalf("the last empty entry must prune the map, got %#v", r.CoBots)
	}
	if r.CodexClaimedAt != nil {
		t.Fatalf("clearing must zero the legacy mirror, got %v", r.CodexClaimedAt)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cobots") {
		t.Fatalf("a pruned map must not serialize: %s", data)
	}
	// Clearing one bot must not disturb another's entry.
	var two Round
	two.SetCoCommand("cursor[bot]", 7, t0)
	two.ClaimCo(dialect.CodexBotLogin, t0)
	two.ClearCoClaim(dialect.CodexBotLogin)
	if got := two.Co("cursor[bot]").CommandID; got != 7 {
		t.Fatalf("clearing codex disturbed bugbot's entry: %d", got)
	}
}

// TestNormalizeFoldClearsAStaleMirror pins the direction foldLegacyCodex's doc
// claims but nothing asserted: legacy fields are authoritative BOTH ways, so an
// old binary that zeroed them must clear the mirror rather than leave crq
// acting on a command that no longer exists. Also covers the archive fold.
func TestNormalizeFoldClearsAStaleMirror(t *testing.T) {
	var r Round
	r.Repo, r.PR, r.Head, r.Phase = "owner/repo", 1, "abcdef123", PhaseFired
	r.SetCoCommand(dialect.CodexBotLogin, 11, t0)
	// Simulate an old binary clearing only the legacy fields it knows about.
	r.CodexCommandID, r.CodexCommandedAt, r.CodexClaimedAt = 0, nil, nil

	s := New()
	s.Rounds[Key(r.Repo, r.PR)] = r
	archived := r
	archived.PR = 2
	s.Archive = append(s.Archive, archived)
	s.Normalize(t0)

	if got := s.Round("owner/repo", 1).Co(dialect.CodexBotLogin).CommandID; got != 0 {
		t.Fatalf("an emptied legacy set must clear the mirror, got command %d", got)
	}
	if got := s.Archive[0].Co(dialect.CodexBotLogin).CommandID; got != 0 {
		t.Fatalf("the archive must be folded too, got command %d", got)
	}
}

// TestRequestedRoundsExcludesCoOnly pins what the "Recently requested" table
// means: rounds for which crq actually asked the primary reviewer. A
// co-reviewer-only round carries a FiredAt because that anchors its evidence
// floor, not because a review was requested — and on a CodeRabbit-Free private
// repo every push produced one, which crowded the real request history off the
// end of the cap.
func TestRequestedRoundsExcludesCoOnly(t *testing.T) {
	s := New()
	fired := t0.Add(time.Minute)

	requested := Round{Repo: "o/public", PR: 1, Head: "aaaaaaaa1", Seq: 1, Phase: PhaseCompleted, FiredAt: &fired}
	s.Rounds[Key(requested.Repo, requested.PR)] = requested
	coOnly := Round{Repo: "o/private", PR: 2, Head: "bbbbbbbb2", Seq: 2, Phase: PhaseCompleted, FiredAt: &fired, CoOnly: true}
	s.Rounds[Key(coOnly.Repo, coOnly.PR)] = coOnly
	archivedCoOnly := coOnly
	archivedCoOnly.PR = 3
	s.Archive = append(s.Archive, archivedCoOnly)

	got := requestedRounds(s)
	if len(got) != 1 || got[0].Repo != "o/public" {
		t.Fatalf("only genuinely requested reviews belong in the table, got %+v", got)
	}
	// The round itself is untouched — it is still the dedup marker and still
	// carries its evidence anchor.
	if r := s.Round("o/private", 2); r == nil || r.FiredAt == nil || !r.CoOnly {
		t.Fatalf("a co-only round must keep its anchor and marker, got %+v", r)
	}
}

func TestMoveToFrontReordersAndCanBeRepeated(t *testing.T) {
	st := New()
	for pr := 1; pr <= 3; pr++ {
		if _, err := st.NewRound("o/r", pr, "aaaaaaaa1", t0); err != nil {
			t.Fatal(err)
		}
	}

	if !st.MoveToFront("o/r", 3) {
		t.Fatal("MoveToFront rejected a tracked round")
	}
	if got := st.NextEligible(t0); got == nil || got.PR != 3 || got.Seq != -1 {
		t.Fatalf("first prioritized round = %+v, want PR 3 at sequence -1", got)
	}
	if !st.MoveToFront("o/r", 2) {
		t.Fatal("second MoveToFront rejected a tracked round")
	}
	if got := st.NextEligible(t0); got == nil || got.PR != 2 || got.Seq != -2 {
		t.Fatalf("second prioritized round = %+v, want PR 2 at sequence -2", got)
	}
	if st.MoveToFront("o/r", 99) {
		t.Fatal("MoveToFront accepted an untracked round")
	}

	reserved := st.Rounds[Key("o/r", 1)]
	reserved.Phase = PhaseReserved
	st.PutRound(reserved)
	seq := reserved.Seq
	if st.MoveToFront("o/r", 1) {
		t.Fatal("MoveToFront accepted a reserved round and changed its identity")
	}
	if got := st.Round("o/r", 1); got == nil || got.Seq != seq {
		t.Fatalf("reserved round = %+v, want sequence %d unchanged", got, seq)
	}
}
