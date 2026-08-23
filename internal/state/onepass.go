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

// ClearOnePassRepo removes temporary campaign hand-offs when the repository
// returns to its ordinary solver policy.
func (s *State) ClearOnePassRepo(repo string) bool {
	prefix := normalizeRepoKey(repo) + "#"
	changed := false
	for key := range s.OnePass {
		if strings.HasPrefix(key, prefix) {
			delete(s.OnePass, key)
			changed = true
		}
	}
	return changed
}
