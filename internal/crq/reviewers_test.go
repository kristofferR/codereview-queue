package crq

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
)

// isolatedConfig loads config from an empty file so only the env vars a test sets
// are in play.
func isolatedConfig(t *testing.T, env map[string]string) Config {
	t.Helper()
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	for _, key := range []string{
		"CRQ_BOT", "CRQ_REQUIRED_BOTS", "CRQ_FEEDBACK_BOTS", "CRQ_COBOTS",
		"CRQ_COBOT_CODEX_TRIGGER", "CRQ_COBOT_BUGBOT_REQUIRED",
	} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Budget is the property the queue exists for, so it has to be right before
// anything is built on it: exactly one reviewer is account-metered, and it is the
// configured primary.
func TestReviewersRecordWhatEachOneCosts(t *testing.T) {
	cfg := isolatedConfig(t, nil)

	primary, ok := cfg.Primary()
	if !ok {
		t.Fatal("a primary must be configured by default")
	}
	if primary.Login != "coderabbitai[bot]" || !primary.Metered() {
		t.Errorf("primary = %+v, want the account-metered CodeRabbit", primary)
	}
	if !primary.Required {
		t.Error("the primary gates convergence: a round it paid for is not done until it answers")
	}

	metered := 0
	for _, r := range cfg.Reviewers {
		if r.Metered() {
			metered++
			continue
		}
		if r.Budget != dialect.BudgetNone {
			t.Errorf("%s has budget %q, want none", r.Login, r.Budget)
		}
	}
	if metered != 1 {
		t.Errorf("%d metered reviewers, want exactly 1 — the queue serializes one allowance", metered)
	}
	if len(cfg.FreeRunning())+metered != len(cfg.Reviewers) {
		t.Error("every reviewer is either metered or free-running")
	}
}

// The legacy lists are views of the one list now, so they cannot answer
// differently from it. These are the questions each used to answer separately.
func TestLegacyListsAreDerivedFromTheReviewers(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"defaults", nil},
		{"codex required", map[string]string{"CRQ_REQUIRED_BOTS": "coderabbitai[bot],chatgpt-codex-connector[bot]"}},
		{"bugbot required via its own key", map[string]string{"CRQ_COBOT_BUGBOT_REQUIRED": "true"}},
		{"co-reviewers disabled", map[string]string{"CRQ_COBOTS": ""}},
		{"single co-reviewer", map[string]string{"CRQ_COBOTS": "codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := isolatedConfig(t, tc.env)

			want := cfg.reviewerLogins(func(r Reviewer) bool { return r.Required })
			if len(want) != len(cfg.RequiredBots) {
				t.Fatalf("RequiredBots = %v, want %v", cfg.RequiredBots, want)
			}
			for i := range want {
				if cfg.RequiredBots[i] != want[i] {
					t.Fatalf("RequiredBots = %v, want %v", cfg.RequiredBots, want)
				}
			}
			// Every co-reviewer entry corresponds to a free-running reviewer.
			if len(cfg.CoBots) != len(cfg.FreeRunning()) {
				t.Errorf("%d co-bots but %d free-running reviewers", len(cfg.CoBots), len(cfg.FreeRunning()))
			}
			// The primary is never in the co-reviewer set: that is what makes it
			// the one the fire slot serializes.
			primary, _ := cfg.Primary()
			for _, r := range cfg.FreeRunning() {
				if r.Login == primary.Login {
					t.Errorf("the metered primary must not appear as free-running: %s", r.Login)
				}
			}
		})
	}
}

// Feedback bots are the one list an operator may widen beyond who reviews, to
// surface a bot's findings without waiting for it — so an explicit setting still
// wins over the derivation.
func TestExplicitFeedbackBotsSurviveTheDerivation(t *testing.T) {
	cfg := isolatedConfig(t, map[string]string{"CRQ_FEEDBACK_BOTS": "someone[bot]"})
	if len(cfg.FeedbackBots) != 1 || cfg.FeedbackBots[0] != "someone[bot]" {
		t.Errorf("FeedbackBots = %v, want the explicit setting to win", cfg.FeedbackBots)
	}

	// With none set, it covers everyone who reviews.
	cfg = isolatedConfig(t, nil)
	if len(cfg.FeedbackBots) != len(cfg.Reviewers) {
		t.Errorf("FeedbackBots = %v, want one per reviewer (%d)", cfg.FeedbackBots, len(cfg.Reviewers))
	}
}

// The derivation has to describe the configuration that exists, not a tidier one.
// Each row here was a silent behaviour change found in review, hiding inside a
// change that claimed to be a pure refactor.
func TestDerivationLosesNothing(t *testing.T) {
	t.Run("a required bot outside the registry still gates", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_REQUIRED_BOTS": "coderabbitai[bot],sonar[bot]",
		})
		found := false
		for _, login := range cfg.RequiredBots {
			if login == "sonar[bot]" {
				found = true
			}
		}
		if !found {
			t.Errorf("RequiredBots = %v, want sonar[bot] retained — dropping it stops gating a reviewer the operator asked to wait for", cfg.RequiredBots)
		}
	})

	t.Run("the primary is not required unless it is listed", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_REQUIRED_BOTS": "chatgpt-codex-connector[bot]",
		})
		primary, _ := cfg.Primary()
		if primary.Required {
			t.Error("an explicit required list that omits the primary must be honoured")
		}
		for _, login := range cfg.RequiredBots {
			if login == primary.Login {
				t.Errorf("RequiredBots = %v, want the primary excluded", cfg.RequiredBots)
			}
		}
	})

	t.Run("a primary that is also a registry bot appears once", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{"CRQ_BOT": "chatgpt-codex-connector[bot]"})
		seen := 0
		for _, r := range cfg.Reviewers {
			if r.Login == "chatgpt-codex-connector[bot]" {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("codex appears %d times; it cannot be both metered and free-running", seen)
		}
	})

	t.Run("a disabled registry primary does not return as a co-reviewer", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_BOT":           "chatgpt-codex-connector[bot]",
			"CRQ_REQUIRED_BOTS": "chatgpt-codex-connector[bot],cursor[bot]",
		})
		got := cfg.ForRepo(RepoReviewers{PrimaryOff: true})
		if _, ok := got.Primary(); ok {
			t.Fatal("Primary() found a registry-backed primary after the repository disabled it")
		}
		for _, reviewer := range got.Reviewers {
			if sameBot(reviewer.Login, cfg.Bot) {
				t.Fatalf("reviewers = %+v, want the disabled primary absent rather than rebuilt as a co-reviewer", got.Reviewers)
			}
		}
		if !hasLogin(got.RequiredBots, "cursor[bot]") {
			t.Fatalf("required = %v, want the remaining co-reviewer to keep gating", got.RequiredBots)
		}
	})

	t.Run("a primary that is also a registry bot is asked once", func(t *testing.T) {
		// Appearing once in Reviewers is not enough: CoBots still drives the
		// co-reviewer trigger post, so an entry left there means DecideFire posts
		// the review command and fireCoOnly posts the co-reviewer command — the
		// same reviewer asked twice. Pointing crq at Codex as the primary is a
		// real configuration, so this is a live double-post, not a hypothetical.
		cfg := isolatedConfig(t, map[string]string{"CRQ_BOT": "chatgpt-codex-connector[bot]"})
		found := false
		for _, cb := range cfg.CoBots {
			if dialect.NormalizeBotName(cb.Login) != dialect.NormalizeBotName(cfg.Bot) {
				continue
			}
			found = true
			if cb.Trigger != engine.TriggerNever {
				t.Errorf("the primary's co-trigger is %q, so crq would ask it twice", cb.Trigger)
			}
		}
		// The entry must SURVIVE, silenced: it is where this bot's wording and
		// check-run hooks come from, and the primary needs those to be believed.
		if !found {
			t.Error("removing the primary from CoBots costs it its registry evidence hooks")
		}
		// It is still a reviewer, and still the metered one.
		if primary, ok := cfg.Primary(); !ok || primary.Login != cfg.Bot {
			t.Errorf("Primary() = %+v ok=%v, want the configured primary", primary, ok)
		}
	})

	t.Run("an optional primary still contributes findings", func(t *testing.T) {
		// Leaving the primary out of CRQ_REQUIRED_BOTS says not to wait for it.
		// It does not make findings from a review that DID run disappear.
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_REQUIRED_BOTS": "chatgpt-codex-connector[bot]",
		})
		found := false
		for _, login := range cfg.FeedbackBots {
			if login == cfg.Bot {
				found = true
			}
		}
		if !found {
			t.Errorf("FeedbackBots = %v, want the optional primary surfaced", cfg.FeedbackBots)
		}
	})

	t.Run("the primary knows how to be triggered", func(t *testing.T) {
		cfg := isolatedConfig(t, nil)
		primary, _ := cfg.Primary()
		if primary.Command != cfg.ReviewCommand || primary.Command == "" {
			t.Errorf("primary command = %q, want the resolved trigger %q — an empty one means crq cannot ask it at all",
				primary.Command, cfg.ReviewCommand)
		}
	})
}

// Per-repo reviewers answer the request crq exists to make easy: which bots run
// on which project. The override has to reach the DERIVED views too, or crq
// would still gate on, and surface findings from, a bot this repository excluded.
func TestForRepoOverridesEveryDerivedView(t *testing.T) {
	// isolatedConfig blanks CRQ_COBOTS, so ask for the shipped default explicitly.
	fleet := isolatedConfig(t, map[string]string{"CRQ_COBOTS": "codex,bugbot,macroscope"})
	if len(fleet.CoBots) < 2 {
		t.Fatalf("this test needs several co-reviewers by default, got %d", len(fleet.CoBots))
	}

	t.Run("no override changes nothing", func(t *testing.T) {
		if got := fleet.ForRepo(RepoReviewers{}); len(got.Reviewers) != len(fleet.Reviewers) {
			t.Errorf("reviewers = %d, want the fleet's %d", len(got.Reviewers), len(fleet.Reviewers))
		}
	})

	t.Run("only the chosen co-reviewer runs", func(t *testing.T) {
		only := fleet.ForRepo(RepoReviewers{
			CoBots: []string{dialect.CodexBotLogin}, SetCoBots: true,
			Required: []string{dialect.CodexBotLogin}, SetRequired: true,
		})
		if len(only.CoBots) != 1 || dialect.NormalizeBotName(only.CoBots[0].Login) != dialect.NormalizeBotName(dialect.CodexBotLogin) {
			t.Fatalf("CoBots = %+v, want only codex", only.CoBots)
		}
		// The derived views must agree, or crq gates on a bot this repo excluded.
		if len(only.RequiredBots) != 1 || dialect.NormalizeBotName(only.RequiredBots[0]) != dialect.NormalizeBotName(dialect.CodexBotLogin) {
			t.Errorf("RequiredBots = %v, want only codex", only.RequiredBots)
		}
		for _, login := range only.FeedbackBots {
			if dialect.NormalizeBotName(login) == "cursor" {
				t.Errorf("FeedbackBots = %v still surfaces an excluded bot", only.FeedbackBots)
			}
		}
		// The primary is fleet-wide and still configured, just not gating here.
		primary, ok := only.Primary()
		if !ok || primary.Login != fleet.Bot {
			t.Errorf("primary = %+v ok=%v, want it kept", primary, ok)
		}
	})

	t.Run("chosen to be none is not the same as unset", func(t *testing.T) {
		none := fleet.ForRepo(RepoReviewers{CoBots: []string{}, SetCoBots: true})
		if len(none.CoBots) != 0 {
			t.Errorf("CoBots = %+v, want none — an empty choice must survive", none.CoBots)
		}
		if _, ok := none.Primary(); !ok {
			t.Error("excluding every co-reviewer must not remove the primary")
		}
	})
}

// Deciding and committing are two steps, and BOTH layers that name reviewers
// can move between them. The repository override was already revalidated inside
// the CAS; the fleet record was not, so an operator changing the fleet primary
// or its co-reviewers could have their save report success and still be
// overtaken by an in-flight decision posting the commands they had just
// replaced.
func TestAFleetReviewerChangeInvalidatesADecisionInFlight(t *testing.T) {
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	st := &State{}
	cfg := firingConfig().WithFleet(st.Fleet)
	if reviewersChanged(st, "o/repo", cfg) {
		t.Fatal("nothing has been recorded, so nothing has changed")
	}

	st.SetFleetDefaults(FleetDefaults{CoBots: []string{"codex"}, SetCoBots: true}, "atlas", now)
	if !reviewersChanged(st, "o/repo", cfg) {
		t.Error("a fleet co-reviewer change must invalidate a decision made before it")
	}
	// And the configuration built from the new record commits against it.
	if reviewersChanged(st, "o/repo", firingConfig().WithFleet(st.Fleet)) {
		t.Error("the decision made from the current record must still commit")
	}
}
