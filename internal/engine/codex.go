package engine

import (
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

// The Codex-specific quirks layered over the generic co-reviewer algebra in
// coreview.go. Everything bot-shape-generic lives there and is keyed by login;
// only what is genuinely true of Codex alone belongs here.

// codexBot is the Codex GitHub app login the engine flips in ReviewedBy when
// its thumbs-up quirk satisfies a gate. The dialect owns the literal and the
// normalization (CodexBotLogin/IsCodexBot); this consumes the constant.
const codexBot = dialect.CodexBotLogin

// CodexActiveThisRound is CoActiveThisRound plus Codex's thumbs-up quirk: a
// current +1 on the PR or on the fired command counts as participation. No
// other co-reviewer signals with a reaction.
func CodexActiveThisRound(r state.Round, obs Observation) bool {
	return CoActiveThisRound(r, obs, codexBot) || obs.CodexThumbsUp
}

// CodexOnlyEligible reports whether an account-blocked round may degrade to a
// Codex-only round. The rate-limit degrade is deliberately Codex-specific: it
// trades a blocked CodeRabbit round for Codex's review, and only Codex is
// modelled as a full stand-in reviewer (see Policy.RateLimitCoDegrade).
func CodexOnlyEligible(r state.Round, obs Observation, blockedUntil *time.Time, now time.Time) bool {
	return CoOnlyEligible(r, obs, codexBot, blockedUntil, now)
}
