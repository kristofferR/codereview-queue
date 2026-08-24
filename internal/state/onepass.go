package state

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// OnePassProgress is the durable hand-off between an unattended fixer and the
// watcher that merges its result. ReadyHead is exact: any later push invalidates
// the hand-off and requires a fresh fixer session, but never another review.
type OnePassProgress struct {
	AttemptHead string     `json:"attempt_head,omitempty"`
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	ReadyHead   string     `json:"ready_head,omitempty"`
	// ReadyBase is the base revision the finalizer demonstrably integrated.
	// ReadyHead alone is insufficient: GitHub can later report the same head
	// conflict-free against a different base the agent never inspected.
	ReadyBase string `json:"ready_base,omitempty"`
	// VerificationPending means ReadyBase was the last base proven from the
	// checkout after the fixer succeeded, but GitHub's post-session pull read
	// failed. The exact-head hand-off remains retryable without launching a
	// second fixer; merging waits until GitHub reports this same base again.
	VerificationPending bool       `json:"verification_pending,omitempty"`
	ReadyAt             *time.Time `json:"ready_at,omitempty"`
	By                  string     `json:"by,omitempty"`

	unknown unknownFields
}

// OnePassEvidence is the campaign-scoped record of which reviewers may consume
// the single review and which pull requests have already spent it. Reviewer
// identities are append-only for the life of a campaign: replacing a reviewer
// must not make its completed review disappear and reopen the PR for another.
type OnePassEvidence struct {
	Campaign  string               `json:"campaign"`
	Reviewers []string             `json:"reviewers,omitempty"`
	Reviewed  map[string]time.Time `json:"reviewed,omitempty"`

	unknown unknownFields
}

// BeginOnePassCampaign resets review evidence for a new off-to-on transition.
func (s *State) BeginOnePassCampaign(repo, campaign string, reviewers []string) {
	if s.OnePassEvidence == nil {
		s.OnePassEvidence = map[string]OnePassEvidence{}
	}
	evidence := OnePassEvidence{Campaign: campaign}
	s.OnePassEvidence[normalizeRepoKey(repo)] = evidence
	s.RememberOnePassReviewers(repo, campaign, reviewers)
}

// RememberOnePassReviewers extends the campaign's reviewer identity set. It
// never removes identities until the campaign ends, because an answer from a
// reviewer removed moments later still consumed the campaign's only round.
func (s *State) RememberOnePassReviewers(repo, campaign string, reviewers []string) bool {
	if campaign == "" {
		return false
	}
	if s.OnePassEvidence == nil {
		s.OnePassEvidence = map[string]OnePassEvidence{}
	}
	key := normalizeRepoKey(repo)
	evidence, ok := s.OnePassEvidence[key]
	sameCampaign := ok && evidence.Campaign == campaign
	if !sameCampaign {
		evidence = OnePassEvidence{Campaign: campaign}
	}
	seen := make(map[string]bool, len(evidence.Reviewers)+len(reviewers))
	for _, login := range evidence.Reviewers {
		login = coBotKey(login)
		if login != "" {
			seen[login] = true
		}
	}
	changed := !sameCampaign
	for _, login := range reviewers {
		login = coBotKey(login)
		if login == "" || seen[login] {
			continue
		}
		seen[login] = true
		evidence.Reviewers = append(evidence.Reviewers, login)
		changed = true
	}
	if !changed {
		return false
	}
	sort.Strings(evidence.Reviewers)
	s.OnePassEvidence[key] = evidence
	return true
}

// OnePassReviewersFor returns every reviewer identity that has belonged to the
// campaign. A different campaign never inherits the previous one's scope.
func (s *State) OnePassReviewersFor(repo, campaign string) []string {
	evidence, ok := s.OnePassEvidence[normalizeRepoKey(repo)]
	if !ok || evidence.Campaign != campaign {
		return nil
	}
	return append([]string(nil), evidence.Reviewers...)
}

// MarkOnePassReviewed durably consumes the campaign's review for this PR.
func (s *State) MarkOnePassReviewed(repo string, pr int, campaign string, now time.Time) bool {
	if campaign == "" {
		return false
	}
	key := normalizeRepoKey(repo)
	evidence, ok := s.OnePassEvidence[key]
	if !ok || evidence.Campaign != campaign {
		evidence = OnePassEvidence{Campaign: campaign}
	}
	if evidence.Reviewed == nil {
		evidence.Reviewed = map[string]time.Time{}
	}
	prKey := strconv.Itoa(pr)
	if !evidence.Reviewed[prKey].IsZero() {
		return false
	}
	evidence.Reviewed[prKey] = now.UTC()
	if s.OnePassEvidence == nil {
		s.OnePassEvidence = map[string]OnePassEvidence{}
	}
	s.OnePassEvidence[key] = evidence
	return true
}

// OnePassReviewed reports whether this PR already consumed the named
// campaign's single review.
func (s *State) OnePassReviewed(repo string, pr int, campaign string) bool {
	evidence, ok := s.OnePassEvidence[normalizeRepoKey(repo)]
	return ok && evidence.Campaign == campaign && !evidence.Reviewed[strconv.Itoa(pr)].IsZero()
}

// OnePassReviewerAnswered reports durable completed-review evidence from any
// identity that has belonged to this campaign. It includes primary and
// co-reviewer answers in current and archived rounds, plus the unbounded
// co-reviewer answer index.
func (s *State) OnePassReviewerAnswered(repo string, pr int, campaign string) bool {
	allowed := map[string]bool{}
	for _, login := range s.OnePassReviewersFor(repo, campaign) {
		allowed[coBotKey(login)] = true
	}
	if len(allowed) == 0 {
		return false
	}
	key := Key(repo, pr)
	for login, answered := range s.CoAnswers[key] {
		if allowed[coBotKey(login)] && !answered.IsZero() {
			return true
		}
	}
	answeredRound := func(round Round) bool {
		if round.PrimaryAnsweredAt != nil && allowed[coBotKey(round.PrimaryAnsweredBy)] {
			return true
		}
		for login, co := range round.CoBots {
			if allowed[coBotKey(login)] && co.AnsweredAt != nil {
				return true
			}
		}
		return false
	}
	if round := s.Round(repo, pr); round != nil && answeredRound(*round) {
		return true
	}
	for i := range s.Archive {
		if Key(s.Archive[i].Repo, s.Archive[i].PR) == key && answeredRound(s.Archive[i]) {
			return true
		}
	}
	return false
}

// OnePassReady reports whether a successful fixer released this exact head for
// merge. A short stored SHA is accepted only as a prefix of the full GitHub SHA.
func (s *State) OnePassReady(repo string, pr int, head string) bool {
	p, ok := s.OnePass[Key(repo, pr)]
	return ok && p.ReadyHead != "" && head != "" &&
		(strings.HasPrefix(head, p.ReadyHead) || strings.HasPrefix(p.ReadyHead, head))
}

// OnePassReadyOn reports whether both sides of the finalized combination still
// match the latest observation. GitHub's merge API atomically binds the merge
// to ReadyHead but exposes no expected-base field, so this rejects an already
// advanced base while a base push racing the final request is included in
// GitHub's server-side merge transaction.
func (s *State) OnePassReadyOn(repo string, pr int, head, base string) bool {
	p, ok := s.OnePass[Key(repo, pr)]
	return ok && s.OnePassReady(repo, pr, head) && p.ReadyBase != "" && base != "" &&
		(strings.HasPrefix(base, p.ReadyBase) || strings.HasPrefix(p.ReadyBase, base))
}

// OnePassProgressFor returns the campaign hand-off, when a fixer has already
// completed for this pull request.
func (s *State) OnePassProgressFor(repo string, pr int) (OnePassProgress, bool) {
	p, ok := s.OnePass[Key(repo, pr)]
	return p, ok
}

// MarkOnePassReady records the head a successful fixer actually left on the PR.
func (s *State) MarkOnePassReady(repo string, pr int, head, base, by string, now time.Time) {
	s.markOnePassReady(repo, pr, head, base, by, now, false)
}

// MarkOnePassVerificationPending preserves a successful fixer hand-off when
// the post-session GitHub read is temporarily unavailable. It spends the sole
// fixer attempt while allowing the merge gate, not another agent, to retry.
func (s *State) MarkOnePassVerificationPending(repo string, pr int, head, base, by string, now time.Time) {
	s.markOnePassReady(repo, pr, head, base, by, now, true)
}

func (s *State) markOnePassReady(repo string, pr int, head, base, by string, now time.Time, verificationPending bool) {
	if s.OnePass == nil {
		s.OnePass = map[string]OnePassProgress{}
	}
	key := Key(repo, pr)
	at := now.UTC()
	next := OnePassProgress{
		AttemptHead: head, AttemptedAt: &at,
		ReadyHead: head, ReadyBase: base, VerificationPending: verificationPending,
		ReadyAt: &at, By: by,
	}
	if prev, ok := s.OnePass[key]; ok {
		if prev.AttemptHead != "" {
			next.AttemptHead, next.AttemptedAt = prev.AttemptHead, prev.AttemptedAt
		}
		next.unknown = carryUnknown(next.unknown, prev.unknown)
	}
	s.OnePass[key] = next
}

// InvalidateClosedOnePass converts every ready hand-off for a PR absent from
// the repository's authoritative open list into a terminal attempted record.
// Reopening the same head must not resurrect pre-closure merge authorization;
// a person has to start a new campaign to make that PR eligible again.
func (s *State) InvalidateClosedOnePass(repo string, open map[int]bool, by string, now time.Time) []int {
	prefix := normalizeRepoKey(repo) + "#"
	var closed []int
	for key, progress := range s.OnePass {
		if !strings.HasPrefix(key, prefix) || progress.ReadyHead == "" {
			continue
		}
		pr, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || pr <= 0 || open[pr] {
			continue
		}
		head := progress.AttemptHead
		if head == "" {
			head = progress.ReadyHead
		}
		s.MarkOnePassAttempted(repo, pr, head, by, now)
		closed = append(closed, pr)
	}
	sort.Ints(closed)
	return closed
}

// MarkOnePassAttempted records that the campaign's single fixer session ran
// but did not release a mergeable head. Provider outages and watcher shutdowns
// do not call this method, so they remain retryable without spending the one
// real code-fix attempt.
func (s *State) MarkOnePassAttempted(repo string, pr int, head, by string, now time.Time) {
	if s.OnePass == nil {
		s.OnePass = map[string]OnePassProgress{}
	}
	key := Key(repo, pr)
	at := now.UTC()
	next := OnePassProgress{AttemptHead: head, AttemptedAt: &at, By: by}
	if prev, ok := s.OnePass[key]; ok {
		next.unknown = carryUnknown(next.unknown, prev.unknown)
	}
	s.OnePass[key] = next
}

// ClearOnePassProgress makes the PR eligible for another fixer session while
// retaining the one-review cap. It is used when the ready head changed, became
// conflicted, or failed checks before merge.
func (s *State) ClearOnePassProgress(repo string, pr int) bool {
	key := Key(repo, pr)
	if _, ok := s.OnePass[key]; !ok {
		return false
	}
	delete(s.OnePass, key)
	return true
}

// ClearOnePassRepo removes temporary campaign hand-offs and review evidence
// when the repository returns to its ordinary solver policy.
func (s *State) ClearOnePassRepo(repo string) bool {
	prefix := normalizeRepoKey(repo) + "#"
	changed := false
	for key := range s.OnePass {
		if strings.HasPrefix(key, prefix) {
			delete(s.OnePass, key)
			changed = true
		}
	}
	key := normalizeRepoKey(repo)
	if _, ok := s.OnePassEvidence[key]; ok {
		delete(s.OnePassEvidence, key)
		changed = true
	}
	return changed
}
