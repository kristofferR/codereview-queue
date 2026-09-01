package state

import (
	"testing"
	"time"
)

func TestReviewedHeadsAreDistinctAndResettable(t *testing.T) {
	var st State
	if !st.NoteReviewedHead("Owner/Repo", 7, "head-a") {
		t.Fatal("first reviewed head was not recorded")
	}
	if st.NoteReviewedHead("owner/repo", 7, "head-a") {
		t.Fatal("same reviewed head was counted twice")
	}
	st.NoteReviewedHead("owner/repo", 7, "head-b")
	if got := st.ReviewRoundCount("owner/repo", 7); got != 2 {
		t.Fatalf("review round count = %d, want 2", got)
	}
	st.ResetReviewBudget("owner/repo", 7)
	if got := st.ReviewRoundCount("owner/repo", 7); got != 0 {
		t.Fatalf("reset review round count = %d, want 0", got)
	}
	st.ClearReviewBudget("owner/repo", 7)
	if _, ok := st.ReviewedHeads[Key("owner/repo", 7)]; ok {
		t.Fatal("cleared review budget kept its ledger")
	}
}

func TestNormalizeBootstrapsReviewBudgetOnce(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	answered := now.Add(-time.Hour)
	st := State{
		Rounds: map[string]Round{
			Key("owner/repo", 7): {Repo: "owner/repo", PR: 7, Head: "head-c", Phase: PhaseQueued},
		},
		Archive: []Round{
			{Repo: "owner/repo", PR: 7, Head: "head-a", PrimaryAnsweredAt: &answered},
			{Repo: "owner/repo", PR: 7, Head: "head-b", CoBots: map[string]CoBotRound{"bot": {AnsweredAt: &answered}}},
			{Repo: "owner/repo", PR: 7, Head: "unanswered"},
		},
	}
	st.Normalize(now)
	if got := st.ReviewRoundCount("owner/repo", 7); got != 2 {
		t.Fatalf("bootstrapped review round count = %d, want 2", got)
	}

	st.ResetReviewBudget("owner/repo", 7)
	st.Normalize(now.Add(time.Minute))
	if got := st.ReviewRoundCount("owner/repo", 7); got != 0 {
		t.Fatalf("Normalize rebuilt an explicitly reset budget: %d", got)
	}
}
