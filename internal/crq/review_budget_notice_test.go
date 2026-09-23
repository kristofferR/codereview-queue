package crq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/engine"
)

func TestReviewBudgetNoticePreservesHoldAndHandlesPostingRaces(t *testing.T) {
	for _, name := range []string{"dry run", "post failure", "released during post", "replaced during post", "competing pump"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := firingConfig()
			cfg.MaxReviewRounds = 1
			cfg.DryRun = name == "dry run"
			gh := newFakeGitHub()
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, gh, store, nil)
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			svc.now = func() time.Time { return now }
			var round Round
			if _, err := store.Update(ctx, func(st *State) error {
				r, err := st.NewRound("owner/repo", 7, "new-head", now)
				if err != nil {
					return err
				}
				round = *r
				st.NoteReviewedHead("owner/repo", 7, "old-head")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			fire := func() (PumpResult, error) {
				return svc.applyFire(ctx, cfg, round, engine.Observation{Head: round.Head, Open: true}, engine.FireDecision{Verdict: engine.FirePost}, now)
			}
			switch name {
			case "post failure":
				gh.postErrs = map[string]error{fakeKey("owner/repo", 7): errors.New("comments disabled")}
			case "released during post":
				gh.postHook = func() {
					if _, err := svc.Unhold(ctx, "owner/repo", 7); err != nil {
						t.Error(err)
					}
				}
			case "replaced during post":
				gh.postHook = func() {
					if _, err := store.Update(ctx, func(st *State) error {
						st.Hold("owner/repo", 7, "operator replacement", "operator", now)
						return nil
					}); err != nil {
						t.Error(err)
					}
				}
			case "competing pump":
				gh.postHook = func() {
					result, err := fire()
					if err != nil || result.Action != "lost_race" {
						t.Errorf("competing pump = %+v, %v", result, err)
					}
				}
			}
			result, err := fire()
			if err != nil {
				t.Fatal(err)
			}
			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			hold, held := st.HeldPR("owner/repo", 7)
			switch name {
			case "dry run":
				if held || len(gh.posted) != 0 || result.Action != "held" {
					t.Fatalf("dry run mutated state: held=%v comments=%v result=%+v", held, gh.posted, result)
				}
			case "post failure":
				if !held || result.Action != "held" || !strings.Contains(result.Warning, "comments disabled") {
					t.Fatalf("post failure must retain hold and report warning: held=%v result=%+v", held, result)
				}
			case "released during post":
				if held || result.Action != "lost_race" || len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
					t.Fatalf("released hold kept stale notice: held=%v result=%+v deleted=%v", held, result, gh.deleteCalls)
				}
			case "replaced during post":
				if !held || hold.Reason != "operator replacement" || result.Reason != hold.Reason || len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
					t.Fatalf("replacement hold was lost or stale notice survived: hold=%+v result=%+v deleted=%v", hold, result, gh.deleteCalls)
				}
			case "competing pump":
				if !held || result.Action != "held" {
					t.Fatalf("hold lost: %+v", result)
				}
				assertOnlyHoldComment(t, gh)
			}
		})
	}
}
