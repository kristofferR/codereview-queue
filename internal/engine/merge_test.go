package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

func TestMergeGate(t *testing.T) {
	pass := func(name string) CISeen { return CISeen{Name: name, State: CIPass} }
	cases := []struct {
		name     string
		in       MergeSeen
		failed   []string
		pending  []string
		conflict bool
		behind   bool
		wait     string
		blocked  string
	}{
		{name: "green and clean", in: MergeSeen{CI: []CISeen{pass("test")}, State: "clean"}},
		{name: "nothing observed is not a gate", in: MergeSeen{}},
		{
			// Every check gates, not only required ones: a repository whose only
			// required check lints the description still has tests that matter.
			name:   "a failing optional check fails",
			in:     MergeSeen{CI: []CISeen{pass("lint"), {Name: "test", State: CIFail}}, Required: []string{"lint"}, State: "unstable"},
			failed: []string{"test"},
		},
		{
			// push and pull_request runs of one job share a name.
			name:   "duplicate names keep the worst",
			in:     MergeSeen{CI: []CISeen{pass("test"), {Name: "test", State: CIFail}}},
			failed: []string{"test"},
		},
		{
			name:    "a required check that has not reported is pending",
			in:      MergeSeen{CI: []CISeen{pass("test")}, Required: []string{"build"}, State: "blocked"},
			pending: []string{"build"},
			wait:    "waiting for CI: build",
		},
		{name: "unreadable CI waits", in: MergeSeen{CIUnknown: true, State: "clean"}, wait: "could not read CI for this head"},
		{name: "mergeability still computing", in: MergeSeen{State: "unknown"}, wait: "waiting for GitHub to compute mergeability"},
		{name: "conflict", in: MergeSeen{State: "dirty"}, conflict: true},
		{name: "behind a strict base", in: MergeSeen{State: "behind"}, behind: true},
		{name: "draft", in: MergeSeen{State: "draft"}, blocked: "the pull request is a draft"},
		{
			name:    "protection blocks a green PR",
			in:      MergeSeen{CI: []CISeen{pass("test")}, State: "blocked"},
			blocked: "branch protection still blocks merging (a required check, a required approval, or another rule)",
		},
		{
			// The failure is a finding first; dismissing it must not make the
			// PR read as done while GitHub still refuses it.
			name:    "a failed required check stays blocked",
			in:      MergeSeen{CI: []CISeen{{Name: "test", State: CIFail}}, Required: []string{"test"}, State: "blocked"},
			failed:  []string{"test"},
			blocked: "branch protection still blocks merging (a required check, a required approval, or another rule)",
		},
		{
			name:    "changes requested",
			in:      MergeSeen{State: "clean", ChangesRequested: []string{"alice"}},
			blocked: "changes requested by alice",
		},
		{
			name:    "a person's open thread",
			in:      MergeSeen{State: "clean", PersonThreads: []string{"https://x/1"}},
			blocked: "1 unresolved review thread(s) from people: https://x/1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := MergeGate(tc.in)
			var failed []string
			for _, c := range g.Failed {
				failed = append(failed, c.Name)
			}
			if !reflect.DeepEqual(failed, tc.failed) || !reflect.DeepEqual(g.Pending, tc.pending) ||
				g.Conflict != tc.conflict || g.Behind != tc.behind || g.Wait != tc.wait || g.Blocked != tc.blocked {
				t.Errorf("MergeGate = failed %v pending %v conflict %t behind %t wait %q blocked %q",
					failed, g.Pending, g.Conflict, g.Behind, g.Wait, g.Blocked)
			}
		})
	}
}

func TestGateFindingsAreHeadBound(t *testing.T) {
	gate := MergeGate(MergeSeen{CI: []CISeen{{Name: "test", State: CIFail, URL: "https://ci/1"}}, State: "dirty", Base: "main"})
	first := GateFindings(gate, nextHead)
	if len(first) != 2 || first[0].Source != SourceCICheck || first[1].Source != SourceMergeConflict {
		t.Fatalf("findings = %+v, want a ci_check and a merge_conflict", first)
	}
	if first[0].Commit != nextHead || first[0].ThreadID != "" || first[0].URL != "https://ci/1" {
		t.Errorf("ci finding = %+v, want head-bound, threadless, linked", first[0])
	}
	if again := GateFindings(gate, nextHead); again[0].ID != first[0].ID {
		t.Error("the same failure on the same head must keep its ID, or a dismissal cannot stick")
	}
	if other := GateFindings(gate, "b32eb5bfc"); other[0].ID == first[0].ID {
		t.Error("a dismissal must not carry to a new head")
	}
	if len(FindingsOnHead(first, nextHead)) != 0 || len(FindingsForActiveRound(first, nextHead, time.Time{})) != 0 {
		t.Error("gate findings are not reviewer feedback and never hold a review back")
	}
}

func TestNextActionMergeGate(t *testing.T) {
	reviewed := completionOf(map[string]bool{nextPrimary: true})
	pending := completionOf(map[string]bool{nextPrimary: false})
	fired := state.Round{Phase: state.PhaseFired}
	deadline := t0.Add(-time.Minute)
	expiredDispatch := state.Round{Phase: state.PhaseFired, WaitDeadline: &deadline, Dispatch: &state.DispatchClaim{Heartbeat: t0}}
	ciFail := GateFindings(MergeGate(MergeSeen{CI: []CISeen{{Name: "test", State: CIFail}}}), nextHead)
	cases := []struct {
		name string
		in   NextInput
		want ActionKind
	}{
		{"green gate is done", NextInput{Obs: openObs(), Completion: reviewed}, ActionDone},
		{"running CI waits", NextInput{Obs: openObs(), Completion: reviewed, Merge: Gate{Wait: "waiting for CI: test"}}, ActionWait},
		{"a person blocks", NextInput{Obs: openObs(), Completion: reviewed, Merge: Gate{Blocked: "changes requested by alice"}}, ActionBlocked},
		{"red CI is work", NextInput{Obs: openObs(), Completion: reviewed, Findings: ciFail}, ActionFix},
		{"red CI is work while a review runs", NextInput{Obs: openObs(), Completion: pending, Round: fired, Findings: ciFail}, ActionFix},
		// Only a push clears a red check, so a local fix must not hear `fix` again.
		{"a local CI fix pushes", NextInput{Obs: openObs(), Completion: reviewed, Findings: ciFail, LocalWork: true}, ActionPush},
		{"a local CI fix holds for a running review", NextInput{Obs: openObs(), Completion: pending, Round: fired, Findings: ciFail, LocalWork: true}, ActionHold},
		{"local work pushes before the gate is read", NextInput{Obs: openObs(), Completion: reviewed, LocalWork: true, Merge: Gate{Wait: "waiting for CI: test"}}, ActionPush},
		// A fixer admitted after the review deadline must still be told to push
		// its CI fix, not that the deadline elapsed.
		{"an admitted fixer pushes its CI fix", NextInput{Obs: openObs(), Completion: pending, Round: expiredDispatch, Findings: ciFail, LocalWork: true}, ActionPush},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextAction(tc.in, t0)
			if got.Kind != tc.want {
				t.Fatalf("NextAction = %s (%s), want %s", got.Kind, got.Reason, tc.want)
			}
			if got.Kind == ActionWait && !got.At.After(t0) {
				t.Errorf("wait At = %s, want a future recheck", got.At)
			}
		})
	}
}
