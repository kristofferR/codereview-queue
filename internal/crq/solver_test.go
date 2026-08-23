package crq

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOnePassSolverIsTemporaryAndRepositoryScoped(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	merge := "squash"
	if _, err := svc.SetSolver(ctx, "o/r", SolverChange{MergeMethod: &merge}); err == nil {
		t.Fatal("post-fix merge without one-pass was accepted")
	}
	on := true
	if _, err := svc.SetFleetSolver(ctx, SolverChange{OnePass: &on}); err == nil {
		t.Fatal("one-pass was accepted as a fleet-wide default")
	}
	view, err := svc.SetSolver(ctx, "o/r", SolverChange{OnePass: &on, MergeMethod: &merge})
	if err != nil {
		t.Fatal(err)
	}
	if !view.OnePass || view.MergeMethod != "squash" || view.Sources["one_pass"] != "repo" {
		t.Fatalf("one-pass solver view = %+v", view)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.MarkOnePassReady("o/r", 7, "abcdef123", "basebase1", "test", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err = svc.SetSolver(ctx, "o/r", SolverChange{Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.OnePass || view.MergeMethod != "" || view.Overridden {
		t.Fatalf("cleared solver view = %+v", view)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.OnePassProgressFor("o/r", 7); ok {
		t.Fatal("clearing the repository solver kept campaign hand-offs")
	}
}

func TestMergePolicyChangePreservesCompletedOnePassHandoff(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	on, squash := true, "squash"
	if _, err := svc.SetSolver(ctx, "o/r", SolverChange{OnePass: &on, MergeMethod: &squash}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.MarkOnePassReady("o/r", 7, "abcdef123", "basebase1", "test", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	off := ""
	if _, err := svc.SetSolver(ctx, "o/r", SolverChange{MergeMethod: &off}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if progress, ok := st.OnePassProgressFor("o/r", 7); !ok || progress.ReadyHead != "abcdef123" {
		t.Fatalf("merge-only change discarded handoff: %+v, present %t", progress, ok)
	}
}

// Solver settings exist so two repositories the same watcher handles can be
// fixed differently, so what this pins is the layering and — because these
// values are handed to an agent's command line — the validation that stops a
// session dying on its first argument.
func TestSolverLayering(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DispatchMaxAttempts = 3
	cfg.DispatchCommand = []string{"/usr/bin/claude"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// Nothing recorded: every value is this host's env.
	view, err := svc.Solver(ctx, "o/plain")
	if err != nil {
		t.Fatal(err)
	}
	if view.Overridden || view.MaxAttempts != 3 || view.Model != "" {
		t.Fatalf("view = %+v, want the env values with no record", view)
	}
	if view.Sources["one_pass"] != "default" || view.Sources["merge_method"] != "default" {
		t.Fatalf("campaign defaults reported wrong sources: %+v", view.Sources)
	}
	if view.Agent != "/usr/bin/claude" {
		t.Errorf("agent = %q, want the fleet's — it is baked into the session script", view.Agent)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"models":[]`) {
		t.Fatalf("empty model ranking must be a JSON array: %s", raw)
	}
	if got := strings.Join(view.ModelChoices, ","); got != "opus,sonnet,haiku,fable" {
		t.Errorf("model choices = %q, want the server-owned Claude vocabulary", got)
	}

	// A fleet default reaches every repository.
	if _, err := svc.SetFleetSolver(ctx, SolverChange{Effort: strptr("medium")}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/plain"); v.Effort != "medium" || v.Sources["effort"] != "fleet" {
		t.Errorf("view = %+v, want the fleet default applied and named", v)
	}
	if fleet, err := svc.FleetSolver(ctx); err != nil || fleet.Effort != "medium" {
		t.Fatalf("fleet solver = %+v, err %v, want the recorded solver default", fleet, err)
	}

	// Empty is a stated effort too: it asks the agent to choose, rather than
	// inheriting a nonempty fleet answer.
	if _, err := svc.SetSolver(ctx, "o/default-effort", SolverChange{Effort: strptr("")}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/default-effort"); v.Effort != "" || v.Sources["effort"] != "repo" {
		t.Errorf("view = %+v, want the repository's explicit agent default", v)
	}

	// A repository's own record wins over it, field by field: setting the model
	// here must not discard the fleet's effort.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Model: strptr("opus"), MaxAttempts: intptr(5),
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := svc.Solver(ctx, "o/special")
	if v.Model != "opus" || v.Sources["model"] != "repo" {
		t.Errorf("model = %q from %q, want the repository's own", v.Model, v.Sources["model"])
	}
	if v.Effort != "medium" || v.Sources["effort"] != "fleet" {
		t.Errorf("effort = %q from %q, want the fleet default still showing through", v.Effort, v.Sources["effort"])
	}
	if v.MaxAttempts != 5 {
		t.Errorf("attempts = %d, want the repository's 5 over the env's 3", v.MaxAttempts)
	}

	// And the resolved config is what a dispatch would actually run with.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/special"); got.FixModel != "opus" || got.DispatchMaxAttempts != 5 {
		t.Errorf("cfg = model %q attempts %d, want the record applied", got.FixModel, got.DispatchMaxAttempts)
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Models: []string{"opus", "sonnet", "opus"},
	}); err != nil {
		t.Fatal(err)
	}
	v, _ = svc.Solver(ctx, "o/special")
	if got := strings.Join(v.Models, ","); got != "opus,sonnet" {
		t.Errorf("ranked models = %q, want ordered and deduplicated", got)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/special"); strings.Join(got.FixModels, ",") != "opus,sonnet" {
		t.Errorf("dispatch config models = %v, want the ranking", got.FixModels)
	}
	if got := svc.cfgFor(st, "o/plain"); got.DispatchMaxAttempts != 3 {
		t.Errorf("attempts = %d, want another repository unaffected", got.DispatchMaxAttempts)
	}

	// A value the agent would reject is refused here instead.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{Effort: strptr("turbo")}); err == nil {
		t.Error("an unknown effort must be refused before it reaches a command line")
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{Prompt: strptr(strings.Repeat("x", 4001))}); err == nil {
		t.Error("an unbounded standing prompt must be refused — it is appended to every session")
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{MaxAttempts: intptr(99)}); err == nil {
		t.Error("an absurd attempt budget must be refused")
	}
	ask := "uncertain"
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Severities: []string{"minor", "minor"}, AskMode: &ask,
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/special"); strings.Join(v.Severities, ",") != "minor" ||
		v.AskMode != "uncertain" || v.Sources["severities"] != "repo" || v.Sources["ask_mode"] != "repo" {
		t.Errorf("policy view = %+v, want the repository's minor-only, ask-when-uncertain policy", v)
	}
	badAsk := "whenever"
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{AskMode: &badAsk}); err == nil {
		t.Error("an unknown clarification threshold must be refused")
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{Severities: []string{}}); err == nil {
		t.Error("an empty severity policy must direct the operator to the autofix switch")
	}

	// Emptying every field clears the record rather than leaving one that
	// overrides nothing.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Model: strptr(""), MaxAttempts: intptr(0), UnsetSeverities: true, UnsetAskMode: true,
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/special"); v.Overridden {
		t.Error("a record with nothing in it must not report the repository as overridden")
	}
}

func TestAbsentSolverPromptKeepsHostPrompt(t *testing.T) {
	cfg := firingConfig()
	cfg.FixPrompt = "standing host instructions"

	if got := cfg.withSolver(SolverSettings{}).FixPrompt; got != cfg.FixPrompt {
		t.Fatalf("prompt = %q, want host prompt %q when shared state says nothing", got, cfg.FixPrompt)
	}
	if got := cfg.withSolver(SolverSettings{Prompt: "fleet instructions"}).FixPrompt; got != "fleet instructions" {
		t.Fatalf("prompt = %q, want the shared solver prompt", got)
	}
}

func TestExplicitEmptyRepoPromptOverridesFleetPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetFleetSolver(ctx, SolverChange{Prompt: strptr("fleet instructions")}); err != nil {
		t.Fatal(err)
	}
	view, err := svc.SetSolver(ctx, "o/repo", SolverChange{Prompt: strptr("")})
	if err != nil {
		t.Fatal(err)
	}
	if view.Prompt != "" || view.Sources["prompt"] != "repo" {
		t.Fatalf("prompt = %q from %q, want an explicit empty repository prompt",
			view.Prompt, view.Sources["prompt"])
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/repo").FixPrompt; got != "" {
		t.Fatalf("fix session prompt = %q, want repository to clear the fleet prompt", got)
	}
}

func TestRepositoryPromptCanReturnToInheritance(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	if _, err := svc.SetFleetSolver(ctx, SolverChange{Prompt: strptr("fleet instructions")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSolver(ctx, "o/repo", SolverChange{Prompt: strptr("")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSolver(ctx, "o/repo", SolverChange{UnsetPrompt: true}); err != nil {
		t.Fatal(err)
	}
	if view, _ := svc.Solver(ctx, "o/repo"); view.Prompt != "fleet instructions" || view.Sources["prompt"] != "fleet" {
		t.Fatalf("view = %+v, want the inherited fleet prompt", view)
	}
}

// The hosts that will ignore a solver setting have to be named wherever it was
// recorded. Asking only about a repository's own record meant a FLEET model or
// attempt limit — the one every repository inherits — warned about nobody,
// while an old autofix service went on dispatching its install-time values.
func TestSolverNamesLaggingHostsForAFleetRecordToo(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	now := svc.clock().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetHostReport(HostReport{
			Host: "atlas", Caps: CapsSolver - 1, Roles: []string{"autofix"},
		}, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing recorded anywhere: there is no setting to be ignored.
	if v, _ := svc.Solver(ctx, "o/plain"); len(v.Lagging) != 0 {
		t.Errorf("lagging = %v, want nobody named when no record exists", v.Lagging)
	}

	if _, err := svc.SetFleetSolver(ctx, SolverChange{Model: strptr("opus")}); err != nil {
		t.Fatal(err)
	}
	v, err := svc.Solver(ctx, "o/plain")
	if err != nil {
		t.Fatal(err)
	}
	if v.Sources["model"] != "fleet" {
		t.Fatalf("model source = %q, want the fleet default answering", v.Sources["model"])
	}
	if len(v.Lagging) != 1 || v.Lagging[0] != "atlas" {
		t.Errorf("lagging = %v, want the autofix host that predates solver settings named", v.Lagging)
	}
}

// Which agent the fleet fixes with is a per-machine install answer, exported to
// the autofix unit alone. A dashboard process has none of its own, and guessing
// one made a codex fleet check every host for claude and report the setup that
// works as the one that is missing.
func TestSolverAgentComesFromTheHostsThatRunSessions(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig() // no fix agent: this is the serve process
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if v, _ := svc.Solver(ctx, "o/plain"); v.Agent != "" {
		t.Errorf("agent = %q, want silence from a process that has not been told", v.Agent)
	}

	now := svc.clock().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetHostReport(HostReport{
			Host: "atlas", Caps: WriterCaps, Roles: []string{"autofix"}, Agent: "codex",
		}, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/plain"); v.Agent != "codex" {
		t.Errorf("agent = %q, want the one the autofix host reports", v.Agent)
	}

	// The serve heartbeat on that same machine says nothing about the agent,
	// and silence must not erase what the autofix service reported.
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetHostReport(HostReport{Host: "atlas", Caps: WriterCaps, Roles: []string{"serve"}}, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/plain"); v.Agent != "codex" {
		t.Errorf("agent = %q, want the autofix service's answer kept", v.Agent)
	}
}

// The fork policy and the skip list are the two solver settings whose own
// values cannot express "follow the fleet again": false is a real fork policy
// and an empty author list means "skip nobody". Without an unset instruction a
// repository that had set either could only rejoin the fleet by clearing every
// solver setting it had, prompt included.
func TestSolverFieldsCanReturnToInheritance(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetFleetSolver(ctx, SolverChange{
		Effort: strptr("high"), Forks: boolptr(true), SkipAuthors: []string{"dependabot[bot]"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSolver(ctx, "o/repo", SolverChange{
		Effort: strptr(""), Prompt: strptr("this project uses bun"),
		Forks: boolptr(false), SkipAuthors: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := svc.Solver(ctx, "o/repo")
	if v.Effort != "" || v.Sources["effort"] != "repo" ||
		v.Forks || v.Sources["forks"] != "repo" || v.Sources["skip_authors"] != "repo" {
		t.Fatalf("view = %+v, want all three settings held by the repository", v)
	}

	if _, err := svc.SetSolver(ctx, "o/repo", SolverChange{
		UnsetEffort: true, UnsetForks: true, UnsetSkipAuthors: true,
	}); err != nil {
		t.Fatal(err)
	}
	v, _ = svc.Solver(ctx, "o/repo")
	if v.Effort != "high" || v.Sources["effort"] != "fleet" {
		t.Errorf("effort = %q from %q, want the fleet's answer back", v.Effort, v.Sources["effort"])
	}
	if !v.Forks || v.Sources["forks"] != "fleet" {
		t.Errorf("forks = %v from %q, want the fleet's answer back", v.Forks, v.Sources["forks"])
	}
	if len(v.SkipAuthors) != 1 || v.Sources["skip_authors"] != "fleet" {
		t.Errorf("skip authors = %v from %q, want the fleet's list back", v.SkipAuthors, v.Sources["skip_authors"])
	}
	if v.Prompt != "this project uses bun" {
		t.Errorf("prompt = %q, want the settings it was not asked about left alone", v.Prompt)
	}
}
