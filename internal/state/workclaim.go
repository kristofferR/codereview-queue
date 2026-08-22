package state

import "time"

// WorkClaim is a short-lived PR-level lease held by an interactive review
// loop. It is deliberately independent of a Round and its head: the caller
// keeps owning the PR while it fixes, pushes, and waits for the next review.
//
// The autofix watcher checks this lease in the same CAS update that would grant
// a DispatchClaim. That makes "an agent is already working here" a shared fact
// instead of a race between two processes that both saw the same findings.
type WorkClaim struct {
	Owner     string    `json:"owner"`
	By        string    `json:"by,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`

	unknown unknownFields
}

// Live reports whether the interactive owner still excludes unattended work.
func (c WorkClaim) Live(now time.Time) bool {
	return c.Owner != "" && c.ExpiresAt.After(now.UTC())
}

// WorkClaim returns the live interactive claim for repo#pr.
func (s *State) WorkClaim(repo string, pr int, now time.Time) (WorkClaim, bool) {
	claim, ok := s.WorkClaims[Key(repo, pr)]
	return claim, ok && claim.Live(now)
}

// SetWorkClaim records or renews interactive ownership of repo#pr.
func (s *State) SetWorkClaim(repo string, pr int, claim WorkClaim) {
	if s.WorkClaims == nil {
		s.WorkClaims = map[string]WorkClaim{}
	}
	key := Key(repo, pr)
	if previous, ok := s.WorkClaims[key]; ok {
		claim.unknown = carryUnknown(claim.unknown, previous.unknown)
	}
	s.WorkClaims[key] = claim
}

// ReleaseWorkClaim drops repo#pr's claim when owner still owns it. force is for
// the explicit operator escape hatch; routine loop cleanup must never release
// somebody else's lease.
func (s *State) ReleaseWorkClaim(repo string, pr int, owner string, force bool) bool {
	key := Key(repo, pr)
	claim, ok := s.WorkClaims[key]
	if !ok || (!force && claim.Owner != owner) {
		return false
	}
	delete(s.WorkClaims, key)
	return true
}
