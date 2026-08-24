package engine

import (
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
)

// BlockingFindings identifies feedback that can still be acted on or resolved
// before requesting a review of head. Unresolved threads remain actionable
// across commits, while thread-less review-body/prompt findings from an older
// commit cannot be resolved on GitHub — those are superseded by the next
// current-head review and must not deadlock the loop.
func BlockingFindings(findings []dialect.Finding, head string) []dialect.Finding {
	blocking := make([]dialect.Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.ThreadID != "" || finding.Commit == "" || head == "" || dialect.SHAPrefixMatch(finding.Commit, head) {
			blocking = append(blocking, finding)
		}
	}
	return blocking
}

// FindingsOnHead excludes carried review artifacts from older commits — the
// narrower filter for deciding whether visible feedback belongs to the
// current head at all.
func FindingsOnHead(findings []dialect.Finding, head string) []dialect.Finding {
	current := make([]dialect.Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Commit == "" || head == "" || dialect.SHAPrefixMatch(finding.Commit, head) {
			current = append(current, finding)
		}
	}
	return current
}

// FindingsForActiveRound returns feedback that should interrupt an active slot
// wait. In addition to findings attached to the current head, it includes
// delayed review feedback that arrived after this round was enqueued even when
// the reviewer attached it to the previous commit. Pre-existing carried review
// bodies remain excluded so they cannot prevent a replacement review forever.
func FindingsForActiveRound(findings []dialect.Finding, head string, enqueuedAt time.Time) []dialect.Finding {
	current := make([]dialect.Finding, 0, len(findings))
	for _, finding := range findings {
		onHead := finding.Commit == "" || head == "" || dialect.SHAPrefixMatch(finding.Commit, head)
		arrivedDuringRound := !enqueuedAt.IsZero() &&
			!finding.CreatedAt.IsZero() &&
			!finding.CreatedAt.Before(enqueuedAt)
		if onHead || arrivedDuringRound {
			current = append(current, finding)
		}
	}
	return current
}

// Converged reports the loop's terminal condition: no findings and every
// required bot reviewed.
func Converged(findings []dialect.Finding, completion CompletionStatus) bool {
	return len(findings) == 0 && completion.Done
}
