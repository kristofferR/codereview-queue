package main

import (
	"strings"
	"testing"
	"time"

	crqcore "github.com/kristofferR/coderabbit-queue/internal/crq"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

func TestWatchDispatchOptionHonorsFalse(t *testing.T) {
	if got := watchDispatchOption(true, false); got != nil {
		t.Errorf("default dispatch = %v, want the configured default", *got)
	}
	for _, tc := range []struct {
		name                 string
		dispatch, noDispatch bool
	}{
		{name: "standard false form", dispatch: false},
		{name: "observer alias", dispatch: true, noDispatch: true},
		{name: "false plus observer alias", dispatch: false, noDispatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := watchDispatchOption(tc.dispatch, tc.noDispatch)
			if got == nil || *got {
				t.Fatalf("watchDispatchOption(%t, %t) = %v, want explicit false",
					tc.dispatch, tc.noDispatch, got)
			}
		})
	}
}

// The thread commands take IDs bare so a caller can clear a whole round in one
// process. The transcripts that motivated this were full of shell loops running
// one `crq resolve` subprocess per thread, every round.
func TestParseThreadCommand(t *testing.T) {
	const (
		t1 = "PRRT_kwDOAAAAAA1"
		t2 = "PRRT_kwDOAAAAAA2"
	)
	cases := []struct {
		name    string
		args    []string
		reason  bool
		want    []string
		wantRsn string
		wantRes bool
		wantErr bool
	}{
		{name: "bare ids", args: []string{t1, t2}, want: []string{t1, t2}},
		{name: "flag form still works", args: []string{"--thread", t1, "--thread", t2}, want: []string{t1, t2}},
		{name: "mixed forms", args: []string{"--thread", t1, t2}, want: []string{t1, t2}},
		{
			// The old signature demanded a target these commands never used:
			// thread node IDs are globally unique.
			name: "legacy repo and pr are dropped",
			args: []string{"owner/repo", "123", t1},
			want: []string{t1},
		},
		{
			name: "legacy target with flag threads",
			args: []string{"owner/repo", "123", "--thread", t1},
			want: []string{t1},
		},
		{
			// A repo-shaped first arg is only a target when a PR number follows,
			// so an ID that happens to contain "/" is not eaten.
			name: "slashed id without a pr number is a thread",
			args: []string{"weird/id", t1},
			want: []string{"weird/id", t1},
		},
		{name: "dangling --thread is an error", args: []string{"--thread"}, wantErr: true},
		{
			name: "unknown flag is an error, not a thread id",
			args: []string{"--treahd", t1}, wantErr: true,
		},
		{
			// Declining resolves by default: a thread left open keeps its finding
			// actionable, so the loop would repeat `fix` forever.
			name: "decline resolves by default",
			args: []string{t1, "--reason", "not a real issue"}, reason: true,
			want: []string{t1}, wantRsn: "not a real issue", wantRes: true,
		},
		{
			name: "decline --keep-open leaves the disagreement open",
			args: []string{t1, "--reason", "still discussing", "--keep-open"}, reason: true,
			want: []string{t1}, wantRsn: "still discussing", wantRes: false,
		},
		{
			// --resolve used to be required; accepting it as a no-op keeps existing
			// callers working now that it is the default.
			name: "legacy --resolve still accepted",
			args: []string{t1, "--reason", "no", "--resolve"}, reason: true,
			want: []string{t1}, wantRsn: "no", wantRes: true,
		},
		{
			// resolve has no --reason, so passing one must fail rather than be
			// swallowed as a positional.
			name: "reason flag is rejected by resolve",
			args: []string{t1, "--reason", "x"}, wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			threads, reason, resolve, ok := parseThreadCommand(tc.args, tc.reason)
			if tc.wantErr {
				if ok {
					t.Fatalf("parseThreadCommand(%v) = %v, want an error", tc.args, threads)
				}
				return
			}
			if !ok {
				t.Fatalf("parseThreadCommand(%v) failed, want %v", tc.args, tc.want)
			}
			if strings.Join(threads, ",") != strings.Join(tc.want, ",") {
				t.Errorf("threads = %v, want %v", threads, tc.want)
			}
			if reason != tc.wantRsn {
				t.Errorf("reason = %q, want %q", reason, tc.wantRsn)
			}
			if resolve != tc.wantRes {
				t.Errorf("resolve = %v, want %v", resolve, tc.wantRes)
			}
		})
	}
}

// A mistyped flag used to be dropped as a non-positional, so `--wiat` ran the
// non-blocking form and looked like success.
func TestUnknownFlag(t *testing.T) {
	if _, found := unknownFlag([]string{"owner/repo", "1", "--wait"}, "--wait"); found {
		t.Error("a known flag must be accepted")
	}
	bad, found := unknownFlag([]string{"owner/repo", "1", "--wiat"}, "--wait")
	if !found || bad != "--wiat" {
		t.Errorf("unknownFlag = (%q, %v), want (--wiat, true)", bad, found)
	}
}

func TestReasonFlagDetection(t *testing.T) {
	for _, args := range [][]string{
		{"owner/repo", "1", "--reason", ""},
		{"owner/repo", "1", "--reason="},
		{"--reason=unused", "owner/repo", "1"},
	} {
		if !hasReasonArg(args) {
			t.Errorf("hasReasonArg(%v) = false, want true", args)
		}
	}
	if hasReasonArg([]string{"owner/repo", "1"}) {
		t.Error("arguments without --reason were reported as having it")
	}
}

func TestSolverTargetIsUnambiguous(t *testing.T) {
	if err := validateSolverTarget("owner/repo", true); err == nil {
		t.Fatal("a repository together with --fleet must be rejected")
	}
	for _, target := range []struct {
		repo  string
		fleet bool
	}{
		{repo: "owner/repo"},
		{fleet: true},
	} {
		if err := validateSolverTarget(target.repo, target.fleet); err != nil {
			t.Errorf("valid target %+v was rejected: %v", target, err)
		}
	}
}

func TestRunSolverParsesOnePassCampaign(t *testing.T) {
	cfg := crqcore.Config{Host: "test"}
	store := crqcore.NewMemoryStore(cfg)
	svc := crqcore.NewService(cfg, nil, store, nil)
	if code := runSolver(t.Context(), svc, []string{
		"set", "owner/repo", "--models", "gpt-5.6-sol", "--effort", "medium",
		"--attempts", "1", "--one-pass", "on", "--merge", "squash",
	}); code != 0 {
		t.Fatalf("runSolver campaign exit = %d", code)
	}
	view, err := svc.Solver(t.Context(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !view.OnePass || view.MergeMethod != "squash" || view.Model != "gpt-5.6-sol" || view.Effort != "medium" || view.MaxAttempts != 1 {
		t.Fatalf("campaign view = %+v", view)
	}
}

func TestMutationFlagsRequireSetAction(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		run  func() int
	}{
		{name: "fleet show", run: func() int {
			return runFleet(ctx, nil, []string{"--bots", "codex"})
		}},
		{name: "fleet clear", run: func() int {
			return runFleet(ctx, nil, []string{"clear", "--weekly-limit", "60"})
		}},
		{name: "solver show", run: func() int {
			return runSolver(ctx, nil, []string{"owner/repo", "--forks", "on"})
		}},
		{name: "solver clear", run: func() int {
			return runSolver(ctx, nil, []string{"clear", "owner/repo", "--attempts", "2"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.run(); code == 0 {
				t.Fatal("mutation flag outside set action succeeded")
			}
		})
	}
}

func TestSolverAgentHostMarksHistoricalProbesStale(t *testing.T) {
	const stamp = "2026-07-28T10:00:00Z"
	now, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	report := state.HostReport{
		Host:  "fixer",
		Roles: []string{"serve"},
		Tools: []state.ToolReport{{Name: "codex", Path: "/usr/bin/codex"}},
		At:    now,
	}

	got := solverAgentHost(report, "codex", now)
	if !got.Stale {
		t.Fatal("a host that no longer reports the autofix role must be stale")
	}
	if got.Has == nil || !*got.Has || got.Path != "/usr/bin/codex" {
		t.Fatalf("historical probe = %+v, want the last known installed path", got)
	}
}

// parseDismissArgs decides what counts as a finding ID, so a typo must not
// quietly become one.
func TestParseDismissArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		rest   []string
		reason string
		ok     bool
	}{
		{"separate reason", []string{"o/r", "7", "abc", "--reason", "why"}, []string{"o/r", "7", "abc"}, "why", true},
		{"equals reason", []string{"o/r", "7", "abc", "--reason=why"}, []string{"o/r", "7", "abc"}, "why", true},
		{"several ids", []string{"o/r", "7", "a", "b", "--reason", "why"}, []string{"o/r", "7", "a", "b"}, "why", true},
		{"reason first", []string{"--reason", "why", "o/r", "7", "a"}, []string{"o/r", "7", "a"}, "why", true},
		{"missing value", []string{"o/r", "7", "a", "--reason"}, nil, "", false},
		{"unknown flag", []string{"o/r", "7", "a", "--resaon", "why"}, nil, "", false},
		{"empty reason", []string{"o/r", "7", "a", "--reason", ""}, []string{"o/r", "7", "a"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, reason, ok := parseDismissArgs(tc.args)
			if ok != tc.ok || reason != tc.reason {
				t.Fatalf("got rest=%v reason=%q ok=%v, want reason=%q ok=%v", rest, reason, ok, tc.reason, tc.ok)
			}
			if !tc.ok {
				return
			}
			if len(rest) != len(tc.rest) {
				t.Fatalf("rest = %v, want %v", rest, tc.rest)
			}
			for i := range rest {
				if rest[i] != tc.rest[i] {
					t.Fatalf("rest = %v, want %v", rest, tc.rest)
				}
			}
		})
	}
}
