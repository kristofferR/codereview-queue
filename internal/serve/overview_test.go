package serve

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

func TestAutofixSessionKeepsTheHeadOwnedByItsClaim(t *testing.T) {
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	key := state.Key("o/repo", 1)
	claim := state.DispatchClaim{Host: "atlas", Token: "session-1", Model: "fallback-model", At: now, Heartbeat: now}
	st := state.New()
	st.Rounds[key] = state.Round{Repo: "o/repo", PR: 1, Head: "new-head"}
	st.Archive = []state.Round{{
		Repo: "O/Repo", PR: 1, Head: "old-head", Dispatch: &claim,
	}}
	st.Dispatches = map[string]state.DispatchClaim{key: claim}

	view := autofixView(st, now, nil)
	if len(view.Sessions) != 1 ||
		view.Sessions[0].Head != "old-head" || view.Sessions[0].Model != "fallback-model" {
		t.Fatalf("sessions = %+v, want the live session's archived head and selected fallback model", view.Sessions)
	}
}

func TestFinishedRowsShowMergedOutcome(t *testing.T) {
	st := state.New()
	st.Archive = []state.Round{{
		Repo: "o/repo", PR: 1, Head: "merged-head", Phase: state.PhaseAbandoned, Note: "merged",
	}}

	rows := finishedRows(st)
	if len(rows) != 1 || rows[0].Outcome != "merged" {
		t.Fatalf("finished rows = %+v, want merged outcome", rows)
	}
}

func TestPrimaryOffQueueRowWaitsForALiveFireSlot(t *testing.T) {
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	blocked := now.Add(time.Hour)
	fired := now
	st := state.New()
	st.Account.BlockedUntil = &blocked
	st.LastFired = &fired
	st.Rounds[state.Key("o/free", 1)] = state.Round{
		Repo: "o/free", PR: 1, Head: "free-head", Phase: state.PhaseQueued, Seq: 1, EnqueuedAt: now,
	}
	st.Rounds[state.Key("o/metered", 2)] = state.Round{
		Repo: "o/metered", PR: 2, Head: "metered-head", Phase: state.PhaseReserved,
		Seq: 2, EnqueuedAt: now.Add(-time.Minute), ReservedAt: &now, Token: "slot-token",
	}
	st.FireSlot = &state.FireSlot{Key: state.Key("o/metered", 2), Token: "slot-token", Since: now}
	botsFor := func(string) []BotName {
		return []BotName{{Login: "chatgpt-codex-connector[bot]", Name: "Codex"}}
	}

	ov := BuildOverview(st, now, 90*time.Second, time.Minute, botsFor, 0, nil)
	if len(ov.Queue) != 1 {
		t.Fatalf("queue = %+v, want one row", ov.Queue)
	}
	row := ov.Queue[0]
	if row.Why != state.WaitSlotBusy || row.ReadyAt != nil || row.Position != 0 {
		t.Fatalf("primary-off row = %+v, want the live slot that makes Pump return to remain visible", row)
	}
}

func TestPRSessionKeepsTheArchivedHeadOwnedByItsClaim(t *testing.T) {
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	key := state.Key("o/repo", 1)
	claim := state.DispatchClaim{Host: "atlas", Token: "session-1", Model: "fallback-model", At: now, Heartbeat: now}
	st := state.New()
	st.Rounds[key] = state.Round{
		Repo: "o/repo", PR: 1, Head: "new-head", Phase: state.PhaseQueued, EnqueuedAt: now,
	}
	st.Archive = []state.Round{{
		Repo: "O/Repo", PR: 1, Head: "old-head", Dispatch: &claim,
	}}
	st.Dispatches = map[string]state.DispatchClaim{key: claim}

	view := buildPRView(st, "o/repo", 1, nil, time.Minute, now, nil)
	if view.Round == nil || view.Round.Fixing == nil ||
		view.Round.Fixing.Head != "old-head" || view.Round.Fixing.Model != "fallback-model" {
		t.Fatalf("PR fixing session = %+v, want its archived head and selected fallback model", view.Round)
	}
}
