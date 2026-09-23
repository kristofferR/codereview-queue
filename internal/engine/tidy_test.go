package engine

import (
	"testing"
	"time"
)

// Every guard exists because removing the wrong comment costs a review. The
// expensive mistake is deleting one crq would otherwise have adopted: the next
// pump sees no command, posts another, and buys a second review of the same code.
func TestStaleCommands(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	head := base.Add(30 * time.Minute)
	answered := map[string]time.Time{"coderabbitai": base.Add(40 * time.Minute)}

	cases := []struct {
		name string
		in   TidyInput
		want []int64
	}{
		{
			name: "answered, old, and no round depends on it",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    answered,
				Commands:      []CommandComment{{ID: 1, Bot: "coderabbitai", CreatedAt: base}},
			},
			want: []int64{1},
		},
		{
			name: "a round that has not progressed keeps its command",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    answered,
				Live:          map[int64]bool{1: true, 2: true},
				Commands: []CommandComment{
					{ID: 1, Bot: "coderabbitai", CreatedAt: base},
					{ID: 2, Bot: "codex", CreatedAt: base},
				},
			},
		},
		{
			name: "a finished reviewer releases its own command while another reviewer waits",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    map[string]time.Time{"codex": head.Add(2 * time.Minute)},
				Live:          map[int64]bool{9: true},
				Settled:       map[int64]bool{9: true},
				Commands:      []CommandComment{{ID: 9, Bot: "codex", CreatedAt: head.Add(time.Minute)}},
			},
			want: []int64{9},
		},
		{
			name: "a reaction target keeps its completion evidence",
			in: TidyInput{
				AdoptableFrom:   head,
				AnsweredAt:      answered,
				ReactionTargets: map[int64]bool{1: true},
				Commands:        []CommandComment{{ID: 1, Bot: "coderabbitai", CreatedAt: base}},
			},
		},
		{
			name: "no evidence this bot ever acted",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    answered,
				Commands:      []CommandComment{{ID: 6, Bot: "codex", CreatedAt: base}},
			},
		},
		{
			name: "newer than the head: still adoptable",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    map[string]time.Time{"coderabbitai": head.Add(2 * time.Minute)},
				Commands:      []CommandComment{{ID: 7, Bot: "coderabbitai", CreatedAt: head.Add(time.Minute)}},
			},
		},
		{
			// This is the case that leaves a rate-limited PR with a column of
			// identical review requests: the retry that replaced this command is
			// newer than the head too, so the adoption guard alone never clears it.
			name: "a retry the round replaced is spent despite the head",
			in: TidyInput{
				AdoptableFrom: head,
				AnsweredAt:    map[string]time.Time{"coderabbitai": head.Add(2 * time.Minute)},
				Superseded:    map[int64]bool{8: true},
				Commands:      []CommandComment{{ID: 8, Bot: "coderabbitai", CreatedAt: head.Add(time.Minute)}},
			},
			want: []int64{8},
		},
		{
			// An unreadable head commit is not permission to delete: the command
			// may still be adoptable once the read recovers.
			name: "no head timestamp keeps what the guard cannot clear",
			in: TidyInput{
				AnsweredAt: map[string]time.Time{"coderabbitai": base.Add(time.Hour)},
				Commands:   []CommandComment{{ID: 1, Bot: "coderabbitai", CreatedAt: base}},
			},
		},
		{
			name: "no head timestamp still releases a replaced command",
			in: TidyInput{
				AnsweredAt: map[string]time.Time{"coderabbitai": base.Add(time.Hour)},
				Superseded: map[int64]bool{1: true},
				Commands:   []CommandComment{{ID: 1, Bot: "coderabbitai", CreatedAt: base}},
			},
			want: []int64{1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StaleCommands(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("stale = %v, want %v", got, tc.want)
			}
			for i, id := range tc.want {
				if got[i] != id {
					t.Fatalf("stale = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
