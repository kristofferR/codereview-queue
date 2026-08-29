package dialect

import "strings"

// A co-reviewer is a review bot that is not the configured primary reviewer
// and does not spend CodeRabbit account quota: Codex, Cursor Bugbot,
// Macroscope. This file is the static registry of the co-reviewers crq knows —
// deliberately not a plugin system. Adding a bot means one entry here, its
// wording helpers in its own file, corpus files, and golden rows; engine/state/
// crq consume only the registry's structure, never the wording.

// checkRunFailed reports whether a completed check run's conclusion means the
// run did not produce a review. GitHub reports these alongside status
// "completed", so status alone cannot distinguish a finished review from a
// crashed one.
//
// "skipped" is deliberately absent: it is not universally a non-delivery.
// Macroscope's Correctness Check concludes `skipped` for the benign
// "No code objects were reviewed." outcome, which is a real clean verdict.
// Each bot decides what its own skip means.
func checkRunFailed(conclusion string) bool {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "cancelled", "canceled", "timed_out", "action_required",
		"stale", "startup_failure":
		return true
	}
	return false
}

// CheckVerdict classifies one check run for co-reviewer evidence purposes.
type CheckVerdict int

const (
	CheckUnrelated  CheckVerdict = iota // not this bot's review check
	CheckInProgress                     // review still running — evidence it is active, not done
	// CheckAuxiliary is one of this bot's checks that is NOT its review verdict
	// (Macroscope publishes Approvability and repo-custom checks beside its
	// Correctness Check). It proves participation, never completion: counting
	// it as a review lets a round converge while the real review is still
	// running and its findings have not landed.
	CheckAuxiliary
	// CheckFailed is a review run that terminated without producing a review
	// (conclusion failure/cancelled/timed_out/action_required). The bot tried
	// and did not deliver, so it is activity but never review evidence.
	CheckFailed
	// CheckUnable is a completed run whose own output says the bot could not
	// review this commit at all — Macroscope's billing-issue skip. Like
	// CheckFailed it is never review evidence, but unlike it a re-trigger cannot
	// help: the run IS the bot's answer. So it suppresses the self-heal trigger
	// (nudging a billing-blocked workspace every round is pure comment spam) and
	// disengages the dynamic completion gate, exactly as Codex's usage-limit
	// notice does on the comment side.
	CheckUnable
	CheckDone      // review finished (findings, if any, gate via threads)
	CheckDoneClean // review finished and explicitly found nothing
)

// CoEvent is the classification a co-reviewer gives one of its own issue
// comments: the dominant kind plus the fields specific kinds carry.
type CoEvent struct {
	Kind     EventKind
	SHA      string // EvCoClean/EvCoInProgress: reviewed or running commit SHA
	Approved *bool  // EvCoVerdict: approvability verdict
}

// CoReviewer is one registry entry: the bot's identity plus the wording
// hooks the rest of crq calls without knowing any bot's phrasing. Function
// fields are nil when the bot has no such concept.
// Budget is what a reviewer costs the account, which is the only reason crq
// queues anything at all. A reviewer whose reviews are metered account-wide must
// be serialized — one fire at a time, gated on the remaining quota — while one
// that costs nothing per review has no reason to wait behind anybody.
//
// It travels as registry data rather than as a name comparison, so the rules can
// ask what a reviewer costs instead of asking whether it is CodeRabbit.
type Budget string

const (
	// BudgetAccount: reviews draw on a shared account-wide allowance.
	BudgetAccount Budget = "account"
	// BudgetNone: reviews cost nothing crq has to ration.
	BudgetNone Budget = "none"
)

type CoReviewer struct {
	Login   string // GitHub login as REST reports it ("cursor[bot]")
	Name    string // config key ("bugbot") — CRQ_COBOT_<NAME>_* env vars
	AppSlug string // check-run app ownership; "" = bot posts no check runs
	Command string // default manual trigger comment; "" = bot has none
	// TriggerAliases are alternate command spellings recognized as this bot's
	// trigger in addition to the (config-resolved) Command.
	TriggerAliases []string
	// LegacyCommandEnv is a pre-registry env var still honoured for this bot's
	// command ("" = none). Carried here so config stays uniform instead of
	// naming individual bots.
	LegacyCommandEnv string
	// DefaultTrigger / RequiredTrigger are this bot's default trigger modes
	// when it is merely enabled, and when it is configured-required. Plain
	// strings because dialect has zero dependencies and cannot see
	// engine.TriggerMode; crq maps them. RequiredTrigger "" means "same as
	// DefaultTrigger".
	DefaultTrigger  string
	RequiredTrigger string
	// Budget is what this reviewer costs the shared account. Every co-reviewer
	// here is BudgetNone — that is what makes it a co-reviewer rather than the
	// metered primary — but it is stated rather than assumed so the queue rules
	// can read it off the registry.
	Budget Budget

	// ClassifyComment classifies an issue comment authored by this bot.
	// Kind EvOther means "no special meaning" (may still be actionable text).
	ClassifyComment func(body string) CoEvent
	// ClassifyCheck classifies a check run owned by this bot's app.
	ClassifyCheck func(name, title, summary, status, conclusion string) CheckVerdict
	// ReviewBodyFindings extracts findings only representable in a review body
	// (Codex blob-link items); nil for bots that keep findings in threads.
	ReviewBodyFindings func(body string, review ReviewMeta) []Finding
	// ReviewedCommitSHA extracts the commit the bot says it reviewed from a
	// review/comment body ("" when the text names none).
	ReviewedCommitSHA func(text string) string
	// ResolvedInSHA extracts the commit an edited finding comment says resolved
	// it ("" when the body carries no such marker).
	ResolvedInSHA func(body string) string
	// FindingDedupeKey extracts a bot-stable finding identity (Bugbot BUG_ID)
	// so the same bug re-reported in a new thread dedupes to one finding.
	FindingDedupeKey func(body string) (string, bool)
	// Price estimates what one review of this diff costs. Nil means the bot
	// charges nothing per review — it is covered by a subscription — which is
	// true of every co-reviewer here except Macroscope.
	Price func(d DiffStat) CostEstimate
}

// Is reports whether login names this co-reviewer, tolerating the "[bot]"
// suffix difference between REST and GraphQL logins.
func (c CoReviewer) Is(login string) bool {
	return NormalizeBotName(login) == NormalizeBotName(c.Login)
}

// matchesCommand reports whether a trimmed comment body is this bot's trigger
// command: the (config-resolved) Command or one of the registry's aliases.
func (c CoReviewer) matchesCommand(trimmed string) bool {
	if c.Command != "" && trimmed == c.Command {
		return true
	}
	for _, alias := range c.TriggerAliases {
		if trimmed == alias {
			return true
		}
	}
	return false
}

// KnownCoReviewers returns the registry entries with their default commands.
// Callers that resolve per-bot config (trigger command overrides) mutate their
// own copy — the returned slice is fresh on every call.
func KnownCoReviewers() []CoReviewer {
	return []CoReviewer{
		{
			Login:   CodexBotLogin,
			Name:    "codex",
			Command: "@codex review",
			Budget:  BudgetNone,
			// Codex predates the registry: its command env var is still read,
			// and it only ever triggered at fire time when required.
			LegacyCommandEnv: "CRQ_CODEX_CMD",
			DefaultTrigger:   "never",
			RequiredTrigger:  "always",
			ClassifyComment: func(body string) CoEvent {
				if IsCodexNoActionReviewCompletion(body) {
					return CoEvent{Kind: EvCoClean, SHA: CodexReviewedCommitSHA(body)}
				}
				if sha := CodexReviewInProgressSHA(body); sha != "" {
					return CoEvent{Kind: EvCoInProgress, SHA: sha}
				}
				switch {
				case IsCodexUsageLimit(body):
					// The usage-limit exhaustion notice is distinct from other acks:
					// the dynamic completion gate reads it to stop waiting on a Codex
					// that cannot finish this round.
					return CoEvent{Kind: EvCoUnable}
				case IsNonActionableText(body):
					return CoEvent{Kind: EvCoNotice}
				}
				return CoEvent{Kind: EvOther}
			},
			ReviewBodyFindings: func(body string, review ReviewMeta) []Finding {
				return ParseCodexReviewFindings(body, review, CodexBotLogin)
			},
			ReviewedCommitSHA: CodexReviewedCommitSHA,
		},
		{
			Login:          BugbotLogin,
			Name:           "bugbot",
			AppSlug:        "cursor",
			Command:        "bugbot run",
			Budget:         BudgetNone,
			TriggerAliases: []string{"bugbot run", "cursor review"},
			// Auto-reviews every push, so crq only nudges one that went silent.
			DefaultTrigger: "selfheal",
			// Bugbot posts no classifiable issue comments: its findings live in
			// review threads and its clean verdict only in the check run.
			ClassifyComment:   func(string) CoEvent { return CoEvent{Kind: EvOther} },
			ClassifyCheck:     ClassifyBugbotCheck,
			ReviewedCommitSHA: BugbotReviewedCommitSHA,
			FindingDedupeKey:  BugbotFindingDedupeKey,
		},
		{
			Login:           MacroscopeLogin,
			Name:            "macroscope",
			AppSlug:         "macroscopeapp",
			Command:         "@macroscope-app review",
			Budget:          BudgetNone,
			DefaultTrigger:  "selfheal",
			ClassifyComment: ClassifyMacroscopeComment,
			ClassifyCheck:   ClassifyMacroscopeCheck,
			ResolvedInSHA:   MacroscopeResolvedInSHA,
			// The one co-reviewer that bills per review rather than per seat.
			Price: EstimateMacroscope,
		},
	}
}

// CoReviewerByName resolves a registry entry by config name ("bugbot") or by
// login ("cursor[bot]", "cursor").
func CoReviewerByName(nameOrLogin string) (CoReviewer, bool) {
	for _, c := range KnownCoReviewers() {
		if c.Name == nameOrLogin || c.Is(nameOrLogin) {
			return c, true
		}
	}
	return CoReviewer{}, false
}

// ClassifyCheckRun dispatches one check run to the co-reviewer that owns it
// (by app slug) and returns that bot's login with its verdict. Checks owned by
// no known co-reviewer come back ("", CheckUnrelated).
func ClassifyCheckRun(appSlug, name, title, summary, status, conclusion string) (string, CheckVerdict) {
	for _, c := range KnownCoReviewers() {
		if c.AppSlug == "" || c.AppSlug != appSlug || c.ClassifyCheck == nil {
			continue
		}
		if v := c.ClassifyCheck(name, title, summary, status, conclusion); v != CheckUnrelated {
			return c.Login, v
		}
	}
	return "", CheckUnrelated
}
