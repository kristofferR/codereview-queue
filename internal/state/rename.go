package state

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// MissingRepo is a repository GitHub has stopped resolving: deleted, or no
// longer visible to crq's credential. The two look the same from here.
type MissingRepo struct {
	Since time.Time `json:"since"`
	// Days counts the separate days on which a lookup confirmed it missing, and
	// LastDay is the latest of them. Forgetting waits on this count rather than
	// on the calendar: a week in which nobody looked proves nothing about the
	// repository, and neither do two failures a week apart.
	Days    int    `json:"days"`
	LastDay string `json:"last_day,omitempty"`

	unknown unknownFields
}

// RepoMissing reports whether repo is marked missing, and since when.
func (s *State) RepoMissing(repo string) (MissingRepo, bool) {
	missing, ok := s.MissingRepos[normalizeRepoKey(repo)]
	return missing, ok
}

// NoteRepoMissing marks repo missing, and — when confirmed — counts one more day
// on which a lookup said so, at most one a day. A lookup in which nothing else
// resolved either marks but does not confirm: it says more about the credential
// than about the repository. It returns the record and whether it changed.
func (s *State) NoteRepoMissing(repo string, now time.Time, confirmed bool) (MissingRepo, bool) {
	key := normalizeRepoKey(repo)
	today := now.UTC().Format("2006-01-02")
	missing, marked := s.MissingRepos[key]
	changed := !marked
	if !marked {
		missing.Since = now.UTC()
	}
	if confirmed && missing.LastDay != today {
		missing.Days++
		missing.LastDay = today
		changed = true
	}
	if !changed {
		return missing, false
	}
	if s.MissingRepos == nil {
		s.MissingRepos = map[string]MissingRepo{}
	}
	s.MissingRepos[key] = missing
	return missing, true
}

// ClearRepoMissing unmarks a repository that resolves again.
func (s *State) ClearRepoMissing(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.MissingRepos[key]; !ok {
		return false
	}
	delete(s.MissingRepos, key)
	return true
}

// ResolveRepo is the name crq keeps repo's records under: the one a recorded
// rename leads to, or repo itself.
func (s *State) ResolveRepo(repo string) string {
	key := normalizeRepoKey(repo)
	if current := s.RepoAliases[key]; current != "" {
		return current
	}
	return key
}

// ForgetAlias drops a recorded rename, for a name that no longer leads where it
// did — GitHub stops redirecting once another repository takes the name.
func (s *State) ForgetAlias(name string) bool {
	key := normalizeRepoKey(name)
	if _, ok := s.RepoAliases[key]; !ok {
		return false
	}
	delete(s.RepoAliases, key)
	return true
}

// RenameRepo moves everything recorded for a repository to the name GitHub now
// knows it by: its enrollment and settings, and every per-pull-request record,
// since pull request numbers survive a rename. It also records the rename, so a
// command that still names the repository the old way is led to the new one.
//
// Where the new name already has an entry, that entry wins and the old one is
// dropped. Both describe the same repository, and the one under its current
// name is the one still being written. Reports whether anything changed.
func (s *State) RenameRepo(from, to string) bool {
	from, to = normalizeRepoKey(from), normalizeRepoKey(to)
	if from == "" || to == "" || from == to {
		return false
	}
	repo := func(key string) (string, bool) { return to, key == from }
	prefix := from + "#"
	pull := func(key string) (string, bool) {
		if !strings.HasPrefix(key, prefix) {
			return "", false
		}
		return to + "#" + key[len(prefix):], true
	}

	changed := false
	note := func(moved bool) { changed = changed || moved }

	note(moveKeys(s.Enrolled, repo))
	note(moveKeys(s.Repos, repo))
	note(moveKeys(s.RepoSolver, repo))
	note(moveKeys(s.RepoAutofix, repo))
	note(moveKeys(s.OnePassEvidence, repo))

	note(moveKeys(s.Holds, pull))
	note(moveKeys(s.HoldTokens, pull))
	note(moveKeys(s.CoActivity, pull))
	note(moveKeys(s.CoAnswers, pull))
	note(moveKeys(s.ReviewedHeads, pull))
	note(moveKeys(s.Dispatches, pull))
	note(moveKeys(s.WorkClaims, pull))
	note(moveKeys(s.OnePass, pull))
	note(moveKeys(s.TidiedCommands, pull))
	note(moveKeys(s.WrittenCommands, pull))
	note(moveKeys(s.TidyReactionCursors, pull))

	// Rounds name their repository inside the record as well as in the key.
	for key, round := range s.Rounds {
		next, ok := pull(key)
		if !ok {
			continue
		}
		delete(s.Rounds, key)
		if _, taken := s.Rounds[next]; !taken {
			round.Repo = to
			s.Rounds[next] = round
			// The slot follows the round that holds it. A round displaced by one
			// already at the destination takes nothing with it: BusyPulls keeps
			// a rename from happening while such a round still holds the slot.
			if s.FireSlot != nil && s.FireSlot.Key == key {
				s.FireSlot.Key = next
			}
		}
		changed = true
	}
	for i := range s.Archive {
		if normalizeRepoKey(s.Archive[i].Repo) == from {
			s.Archive[i].Repo = to
			changed = true
		}
	}
	for _, notes := range s.CoUnable {
		for login, unable := range notes {
			if normalizeRepoKey(unable.Repo) == from {
				unable.Repo = to
				notes[login] = unable
				changed = true
			}
		}
	}
	for _, list := range []string{"CRQ_REPOS", "CRQ_EXCLUDE"} {
		if renamed, ok := renameInList(s.Fleet.Env[list], from, to); ok {
			s.Fleet.Env[list] = renamed
			changed = true
		}
	}

	// A repository that answers under a new name is not missing.
	if _, marked := s.MissingRepos[from]; marked {
		delete(s.MissingRepos, from)
		changed = true
	}
	// Earlier names follow along, and the current name leads nowhere.
	for old, current := range s.RepoAliases {
		if current == from {
			s.RepoAliases[old] = to
			changed = true
		}
	}
	if _, stale := s.RepoAliases[to]; stale {
		delete(s.RepoAliases, to)
		changed = true
	}
	if s.RepoAliases[from] != to {
		if s.RepoAliases == nil {
			s.RepoAliases = map[string]string{}
		}
		s.RepoAliases[from] = to
		changed = true
	}
	return changed
}

// BusyPulls lists the pull requests of repo that something is at work on which
// holds the repository's name in its own memory: an unattended fix session, an
// interactive loop, or the round whose posted command holds the fire slot.
// Their records cannot be moved from under them — a session that can no longer
// find its claim stops renewing it and a second one is started on the same pull
// request, and a slot that loses its round is taken for free while the command
// is still unanswered.
func (s *State) BusyPulls(repo string, now time.Time) []int {
	repo = normalizeRepoKey(repo)
	prefix := repo + "#"
	busy := map[int]bool{}
	note := func(key string) {
		if pr, err := strconv.Atoi(strings.TrimPrefix(key, prefix)); err == nil {
			busy[pr] = true
		}
	}
	for key, claim := range s.Dispatches {
		if strings.HasPrefix(key, prefix) && claim.Live(now) {
			note(key)
		}
	}
	for key, claim := range s.WorkClaims {
		if strings.HasPrefix(key, prefix) && claim.Live(now) {
			note(key)
		}
	}
	for _, round := range s.Rounds {
		if normalizeRepoKey(round.Repo) == repo && round.DispatchHeld(now) {
			busy[round.PR] = true
		}
	}
	for i := range s.Archive {
		if normalizeRepoKey(s.Archive[i].Repo) == repo && s.Archive[i].DispatchHeld(now) {
			busy[s.Archive[i].PR] = true
		}
	}
	if slot := s.SlotRound(); slot != nil && normalizeRepoKey(slot.Repo) == repo {
		busy[slot.PR] = true
	}
	pulls := make([]int, 0, len(busy))
	for pr := range busy {
		pulls = append(pulls, pr)
	}
	sort.Ints(pulls)
	return pulls
}

// DropRepo forgets a repository that no longer exists: its enrollment and
// settings, and every per-pull-request record. Reports whether anything was
// recorded for it.
//
// This is the one place rounds are deleted rather than archived. The archive is
// a bounded history of pull requests that can still be looked at; these cannot,
// and pushing them through it would evict the ones that can.
func (s *State) DropRepo(repo string) bool {
	repo = normalizeRepoKey(repo)
	if repo == "" {
		return false
	}
	prefix := repo + "#"
	changed := false
	note := func(dropped bool) { changed = changed || dropped }
	named := func(key string) bool { return key == repo }

	note(dropKeys(s.Enrolled, named))
	note(dropKeys(s.Repos, named))
	note(dropKeys(s.RepoSolver, named))
	note(dropKeys(s.RepoAutofix, named))
	note(dropKeys(s.OnePassEvidence, named))
	note(dropKeys(s.MissingRepos, named))
	note(dropKeys(s.RepoAliases, named))
	for old, current := range s.RepoAliases {
		if current == repo {
			delete(s.RepoAliases, old)
			changed = true
		}
	}

	pull := func(key string) bool { return strings.HasPrefix(key, prefix) }
	// A slot still held for a command this repository's round posted stays held
	// until its own deadline; only the reference to the round goes.
	note(dropKeys(s.Rounds, pull))
	note(dropKeys(s.Holds, pull))
	note(dropKeys(s.HoldTokens, pull))
	note(dropKeys(s.CoActivity, pull))
	note(dropKeys(s.CoAnswers, pull))
	note(dropKeys(s.ReviewedHeads, pull))
	note(dropKeys(s.Dispatches, pull))
	note(dropKeys(s.WorkClaims, pull))
	note(dropKeys(s.OnePass, pull))
	note(dropKeys(s.TidiedCommands, pull))
	note(dropKeys(s.WrittenCommands, pull))
	note(dropKeys(s.TidyReactionCursors, pull))

	kept := s.Archive[:0]
	for _, round := range s.Archive {
		if normalizeRepoKey(round.Repo) == repo {
			changed = true
			continue
		}
		kept = append(kept, round)
	}
	s.Archive = kept
	return changed
}

// KnownRepos lists every repository this state records anything for, sorted.
func (s *State) KnownRepos() []string {
	seen := map[string]bool{}
	for _, keys := range [][]string{
		mapKeys(s.Enrolled), mapKeys(s.Repos), mapKeys(s.RepoSolver), mapKeys(s.RepoAutofix),
		mapKeys(s.OnePassEvidence), mapKeys(s.MissingRepos),
	} {
		for _, key := range keys {
			seen[key] = true
		}
	}
	for _, keys := range [][]string{
		mapKeys(s.Rounds), mapKeys(s.Holds), mapKeys(s.CoActivity), mapKeys(s.CoAnswers), mapKeys(s.ReviewedHeads),
		mapKeys(s.Dispatches), mapKeys(s.WorkClaims), mapKeys(s.OnePass),
		mapKeys(s.TidiedCommands), mapKeys(s.WrittenCommands), mapKeys(s.TidyReactionCursors),
	} {
		for _, key := range keys {
			if repo, _, ok := strings.Cut(key, "#"); ok {
				seen[repo] = true
			}
		}
	}
	for i := range s.Archive {
		seen[normalizeRepoKey(s.Archive[i].Repo)] = true
	}
	delete(seen, "")
	repos := mapKeys(seen)
	sort.Strings(repos)
	return repos
}

// moveKeys renames the entries of m that rekey claims. An entry already at the
// destination is kept and the one being moved is dropped.
func moveKeys[V any](m map[string]V, rekey func(string) (string, bool)) bool {
	moved := false
	for _, key := range mapKeys(m) {
		next, ok := rekey(key)
		if !ok {
			continue
		}
		value := m[key]
		delete(m, key)
		if _, taken := m[next]; !taken {
			m[next] = value
		}
		moved = true
	}
	return moved
}

func dropKeys[V any](m map[string]V, match func(string) bool) bool {
	dropped := false
	for key := range m {
		if match(key) {
			delete(m, key)
			dropped = true
		}
	}
	return dropped
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// renameInList rewrites one repository in a comma-separated list, dropping it
// instead when the new name is already listed.
func renameInList(list, from, to string) (string, bool) {
	if strings.TrimSpace(list) == "" {
		return list, false
	}
	parts := strings.Split(list, ",")
	hasTo := false
	for _, part := range parts {
		if normalizeRepoKey(part) == to {
			hasTo = true
		}
	}
	out := make([]string, 0, len(parts))
	found := false
	for _, part := range parts {
		if normalizeRepoKey(part) != from {
			out = append(out, part)
			continue
		}
		found = true
		if !hasTo {
			out = append(out, to)
			hasTo = true
		}
	}
	return strings.Join(out, ","), found
}
