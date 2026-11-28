package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// repoMissingDays is how many separate days a repository must be confirmed
// missing before crq forgets it.
//
// Not at once, because "not found" is also what a credential that lost its
// access sees — for every private repository at the same moment. Forgetting on
// sight would let one bad token erase the fleet's enrollment and its review
// history. Counting days on which a lookup actually said so, rather than the
// time since the first one, means a week with the daemon down or GitHub
// unreachable is a week that proved nothing.
const repoMissingDays = 7

// reconcileRepos brings what crq has on record into line with what GitHub just
// said about each repository: one that was renamed is moved to its new name,
// one that no longer resolves is marked missing and its rounds taken out of
// scheduling (and it is forgotten once it has been confirmed missing on
// repoMissingDays separate days), and one that came back is unmarked.
//
// It returns the state to carry on with — the one it wrote, or st unchanged —
// and the repositories the pass must leave alone for now: a rename is known for
// them, but something in flight still holds the old name, and acting under the
// new one before its records have moved would do the same work twice. seen is
// keyed by the names the lookup was asked about.
//
// guard, when given, is checked against the state each write lands on and
// vetoes it: autoreview passes one that requires it still to hold the leader
// lease, so a pass that was overtaken while it waited on GitHub applies nothing.
func (s *Service) reconcileRepos(ctx context.Context, st State, seen map[string]ghapi.RepoState, guard func(*State) error) (State, map[string]bool, error) {
	type rename struct{ from, to string }
	var renames []rename
	var missing, back, stale []string
	now := s.clock().UTC()
	today := now.Format("2006-01-02")
	known := map[string]bool{}
	for _, repo := range st.KnownRepos() {
		known[repo] = true
	}
	asked := make([]string, 0, len(seen))
	resolved := 0
	for repo, found := range seen {
		asked = append(asked, repo)
		if found.Err == nil && !found.Missing {
			resolved++
		}
	}
	sort.Strings(asked)
	// A lookup in which nothing resolved says more about the credential than
	// about any one repository. It still takes a missing repository out of
	// scheduling, but it is no evidence towards forgetting one.
	confirmed := resolved > 0
	for _, repo := range asked {
		found := seen[repo]
		alias, aliased := st.RepoAliases[repo]
		record, marked := st.RepoMissing(repo)
		switch {
		case found.Err != nil:
			// The caller reports it; nothing is concluded from a failed lookup.
		case found.Missing && aliased:
			stale = append(stale, repo) // the old name no longer leads anywhere
		case found.Missing:
			switch {
			case s.cfg.ExcludeRepos[repo] && !known[repo]:
				// Named only so that a rename of it would be noticed.
			case !marked, confirmed && record.LastDay != today:
				missing = append(missing, repo)
			}
		default:
			current := NormalizeRepo(found.Name)
			switch {
			case current == repo && aliased:
				stale = append(stale, repo) // a repository of its own again
			case current == repo && marked:
				back = append(back, repo)
			case current != repo && (!aliased || alias != current || known[repo]):
				renames = append(renames, rename{from: repo, to: current})
			}
		}
	}
	if len(renames)+len(missing)+len(back)+len(stale) == 0 {
		return st, nil, nil
	}

	var notes []string
	var holdOff map[string]bool
	var deferred []rename
	apply := func(st *State) bool {
		notes, holdOff, deferred = notes[:0], map[string]bool{}, deferred[:0]
		changed := false
		for _, name := range stale {
			if st.ForgetAlias(name) {
				changed = true
				notes = append(notes, name+" no longer leads to the repository that was renamed away from it")
			}
		}
		for _, r := range renames {
			// A fix session or an interactive loop holds the old name in its own
			// memory. Moving its claim out from under it reads to the watcher
			// as "nobody is fixing this", and a second session starts.
			busy := append(st.BusyPulls(r.from, now), triggerClaimedPulls(st, r.from, now)...)
			if len(busy) > 0 {
				holdOff[r.from], holdOff[r.to] = true, true
				deferred = append(deferred, r)
				// Nor may the same pull request be reviewed from the other side
				// meanwhile: a round already waiting under the new name would
				// fire for a pull request somebody is in the middle of.
				for _, pr := range busy {
					round := st.Round(r.to, pr)
					if round == nil || (round.Phase != PhaseQueued && round.Phase != PhaseAwaitingRetry) {
						continue
					}
					st.EndRound(r.to, pr, "busy under the repository's old name")
					releaseSlot(st, QueueKey(r.to, pr), round.Token)
					changed = true
				}
				continue
			}
			cfg := s.cfg.WithFleet(st.Fleet)
			record, recorded := st.Enrollment(r.from)
			_, destination := st.Enrollment(r.to)
			// An off record under the old name is usually the workaround for
			// this very rename. Where the new name is listed outright, that
			// listing is the later and more deliberate statement, so the off
			// record is dropped instead of being carried over to switch it off.
			if recorded && !record.Enabled && cfg.AllowRepos[r.to] {
				st.ClearEnrollment(r.from)
				recorded, changed = false, true
			}
			// What enrolled it, not whether it is reviewed right now: a
			// repository marked missing reads as not reviewed until this very
			// rename explains where it went.
			reviewed := s.enrollmentDecided(*st, r.from).Enabled
			if st.RenameRepo(r.from, r.to) {
				changed = true
				notes = append(notes, r.from+" was renamed to "+r.to+"; its records moved with it")
			}
			// A name only this host's env enrolled leaves no record to move, and
			// the new name would silently stop being reviewed. Never where a
			// record already answers for either name: that answer stands.
			if reviewed && !recorded && !destination && !s.reviewsRepo(*st, r.to) {
				st.SetEnrollment(r.to, RepoEnrollment{Enabled: true, Reason: "renamed from " + r.from, By: s.cfg.Host, UpdatedAt: &now})
				changed = true
				notes = append(notes, r.to+" enrolled: it is what "+r.from+" is called now")
			}
			// Rounds that arrived under a name the fleet has switched off must
			// not fire there. Only a record decides that: this host's own env
			// says nothing about what another host still reviews.
			if record, ok := st.Enrollment(r.to); ok && !record.Enabled && s.abandonPendingRounds(st, r.to) {
				changed = true
			}
		}
		for _, repo := range missing {
			_, already := st.RepoMissing(repo)
			record, noted := st.NoteRepoMissing(repo, now, confirmed)
			if !noted {
				continue
			}
			changed = true
			switch {
			case !already:
				s.endRoundsOfMissingRepo(st, repo, now)
				notes = append(notes, repo+" is not on GitHub any more (deleted, or not visible to crq); it is skipped, and forgotten if it stays that way")
			case record.Days >= repoMissingDays:
				st.DropRepo(repo)
				notes = append(notes, fmt.Sprintf("%s was confirmed missing on %d separate days since %s; forgotten",
					repo, record.Days, record.Since.Format("2006-01-02")))
			}
		}
		for _, repo := range back {
			if st.ClearRepoMissing(repo) {
				changed = true
				notes = append(notes, repo+" is back on GitHub; reviewing it again")
			}
		}
		return changed
	}

	next := st
	if s.cfg.DryRun {
		// A dry run reports and writes nothing, but the rest of the pass should
		// still see the repositories as they would be.
		next = cloneState(st)
		apply(&next)
	} else {
		wrote := false
		written, err := s.store.Update(ctx, func(st *State) error {
			if guard != nil {
				if err := guard(st); err != nil {
					return err
				}
			}
			if wrote = apply(st); !wrote {
				return ErrNoChange
			}
			return nil
		})
		if err != nil {
			return st, nil, err
		}
		next = written
		if wrote {
			s.sync(ctx, next)
		}
	}
	if s.log != nil {
		for _, note := range notes {
			s.log.Printf("repos: %s", note)
		}
	}
	for _, r := range deferred {
		s.warnOnce("deferred "+r.from+" "+r.to, "%s was renamed to %s; its records move once the work in flight under the old name has finished, and both names are left alone until then", r.from, r.to)
	}
	for _, r := range renames {
		switch {
		case s.cfg.AllowRepos[r.from]:
			s.warnOnce("renamed "+r.from, "%s is in CRQ_REPOS but GitHub now calls it %s; update this host's env", r.from, r.to)
		case s.cfg.ExcludeRepos[r.from]:
			s.warnOnce("renamed "+r.from, "%s is in CRQ_EXCLUDE but GitHub now calls it %s; it stays excluded, update this host's env", r.from, r.to)
		}
	}
	for _, repo := range missing {
		if s.cfg.AllowRepos[repo] {
			s.warnOnce("missing "+repo, "%s is in CRQ_REPOS but GitHub does not know it (deleted, or not visible to crq); remove it from this host's env", repo)
		}
	}
	return next, holdOff, nil
}

// endRoundsOfMissingRepo takes a repository's rounds out of scheduling. Every
// read of one of its pull requests now fails, and a round that Pump keeps
// choosing — or that holds the fire slot — stalls every other repository
// behind an error.
//
// The command a round posted, or is in the middle of posting, may still be in
// flight if the repository is merely invisible rather than gone. So the slot
// such a round holds stays held — but only until that command's own deadline,
// which is what bounds a claim left behind by a poster that died.
func (s *Service) endRoundsOfMissingRepo(st *State, repo string, now time.Time) {
	inflight := s.cfg.WithFleet(st.Fleet).InflightTimeout
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) != repo || !round.Active() {
			continue
		}
		key := QueueKey(round.Repo, round.PR)
		holdsSlot := st.FireSlot != nil && st.FireSlot.Key == key && st.FireSlot.Token == round.Token
		st.EndRound(round.Repo, round.PR, "repository missing")
		var until time.Time
		switch {
		case !holdsSlot:
		case round.Phase == PhaseFired && round.FiredAt != nil:
			until = round.FiredAt.UTC().Add(inflight)
		case round.Phase == PhaseReserved && round.ReservedAt != nil:
			until = round.ReservedAt.UTC().Add(triggerClaimTTL)
		}
		if until.After(now) {
			st.HoldSlotUntil(until)
			continue
		}
		releaseSlot(st, key, round.Token)
	}
}

// CurrentRepo is the name crq keeps repo's records under. A checkout whose
// remote still carries a repository's old name asks about it that way, and
// answering under the old name would start a second set of records — and a
// second review — for pull requests crq already tracks under the new one.
func (s *Service) CurrentRepo(ctx context.Context, repo string) string {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return repo
	}
	if current := st.ResolveRepo(repo); current != NormalizeRepo(repo) {
		return current
	}
	return repo
}

// lookupsFor splits the repositories a pass should ask GitHub about: the scan
// targets, whose open pull requests are wanted, and every other repository the
// state records anything for, whose name alone is checked. The second list is
// what finds a rename nobody is scanning for — a record that was switched off,
// or a repository an owner-wide search now returns under another name.
func (s *Service) lookupsFor(st State, targets []string) (withPRs, namesOnly []string) {
	seen := map[string]bool{}
	for _, repo := range targets {
		if repo = NormalizeRepo(repo); repo != "" && !seen[repo] {
			seen[repo] = true
			withPRs = append(withPRs, repo)
		}
	}
	gate := NormalizeRepo(s.cfg.GateRepo)
	name := func(repo string) {
		if repo = NormalizeRepo(repo); !seen[repo] && repo != gate && checkRepoShape(repo) == nil {
			seen[repo] = true
			namesOnly = append(namesOnly, repo)
		}
	}
	for _, repo := range st.KnownRepos() {
		name(repo)
	}
	// An old name is asked about for as long as it is remembered, because it
	// stops leading to the renamed repository the day another one takes it.
	for old := range st.RepoAliases {
		name(old)
	}
	// And the kill switch, so that renaming a repository does not lift it.
	for repo := range s.cfg.WithFleet(st.Fleet).ExcludeRepos {
		name(repo)
	}
	sort.Strings(namesOnly)
	return withPRs, namesOnly
}

// warnOnce logs a standing problem once per process instead of once per pass.
func (s *Service) warnOnce(key, format string, args ...any) {
	if s.log == nil || s.warned == nil {
		return
	}
	if _, said := s.warned.LoadOrStore(key, true); !said {
		s.log.Printf("warning: "+format, args...)
	}
}

// mergedSweepEvery is how long a repository waits before its leftover records
// are asked about again.
const mergedSweepEvery = 6 * time.Hour

// mergedSweepBatch is how many pull requests one pass asks about: one query.
const mergedSweepBatch = 100

type mergedSweepCursor struct {
	after  int       // the highest pull request number asked about this round
	doneAt time.Time // when the repository was last covered completely
}

// sweepMergedRecords retires what crq still keeps for pull requests that have
// since merged, one repository and one query a pass.
//
// The records are kept without bound so a closed pull request can reopen with
// its evidence intact, and a merge is what retires them. But a merge is only
// noticed while a round or the bounded archive still points at the pull
// request; one that merged after both had moved on kept its records for ever,
// and they had grown to a third of the state every write uploads.
func (s *Service) sweepMergedRecords(ctx context.Context, st State) error {
	if s.cfg.DryRun {
		return nil
	}
	leftovers := map[string]map[int]bool{}
	note := func(key string) {
		repo, pr, ok := parseHoldKey(key)
		if !ok || st.Round(repo, pr) != nil {
			return
		}
		if leftovers[repo] == nil {
			leftovers[repo] = map[int]bool{}
		}
		leftovers[repo][pr] = true
	}
	for key := range st.TidiedCommands {
		note(key)
	}
	for key := range st.WrittenCommands {
		note(key)
	}
	for key := range st.TidyReactionCursors {
		note(key)
	}
	for key := range st.ReviewedHeads {
		note(key)
	}
	for _, index := range []map[string]map[string]time.Time{st.CoActivity, st.CoAnswers} {
		for key := range index {
			note(key)
		}
	}
	repos := make([]string, 0, len(leftovers))
	for repo := range leftovers {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	now := s.clock()
	if s.mergedSweep == nil {
		s.mergedSweep = map[string]mergedSweepCursor{}
	}
	for _, repo := range repos {
		cursor := s.mergedSweep[repo]
		if !cursor.doneAt.IsZero() && now.Sub(cursor.doneAt) < mergedSweepEvery {
			continue
		}
		var prs []int
		for pr := range leftovers[repo] {
			if pr > cursor.after {
				prs = append(prs, pr)
			}
		}
		sort.Ints(prs)
		if len(prs) > mergedSweepBatch {
			prs = prs[:mergedSweepBatch]
			cursor.after = prs[len(prs)-1]
		} else {
			cursor = mergedSweepCursor{doneAt: now}
		}
		if len(prs) == 0 {
			s.mergedSweep[repo] = cursor
			continue
		}
		merged, err := s.gh.MergedPRs(ctx, repo, prs)
		if errors.Is(err, ghapi.ErrNotFound) {
			// The repository is gone; reconcileRepos deals with that. Its turn
			// is over all the same, or it would block the others behind it.
			s.mergedSweep[repo] = mergedSweepCursor{doneAt: now}
			return nil
		}
		if err != nil {
			if !abortsPass(err) {
				// This repository's own failure. Its batch is kept for the next
				// round, but its turn is over, so a query that keeps failing
				// cannot hold up every repository sorted after it.
				s.mergedSweep[repo] = mergedSweepCursor{after: s.mergedSweep[repo].after, doneAt: now}
			}
			return fmt.Errorf("%s: %w", repo, err)
		}
		retired := 0
		_, err = s.store.Update(ctx, func(st *State) error {
			retired = 0
			for _, pr := range prs {
				// Rechecked on the state the write lands on: a round may have
				// appeared since, and then its own retirement owns the merge.
				if !merged[pr] || st.Round(repo, pr) != nil {
					continue
				}
				cleared := st.ClearOnePassProgress(repo, pr)
				if st.RetireMerged(repo, pr) || cleared {
					retired++
				}
			}
			if retired == 0 {
				return ErrNoChange
			}
			return nil
		})
		if err != nil {
			return err
		}
		// Only now is this batch behind it: a failure above leaves the cursor
		// where it was, and the next pass asks again.
		s.mergedSweep[repo] = cursor
		if retired > 0 && s.log != nil {
			s.log.Printf("repos: %s: retired the records of %d merged pull request(s)", repo, retired)
		}
		return nil // one repository a pass
	}
	return nil
}
