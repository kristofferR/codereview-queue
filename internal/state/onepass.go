package state

import (
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
	ReadyAt     *time.Time `json:"ready_at,omitempty"`
	By          string     `json:"by,omitempty"`

	unknown unknownFields
}

// OnePassReady reports whether a successful fixer released this exact head for
// merge. A short stored SHA is accepted only as a prefix of the full GitHub SHA.
func (s *State) OnePassReady(repo string, pr int, head string) bool {
	p, ok := s.OnePass[Key(repo, pr)]
	return ok && p.ReadyHead != "" && head != "" &&
		(strings.HasPrefix(head, p.ReadyHead) || strings.HasPrefix(p.ReadyHead, head))
}

// OnePassProgressFor returns the campaign hand-off, when a fixer has already
// completed for this pull request.
func (s *State) OnePassProgressFor(repo string, pr int) (OnePassProgress, bool) {
	p, ok := s.OnePass[Key(repo, pr)]
	return p, ok
}

// MarkOnePassReady records the head a successful fixer actually left on the PR.
func (s *State) MarkOnePassReady(repo string, pr int, head, by string, now time.Time) {
	if s.OnePass == nil {
		s.OnePass = map[string]OnePassProgress{}
	}
	key := Key(repo, pr)
	at := now.UTC()
	next := OnePassProgress{
		AttemptHead: head, AttemptedAt: &at,
		ReadyHead: head, ReadyAt: &at, By: by,
	}
	if prev, ok := s.OnePass[key]; ok {
		if prev.AttemptHead != "" {
			next.AttemptHead, next.AttemptedAt = prev.AttemptHead, prev.AttemptedAt
		}
		next.unknown = carryUnknown(next.unknown, prev.unknown)
	}
	s.OnePass[key] = next
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
