package serve

import (
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

func TestDiffStatesReportsSharedSettingsAndClearEdges(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	prev := state.New()
	prev.Rev = 1
	next := prev
	next.Rev = 2
	next.Fleet.MinInterval = "2m"
	next.Fleet.By, next.Fleet.UpdatedAt = "atlas", &at
	next.Enrolled = map[string]state.RepoEnrollment{
		"o/enrolled": {Enabled: true, By: "atlas", UpdatedAt: &at},
	}
	next.RepoSolver = map[string]state.SolverSettings{
		"o/solver": {Prompt: "use bun", By: "atlas", UpdatedAt: &at},
	}

	events := diffStates(prev, next, at)
	for _, want := range []string{
		"Fleet defaults changed",
		"Repository enrolled for review",
		"Fix-session settings changed",
	} {
		if !hasEventText(events, want) {
			t.Errorf("events = %+v, want %q", events, want)
		}
	}

	cleared := next
	cleared.Rev = 3
	cleared.Fleet = state.FleetDefaults{}
	cleared.Enrolled = nil
	cleared.RepoSolver = nil
	events = diffStates(next, cleared, at.Add(time.Minute))
	for _, want := range []string{
		"Fleet defaults cleared",
		"Enrollment decision cleared — back to this host's configuration",
		"Fix-session settings cleared — back to the fleet default",
	} {
		if !hasEventText(events, want) {
			t.Errorf("clear events = %+v, want %q", events, want)
		}
	}
}

func TestDiffStatesReportsTheFirstMutationAfterARevisionZeroLoad(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	prev := state.New()
	next := prev
	next.Rev = 1
	next.Enrolled = map[string]state.RepoEnrollment{
		"o/enrolled": {Enabled: true, By: "atlas", UpdatedAt: &at},
	}

	events := diffStates(prev, next, at)
	if !hasEventText(events, "Repository enrolled for review") {
		t.Fatalf("events = %+v, want the revision-zero predecessor to produce a diff", events)
	}
}

func hasEventText(events []Event, want string) bool {
	for _, event := range events {
		if event.Text == want {
			return true
		}
	}
	return false
}
