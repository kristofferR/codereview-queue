package dialect

import "strings"

// A bot replies to the agent's decline of a review finding with one of two
// verdicts: it withdraws the finding (concedes) or retains it (contests). crq
// must read that reply — a contested rebuttal on a thread the agent already
// resolved would otherwise be silently dropped, and the agent would never learn
// the reviewer stood its ground. These classifiers own that wording; the
// surfacing decision lives in crq/feedback. The golden corpus pins CodeRabbit's
// real phrasing from testdata/coderabbit/reply-*.md.

// reviewWithdrawnMarker is CodeRabbit's machine-readable concession marker. It
// beats every prose heuristic below: the wording of a concession varies freely
// ("you're right—thank you for the precise correction"), and misreading one as
// a rebuttal re-surfaces a settled finding and blocks convergence.
const reviewWithdrawnMarker = "<review_comment_withdrawn>"

// ReplyVerdict is what a bot's reply to a declined finding amounts to. It exists
// so the CLASSIFICATION and the wording that describes it stay together: the
// caller orchestrates, and does not decide how a verdict reads.
type ReplyVerdict int

const (
	// ReplyUnclear is neither a withdrawal nor a stated rebuttal. It is still
	// surfaced — a buried rebuttal is the worse failure — but it must not be
	// announced as a contest, which is what made an agent re-address a rebuttal
	// that did not exist.
	ReplyUnclear ReplyVerdict = iota
	// ReplyWithdrawn concedes: the decline stands and the thread is done.
	ReplyWithdrawn
	// ReplyRetained contests: the finding stands and the agent must answer it.
	ReplyRetained
)

// ClassifyDeclineReply reads a bot's reply to a declined finding.
func ClassifyDeclineReply(text string) ReplyVerdict {
	switch {
	case IsReviewFindingWithdrawn(text):
		return ReplyWithdrawn
	case IsReviewFindingRetained(text):
		return ReplyRetained
	default:
		return ReplyUnclear
	}
}

// TitlePrefix is how a verdict introduces itself in a finding title. Only a
// stated rebuttal claims to contest; anything else asks the caller to read it.
func (v ReplyVerdict) TitlePrefix() string {
	switch v {
	case ReplyRetained:
		return "Reviewer contests your reply — re-address or reply again: "
	default:
		return "Reviewer replied after your decline — read it and confirm the decline stands: "
	}
}

// IsReviewFindingWithdrawn reports whether a bot's reply concedes and withdraws
// its finding — the agent's decline stands and the thread is done.
func IsReviewFindingWithdrawn(text string) bool {
	if strings.Contains(text, reviewWithdrawnMarker) {
		return true
	}
	t := NormalizeReviewText(text)
	return strings.Contains(t, "should be withdrawn") ||
		strings.Contains(t, "finding remains withdrawn") ||
		strings.Contains(t, "finding is withdrawn") ||
		strings.Contains(t, "withdrawing this") ||
		strings.Contains(t, "withdrawing the finding") ||
		strings.Contains(t, "withdrawing my") ||
		strings.Contains(t, "i'll withdraw") ||
		strings.Contains(t, "i will withdraw") ||
		strings.Contains(t, "finding was incorrect") ||
		strings.Contains(t, "my mistake") ||
		// Agreeing to drop the request concedes too: the bot accepted the
		// decline and answers every further reply with another acknowledgement.
		strings.Contains(t, "i won't request this change") ||
		strings.Contains(t, "i won't request that change")
}

// IsReviewFindingRetained reports whether a bot's reply retains or contests its
// finding despite the agent's decline — a rebuttal the agent must re-address.
func IsReviewFindingRetained(text string) bool {
	t := NormalizeReviewText(text)
	return strings.Contains(t, "retaining the finding") ||
		strings.Contains(t, "retaining this") ||
		strings.Contains(t, "keeping this finding") ||
		strings.Contains(t, "keeping the finding") ||
		strings.Contains(t, "i disagree") ||
		strings.Contains(t, "i'm not convinced") ||
		strings.Contains(t, "still stands") ||
		strings.Contains(t, "still applies") ||
		strings.Contains(t, "still holds")
}
