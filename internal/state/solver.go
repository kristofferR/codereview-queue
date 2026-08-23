package state

import (
	"sort"
	"time"
)

// SolverSettings is how a fix session should be run: which model, how hard it
// should think, what else to tell it, and the limits crq itself enforces.
//
// It is recorded per repository and, as a default, for the whole fleet. The
// reason it is not one fleet-wide answer is that the repositories differ more
// than the fleet does: a Go service worth a slow careful model and five
// attempts sits next to a docs repository where a fast one and a single
// attempt is the right trade.
//
// What is NOT here is the agent binary. That is chosen at install time and
// baked into the session script, because switching between claude and codex is
// a different command line rather than a different flag — and a per-repo agent
// would mean the watcher could not know what it was about to run until it ran
// it. Model and effort ARE per repo, because every agent has them.
type SolverSettings struct {
	// Models is the preferred model followed by ordered fallbacks. SetModels
	// distinguishes an explicit "agent default" (an empty list) from inherit.
	//
	// Model is retained for state written by v2.0 binaries. New writers keep it
	// equal to the first model so older readers still run the preferred choice.
	Models    []string `json:"models,omitempty"`
	SetModels bool     `json:"set_models,omitempty"`
	Model     string   `json:"model,omitempty"`
	// SetEffort distinguishes an explicit "agent default" (an empty effort)
	// from inheritance, just as SetModels does for the model ranking.
	Effort    string `json:"effort,omitempty"`
	SetEffort bool   `json:"set_effort,omitempty"`
	// Prompt is extra instruction appended to the fix prompt, for the standing
	// context a repository needs every time ("this project uses bun, never npm").
	// SetPrompt distinguishes an explicit empty prompt from inheritance.
	Prompt    string `json:"prompt,omitempty"`
	SetPrompt bool   `json:"set_prompt,omitempty"`
	// MaxAttempts bounds fix sessions per head, so a fix that keeps not working
	// stops. Nil inherits.
	MaxAttempts *int `json:"max_attempts,omitempty"`
	// Severities limits which findings autofix hands to an agent. SetSeverities
	// distinguishes an explicit empty selection ("fix nothing") from inherit.
	Severities    []string `json:"severities,omitempty"`
	SetSeverities bool     `json:"set_severities,omitempty"`
	// AskMode controls how readily a fix session stops for clarification.
	// SetAskMode distinguishes an explicit choice from inherit.
	AskMode    string `json:"ask_mode,omitempty"`
	SetAskMode bool   `json:"set_ask_mode,omitempty"`
	// Forks allows sessions on pull requests whose head branch lives in another
	// repository. Off by default fleet-wide: a session runs an agent over that
	// branch's code with approvals bypassed and a write token in reach.
	Forks *bool `json:"forks,omitempty"`
	// SkipAuthors are pull-request authors crq does not enqueue here. Set*
	// distinguishes "not chosen" from "chosen to be nobody".
	SkipAuthors    []string `json:"skip_authors,omitempty"`
	SetSkipAuthors bool     `json:"set_skip_authors,omitempty"`
	// OnePass turns the automated workflow into one review round followed by a
	// fixer session and, optionally, a merge. It is deliberately repository-
	// scoped: bulk campaigns need the throughput, while ordinary feature work
	// should keep the normal incremental review loop.
	OnePass    bool `json:"one_pass,omitempty"`
	SetOnePass bool `json:"set_one_pass,omitempty"`
	// MergeMethod is empty/off, merge, squash, or rebase. SetMerge distinguishes
	// an explicit off from inheritance. A merge is attempted only after the
	// one-pass fixer completed successfully for the exact current head.
	MergeMethod string `json:"merge_method,omitempty"`
	SetMerge    bool   `json:"set_merge,omitempty"`

	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	// unknown carries members a newer binary wrote inside this record, for the
	// same reason FleetDefaults does: it is nested, so no outer carrier sees it.
	unknown unknownFields
}

// Empty reports whether this record says nothing at all, which is how a "clear"
// is distinguished from a setting of every field to its zero value.
//
// A carried member counts, for the same reason it does in FleetDefaults:
// clearing the last field THIS binary knows would otherwise drop the repository
// record — or collapse the fleet defaults around it — and take a newer binary's
// solver setting with it.
func (s SolverSettings) Empty() bool {
	return !s.SetModels && len(s.Models) == 0 && s.Model == "" &&
		!s.SetEffort && s.Effort == "" && !s.SetPrompt && s.Prompt == "" &&
		s.MaxAttempts == nil && !s.SetSeverities && len(s.Severities) == 0 &&
		!s.SetAskMode && s.AskMode == "" &&
		s.Forks == nil && !s.SetSkipAuthors && !s.SetOnePass && !s.SetMerge &&
		len(s.unknown) == 0
}

// Merge layers one record over another: every field this one states wins, and
// every field it leaves absent keeps the base's answer. That is what makes
// "fleet default, then repository" work without either layer knowing about the
// other.
func (s SolverSettings) Merge(over SolverSettings) SolverSettings {
	out := s
	if over.SetModels || len(over.Models) > 0 {
		out.Models = append([]string(nil), over.Models...)
		out.SetModels = true
		out.Model = firstModel(out.Models)
	} else if over.Model != "" {
		// A legacy record is a one-entry ranking.
		out.Models = []string{over.Model}
		out.SetModels = true
		out.Model = over.Model
	}
	if over.SetEffort || over.Effort != "" {
		out.Effort = over.Effort
		out.SetEffort = true
	}
	if over.SetPrompt || over.Prompt != "" {
		out.Prompt = over.Prompt
		out.SetPrompt = true
	}
	if over.MaxAttempts != nil {
		out.MaxAttempts = over.MaxAttempts
	}
	if over.SetSeverities || len(over.Severities) > 0 {
		out.Severities = append([]string(nil), over.Severities...)
		out.SetSeverities = true
	}
	if over.SetAskMode || over.AskMode != "" {
		out.AskMode = over.AskMode
		out.SetAskMode = true
	}
	if over.Forks != nil {
		out.Forks = over.Forks
	}
	if over.SetSkipAuthors {
		out.SkipAuthors, out.SetSkipAuthors = over.SkipAuthors, true
	}
	if over.SetOnePass {
		out.OnePass, out.SetOnePass = over.OnePass, true
	}
	if over.SetMerge {
		out.MergeMethod, out.SetMerge = over.MergeMethod, true
	}
	if over.UpdatedAt != nil {
		out.By, out.UpdatedAt = over.By, over.UpdatedAt
	}
	return out
}

// RankedModels returns the effective ordered model list. A nil/empty result
// means "use the agent's own default".
func (s SolverSettings) RankedModels() []string {
	if s.SetModels || len(s.Models) > 0 {
		return append([]string(nil), s.Models...)
	}
	if s.Model != "" {
		return []string{s.Model}
	}
	return nil
}

func firstModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	return models[0]
}

// Solver returns repo's own solver record, and whether one exists.
func (s *State) Solver(repo string) (SolverSettings, bool) {
	sv, ok := s.RepoSolver[normalizeRepoKey(repo)]
	return sv, ok
}

// SetSolver records repo's solver settings, replacing any earlier ones.
func (s *State) SetSolver(repo string, sv SolverSettings, by string, now time.Time) {
	if s.RepoSolver == nil {
		s.RepoSolver = map[string]SolverSettings{}
	}
	at := now.UTC()
	sv.By, sv.UpdatedAt = by, &at
	s.RepoSolver[normalizeRepoKey(repo)] = sv
}

// ClearSolver drops repo's record, returning it to the fleet default.
func (s *State) ClearSolver(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.RepoSolver[key]; !ok {
		return false
	}
	delete(s.RepoSolver, key)
	return true
}

// SolverRepos lists every repository with its own record, sorted.
func (s *State) SolverRepos() []string {
	out := make([]string, 0, len(s.RepoSolver))
	for repo := range s.RepoSolver {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// EffectiveSolver is the fleet default with repo's own record layered over it.
func (s *State) EffectiveSolver(repo string) SolverSettings {
	sv, _ := s.Solver(repo)
	return s.Fleet.Solver.Merge(sv)
}
