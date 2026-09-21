package crq

import (
	"context"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Only a finding from the latest review of this head needs confirmation.
// Resolved older reviews must not invalidate a subsequent clean review.
func resolvedLatestReview(thread reviewThread, latest map[string]ghapi.Review, reviews []ghapi.Review, head string) bool {
	if !thread.IsResolved || thread.IsOutdated || head == "" {
		return false
	}
	nodes := thread.Comments.Nodes
	if len(nodes) == 0 {
		return false
	}
	first := nodes[0]
	review, ok := latest[dialect.NormalizeBotName(first.Author.Login)]
	if !ok || review.ID == 0 ||
		!dialect.SHAPrefixMatch(review.CommitID, head) {
		return false
	}
	if first.PullRequestReview.DatabaseID != review.ID && !carrierForReview(first.PullRequestReview.DatabaseID, review, reviews) {
		return false
	}
	// An explicit withdrawal by the same reviewer is already confirmation.
	if len(nodes) > 1 && thread.Comments.TotalCount <= len(nodes) {
		last := nodes[len(nodes)-1]
		if dialect.NormalizeBotName(last.Author.Login) == dialect.NormalizeBotName(first.Author.Login) &&
			dialect.ClassifyDeclineReply(last.Body) == dialect.ReplyWithdrawn {
			return false
		}
	}
	return true
}

// Some reviewers post inline batches in empty COMMENTED carrier reviews before
// publishing their substantive summary. Only carriers since the preceding real
// review belong to this verdict; older resolved findings have been re-reviewed.
func carrierForReview(id int64, latest ghapi.Review, reviews []ghapi.Review) bool {
	var carrier *ghapi.Review
	for _, review := range reviews {
		if review.ID == id {
			carrier = &review
			break
		}
	}
	if carrier == nil || carrier.State != "COMMENTED" || carrier.Body != "" ||
		carrier.CommitID != latest.CommitID || dialect.NormalizeBotName(carrier.User.Login) != dialect.NormalizeBotName(latest.User.Login) ||
		!reviewNewer(latest, *carrier) {
		return false
	}
	for _, review := range reviews {
		if dialect.NormalizeBotName(review.User.Login) == dialect.NormalizeBotName(latest.User.Login) &&
			review.ID != latest.ID && review.State != "PENDING" && !(review.State == "COMMENTED" && review.Body == "") &&
			reviewNewer(review, *carrier) && reviewNewer(latest, review) {
			return false
		}
	}
	return true
}

// queueConfirmation retains the old round in the archive and records one new
// evidence window. CAS plus ConfirmationAfter make concurrent/repeated callers
// idempotent. Subsequent decisions still use the normal fire owner and quota.
func (s *Service) queueConfirmation(ctx context.Context, feedback FeedbackReport) (bool, error) {
	if s.cfg.DryRun || feedback.PrimaryAckPending {
		return false, nil
	}
	head, open, _, err := s.pullHead(ctx, feedback.Repo, feedback.PR)
	if err != nil || !open || head != feedback.Head {
		return false, err
	}
	now := s.clock().UTC()
	queued := false
	_, err = s.store.Update(ctx, func(st *State) error {
		queued = false
		r := st.Round(feedback.Repo, feedback.PR)
		if (r != nil && (r.Head != feedback.Head || r.Seq != feedback.confirmationRoundSeq || r.ConfirmationAfter != nil)) ||
			(r == nil && feedback.confirmationRoundSeq != 0) ||
			reviewersChanged(st, feedback.Repo, feedback.config) || s.cfgFor(*st, feedback.Repo).OnePass {
			return ErrNoChange
		}
		if _, held := st.HeldPR(feedback.Repo, feedback.PR); held {
			return ErrNoChange
		}
		// Retire only this observed round's acknowledged fire, in the same CAS
		// as its replacement. A stale caller must never complete the new pass.
		if st.FireSlot != nil && st.FireSlot.Key == QueueKey(feedback.Repo, feedback.PR) {
			if r == nil || st.FireSlot.Token != r.Token || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
				return ErrNoChange
			}
			releaseSlot(st, QueueKey(feedback.Repo, feedback.PR), r.Token)
		}
		fresh, err := st.Supersede(feedback.Repo, feedback.PR, head, now)
		if err != nil {
			return err
		}
		fresh.ConfirmationAfter = &now
		fresh.LastAttemptAt = &now
		fresh.Note = "confirmation after findings were resolved or dismissed without changing the head"
		for _, cb := range feedback.config.CoBots {
			fresh.ForceCoReviewers = append(fresh.ForceCoReviewers, cb.Login)
		}
		st.PutRound(*fresh)
		queued = true
		return nil
	})
	return queued && err == nil, err
}

func (s *Service) confirmationBlocked(ctx context.Context, report FeedbackReport) (bool, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return false, err
	}
	r := st.Round(report.Repo, report.PR)
	return r != nil && r.Head == report.Head && r.ConfirmationAfter != nil, nil
}

func (s *Service) confirmationReady(report FeedbackReport) bool {
	until := s.settleUntil(report)
	return report.confirmationRequired && !report.config.OnePass &&
		len(report.Findings) == 0 && allReviewed(report.ReviewedBy) && !report.PrimaryAckPending &&
		(until == nil || !s.clock().Before(*until))
}

func confirmationCutoff(round *Round, head string) time.Time {
	if round != nil && round.Head == head && round.ConfirmationAfter != nil {
		return round.ConfirmationAfter.UTC()
	}
	return time.Time{}
}

func confirmationFresh(at, cutoff time.Time) bool {
	return cutoff.IsZero() || (!at.IsZero() && !at.Before(cutoff))
}

func confirmationEvent(ev dialect.BotEvent, cutoff time.Time) bool {
	// Account/plan limitations still apply even when their notices predate the
	// confirmation. They are not evidence that the new review completed.
	return confirmationFresh(ev.ObservedTime(), cutoff) || ev.SummaryOnly ||
		ev.Kind == dialect.EvRateLimited || ev.Kind == dialect.EvSkipped
}

// Confirmation commands must not adopt an old command for the same SHA.
func confirmationCommandFloor(round *Round, cutoff time.Time) time.Time {
	if round != nil && round.ConfirmationAfter != nil && round.ConfirmationAfter.After(cutoff) {
		return round.ConfirmationAfter.UTC()
	}
	return cutoff
}
