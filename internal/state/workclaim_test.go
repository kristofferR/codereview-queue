package state

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWorkClaimLivesAcrossHeadsAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	st := New()
	claim := WorkClaim{
		Owner: "session-a", By: "mac:feature", ClaimedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	st.SetWorkClaim("Owner/Repo", 12, claim)

	got, ok := st.WorkClaim("owner/repo", 12, now.Add(30*time.Minute))
	if !ok || got.Owner != claim.Owner {
		t.Fatalf("live claim = %#v, %t; want session-a", got, ok)
	}
	st.Normalize(now.Add(2 * time.Hour))
	if _, ok := st.WorkClaim("owner/repo", 12, now.Add(2*time.Hour)); ok {
		t.Fatal("expired work claim survived normalization")
	}
}

func TestWorkClaimReleaseIsOwnerScopedUnlessForced(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	st := New()
	st.SetWorkClaim("owner/repo", 12, WorkClaim{
		Owner: "session-a", ClaimedAt: now, ExpiresAt: now.Add(time.Hour),
	})

	if st.ReleaseWorkClaim("owner/repo", 12, "session-b", false) {
		t.Fatal("another owner released the work claim")
	}
	if _, ok := st.WorkClaim("owner/repo", 12, now); !ok {
		t.Fatal("failed release removed the claim")
	}
	if !st.ReleaseWorkClaim("owner/repo", 12, "", true) {
		t.Fatal("forced operator release did not remove the claim")
	}
}

func TestWorkClaimPreservesUnknownFieldsAfterKnownFieldChanges(t *testing.T) {
	raw := []byte(`{"owner":"session-a","by":"mac:old","claimed_at":"2026-08-14T12:00:00Z","expires_at":"2026-08-14T13:00:00Z","future":{"mode":"new","levels":[1,2,3]}}`)
	var claim WorkClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	claim.By = "mac:new"

	encoded, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip struct {
		By     string          `json:"by"`
		Future json.RawMessage `json:"future"`
	}
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.By != "mac:new" {
		t.Fatalf("known field = %q, want mac:new", roundTrip.By)
	}
	if got, want := string(roundTrip.Future), `{"mode":"new","levels":[1,2,3]}`; got != want {
		t.Fatalf("unknown field = %s, want %s", got, want)
	}
}
