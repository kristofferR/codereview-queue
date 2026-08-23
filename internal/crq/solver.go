package crq

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// SolverView is how one repository's fix sessions will actually be run.
type SolverView struct {
	Repo string `json:"repo"`
	// Overridden says this repository has its own record rather than following
	// the fleet default.
	Overridden bool `json:"overridden"`
	// Agent is the fleet's, always: it is chosen at install time and baked into
	// the session script, because switching agents is a different command line
	// rather than a different flag. Reported so a reader knows what the model
	// and effort below are being handed to.
	Agent string `json:"agent,omitempty"`

	Models       []string `json:"models"`
	ModelChoices []string `json:"model_choices"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	MaxAttempts  int      `json:"max_attempts"`
	Severities   []string `json:"severities"`
	AskMode      string   `json:"ask_mode"`
	Forks        bool     `json:"forks"`
	SkipAuthors  []string `json:"skip_authors"`
	OnePass      bool     `json:"one_pass"`
	MergeMethod  string   `json:"merge_method,omitempty"`

	// Sources says, per setting, whether the value came from this repository's
	// record, the fleet default, or this host's env.
	Sources   map[string]string `json:"sources"`
	By        string            `json:"by,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
	Lagging   []string          `json:"lagging_hosts,omitempty"`
}

// Solver reports how repo's fix sessions will run.
func (s *Service) Solver(ctx context.Context, repo string) (SolverView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return SolverView{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return SolverView{}, err
	}
	return s.solverViewOf(st, repo), nil
}

func (s *Service) solverViewOf(st State, repo string) SolverView {
	cfg := s.cfgFor(st, repo)
	own, has := st.Solver(repo)
	fleet := st.Fleet.Solver

	view := SolverView{
		Repo: repo, Overridden: has && !own.Empty(),
		Models: append([]string{}, cfg.FixModels...),
		Model:  cfg.FixModel, Effort: cfg.FixEffort, Prompt: cfg.FixPrompt,
		MaxAttempts: cfg.DispatchMaxAttempts, Forks: cfg.DispatchForks,
		Severities: sortedKeys(cfg.FixSeverities), AskMode: cfg.FixAskMode,
		SkipAuthors: sortedKeys(cfg.SkipAuthors),
		OnePass:     cfg.OnePass, MergeMethod: cfg.MergeMethod,
		Sources: map[string]string{},
	}
	// Three layers, and the view names which one answered — the same
	// distinction the fleet settings make, for the same reason: a value showing
	// "env" is this host's file, and changing it here starts a record.
	source := func(key string, inRepo, inFleet bool) {
		switch {
		case inRepo:
			view.Sources[key] = "repo"
		case inFleet:
			view.Sources[key] = "fleet"
		default:
			view.Sources[key] = "env"
		}
	}
	source("models", own.SetModels || len(own.Models) > 0 || own.Model != "",
		fleet.SetModels || len(fleet.Models) > 0 || fleet.Model != "")
	view.Sources["model"] = view.Sources["models"]
	source("effort", own.SetEffort || own.Effort != "",
		fleet.SetEffort || fleet.Effort != "")
	source("prompt", own.SetPrompt || own.Prompt != "", fleet.SetPrompt || fleet.Prompt != "")
	source("max_attempts", own.MaxAttempts != nil, fleet.MaxAttempts != nil)
	source("severities", own.SetSeverities || len(own.Severities) > 0,
		fleet.SetSeverities || len(fleet.Severities) > 0)
	source("ask_mode", own.SetAskMode || own.AskMode != "",
		fleet.SetAskMode || fleet.AskMode != "")
	source("forks", own.Forks != nil, fleet.Forks != nil)
	source("skip_authors", own.SetSkipAuthors, fleet.SetSkipAuthors)
	source("one_pass", own.SetOnePass, fleet.SetOnePass)
	source("merge_method", own.SetMerge, fleet.SetMerge)
	if !own.SetOnePass && !fleet.SetOnePass {
		view.Sources["one_pass"] = "default"
	}
	if !own.SetMerge && !fleet.SetMerge {
		view.Sources["merge_method"] = "default"
	}

	// This host's own answer when it runs fix sessions, and the fleet's
	// self-reports otherwise. The dashboard is normally the second case: the
	// agent is exported to the autofix unit alone, so a process serving the page
	// has none of its own and reported nothing — which the page then had to guess
	// at, and it guessed claude on a fleet fixing with codex.
	view.Agent = s.cfg.fixAgent()
	if view.Agent == "" {
		view.Agent = st.FixAgent(s.clock().UTC())
	}
	view.ModelChoices = modelChoicesFor(view.Agent, view.Models)
	if has && own.UpdatedAt != nil {
		view.By = own.By
		view.UpdatedAt = own.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	// The warning belongs to the SETTING, not to the layer that happens to hold
	// it. Asking only about this repository's own record meant a fleet-wide
	// model, effort, fork policy or attempt limit — which every repository
	// inheriting it is run with — warned about nobody, while an old autofix
	// service went on dispatching its install-time values.
	//
	// The autofix role too, not just the queue's drivers: these settings are
	// consumed when a fix session STARTS, and the watcher that starts one holds
	// neither the leader lease nor the fire slot.
	if (has && own.UpdatedAt != nil) || fleet.UpdatedAt != nil {
		caps, roles := CapsSolver, []string{"autofix"}
		if cfg.OnePass || cfg.MergeMethod != "" {
			caps, roles = CapsOnePass, []string{"autoreview", "autofix"}
		}
		view.Lagging = st.LaggingRoleWriters(caps, s.clock().UTC(), roles...)
	}
	return view
}

// modelChoicesFor keeps agent-specific model vocabulary on the server. The
// browser receives both the supported choices and any already-recorded custom
// value, so a newer/older model is never made uneditable by a binary upgrade.
func modelChoicesFor(agent string, selected []string) []string {
	name := strings.ToLower(filepath.Base(agent))
	var known []string
	switch name {
	case "claude":
		known = []string{"opus", "sonnet", "haiku", "fable"}
	case "codex":
		known = []string{"gpt-5.6-sol", "gpt-5.6-terra", "codex-auto-review"}
	default:
		known = []string{"gpt-5.6-sol", "gpt-5.6-terra", "codex-auto-review", "opus", "sonnet", "haiku", "fable"}
	}
	out := make([]string, 0, len(selected)+len(known))
	seen := map[string]bool{}
	for _, model := range append(append([]string{}, selected...), known...) {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

// SolverChange is a proposed edit. Absent fields are left alone, so a form
// posting its whole state cannot clobber a setting changed a second earlier.
type SolverChange struct {
	Models      []string `json:"models"`
	Model       *string  `json:"model"`
	Effort      *string  `json:"effort"`
	Prompt      *string  `json:"prompt"`
	MaxAttempts *int     `json:"max_attempts"`
	Severities  []string `json:"severities"`
	AskMode     *string  `json:"ask_mode"`
	Forks       *bool    `json:"forks"`
	SkipAuthors []string `json:"skip_authors"`
	OnePass     *bool    `json:"one_pass"`
	MergeMethod *string  `json:"merge_method"`
	// Unset* hands ONE setting back to the layer beneath, the same instruction
	// FleetChange's Unset* fields carry. An empty model ranking and an empty
	// effort both mean "use the agent default", false is a real fork policy, and
	// an empty author list is "skip nobody", so none can also mean inheritance.
	UnsetModels      bool `json:"unset_models,omitempty"`
	UnsetEffort      bool `json:"unset_effort,omitempty"`
	UnsetPrompt      bool `json:"unset_prompt,omitempty"`
	UnsetSeverities  bool `json:"unset_severities,omitempty"`
	UnsetAskMode     bool `json:"unset_ask_mode,omitempty"`
	UnsetForks       bool `json:"unset_forks,omitempty"`
	UnsetSkipAuthors bool `json:"unset_skip_authors,omitempty"`
	UnsetOnePass     bool `json:"unset_one_pass,omitempty"`
	UnsetMerge       bool `json:"unset_merge,omitempty"`
	// Clear drops the whole record, returning every setting to the fleet default.
	Clear bool `json:"clear"`
}

// knownEfforts are the reasoning levels every supported agent understands. An
// unknown one is refused rather than passed through: it would reach the agent
// as a flag value, and a session that dies on its first argument is a fix that
// silently never happens.
var knownEfforts = []string{"low", "medium", "high", "xhigh", "max"}
var knownAskModes = []string{"blocked", "uncertain", "ambiguous"}

// SetSolver records how repo's fix sessions should run.
func (s *Service) SetSolver(ctx context.Context, repo string, change SolverChange) (SolverView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return SolverView{}, err
	}
	now := s.clock().UTC()
	campaign := randomToken()
	st, err := s.store.Update(ctx, func(st *State) error {
		if change.Clear {
			before := st.EffectiveSolver(repo)
			cleared := st.ClearSolver(repo)
			after := st.EffectiveSolver(repo)
			progress := false
			if before.OnePass != after.OnePass {
				progress = st.ClearOnePassRepo(repo)
			}
			if !cleared && !progress {
				return ErrNoChange
			}
			return nil
		}
		sv, _ := st.Solver(repo)
		next, err := applySolverChange(sv, change)
		if err != nil {
			return err
		}
		if next.Empty() {
			// Setting every field back to nothing IS clearing it: leaving an
			// empty record behind would report the repository as overridden
			// while overriding nothing.
			before := st.EffectiveSolver(repo)
			cleared := st.ClearSolver(repo)
			after := st.EffectiveSolver(repo)
			progress := false
			if before.OnePass != after.OnePass {
				progress = st.ClearOnePassRepo(repo)
			}
			if !cleared && !progress {
				return ErrNoChange
			}
			return nil
		}
		effective := st.Fleet.Solver.Merge(next)
		if effective.MergeMethod != "" && !effective.OnePass {
			return errors.New("post-fix merge requires one-pass review mode")
		}
		before := st.EffectiveSolver(repo)
		if before.OnePass != effective.OnePass {
			st.ClearOnePassRepo(repo)
		}
		if !before.OnePass && effective.OnePass {
			next.OnePassCampaign = campaign
		} else if !effective.OnePass {
			next.OnePassCampaign = ""
		}
		st.SetSolver(repo, next, s.cfg.Host, now)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return SolverView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return SolverView{}, err
		}
	}
	return s.solverViewOf(st, repo), nil
}

// SetFleetSolver records the fleet-wide default every repository inherits.
func (s *Service) SetFleetSolver(ctx context.Context, change SolverChange) (SolverSettings, error) {
	if change.OnePass != nil || change.MergeMethod != nil || change.UnsetOnePass || change.UnsetMerge {
		return SolverSettings{}, errors.New("one-pass review and post-fix merge are repository-scoped settings")
	}
	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if change.Clear {
			if st.Fleet.Solver.Empty() {
				return ErrNoChange
			}
			fd := st.Fleet
			fd.Solver = SolverSettings{}
			st.SetFleetDefaults(fd, s.cfg.Host, now)
			return nil
		}
		next, err := applySolverChange(st.Fleet.Solver, change)
		if err != nil {
			return err
		}
		if next.Empty() {
			if st.Fleet.Solver.Empty() {
				return ErrNoChange
			}
			fd := st.Fleet
			fd.Solver = SolverSettings{}
			st.SetFleetDefaults(fd, s.cfg.Host, now)
			return nil
		}
		if next.MergeMethod != "" && !next.OnePass {
			return errors.New("post-fix merge requires one-pass review mode")
		}
		at := now
		next.By, next.UpdatedAt = s.cfg.Host, &at
		fd := st.Fleet
		fd.Solver = next
		st.SetFleetDefaults(fd, s.cfg.Host, now)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return SolverSettings{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else if st, _, err = s.store.Load(ctx); err != nil {
		return SolverSettings{}, err
	}
	return st.Fleet.Solver, nil
}

// FleetSolver reports the recorded fleet-wide solver default.
func (s *Service) FleetSolver(ctx context.Context) (SolverSettings, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return SolverSettings{}, err
	}
	return st.Fleet.Solver, nil
}

// applySolverChange folds a change onto a record, validating it.
func applySolverChange(sv SolverSettings, change SolverChange) (SolverSettings, error) {
	if change.UnsetModels {
		sv.Models, sv.SetModels, sv.Model = nil, false, ""
	} else if change.Models != nil {
		models := make([]string, 0, len(change.Models))
		seen := map[string]bool{}
		for _, model := range change.Models {
			model = strings.TrimSpace(model)
			if model != "" && !seen[model] {
				seen[model] = true
				models = append(models, model)
			}
		}
		if len(models) > 8 {
			return sv, errors.New("model ranking is limited to 8 entries")
		}
		sv.Models, sv.SetModels = models, true
		sv.Model = ""
		if len(models) > 0 {
			sv.Model = models[0]
		}
	} else if change.Model != nil {
		// Legacy single-model clients still produce a valid one-entry ranking.
		model := strings.TrimSpace(*change.Model)
		if model == "" {
			sv.Models, sv.SetModels, sv.Model = nil, false, ""
		} else {
			sv.Models, sv.SetModels, sv.Model = []string{model}, true, model
		}
	}
	if change.UnsetEffort {
		sv.Effort, sv.SetEffort = "", false
	} else if change.Effort != nil {
		effort := strings.ToLower(strings.TrimSpace(*change.Effort))
		if effort != "" && !containsString(knownEfforts, effort) {
			return sv, fmt.Errorf("effort %q is not one of %s", effort, strings.Join(knownEfforts, ", "))
		}
		sv.Effort, sv.SetEffort = effort, true
	}
	if change.UnsetPrompt {
		sv.Prompt, sv.SetPrompt = "", false
	} else if change.Prompt != nil {
		prompt := strings.TrimSpace(*change.Prompt)
		// The prompt is appended to every fix session's instructions, so a
		// runaway one is a runaway cost on every pull request in the repository.
		if len(prompt) > 4000 {
			return sv, errors.New("extra instructions are limited to 4000 characters — they are appended to every fix session")
		}
		sv.Prompt, sv.SetPrompt = prompt, true
	}
	if change.MaxAttempts != nil {
		n := *change.MaxAttempts
		if n < 0 || n > 20 {
			return sv, errors.New("max attempts must be between 0 and 20")
		}
		if n == 0 {
			sv.MaxAttempts = nil // back to inherited
		} else {
			sv.MaxAttempts = &n
		}
	}
	if change.UnsetSeverities {
		sv.Severities, sv.SetSeverities = nil, false
	} else if change.Severities != nil {
		if len(change.Severities) == 0 {
			return sv, errors.New("choose at least one severity, or turn autofix off for the repository")
		}
		seen := map[string]bool{}
		severities := make([]string, 0, len(change.Severities))
		for _, severity := range change.Severities {
			severity = strings.ToLower(strings.TrimSpace(severity))
			if !dialect.IsSeverity(severity) {
				return sv, fmt.Errorf("severity %q is not one of %s", severity, strings.Join(dialect.KnownSeverities(), ", "))
			}
			if !seen[severity] {
				seen[severity] = true
				severities = append(severities, severity)
			}
		}
		sv.Severities, sv.SetSeverities = severities, true
	}
	if change.UnsetAskMode {
		sv.AskMode, sv.SetAskMode = "", false
	} else if change.AskMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*change.AskMode))
		if !containsString(knownAskModes, mode) {
			return sv, fmt.Errorf("ask mode %q is not one of %s", mode, strings.Join(knownAskModes, ", "))
		}
		sv.AskMode, sv.SetAskMode = mode, true
	}
	if change.UnsetForks {
		sv.Forks = nil
	} else if change.Forks != nil {
		forks := *change.Forks
		sv.Forks = &forks
	}
	if change.UnsetSkipAuthors {
		sv.SkipAuthors, sv.SetSkipAuthors = nil, false
	} else if change.SkipAuthors != nil {
		authors := make([]string, 0, len(change.SkipAuthors))
		for _, a := range change.SkipAuthors {
			if a = strings.TrimSpace(a); a != "" {
				authors = append(authors, a)
			}
		}
		sort.Strings(authors)
		sv.SkipAuthors, sv.SetSkipAuthors = authors, true
	}
	if change.UnsetOnePass {
		sv.OnePass, sv.SetOnePass, sv.OnePassCampaign = false, false, ""
	} else if change.OnePass != nil {
		sv.OnePass, sv.SetOnePass = *change.OnePass, true
		if !sv.OnePass {
			sv.OnePassCampaign = ""
		}
	}
	if change.UnsetMerge {
		sv.MergeMethod, sv.SetMerge = "", false
	} else if change.MergeMethod != nil {
		method := strings.ToLower(strings.TrimSpace(*change.MergeMethod))
		if method == "off" {
			method = ""
		}
		switch method {
		case "", "merge", "squash", "rebase":
			sv.MergeMethod, sv.SetMerge = method, true
		default:
			return sv, fmt.Errorf("merge method %q is not one of off, merge, squash, rebase", method)
		}
	}
	return sv, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SolverIn answers for an already-loaded state, so a caller rendering many
// repositories does not re-read the ref once per row.
func (s *Service) SolverIn(st State, repo string) SolverView {
	return s.solverViewOf(st, NormalizeRepo(repo))
}
