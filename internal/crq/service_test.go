package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type fakeGitHub struct {
	mu              sync.Mutex
	pulls           map[string]ghapi.Pull
	pullReads       map[string]int
	pullErrOnRead   map[string]int
	pullErrs        map[string]error
	mergeErrs       map[string]error
	mergeResults    map[string]ghapi.MergeResult
	merged          []string
	commits         map[string]ghapi.Commit
	commitErrs      map[string]error
	reviews         map[string][]ghapi.Review
	comments        map[string][]ghapi.IssueComment
	reviewComments  map[string][]ghapi.ReviewComment
	issueReactions  map[string][]ghapi.Reaction
	reactions       map[int64][]ghapi.Reaction
	reactionErrs    map[int64]error
	reactionReads   []int64
	checkRuns       map[string][]ghapi.CheckRun // key: ref (short or full sha)
	checkRunErrs    map[string]error
	postBodyErrs    map[string]error // body → error (selective trigger-post failures)
	listPullErrs    map[string]error // repo → error (a repository the token cannot read)
	deleteErrs      map[int64]error  // comment id → error (GitHub refuses the delete)
	deleteAfterErrs map[int64]error  // comment id → error after GitHub applies the delete
	posted          []string
	deleted         []int64
	deleteCalls     []int64
	commentID       int64
	createdIssues   []int
	nextIssueNumber int
	postErrs        map[string]error
	postHook        func()
	graphQL         func(query string, vars map[string]any, out any) error
	// stateRef is the SHA GetRef reports; tests that exercise `crq wait` move it
	// to signal "the queue advanced".
	stateRef    string
	refReads    int
	reviewReads int
	searchPRs   []ghapi.SearchPR
	// searches counts EachOpenPR calls, which is what says whether a pass went
	// looking at all — an empty result set and a search never made are the same
	// enqueue count and very different REST bills.
	searches   int
	ownerRepos []ghapi.Repo
	owners     []string
	getComment func(repo string, id int64) (ghapi.IssueComment, error)
	// now, when set, timestamps posted comments off the same injected clock the
	// service uses, so a fire's recorded FiredAt tracks the fake wall clock the
	// replay suite advances. nil falls back to real time (all existing tests).
	now func() time.Time
}

func TestObservedBlockUsesTheStateResolvedPrimaryAndFallback(t *testing.T) {
	ctx := context.Background()
	startup := firingConfig()
	startup.Bot = "startup-primary[bot]"
	startup.RateLimitFallback = 5 * time.Minute
	store := NewMemoryStore(startup)
	svc := NewService(startup, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	effective := startup
	effective.Bot = "fleet-primary[bot]"
	effective.RateLimitFallback = 47 * time.Minute
	obs := observation{eng: engine.Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: effective.Bot,
		CommentID: 91, UpdatedAt: now,
	}}}}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.recordObservedBlock(ctx, effective, obs, st, now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(effective.RateLimitFallback)
	if updated == nil || updated.Account.BlockedUntil == nil || !updated.Account.BlockedUntil.Equal(want) {
		t.Fatalf("blocked until = %v, want the fleet-resolved fallback %s", updated, want)
	}
}

func (f *fakeGitHub) clock() time.Time {
	if f.now != nil {
		return f.now().UTC()
	}
	return time.Now().UTC()
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		pulls:           map[string]ghapi.Pull{},
		pullReads:       map[string]int{},
		pullErrOnRead:   map[string]int{},
		pullErrs:        map[string]error{},
		mergeErrs:       map[string]error{},
		mergeResults:    map[string]ghapi.MergeResult{},
		commits:         map[string]ghapi.Commit{},
		commitErrs:      map[string]error{},
		reviews:         map[string][]ghapi.Review{},
		comments:        map[string][]ghapi.IssueComment{},
		reviewComments:  map[string][]ghapi.ReviewComment{},
		issueReactions:  map[string][]ghapi.Reaction{},
		reactions:       map[int64][]ghapi.Reaction{},
		reactionErrs:    map[int64]error{},
		deleteErrs:      map[int64]error{},
		deleteAfterErrs: map[int64]error{},
	}
}

func fakeKey(repo string, pr int) string { return QueueKey(repo, pr) }

// ListCheckRuns serves the runs stored under the requested ref, tolerating a
// stored short-SHA key for a full-SHA request and vice versa (observe fetches
// by the pull's full head SHA).
func (f *fakeGitHub) ListCheckRuns(_ context.Context, _ string, ref string) ([]ghapi.CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.checkRunErrs[ref]; err != nil {
		return nil, err
	}
	if runs, ok := f.checkRuns[ref]; ok {
		return append([]ghapi.CheckRun(nil), runs...), nil
	}
	// Prefix fallback (tests seed short SHAs, observe asks with the full one),
	// but only when exactly one key matches: returning from a map range picked
	// an arbitrary entry whenever two refs prefixed each other.
	var match []ghapi.CheckRun
	found := 0
	for key, runs := range f.checkRuns {
		if key == "" || ref == "" {
			continue
		}
		if strings.HasPrefix(ref, key) || strings.HasPrefix(key, ref) {
			match, found = runs, found+1
		}
	}
	if found == 1 {
		return append([]ghapi.CheckRun(nil), match...), nil
	}
	return nil, nil
}

func (f *fakeGitHub) setCheckRuns(ref string, runs ...ghapi.CheckRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkRuns == nil {
		f.checkRuns = map[string][]ghapi.CheckRun{}
	}
	f.checkRuns[ref] = runs
}

func (f *fakeGitHub) GetPull(_ context.Context, repo string, pr int) (ghapi.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(repo, pr)
	if f.pullReads == nil {
		f.pullReads = map[string]int{}
	}
	f.pullReads[key]++
	if f.pullErrOnRead[key] == f.pullReads[key] {
		return ghapi.Pull{}, errors.New("injected pull read failure")
	}
	if err := f.pullErrs[key]; err != nil {
		return ghapi.Pull{}, err
	}
	pull, ok := f.pulls[key]
	if !ok {
		return ghapi.Pull{}, errors.New("missing pull")
	}
	return pull, nil
}

func (f *fakeGitHub) MergePull(_ context.Context, repo string, pr int, sha, method string) (ghapi.MergeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(repo, pr)
	if err := f.mergeErrs[key]; err != nil {
		return ghapi.MergeResult{}, err
	}
	f.merged = append(f.merged, fmt.Sprintf("%s@%s:%s", key, sha, method))
	if result, ok := f.mergeResults[key]; ok {
		return result, nil
	}
	return ghapi.MergeResult{SHA: sha, Merged: true, Message: "Pull Request successfully merged"}, nil
}

func (f *fakeGitHub) GetCommit(_ context.Context, repo, sha string) (ghapi.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.commitErrs[sha]; err != nil {
		return ghapi.Commit{}, err
	}
	return f.commits[sha], nil
}

func (f *fakeGitHub) ListReviews(_ context.Context, repo string, pr int) ([]ghapi.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewReads++
	return append([]ghapi.Review(nil), f.reviews[fakeKey(repo, pr)]...), nil
}

// reviewPolls is how many times anything has read this PR's reviews. A test that
// needs a running loop to have reached its polling cycle waits on this instead of
// sleeping: a fixed sleep loses under parallel load, and did.
func (f *fakeGitHub) reviewPolls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reviewReads
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, repo string, pr int) ([]ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.IssueComment(nil), f.comments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) GetIssueComment(_ context.Context, repo string, id int64) (ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getComment != nil {
		return f.getComment(repo, id)
	}
	for _, comments := range f.comments {
		for _, comment := range comments {
			if comment.ID == id {
				return comment, nil
			}
		}
	}
	return ghapi.IssueComment{}, ghapi.ErrNotFound
}

func (f *fakeGitHub) ListReviewComments(_ context.Context, repo string, pr int) ([]ghapi.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.ReviewComment(nil), f.reviewComments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListIssueReactions(_ context.Context, repo string, pr int) ([]ghapi.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.Reaction(nil), f.issueReactions[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListCommentReactions(_ context.Context, _ string, id int64) ([]ghapi.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionReads = append(f.reactionReads, id)
	if err := f.reactionErrs[id]; err != nil {
		return nil, err
	}
	return append([]ghapi.Reaction(nil), f.reactions[id]...), nil
}

func (f *fakeGitHub) PostIssueComment(_ context.Context, repo string, pr int, body string) (ghapi.IssueComment, error) {
	f.mu.Lock()
	if err := f.postErrs[fakeKey(repo, pr)]; err != nil {
		f.mu.Unlock()
		return ghapi.IssueComment{}, err
	}
	if err := f.postBodyErrs[body]; err != nil {
		f.mu.Unlock()
		return ghapi.IssueComment{}, err
	}
	f.commentID++
	f.posted = append(f.posted, repo+"#"+strconv.Itoa(pr)+":"+body)
	now := f.clock()
	comment := ghapi.IssueComment{ID: f.commentID, Body: body, CreatedAt: now, UpdatedAt: now}
	comment.User.Login = "kristofferR"
	hook := f.postHook
	f.postHook = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return comment, nil
}

func (f *fakeGitHub) CreateIssue(_ context.Context, _ string, _ string, _ string) (ghapi.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextIssueNumber == 0 {
		f.nextIssueNumber = 1000
	}
	f.nextIssueNumber++
	f.createdIssues = append(f.createdIssues, f.nextIssueNumber)
	return ghapi.Issue{Number: f.nextIssueNumber, State: "open"}, nil
}

func (f *fakeGitHub) ListIssueCommentsPage(_ context.Context, repo string, pr, page, perPage int) ([]ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.comments[fakeKey(repo, pr)]
	start := (page - 1) * perPage
	if start < 0 || start >= len(all) {
		return nil, nil
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	return append([]ghapi.IssueComment(nil), all[start:end]...), nil
}

func (f *fakeGitHub) DeleteIssueComment(_ context.Context, repo string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, id)
	if err := f.deleteErrs[id]; err != nil {
		return err
	}
	for key, list := range f.comments {
		for i, c := range list {
			if c.ID == id {
				f.comments[key] = append(list[:i], list[i+1:]...)
				f.deleted = append(f.deleted, id)
				return f.deleteAfterErrs[id]
			}
		}
	}
	return nil
}

func (f *fakeGitHub) SearchOpenPRs(context.Context, string, bool, int) ([]ghapi.SearchPR, error) {
	return nil, nil
}

// ownerRepos backs ListOwnerRepos; empty is a fine default, since only the
// repository picker asks and nothing in the queue depends on it.
func (f *fakeGitHub) ListOwnerRepos(_ context.Context, owner string, _ int) ([]ghapi.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners = append(f.owners, owner)
	return append([]ghapi.Repo(nil), f.ownerRepos...), nil
}

func (f *fakeGitHub) EachOpenPR(_ context.Context, _ string, _ bool, fn func(ghapi.SearchPR) (bool, error)) error {
	f.mu.Lock()
	f.searches++
	prs := append([]ghapi.SearchPR(nil), f.searchPRs...)
	f.mu.Unlock()
	for _, pr := range prs {
		stop, err := fn(pr)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// ListPulls answers the branch->PR lookup target inference uses.
//
// It honours the head filter, because a fake that ignores it would let a caller
// asking for the wrong branch pass: every PR in the repository would come back
// and a single-result assertion would still hold.
func (f *fakeGitHub) ListPulls(_ context.Context, repo string, query url.Values) ([]ghapi.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.listPullErrs[strings.ToLower(repo)]; err != nil {
		return nil, err
	}
	wantRef := ""
	if head := query.Get("head"); head != "" {
		_, wantRef, _ = strings.Cut(head, ":") // "owner:branch"
	}
	var out []ghapi.Pull
	for key, p := range f.pulls {
		if !strings.HasPrefix(key, strings.ToLower(repo)+"#") {
			continue
		}
		if wantRef != "" && p.Head.Ref != wantRef {
			continue
		}
		// GitHub always names the head's repository, and dispatch reads it to
		// tell a fork from a branch of this repository. A fake that left it
		// empty would make every test PR look like an unreadable fork.
		if p.Head.Repo.FullName == "" {
			p.Head.Repo.FullName = repo
		}
		out = append(out, p)
	}
	// Deterministic: map iteration order must not decide what a test sees.
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// GetRef reports the fake state ref. It is what `crq wait` idles on, so tests
// count the reads to assert the wait stays cheap.
func (f *fakeGitHub) GetRef(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refReads++
	if f.stateRef == "" {
		return "ref0", nil
	}
	return f.stateRef, nil
}

func (f *fakeGitHub) setStateRef(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateRef = sha
}

func (f *fakeGitHub) GraphQL(_ context.Context, query string, vars map[string]any, out any) error {
	f.mu.Lock()
	handler := f.graphQL
	f.mu.Unlock()
	if handler != nil {
		return handler(query, vars, out)
	}
	return errors.New("graphql unavailable")
}

// noForcePush is a GraphQL handler reporting no HEAD_REF_FORCE_PUSHED_EVENT, so
// headForcePushCutoff succeeds with a zero cutoff and adoption proceeds. Tests
// exercising successful adoption need it now that a failed force-push lookup
// skips adoption.
func noForcePush(_ string, _ map[string]any, out any) error {
	return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"timelineItems":{"nodes":[]}}}}`), out)
}

func TestLatestHeadForcePushRejectsInvalidRepository(t *testing.T) {
	for _, repo := range []string{"owner", "owner/", "/repo", "owner/repo/extra"} {
		t.Run(repo, func(t *testing.T) {
			gh := newFakeGitHub()
			gh.graphQL = func(_ string, _ map[string]any, _ any) error {
				t.Fatal("invalid repository must not make a GraphQL request")
				return nil
			}
			svc := &Service{gh: gh}

			got, err := svc.latestHeadForcePush(t.Context(), repo, 1)
			if err != nil || got != (headForcePush{}) {
				t.Fatalf("latestHeadForcePush() = %+v, %v; want zero result", got, err)
			}
		})
	}
}

// --- test store fakes (v3) ---

type failNthUpdateStore struct {
	StateStore
	n     int
	err   error
	calls int
}

type supersedeBeforeUpdateStore struct {
	StateStore
	repo     string
	pr       int
	head     string
	now      time.Time
	complete bool
	done     bool
}

type evictBeforeUpdateStore struct {
	StateStore
	repo string
	pr   int
	now  time.Time
	done bool
}

func TestSameCoActivityDetectsRemovedReviewer(t *testing.T) {
	seen := time.Now().UTC()
	before := map[string]CoBotRound{"bugbot": {SeenActiveAt: &seen}}
	if sameCoActivity(before, map[string]CoBotRound{}) {
		t.Fatal("removed reviewer activity must be a change")
	}
}

func (s *supersedeBeforeUpdateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	if !s.done {
		s.done = true
		if _, err := s.StateStore.Update(ctx, func(st *State) error {
			round, err := st.Supersede(s.repo, s.pr, s.head, s.now)
			if err != nil {
				return err
			}
			if s.complete {
				if err := round.Dedupe(s.now); err != nil {
					return err
				}
				st.PutRound(*round)
			}
			return nil
		}); err != nil {
			return State{}, err
		}
	}
	return s.StateStore.Update(ctx, mutate)
}

func (s *evictBeforeUpdateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	if !s.done {
		s.done = true
		if _, err := s.StateStore.Update(ctx, func(st *State) error {
			st.EndRound(s.repo, s.pr, "pr closed")
			for otherPR := 100; otherPR < 100+ArchiveMax; otherPR++ {
				round, err := st.NewRound("o/other", otherPR, "123456789", s.now)
				if err != nil {
					return err
				}
				st.PutRound(*round)
				st.EndRound("o/other", otherPR, "pr closed")
			}
			return nil
		}); err != nil {
			return State{}, err
		}
	}
	return s.StateStore.Update(ctx, mutate)
}

func (s *failNthUpdateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	s.calls++
	if s.calls == s.n {
		return State{}, s.err
	}
	return s.StateStore.Update(ctx, mutate)
}

type holdAfterEnqueueStore struct {
	StateStore
	held bool
}

func (s *holdAfterEnqueueStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	state, err := s.StateStore.Update(ctx, mutate)
	if err != nil || s.held {
		return state, err
	}
	s.held = true
	if _, err := s.StateStore.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", time.Now())
		return nil
	}); err != nil {
		return State{}, err
	}
	return state, nil
}

// retryNoChangeStore invokes the mutate closure twice within one Update: first
// against a state whose round holds the fire slot, then a fresh state with no
// round. It verifies recordFire resets its `recorded` flag between attempts.
type retryNoChangeStore struct{ cfg Config }

func (retryNoChangeStore) Load(context.Context) (State, Revision, error) {
	return DefaultState(Config{}), Revision{}, nil
}

func (s retryNoChangeStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(s.cfg)
	firstRound, _ := first.NewRound("owner/repo", 12, "abcdef123", time.Now().UTC())
	_ = firstRound.Reserve("token", "host", time.Now().UTC())
	first.PutRound(*firstRound)
	first.FireSlot = &FireSlot{Key: QueueKey("owner/repo", 12), Token: "token", Since: time.Now().UTC()}
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(s.cfg)
	if err := mutate(&second); err != nil {
		if errors.Is(err, ErrNoChange) {
			return second, nil
		}
		return State{}, err
	}
	return second, nil
}

func (retryNoChangeStore) SyncDashboard(context.Context, State) error { return nil }

// retryMergedHoldStore simulates a CAS retry where the original hold is
// replaced between mutation attempts.
type retryMergedHoldStore struct {
	cfg Config
	at  time.Time
}

func (retryMergedHoldStore) Load(context.Context) (State, Revision, error) {
	return DefaultState(Config{}), Revision{}, nil
}

func (s retryMergedHoldStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(s.cfg)
	first.HoldWithToken("owner/repo", 12, "original hold", "operator", s.at, "first")
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(s.cfg)
	second.HoldWithToken("owner/repo", 12, "original hold", "operator", s.at, "replacement")
	if err := mutate(&second); err != nil {
		if errors.Is(err, ErrNoChange) {
			return second, nil
		}
		return State{}, err
	}
	return second, nil
}

func (retryMergedHoldStore) SyncDashboard(context.Context, State) error { return nil }

// retryUnholdStore simulates another caller releasing a hold after this caller
// successfully mutated a stale state snapshot.
type retryUnholdStore struct{ cfg Config }

func (retryUnholdStore) Load(context.Context) (State, Revision, error) {
	return DefaultState(Config{}), Revision{}, nil
}

func (s retryUnholdStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(s.cfg)
	first.Hold("owner/repo", 12, "original hold", "operator", time.Now().UTC())
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(s.cfg)
	if err := mutate(&second); err != nil {
		if errors.Is(err, ErrNoChange) {
			return second, nil
		}
		return State{}, err
	}
	return second, nil
}

func (retryUnholdStore) SyncDashboard(context.Context, State) error { return nil }

// adoptionRaceStore loads a queued round with an adoptable command, but every
// Update simulates another worker already holding the fire slot.
type adoptionRaceStore struct {
	cfg       Config
	loadState State
}

func (s *adoptionRaceStore) Load(context.Context) (State, Revision, error) {
	state := cloneState(s.loadState)
	state.Normalize(time.Now().UTC())
	return state, Revision{}, nil
}

func (s *adoptionRaceStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	state := cloneState(s.loadState)
	now := time.Now().UTC()
	other, err := state.NewRound("owner/repo", 99, "999999999", now)
	if err != nil {
		return State{}, err
	}
	if err := other.Reserve("other", "other-host", now); err != nil {
		return State{}, err
	}
	state.PutRound(*other)
	state.FireSlot = &FireSlot{Key: "owner/repo#99", Token: "other", Since: now}
	if err := mutate(&state); err != nil {
		if errors.Is(err, ErrNoChange) {
			return state, nil
		}
		return State{}, err
	}
	return state, nil
}

func (s *adoptionRaceStore) SyncDashboard(context.Context, State) error { return nil }

// --- test helpers ---

func cfgTimeout(cfg Config) time.Duration {
	if cfg.FeedbackWaitTimeout > 0 {
		return cfg.FeedbackWaitTimeout
	}
	return time.Hour
}

// seedRound installs a round for repo#pr at head in the given phase. Fired,
// reviewing, and completed phases record a fire at firedAt with commandID; a
// fired round also holds the global fire slot.
func seedRound(t *testing.T, store StateStore, cfg Config, repo string, pr int, head string, phase Phase, firedAt time.Time, commandID int64) {
	t.Helper()
	_, err := store.Update(context.Background(), func(st *State) error {
		r, err := st.NewRound(repo, pr, head, firedAt)
		if err != nil {
			return err
		}
		switch phase {
		case PhaseQueued:
		case PhaseFired, PhaseReviewing, PhaseCompleted:
			if err := r.Reserve("seedtok", "seedhost", firedAt); err != nil {
				return err
			}
			if err := r.Fire(commandID, firedAt); err != nil {
				return err
			}
			dl := firedAt.Add(cfgTimeout(cfg))
			r.WaitDeadline = &dl
			if phase == PhaseReviewing {
				if err := r.Acknowledge(); err != nil {
					return err
				}
			}
			if phase == PhaseCompleted {
				if err := r.Complete(); err != nil {
					return err
				}
			}
		}
		st.PutRound(*r)
		if phase == PhaseFired {
			st.FireSlot = &FireSlot{Key: QueueKey(repo, pr), Token: "seedtok", Since: firedAt}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func roundPhase(t *testing.T, store StateStore, repo string, pr int) Phase {
	t.Helper()
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := st.Round(repo, pr)
	if r == nil {
		return ""
	}
	return r.Phase
}

func TestPruneCalibrationDeletesOldNoiseKeepsRecent(t *testing.T) {
	gh := newFakeGitHub()
	cfg := Config{
		GateRepo:          "o/gate",
		CalibrationPR:     1,
		Bot:               "coderabbitai[bot]",
		RateLimitCommand:  "@coderabbitai rate limit",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		Scope:             []string{"o"},
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	mkc := func(id int64, login, body string, at time.Time) ghapi.IssueComment {
		c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		c.User.Login = login
		return c
	}
	key := fakeKey("o/gate", 1)
	gh.comments[key] = []ghapi.IssueComment{
		mkc(1, "kristofferR", "@coderabbitai rate limit", old),
		mkc(2, "coderabbitai[bot]", "0 reviews remaining. auto-generated reply by CodeRabbit", old),
		mkc(3, "someone", "unrelated human comment", old),
		mkc(4, "kristofferR", "@coderabbitai rate limit", now),
	}

	deleted := svc.pruneCalibration(context.Background(), 1, now.Add(-2*time.Minute), 80)
	if deleted != 2 {
		t.Fatalf("expected 2 deletions, got %d", deleted)
	}
	remaining := map[int64]bool{}
	for _, c := range gh.comments[key] {
		remaining[c.ID] = true
	}
	if remaining[1] || remaining[2] {
		t.Fatalf("old calibration noise was not pruned: %v", remaining)
	}
	if !remaining[3] || !remaining[4] {
		t.Fatalf("non-noise or recent comment was wrongly pruned: %v", remaining)
	}
}

func TestAutoReviewOnceReleasesLeader(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h", LeaderTTL: time.Minute}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Leader != nil {
		t.Fatalf("one-shot autoreview should release its leader lease, got %#v", st.Leader)
	}
}

func TestAutoReviewAppliesOnePassConfiguredReviewerScope(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	cfg.LeaderTTL = time.Minute
	cfg.AutoReviewMaxScan = 10
	cfg.Reviewers = []Reviewer{
		{Login: cfg.Bot, Required: true, Budget: dialect.BudgetAccount},
		{Login: dialect.CodexBotLogin, Name: "Codex", Budget: dialect.BudgetNone},
	}
	cfg.CoBots = []CoBotConfig{{Name: "codex", Login: dialect.CodexBotLogin}}
	repo, pr, head := "o/app", 4, "abcdef1234567890"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr, Author: "alice"}}
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	review := ghapi.Review{CommitID: "1111111111111111", State: "COMMENTED", Body: "reviewed"}
	review.User.Login = dialect.CodexBotLogin
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	on := true
	if _, err := svc.SetSolver(ctx, repo, SolverChange{OnePass: &on}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round != nil {
		t.Fatalf("one-pass scan queued a second configured review: %+v", round)
	}
}

func TestAutoReviewScanSkipsConfiguredAuthors(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		Bot:               "coderabbitai[bot]",
		ReviewCommand:     "@coderabbitai review",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipAuthors:       authorSet("dependabot[bot]"),
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{
		{Repo: "o/app", Number: 1, Author: "dependabot[bot]"},
		{Repo: "o/app", Number: 2, Author: "Dependabot"},
		{Repo: "o/app", Number: 3, Author: "alice"},
	}
	for pr := 1; pr <= 3; pr++ {
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FiredMarker("o/app", 3) == "" {
		t.Fatalf("only the human-authored PR should be enqueued and fired, got rounds=%#v", st.Rounds)
	}
	if st.Round("o/app", 1) != nil || st.Round("o/app", 2) != nil {
		t.Fatalf("bot-authored PRs must never be queued/fired, got rounds=%#v", st.Rounds)
	}
}

func TestAutoReviewScanSkipsMarkedPRs(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		Bot:               "coderabbitai[bot]",
		ReviewCommand:     "@coderabbitai review",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipMarker:        "<!-- crq:skip-autoreview -->",
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{
		{Repo: "o/app", Number: 1, Author: "alice", Body: "Tiny maintenance change.\n\n<!-- crq:skip-autoreview -->"},
		{Repo: "o/app", Number: 2, Author: "alice", Body: "Review this change."},
	}
	for pr := 1; pr <= 2; pr++ {
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FiredMarker("o/app", 2) == "" {
		t.Fatalf("only the unmarked PR should be reviewed, got rounds=%#v", st.Rounds)
	}
	if st.Round("o/app", 1) != nil {
		t.Fatalf("marked PR must never fire, got %#v", st.Round("o/app", 1))
	}
}

func TestNoteTitlesDoesNotWriteDuringDryRun(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DryRun: true}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "o/r", PR: 1, Head: "aaaaaaaaa", Title: "old title"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	svc.noteTitles(ctx, []queueCandidate{{Repo: "o/r", PR: 1, Title: "new title"}})

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Round("o/r", 1).Title; got != "old title" {
		t.Fatalf("title = %q, want dry-run state unchanged", got)
	}
}

func TestEnqueueBatchAppendsOncePerPR(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx := context.Background()
	items := []queueCandidate{
		{Repo: "o/a", PR: 1, Head: "aaaaaaaa1"},
		{Repo: "o/b", PR: 2, Head: "bbbbbbbb2"},
		{Repo: "o/a", PR: 1, Head: "aaaaaaaa1"},
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st, _, _ := svc.store.Load(ctx)
	queued := st.QueuedRounds(time.Now().UTC())
	if len(queued) != 2 {
		t.Fatalf("expected 2 queued (deduped), got %d", len(queued))
	}
	if queued[0].Seq == queued[1].Seq || queued[0].Seq == 0 {
		t.Fatalf("expected distinct non-zero seqs, got %d and %d", queued[0].Seq, queued[1].Seq)
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st2, _, _ := svc.store.Load(ctx)
	if len(st2.QueuedRounds(time.Now().UTC())) != 2 {
		t.Fatalf("expected still 2 after re-batch, got %d", len(st2.QueuedRounds(time.Now().UTC())))
	}
}

func TestEnqueueBatchDryRunDoesNotWrite(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h", DryRun: true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	if err := svc.enqueueBatch(context.Background(), []queueCandidate{{
		Repo: "o/a", PR: 1, Head: "aaaaaaaa1",
	}}); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/a", 1); round != nil {
		t.Fatalf("dry-run enqueue persisted a round: %+v", round)
	}
}

func TestEnqueueBatchSkipsHeldPRsUnderCAS(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("o/held", 1, "waiting on a decision", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	items := []queueCandidate{
		{Repo: "o/held", PR: 1, Head: "aaaaaaaa1"},
		{Repo: "o/ready", PR: 2, Head: "bbbbbbbb2"},
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Round("o/held", 1); got != nil {
		t.Fatalf("held PR acquired a hidden queue position: %+v", got)
	}
	if got := st.Round("o/ready", 2); got == nil || got.Seq != 1 {
		t.Fatalf("ready PR should receive the first queue position, got %+v", got)
	}
}

func TestEnqueueBatchRejectsCandidateFromObsoleteOnePassPolicy(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	ctx := context.Background()

	if _, err := store.Update(ctx, func(st *State) error {
		st.SetSolver("o/app", SolverSettings{
			SetOnePass: true, OnePass: true, OnePassCampaign: "new-campaign",
		}, "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.enqueueBatch(ctx, []queueCandidate{{
		Repo: "o/app", PR: 1, Head: "aaaaaaaa1", PolicyChecked: true,
	}}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/app", 1); round != nil {
		t.Fatalf("candidate evaluated before one-pass was enabled was enqueued: %+v", round)
	}
}

func TestLatestCalibrationReplyToleratesBotSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", CalibrationPR: 1, CalibrationMarker: "auto-generated reply by CodeRabbit"}
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	comment := ghapi.IssueComment{Body: "0 reviews remaining. auto-generated reply by CodeRabbit", UpdatedAt: now}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/gate", 1)] = []ghapi.IssueComment{comment}

	got, ok, err := svc.latestCalibrationReply(context.Background(), 1, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Body != comment.Body {
		t.Fatalf("expected suffixed bot calibration reply to match suffix-less config, ok=%v got=%#v", ok, got)
	}
}

func TestRenewLeaderRespectsLiveLease(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, LeaderTTL: time.Minute}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx := context.Background()
	if _, held, err := svc.renewLeader(ctx, "ownerA", "tokA"); err != nil || !held {
		t.Fatalf("A should acquire the lease: held=%v err=%v", held, err)
	}
	if _, held, _ := svc.renewLeader(ctx, "ownerA", "tokA"); !held {
		t.Fatal("A should renew its own lease")
	}
	if _, held, _ := svc.renewLeader(ctx, "ownerB", "tokB"); held {
		t.Fatal("B must not steal a live lease")
	}
}

// codexCoBots builds the single Codex co-reviewer entry these tests assume,
// with Codex's historical default trigger: post at fire time exactly when it
// is configured-required (parseCoBots' codex rule).
func codexCoBots(requiredBots []string) []CoBotConfig {
	// Derived from the production parser so the command, grace, and the
	// required→always mapping cannot drift from what crq actually does.
	co, err := parseCoBots(map[string]string{"CRQ_COBOTS": "codex"}, requiredBots)
	if err != nil {
		panic("parseCoBots codex: " + err.Error())
	}
	return co
}

func firingConfig() Config {
	return Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state-v3",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		RateLimitMarker:     "rate limited by coderabbit.ai",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
	}
}

func TestApplyTransitionDropsRetryWhenFleetPolicyChanged(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	repo, pr := "owner/repo", 88
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, "abcdef123", PhaseFired, now.Add(-time.Minute), 9)
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decidedCfg := svc.cfgFor(st, repo)
	changedAt := now.Add(time.Second)
	st.Fleet.UpdatedAt = &changedAt
	round := st.Round(repo, pr)

	err = svc.applyTransition(&st, round, engine.Transition{
		Outcome: engine.OutRetry,
		Reason:  "old in-flight timeout elapsed",
		RetryAt: now.Add(time.Minute),
	}, now, decidedCfg)
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("applyTransition error = %v, want stale retry discarded", err)
	}
	if round.Phase != PhaseFired || st.FireSlot == nil {
		t.Fatalf("stale retry changed the live round or released its slot: round=%+v slot=%+v", round, st.FireSlot)
	}

	until := now.Add(30 * time.Minute)
	noticeAt := now.Add(-time.Minute)
	err = svc.applyTransition(&st, round, engine.Transition{
		Outcome: engine.OutRetry,
		Reason:  dialect.ReasonRateLimited,
		RetryAt: until,
		Blocked: &engine.AccountBlock{
			Until: until, CommentID: 5, CommentUpdated: noticeAt,
		},
	}, now, decidedCfg)
	if err != nil {
		t.Fatalf("applyTransition error = %v, want independent account evidence retained", err)
	}
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(until) {
		t.Fatalf("blocked until = %v, want %s", st.Account.BlockedUntil, until)
	}
	if round.Phase != PhaseFired || st.FireSlot == nil {
		t.Fatalf("account evidence applied the stale retry: round=%+v slot=%+v", round, st.FireSlot)
	}
}

func TestReleaseSlotRequiresTheOwningToken(t *testing.T) {
	until := time.Now().UTC().Add(time.Minute)
	st := State{
		FireSlot:          &FireSlot{Key: "owner/repo#1", Token: "old-token", HoldUntil: &until},
		FireSlotHoldUntil: &until,
	}

	releaseSlot(&st, "owner/repo#1", "replacement-token")
	if st.FireSlot == nil || st.FireSlot.HoldUntil == nil || st.FireSlotHoldUntil == nil {
		t.Fatalf("replacement round cleared the original command's slot hold: %+v", st.FireSlot)
	}

	releaseSlot(&st, "owner/repo#1", "old-token")
	if st.FireSlot != nil || st.FireSlotHoldUntil != nil {
		t.Fatalf("owning token did not clear its slot and compatibility hold: %+v", st.FireSlot)
	}
}

func TestFallbackTokenIsSafeToShorten(t *testing.T) {
	for _, now := range []time.Time{time.Unix(0, 0), time.Unix(0, 1)} {
		if got := fallbackToken(now); len(got) < 8 {
			t.Fatalf("fallbackToken(%v) = %q, want at least 8 characters", now, got)
		}
	}
}

func TestEnqueueIsIdempotentAndPumpFiresOnce(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	first, err := service.Enqueue(ctx, "Owner/Repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Queued || first.Seq != 1 {
		t.Fatalf("first enqueue mismatch: %#v", first)
	}
	second, err := service.Enqueue(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyQueued || second.Queued {
		t.Fatalf("second enqueue mismatch: %#v", second)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Head != "abcdef123" {
		t.Fatalf("pump mismatch: %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one posted review command, got %d", len(gh.posted))
	}
	waiting, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Action != "waiting" {
		t.Fatalf("second pump should wait on in-flight review, got %#v", waiting)
	}
}

func TestPumpPersistsPostedReviewAfterTransientStateFailure(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	inner := NewMemoryStore(cfg)
	store := &failNthUpdateStore{StateStore: inner, n: 3, err: errors.New("transient state write failure")}
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Head != "abcdef123" {
		t.Fatalf("expected fired result after retrying posted-state write, got %#v", pumped)
	}
	state, _, err := inner.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Round("owner/repo", 12)
	if r == nil || r.Phase != PhaseFired || r.CommandID == 0 {
		t.Fatalf("posted review metadata was not persisted after retry: %#v", r)
	}
	// The firing PROCESS, in the form capabilities are recorded under: a bare
	// hostname here can never match a writer entry, so LaggingWriters would name
	// this very process as needing an upgrade for as long as the fire lasts.
	if r.ByHost != cfg.WriterID() {
		t.Errorf("ByHost = %q, want the writer id %q", r.ByHost, cfg.WriterID())
	}
	if state.FiredMarker("owner/repo", 12) != "abcdef123" {
		t.Fatalf("fired marker was not persisted after retry")
	}
	if r.FiredAt == nil || r.WaitDeadline == nil {
		t.Fatalf("feedback wait should be set on the fired round: %#v", r)
	}
	if r.WaitDeadline.Sub(*r.FiredAt) != cfg.FeedbackWaitTimeout {
		t.Fatalf("feedback wait deadline should use CRQ_FEEDBACK_WAIT_TIMEOUT, got %s", r.WaitDeadline.Sub(*r.FiredAt))
	}
}

func TestPumpAdoptsExistingReviewCommandWithoutRefiring(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	headTime := observedAt.Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	service.now = func() time.Time { return observedAt }

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason != "review command already posted" {
		t.Fatalf("expected pump to adopt the existing review command, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("adopting an existing review command must not post another one, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Round("owner/repo", 12)
	if r == nil || r.Phase != PhaseFired || r.CommandID != comment.ID || r.Head != "abcdef123" {
		t.Fatalf("existing review command should be persisted as a fired round, got %#v", r)
	}
	if state.FiredMarker("owner/repo", 12) != "abcdef123" {
		t.Fatalf("existing review command should restore fired dedupe state")
	}
	if r.FiredAt == nil || !r.FiredAt.Equal(comment.CreatedAt) {
		t.Fatalf("adopted review command should set the fired timestamp from the comment, got %#v", r)
	}
	if r.WaitDeadline == nil || !r.WaitDeadline.Equal(comment.CreatedAt.Add(cfg.FeedbackWaitTimeout)) {
		t.Fatalf("adopted review command should set the feedback wait deadline from the comment timestamp, got %#v", r)
	}
	if state.Account.FiresFrom == nil || !state.Account.FiresFrom.Equal(observedAt) {
		t.Fatalf("fire-log coverage = %v, want the command observation time %s",
			state.Account.FiresFrom, observedAt)
	}
}

func TestAdoptableCommandsRequiresExpectedHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", Host: "testhost", Bot: "coderabbitai[bot]", ReviewCommand: "@coderabbitai review"}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	service := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	comments, _ := gh.ListIssueComments(ctx, "owner/repo", 12)
	reviews, _ := gh.ListReviews(ctx, "owner/repo", 12)
	cmds, _, err := service.reviewCommands(ctx, service.cfg, "owner/repo", 12, engine.Observation{Head: "abcdef123", Open: true}, time.Time{}, pull, comments, reviews)
	if err != nil || len(cmds) != 0 {
		t.Fatalf("must not adopt a review command after the PR head changed, cmds=%v err=%v", cmds, err)
	}
}

func TestPumpDryRunDoesNotAdoptExistingCommand(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	// Seed a queued round directly (Enqueue writes, which DryRun would allow, but
	// the point is the pump adopts nothing and posts nothing).
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "dry_run" {
		t.Fatalf("dry-run pump must simulate, not adopt an existing command, got %#v", pumped)
	}
	if roundPhase(t, store, "owner/repo", 12) != PhaseQueued {
		t.Fatalf("dry-run pump must not mutate the round, got phase %s", roundPhase(t, store, "owner/repo", 12))
	}
}

func TestPumpIgnoresStaleCommandAfterRequeue(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(10 * time.Second), UpdatedAt: headTime.Add(10 * time.Second)}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{stale}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	// The round was requeued after a failed attempt: LastAttemptAt sits after the
	// stale command, so it must not be adopted.
	requeuedAt := stale.CreatedAt.Add(20 * time.Second)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("owner/repo", 12)
		r.LastAttemptAt = &requeuedAt
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a command older than the requeue must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command to be posted after requeue, posted=%v", gh.posted)
	}
	if st, _, _ := store.Load(ctx); st.Round("owner/repo", 12).CommandID == stale.ID {
		t.Fatalf("the round must track the fresh command, not the stale one")
	}
}

func TestPumpDoesNotAdoptCommandOlderThanForcePush(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	staleAt := commitTime.Add(10 * time.Minute)
	forcePushAt := commitTime.Add(30 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: staleAt, UpdatedAt: staleAt}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{stale}
	gh.graphQL = func(_ string, _ map[string]any, out any) error {
		payload := `{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"createdAt":"` + forcePushAt.Format(time.RFC3339) + `"}]}}}}`
		return json.Unmarshal([]byte(payload), out)
	}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a command older than the head force-push must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the force-pushed head, posted=%v", gh.posted)
	}
}

func TestPumpDoesNotAdoptCommandAlreadyAnsweredByReview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command}
	answered := ghapi.Review{SubmittedAt: commandAt.Add(5 * time.Minute), CommitID: "9876543210fedcba"}
	answered.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{answered}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("an already-answered command must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the new head, posted=%v", gh.posted)
	}
}

func TestPumpDoesNotAdoptCommandAlreadyAnsweredByCompletionReply(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 78, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: commandAt.Add(time.Minute), UpdatedAt: commandAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a completion-answered command must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the new head, posted=%v", gh.posted)
	}
}

func TestPumpAdoptsCompletionAnsweredCommandWhileTopSummaryIsProcessing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 78, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: commandAt.Add(time.Minute), UpdatedAt: commandAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	summary := ghapi.IssueComment{
		ID:        79,
		Body:      "<!-- review in progress by coderabbit.ai -->\nCurrently processing new changes in this PR. This may take a few minutes, please wait...",
		CreatedAt: commandAt.Add(-time.Hour),
		UpdatedAt: commandAt.Add(2 * time.Minute),
	}
	summary.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{summary, command, reply}
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason != "review command already posted" {
		t.Fatalf("the still-processing command must be adopted instead of replaced, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("processing must suppress a duplicate review trigger, posted=%v", gh.posted)
	}
}

func TestPumpDryRunDoesNotDedupeMutably(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	// The bot already reviewed the head, so DecideFire would dedupe.
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: time.Now().UTC()}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "deduped" {
		t.Fatalf("dry-run should report the dedupe it would perform, got %#v", pumped)
	}
	if roundPhase(t, store, "owner/repo", 12) != PhaseQueued {
		t.Fatalf("a dry-run dedupe must not mutate the round, got %s", roundPhase(t, store, "owner/repo", 12))
	}
}

func TestPumpSkipsAdoptionWhenCommitLookupFails(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gh.commitErrs[pull.Head.SHA] = errors.New("404 not found")
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: time.Now().UTC().Add(-30 * time.Second), UpdatedAt: time.Now().UTC().Add(-30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatalf("a failed head-commit lookup must not wedge the pump: %v", err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("expected pump to skip adoption and post a fresh command, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command to be posted, posted=%v", gh.posted)
	}
}

func TestPumpCompletesRoundWhenReviewSubmitted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: firedAt.Add(time.Minute)}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the submitted review to clear the in-flight slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("a submitted review must complete the round, got %s", p)
	}
}

func TestPumpCompletesRoundOnCompletionReply(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	command := ghapi.IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: firedAt.Add(time.Minute), UpdatedAt: firedAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	// A completion-only round is a re-review: a prior review must exist.
	prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{prior}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, command.ID)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the completion reply to clear the in-flight slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("a completion reply must complete the round, got %s", p)
	}
}

func TestPumpKeepsRoundReviewingOnCompletionReplyForUnreviewedPR(t *testing.T) {
	// CodeRabbit answered the first-ever review command with an instant "Review
	// finished" while the real review was still queued on its side. With no review
	// ever submitted, the round must not complete: it goes to reviewing (the slot
	// is released, but the round stays open) so the wait survives.
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	command := ghapi.IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished.", CreatedAt: firedAt.Add(5 * time.Second), UpdatedAt: firedAt.Add(5 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, command.ID)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must survive a completion reply on a never-reviewed PR (reviewing), got %s", p)
	}
	st, _, _ := store.Load(ctx)
	if st.WaitingHead("owner/repo", 12) != "abcdef123" {
		t.Fatalf("the feedback wait must survive a completion reply on a never-reviewed PR")
	}
}

func TestPumpKeepsRoundReviewingWhenBotOnlyReacted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	command := ghapi.IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command}
	reaction := ghapi.Reaction{}
	reaction.User.Login = cfg.Bot
	gh.reactions[5] = []ghapi.Reaction{reaction}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the reaction to release the slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a bare reaction means the review is still running — the round must stay reviewing, got %s", p)
	}
}

func TestPumpKeepsRoundOpenUntilAllRequiredBotsReview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{SubmittedAt: firedAt.Add(time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must stay open while a required bot has not reviewed, got %s", p)
	}

	// A required-bot review for a different commit must not complete it.
	staleCodex := ghapi.Review{SubmittedAt: firedAt.Add(2 * time.Minute), CommitID: "0123456789abcdef"}
	staleCodex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review, staleCodex}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a required-bot review for another commit must not complete the round, got %s", p)
	}

	// The head review completes it.
	codex := ghapi.Review{SubmittedAt: firedAt.Add(3 * time.Minute), CommitID: "abcdef1234567890"}
	codex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review, staleCodex, codex}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("once every required bot reviewed the head, the round must complete, got %s", p)
	}
}

func TestPumpSweepsReviewingRoundToCompletion(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	// A reviewing round whose slot was already released on a bot reaction.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, firedAt, 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must stay reviewing while the review is still running, got %s", p)
	}

	review := ghapi.Review{SubmittedAt: firedAt.Add(2 * time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("the sweep must complete a reviewing round whose review has landed, got %s", p)
	}
}

func TestPumpDryRunDoesNotSweepReviewing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, time.Now().UTC().Add(-2*time.Hour), 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a dry-run pump must not sweep/mutate a reviewing round, got %s", p)
	}
}

func TestPumpTreatsExistingReviewAdoptionRaceAsLostRace(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	loadState := DefaultState(cfg)
	r, _ := loadState.NewRound("owner/repo", 12, "abcdef123", headTime)
	loadState.PutRound(*r)
	service := NewService(cfg, gh, &adoptionRaceStore{cfg: cfg, loadState: loadState}, nil)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "lost_race" {
		t.Fatalf("expected adoption race to be a benign lost_race, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("adoption race must not post another review command, posted=%d", len(gh.posted))
	}
}

func TestFireRoundRechecksOrphanedSlotHold(t *testing.T) {
	for _, post := range []bool{false, true} {
		name := "adopt"
		if post {
			name = "post"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := firingConfig()
			gh := newFakeGitHub()
			store := NewMemoryStore(cfg)
			now := time.Now().UTC()
			seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, now, 0)
			if _, err := store.Update(ctx, func(st *State) error {
				until := now.Add(cfg.InflightTimeout)
				st.FireSlotHoldUntil = &until
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			round := *st.Round("owner/repo", 12)
			svc := NewService(cfg, gh, store, nil)
			got, err := svc.fireRound(ctx, cfg, round, engine.Observation{}, post, 77, now.Add(-time.Minute), "test", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Action != "lost_race" {
				t.Fatalf("action = %q, want lost_race while an orphaned hold is active", got.Action)
			}
			if len(gh.posted) != 0 {
				t.Fatalf("posted %d review commands while an orphaned hold is active", len(gh.posted))
			}
		})
	}
}

func TestRecordFireResetsRecordedAcrossRetry(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), retryNoChangeStore{cfg: cfg}, nil)
	round := Round{Repo: "owner/repo", PR: 12, Head: "abcdef123"}
	_, err := svc.recordFire(context.Background(), cfg, round, "token", 1, nil, time.Now().UTC(), time.Now().UTC())
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("expected no-change after retry lost the fire slot, got %v", err)
	}
}

func TestUnholdDoesNotPostNoticeAfterCASRetry(t *testing.T) {
	cfg := firingConfig()
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, retryUnholdStore{cfg: cfg}, nil)

	if _, err := svc.Unhold(context.Background(), "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("posted comments = %v, want none after losing the unhold race", gh.posted)
	}
}

func TestWaitReenqueuesAfterClearingStaleRound(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls["owner/repo#12"] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: now.Add(time.Second)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	// A stale fired round for a head that has since moved (abcdef123 → abcdef999).
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, now, 7)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" || result.Head != "abcdef999" {
		t.Fatalf("expected stale round to be superseded and the new head fired, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitReturnsWhenPRIsHeld(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.WaitTimeout = 0
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	// Code 0, not 2: the loop's codes are frozen with 2 meaning an elapsed wait,
	// and a hold is indefinite. Terminal like "skipped", named by the status.
	if code != 0 || result.Action != "held" || result.Reason != "held: waiting on a decision" {
		t.Fatalf("held PR should terminate the legacy wait without claiming a timeout, code=%d result=%#v", code, result)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Round("owner/repo", 12); got != nil {
		t.Fatalf("held PR enqueued a round: %+v", got)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("held PR posted %d review commands", len(gh.posted))
	}
}

func TestWaitReturnsWhenPRIsHeldAfterEnqueue(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.WaitTimeout = 100 * time.Millisecond
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := &holdAfterEnqueueStore{StateStore: NewMemoryStore(cfg)}
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "held" || result.Reason != "held: waiting on a decision" {
		t.Fatalf("hold created after enqueue should terminate the wait, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("held PR posted %d review commands", len(gh.posted))
	}
}

func TestLoopLeavesHeldInflightRoundOpen(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	firedAt := time.Now().Add(-time.Minute)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 7)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	report, code, err := service.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	// Code 0, not 2: 2 is frozen as the elapsed-wait result, and a hold ends
	// only when a person lifts it or the daemon observes a merge. The status is
	// what names the outcome.
	if code != 0 || report.Status != "held" || report.Reason != "held: waiting on a decision" {
		t.Fatalf("held in-flight loop result: code=%d report=%#v", code, report)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round("owner/repo", 12)
	if round == nil || round.Phase != PhaseFired {
		t.Fatalf("held in-flight round was completed: %+v", round)
	}
	if st.FireSlot == nil || st.FireSlot.Key != QueueKey("owner/repo", 12) {
		t.Fatalf("held in-flight round lost its fire slot: %+v", st.FireSlot)
	}
}

func TestWaitFiresRealReviewWhenOnlyCarriedThreadVisible(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	threadCreated := headTime.Add(-time.Hour).Format(time.RFC3339)
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			payload := `{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},` +
				`"nodes":[{"id":"THREAD1","isResolved":false,"isOutdated":false,"path":"a.go","line":1,` +
				`"comments":{"nodes":[{"databaseId":55,"body":"Carried-over finding","url":"http://x","path":"a.go","line":1,` +
				`"createdAt":"` + threadCreated + `","author":{"login":"coderabbitai[bot]"},"commit":{"oid":"fedcba9876543210"}}]}}]}}}}`
			return json.Unmarshal([]byte(payload), out)
		}
		return json.Unmarshal([]byte(`{}`), out)
	}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a carried-over thread on a freshly pushed head must fire a real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitReturnsCurrentHeadFeedbackBeforeReviewSlot(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 91, Body: "Actionable Codex finding on the queued head", CreatedAt: headTime.Add(time.Second), UpdatedAt: headTime.Add(time.Second)}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	service := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || result.Reason != "feedback already available" {
		t.Fatalf("current-head feedback must end the slot wait immediately, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("known-bad head must not spend a review slot, posted=%d", len(gh.posted))
	}
}

func TestLoopReturnsLatePreviousHeadFeedbackAfterQueueing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]"}
	cfg.WaitTimeout = 25 * time.Millisecond
	cfg.PollInterval = time.Millisecond
	gh := newFakeGitHub()
	queuedAt := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{
		ID:          7,
		Body:        "**Actionable comments posted: 1**\n<details><summary>🤖 Prompt for all review comments with AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Delayed finding from the previous head.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: queuedAt.Add(time.Second),
	}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, queuedAt, 0)
	blockedUntil := queuedAt.Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	report, code, err := service.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) != 1 {
		t.Fatalf("a delayed review must end the loop with feedback, code=%d report=%#v", code, report)
	}
	if report.Reason != "hold current head: fix locally, but do not commit or push until every required reviewer finishes" {
		t.Fatalf("the delayed old-head review must not satisfy the current reviewer gate: %#v", report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("known feedback must not spend a review slot, posted=%d", len(gh.posted))
	}
}

func TestWaitFiresRealReviewWhenOnlyCarriedReviewPromptVisible(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a carried-over review prompt on a new head must fire a real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitRepairsPoisonedCompletedRoundWithOnlyCarriedReviewPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	// A poisoned completed round at the head with no real head review.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseCompleted, headTime, 0)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a poisoned completed round must be repaired before firing the real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one replacement review command, posted=%d", len(gh.posted))
	}
}

func TestNeedsReviewToleratesBotSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", Scope: []string{"o"}, ReviewDoneMarker: "summarize by coderabbit.ai"}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 5)] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{review}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	need, _, err := svc.needsReview(context.Background(), DefaultState(cfg), "o/repo", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("incremental autoreview should not re-enqueue a head already reviewed by a suffixed bot login")
	}

	gh.reviews[fakeKey("o/repo", 5)] = nil
	comment := ghapi.IssueComment{Body: "finished; summarize by coderabbit.ai"}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 5)] = []ghapi.IssueComment{comment}
	need, _, err = svc.needsReview(context.Background(), DefaultState(cfg), "o/repo", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("first-review autoreview should not re-enqueue a PR with a suffixed bot completion comment")
	}
}

func TestNeedsReviewSkipsTrackedHeadButNotNewHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Bot: "coderabbitai", Host: "h"}
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head + "aaaaaa0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	// A round parked awaiting retry at the current head: Pump owns re-firing it, so
	// autoreview must not re-enqueue.
	seedRound(t, store, cfg, "o/carrier", 82, head, PhaseFired, time.Now().UTC(), 1)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("o/carrier", 82)
		if err := r.AwaitRetry(time.Now().UTC().Add(40*time.Minute), "rate limited", time.Now().UTC()); err != nil {
			return err
		}
		st.PutRound(*r)
		st.FireSlot = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)

	need, _, err := svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("needsReview must skip a head already tracked by an awaiting_retry round")
	}

	// A new head is not blocked by the prior head's parked round.
	pull.Head.SHA = "bbbbbbbbbccccc"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	need, _, err = svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("a new head must not be blocked by a prior head's parked round")
	}
}

func TestPumpDropsClosedPRWithoutFiring(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr closed" {
		t.Fatalf("expected a closed PR to be dropped, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("must not post a review to a closed PR, posted %d", len(gh.posted))
	}
	if containsActiveRound(store, t, "owner/repo", 12) {
		t.Fatal("closed PR should have been removed from the queue")
	}
}

func TestPumpDropsClosedPRWhileReviewQuotaIsBlocked(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	// The round was enqueued while open; the PR is now merged with a deleted head.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	blockedUntil := time.Now().UTC().Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		st.Account.Source = "calibrate"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr closed" {
		t.Fatalf("expected merged PR cleanup to bypass the quota block, got %#v", pumped)
	}
	if containsActiveRound(store, t, "owner/repo", 12) {
		t.Fatal("merged PR should be removed even while review quota is blocked")
	}
	if len(gh.posted) != 0 {
		t.Fatalf("must not post a review to a merged PR, posted %d", len(gh.posted))
	}
}

func TestPumpDropsMergedPRWhileHeld(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "closed", Merged: true}
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr merged" {
		t.Fatalf("expected merged hold cleanup, got %#v", pumped)
	}
	if containsActiveRound(store, t, "owner/repo", 12) {
		t.Fatal("closed PR should not remain active merely because it is held")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/repo", 12); held {
		t.Fatal("merged PR remained in the held list")
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0], "<!-- crq:hold-merged -->") {
		t.Fatalf("posted comments = %v, want merged-hold notice", gh.posted)
	}
	found := false
	for _, archived := range st.Archive {
		if archived.Repo == "owner/repo" && archived.PR == 12 && archived.Phase == PhaseAbandoned {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("closed PR cleanup did not preserve the abandoned round in the archive")
	}
}

func TestSweepMergedHoldDoesNotRetireAReplacementHoldAfterCASRetry(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/repo", 12)] = ghapi.Pull{State: "closed", Merged: true}
	now := time.Now().UTC()
	svc := NewService(cfg, gh, retryMergedHoldStore{cfg: cfg, at: now}, nil)
	st := DefaultState(cfg)
	st.HoldWithToken("owner/repo", 12, "original hold", "operator", now, "first")

	_, result, changed, err := svc.sweepMergedHold(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || result.Action != "lost_race" {
		t.Fatalf("result = %#v, changed = %t; want lost race", result, changed)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("posted comments = %v, want none for replacement hold", gh.posted)
	}
}

func TestSweepMergedHoldRemovesRetirementNoticeIfReheldBeforePostCompletes(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/repo", 12)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setHoldCapableLeader(t, ctx, store, now)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	gh.postHook = func() {
		if _, err := svc.Hold(ctx, "owner/repo", 12, "waiting for security approval"); err != nil {
			t.Errorf("re-hold during retirement post: %v", err)
		}
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, result, changed, err := svc.sweepMergedHold(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || result.Action != "skipped" {
		t.Fatalf("result = %#v, changed = %t; want merged hold retired", result, changed)
	}
	if len(gh.deleteCalls) != 1 || gh.deleteCalls[0] != 1 {
		t.Fatalf("delete calls = %v, want the stale retirement notice", gh.deleteCalls)
	}
	latest, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hold, held := latest.HeldPR("owner/repo", 12)
	if !held || hold.Reason != "waiting for security approval" {
		t.Fatalf("replacement hold = %+v, held = %t", hold, held)
	}
}

func TestDryRunReportsMergedHeldPRWithoutMutatingState(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/repo", 12)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting on a decision", "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr merged" {
		t.Fatalf("dry-run should report merged hold cleanup, got %#v", pumped)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("owner/repo", 12); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("dry-run mutated held round: %#v", round)
	}
	if _, held := st.HeldPR("owner/repo", 12); !held {
		t.Fatal("dry-run removed the hold")
	}
}

func TestPumpRemovesMergedHoldWithoutARound(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/repo", 62)] = ghapi.Pull{State: "closed", Merged: true}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 62, "waiting on a decision", "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr merged" {
		t.Fatalf("merged standalone hold cleanup = %#v", pumped)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/repo", 62); held {
		t.Fatal("merged PR's standalone hold was not removed")
	}
}

func TestPumpContinuesAfterMergedHoldCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/merged", 62)] = ghapi.Pull{State: "closed", Merged: true}
	ready := ghapi.Pull{State: "open"}
	ready.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/ready", 63)] = ready
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/merged", 62, "waiting on a decision", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, "owner/ready", 63, "abcdef123", PhaseQueued, now, 0)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Repo != "owner/ready" || pumped.PR != 63 {
		t.Fatalf("pump = %#v, want the ready PR to fire", pumped)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/merged", 62); held {
		t.Fatal("merged hold remained after pump")
	}
}

func TestDryRunPumpContinuesAfterMergedHoldCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/merged", 62)] = ghapi.Pull{State: "closed", Merged: true}
	ready := ghapi.Pull{State: "open"}
	ready.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/ready", 63)] = ready
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/merged", 62, "waiting on a decision", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, store, cfg, "owner/ready", 63, "abcdef123", PhaseQueued, now, 0)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "dry_run" || pumped.Repo != "owner/ready" || pumped.PR != 63 {
		t.Fatalf("pump = %#v, want the ready PR's dry-run decision", pumped)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/merged", 62); !held {
		t.Fatal("dry run removed the merged hold")
	}
}

func TestPumpKeepsClosedUnmergedPRHeld(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pulls[fakeKey("owner/repo", 12)] = ghapi.Pull{State: "closed"}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "may be reopened", "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "idle" {
		t.Fatalf("closed unmerged hold changed pump result: %#v", pumped)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/repo", 12); !held {
		t.Fatal("closed unmerged PR lost the hold needed if it is reopened")
	}
}

func TestMergedHoldSweepPropagatesGitHubThrottle(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.pullErrs[fakeKey("owner/repo", 12)] = &ghapi.RateLimitError{Kind: "primary"}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("owner/repo", 12, "waiting", "operator", time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Pump(ctx); !ghapi.IsThrottled(err) {
		t.Fatalf("Pump error = %v, want GitHub throttle for daemon backoff", err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("owner/repo", 12); !held {
		t.Fatal("failed merged-state read removed the hold")
	}
}

func containsActiveRound(store StateStore, t *testing.T, repo string, pr int) bool {
	t.Helper()
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st.ContainsActive(repo, pr)
}

func TestEnqueueDedupesAlreadyReviewedHead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#7"] = pull
	store := NewMemoryStore(cfg)
	// A completed round at the head is the dedup marker.
	seedRound(t, store, cfg, "owner/repo", 7, "abcdef123", PhaseCompleted, time.Now().UTC(), 1)
	service := NewService(cfg, gh, store, nil)
	result, err := service.Enqueue(ctx, "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deduped || result.Head != "abcdef123" {
		t.Fatalf("expected dedupe, got %#v", result)
	}
}

func TestRateLimitedRoundParksAndBlocksAccount(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Bot = "coderabbitai"
	cfg.InflightTimeout = time.Hour
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head + "abcdef0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Enqueue(ctx, "o/carrier", 82); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "fired" {
		t.Fatalf("expected first pump to fire, got %#v", res)
	}

	// CodeRabbit answers with a rate-limit comment and an already-reviewed claim,
	// but no review object. The round must park (retry later), not complete.
	answer := time.Now().UTC().Add(time.Minute)
	rl := ghapi.IssueComment{ID: 501, Body: "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**", CreatedAt: answer, UpdatedAt: answer}
	rl.User.Login = "coderabbitai[bot]"
	ack := ghapi.IssueComment{ID: 502, Body: "<details><summary>✅ Action performed</summary>\n\nReview finished.\n\n> Note: CodeRabbit is an incremental review system and does not re-review already reviewed commits.</details>", CreatedAt: answer, UpdatedAt: answer}
	ack.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/carrier", 82)] = []ghapi.IssueComment{rl, ack}

	res, err = svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "requeued" || res.Reason != warnRateLimited {
		t.Fatalf("expected rate-limited requeue, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	if r := st.Round("o/carrier", 82); r == nil || r.Phase != PhaseAwaitingRetry {
		t.Fatalf("a rate-limited round must park awaiting retry, got %#v", r)
	}
	if st.FiredMarker("o/carrier", 82) != "" {
		t.Fatalf("a parked round must not be a dedup marker")
	}
	if st.Account.BlockedUntil == nil {
		t.Fatal("the real rate-limit response must block the next attempt")
	}
	if !st.ContainsActive("o/carrier", 82) {
		t.Fatal("the unreviewed PR must remain active")
	}

	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	fires := 0
	for _, p := range gh.posted {
		if strings.HasSuffix(p, ":@coderabbitai review") {
			fires++
		}
	}
	if fires != 1 {
		t.Fatalf("expected exactly one review fire, got %d (%v)", fires, gh.posted)
	}
}

func TestRotateCalibrationPersistsNewIssue(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "o/gate", CalibrationPR: 1, Scope: []string{"o"}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	n, err := svc.rotateCalibration(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("expected a fresh issue number, got %d", n)
	}
	st, _, _ := store.Load(ctx)
	if st.CalibrationIssue != n {
		t.Fatalf("expected state.CalibrationIssue=%d, got %d", n, st.CalibrationIssue)
	}
	if svc.calibrationIssue(st) != n {
		t.Fatalf("calibrationIssue should return the rotated issue %d, got %d", n, svc.calibrationIssue(st))
	}
	if len(gh.createdIssues) != 1 {
		t.Fatalf("expected exactly one issue created, got %d", len(gh.createdIssues))
	}
}

func TestMemoryStoreConcurrentUpdatesDoNotLoseMutations(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", StateRef: "crq-state-v3"}
	store := NewMemoryStore(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(ctx, func(st *State) error {
				st.NextSeq++
				return nil
			})
			if err != nil {
				t.Errorf("update failed: %v", err)
			}
		}()
	}
	wg.Wait()
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.NextSeq != 50 {
		t.Fatalf("lost updates: got %d want 50", state.NextSeq)
	}
}

func TestParseAvailableInHandlesMarkdownAndColon(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 24, 0, 0, time.UTC)
	body := "## Review limit reached\n\nYou have reached your review limit.\n\n**Next review available in:** **40 minutes**"
	got := dialect.ParseAvailableIn(body, base)
	if got == nil {
		t.Fatal("expected a parsed reset for the verbatim rate-limit body, got nil")
	}
	if want := base.Add(40 * time.Minute); !got.Equal(want) {
		t.Fatalf("expected reset %v (base+40m), got %v", want, *got)
	}
}

func TestParseAvailableInPlainFormatStillWorks(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC)
	got := dialect.ParseAvailableIn("You are rate limited. Reviews available in 3 minutes.", base)
	if got == nil || !got.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("expected base+3m for the plain format, got %v", got)
	}
	got = dialect.ParseAvailableIn("available in 1 hour and 30 minutes", base)
	if got == nil || !got.Equal(base.Add(90*time.Minute)) {
		t.Fatalf("expected base+90m for compound duration, got %v", got)
	}
}

// TestRefreshQuotaPreservesBlockOnInconclusiveProbe pins the anti-spam fix for
// the calibration path: when a probe is still pending (no fresh reply), a live
// account block must survive the refresh instead of being wiped after the TTL,
// which would let Pump fire queued reviews inside the blocked window.
func TestRefreshQuotaPreservesBlockOnInconclusiveProbe(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo: "owner/gate", StateRef: "crq-state-v3", CalibrationPR: 1,
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		RateLimitCommand:  "@coderabbitai rate limit",
		CalibrationTTL:    2 * time.Minute, Scope: []string{"owner"},
	}
	gh := newFakeGitHub() // no calibration reply on the gate issue → probe stays inconclusive
	store := NewMemoryStore(cfg)
	now := time.Now().UTC()
	block := now.Add(30 * time.Minute)
	askedAt := now.Add(-time.Minute) // a pending probe, still within the TTL window
	checkedAt := now.Add(-time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &block
		st.Account.CalibAskedAt = &askedAt
		st.Account.CheckedAt = &checkedAt
		st.Account.Source = "warning"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	updated, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Account.BlockedUntil == nil || !updated.Account.BlockedUntil.Equal(block) {
		t.Fatalf("an inconclusive probe must preserve the live block %v, got %v", block, updated.Account.BlockedUntil)
	}
}

// TestLoopDegradesToCodexOnlyOnRateLimit: a rate-limited round with Codex
// activity must return Codex findings promptly — marked deferred — instead of
// waiting out the CodeRabbit window.
func TestLoopDegradesToCodexOnlyOnRateLimit(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.FeedbackBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots) // Codex enabled as a co-reviewer
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head + "abcdef0"
	gh.pulls[fakeKey("o/carrier", 90)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Enqueue(ctx, "o/carrier", 90); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.Pump(ctx); err != nil || res.Action != "fired" {
		t.Fatalf("expected fire, got %#v err=%v", res, err)
	}
	answer := time.Now().UTC().Add(time.Second)
	rl := ghapi.IssueComment{ID: 501, Body: "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**", CreatedAt: answer, UpdatedAt: answer}
	rl.User.Login = "coderabbitai[bot]"
	finding := ghapi.IssueComment{ID: 502, Body: "Actionable Codex finding on the current head", CreatedAt: answer.Add(time.Second), UpdatedAt: answer.Add(time.Second)}
	finding.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/carrier", 90)] = []ghapi.IssueComment{rl, finding}
	if res, err := svc.Pump(ctx); err != nil || res.Action != "requeued" {
		t.Fatalf("expected rate-limited requeue, got %#v err=%v", res, err)
	}

	begin := time.Now()
	report, code, err := svc.Loop(ctx, "o/carrier", 90)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) == 0 {
		t.Fatalf("degraded loop must return the codex findings, code=%d report=%#v", code, report)
	}
	if !report.CodeRabbitDeferred || report.DeferredUntil == nil {
		t.Fatalf("the report must mark coderabbit deferred with a window, got %#v", report)
	}
	if report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("coderabbit must still read as unreviewed: %#v", report.ReviewedBy)
	}
	if report.Converged {
		t.Fatal("a deferred round must never read as converged")
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("degraded loop must not wait out the rate-limit window, took %s", elapsed)
	}
}

// A rate-limit notice observed by this Feedback call must affect the same
// report. The default feedback-only Codex setup supplies a SHA-bound clean
// verdict even though Completion.ReviewedBy contains only the pending
// CodeRabbit key.
func TestFeedbackUsesNewAccountBlockToDeferCleanAutoCodex(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.FeedbackBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots) // Codex enabled as a co-reviewer
	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/carrier", 93)] = pull
	cleanAt := time.Now().UTC().Add(-time.Minute)
	clean := ghapi.IssueComment{ID: 701,
		Body:      "Codex Review: Didn't find any major issues. :tada:\n\n**Reviewed commit:** `abcdef1234`",
		CreatedAt: cleanAt, UpdatedAt: cleanAt}
	clean.User.Login = "chatgpt-codex-connector[bot]"
	rateLimitedAt := cleanAt.Add(time.Second)
	rateLimited := ghapi.IssueComment{ID: 702,
		Body:      "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **30 minutes**",
		CreatedAt: rateLimitedAt, UpdatedAt: rateLimitedAt}
	rateLimited.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/carrier", 93)] = []ghapi.IssueComment{clean, rateLimited}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "o/carrier", 93, "abcdef123", PhaseQueued, cleanAt.Add(-time.Minute), 0)

	svc := NewService(cfg, gh, store, nil)
	report, err := svc.Feedback(ctx, "o/carrier", 93)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "deferred" || !report.CodeRabbitDeferred || report.Converged {
		t.Fatalf("clean feedback-only codex must produce a non-converged deferred report, got %#v", report)
	}
	if len(report.ReviewedBy) != 1 || report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("default required-bot state must remain coderabbit-only and pending, got %#v", report.ReviewedBy)
	}
}

// TestLoopDeferredCodexCleanExitsZeroNotConverged: a clean Codex verdict on a
// degraded round exits 0 as "deferred" without completing the round — the
// CodeRabbit review stays owed.
func TestLoopDeferredCodexCleanExitsZeroNotConverged(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.SettleWindow = 0
	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/carrier", 91)] = pull
	store := NewMemoryStore(cfg)
	started := time.Now().UTC().Add(-time.Minute)
	seedRound(t, store, cfg, "o/carrier", 91, "abcdef123", PhaseReviewing, started, 1001)
	blockedUntil := time.Now().UTC().Add(30 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clean := ghapi.IssueComment{ID: 700, Body: "## Codex Review\n\nDidn't find any major issues. Keep them coming!", CreatedAt: started.Add(30 * time.Second), UpdatedAt: started.Add(30 * time.Second)}
	clean.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/carrier", 91)] = []ghapi.IssueComment{clean}

	svc := NewService(cfg, gh, store, nil)
	report, code, err := svc.Loop(ctx, "o/carrier", 91)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || report.Status != "deferred" {
		t.Fatalf("clean codex on a degraded round must exit 0 as deferred, code=%d report=%#v", code, report)
	}
	if report.Converged {
		t.Fatal("deferred must not be converged")
	}
	st, _, _ := store.Load(ctx)
	if r := st.Round("o/carrier", 91); r == nil || r.Phase == PhaseCompleted {
		t.Fatalf("a deferred round must not be completed, got %#v", r)
	}
	if !st.ContainsActive("o/carrier", 91) {
		t.Fatal("the coderabbit review must stay owed (round active)")
	}
	if _, ok := st.WorkClaim("o/carrier", 91, time.Now().UTC()); ok {
		t.Fatal("deferred loop left the interactive work claim behind")
	}
}

// TestPumpPostsCodexDeferredDuringBlockThenFiresCodeRabbit: during a block the
// pump posts only the Codex command and keeps the round queued; once the
// window passes it fires the CodeRabbit review without re-posting Codex.
func TestPumpPostsCodexDeferredDuringBlockThenFiresCodeRabbit(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/carrier", 92)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	if _, err := svc.Enqueue(ctx, "o/carrier", 92); err != nil {
		t.Fatal(err)
	}
	blockedUntil := now.Add(30 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "codex_fired" {
		t.Fatalf("expected the codex-deferred fire, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	r := st.Round("o/carrier", 92)
	if r == nil || r.Phase != PhaseQueued || r.CodexCommandID == 0 {
		t.Fatalf("the round must stay queued with the codex command recorded, got %#v", r)
	}
	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	countPosts := func(suffix string) int {
		n := 0
		for _, p := range gh.posted {
			if strings.HasSuffix(p, suffix) {
				n++
			}
		}
		return n
	}
	if countPosts(":@codex review") != 1 {
		t.Fatalf("exactly one codex command must be posted during the block, got %v", gh.posted)
	}
	if countPosts(":@coderabbitai review") != 0 {
		t.Fatalf("no coderabbit review may fire during the block, got %v", gh.posted)
	}

	// Window opens: the queued round fires CodeRabbit, without re-posting Codex.
	now = blockedUntil.Add(time.Minute)
	res, err = svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "fired" {
		t.Fatalf("expected the coderabbit fire after the window, got %#v", res)
	}
	if countPosts(":@coderabbitai review") != 1 || countPosts(":@codex review") != 1 {
		t.Fatalf("after the window: one coderabbit fire, still one codex command, got %v", gh.posted)
	}
}

func TestPumpScansPastBlockedRoundWithCodexAlreadyRequested(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	for _, target := range []struct {
		repo string
		pr   int
		sha  string
	}{
		{repo: "o/first", pr: 1, sha: "111111111abcdef0"},
		{repo: "o/second", pr: 2, sha: "222222222abcdef0"},
	} {
		pull := ghapi.Pull{State: "open"}
		pull.Head.SHA = target.sha
		gh.pulls[fakeKey(target.repo, target.pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	if _, err := svc.Enqueue(ctx, "o/first", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/second", 2); err != nil {
		t.Fatal(err)
	}
	blockedUntil := now.Add(30 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		first := st.Round("o/first", 1)
		first.CodexCommandID = 700
		commandedAt := now.Add(-time.Minute)
		first.CodexCommandedAt = &commandedAt
		st.PutRound(*first)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "codex_fired" || res.Repo != "o/second" || res.PR != 2 {
		t.Fatalf("the blocked front round must not starve a later codex round, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	if second := st.Round("o/second", 2); second == nil || second.CodexCommandID == 0 {
		t.Fatalf("the later round must record its codex command, got %#v", second)
	}
}

func TestPumpAdoptsExistingCodexCommandDuringBlock(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-2 * time.Minute)
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/carrier", 94)] = pull
	commit := ghapi.Commit{SHA: pull.Head.SHA}
	commit.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = commit
	commandAt := headTime.Add(time.Minute)
	command := ghapi.IssueComment{ID: 702, Body: cfg.CoBots[0].Command, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey("o/carrier", 94)] = []ghapi.IssueComment{command}
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := svc.Enqueue(ctx, "o/carrier", 94); err != nil {
		t.Fatal(err)
	}
	blockedUntil := time.Now().UTC().Add(30 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "codex_adopted" {
		t.Fatalf("expected the existing codex command to be adopted, got %#v", res)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("adopting the codex command must not post another command, got %v", gh.posted)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := st.Round("o/carrier", 94)
	if r == nil || r.Phase != PhaseQueued || r.CodexCommandID != command.ID ||
		r.CodexCommandedAt == nil || !r.CodexCommandedAt.Equal(commandAt) {
		t.Fatalf("the queued round must persist the adopted codex anchor, got %#v", r)
	}
}

func TestFireCoDeferredAdoptionHonorsDryRunAndActiveClaim(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RateLimitCoDegrade = true
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	now := time.Now().UTC()
	seedRound(t, store, cfg, "o/carrier", 95, "abcdef123", PhaseQueued, now.Add(-time.Minute), 0)
	svc := NewService(cfg, gh, store, nil)
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round("o/carrier", 95)

	svc.cfg.DryRun = true
	res, err := svc.fireCoDeferred(ctx, svc.cfg, round, engine.FireDecision{Verdict: engine.FireCoDeferred, Reason: "dry run adopt", AdoptCo: map[string]engine.CommandSeen{dialect.CodexBotLogin: {ID: 703, CreatedAt: now.Add(-time.Minute)}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "codex_fired" || len(gh.posted) != 0 {
		t.Fatalf("dry run must report the existing simulated action without posting, got %#v posts=%v", res, gh.posted)
	}
	st, _, _ = store.Load(ctx)
	if got := st.Round("o/carrier", 95); got == nil || got.CodexCommandID != 0 {
		t.Fatalf("dry run must not persist the adopted command, got %#v", got)
	}

	svc.cfg.DryRun = false
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("o/carrier", 95)
		r.CodexClaimedAt = &now
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err = svc.fireCoDeferred(ctx, svc.cfg, round, engine.FireDecision{Verdict: engine.FireCoDeferred, Reason: "claimed adopt", AdoptCo: map[string]engine.CommandSeen{dialect.CodexBotLogin: {ID: 703, CreatedAt: now.Add(-time.Minute)}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "deduped" {
		t.Fatalf("a fresh in-flight claim must defer adoption, got %#v", res)
	}
	st, _, _ = store.Load(ctx)
	if got := st.Round("o/carrier", 95); got == nil || got.CodexCommandID != 0 || got.CodexClaimedAt == nil {
		t.Fatalf("the active claim must remain untouched, got %#v", got)
	}

	staleNow := now.Add(triggerClaimTTL + time.Second)
	res, err = svc.fireCoDeferred(ctx, svc.cfg, round, engine.FireDecision{Verdict: engine.FireCoDeferred, Reason: "stale claim adopt", AdoptCo: map[string]engine.CommandSeen{dialect.CodexBotLogin: {ID: 703, CreatedAt: now.Add(-time.Minute)}}}, staleNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "codex_adopted" {
		t.Fatalf("an expired claim must allow adoption recovery, got %#v", res)
	}
	st, _, _ = store.Load(ctx)
	if got := st.Round("o/carrier", 95); got == nil || got.CodexCommandID != 703 || got.CodexClaimedAt != nil {
		t.Fatalf("the recovered adoption must replace the stale claim, got %#v", got)
	}
}

// TestPumpRescuesSummaryOnlyRoundBehindBlockedQueue pins a starvation bug found
// by dogfooding on a private CodeRabbit-Free repo. A summary-only round needs no
// CodeRabbit quota and no fire slot — no review is ever coming from CodeRabbit —
// yet it sat queued behind a rate-limited round from another PR. Pump's bounded
// rescue scan DID observe it and DID compute its verdict, then threw the verdict
// away because it only accepted FireCoDeferred, leaving the round queued with an
// empty note while its Codex review went unused.
func TestPumpRescuesSummaryOnlyRoundBehindBlockedQueue(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	// Front of the queue: another PR, blocked, whose Codex trigger is already
	// posted — so it yields FireNo and nothing more can be done for it.
	frontSHA := "1111111111111111"
	frontPull := ghapi.Pull{State: "open"}
	frontPull.Head.SHA = frontSHA
	gh.pulls[fakeKey("o/front", 10)] = frontPull

	// Behind it: a summary-only PR. CodeRabbit posted only its Free-plan
	// walkthrough, which is the whole review it will ever produce.
	backSHA := "2222222222222222"
	backPull := ghapi.Pull{State: "open"}
	backPull.Head.SHA = backSHA
	gh.pulls[fakeKey("o/back", 20)] = backPull
	walkthrough := ghapi.IssueComment{
		ID:        900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/back", 20)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.Enqueue(ctx, "o/front", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/back", 20); err != nil {
		t.Fatal(err)
	}
	blockedUntil := now.Add(30 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		// The front round has already asked Codex, so the degrade has nothing
		// left to post for it and it can only report FireNo while blocked.
		r := st.Round("o/front", 10)
		r.SetCoCommand(dialect.CodexBotLogin, 701, now.Add(-2*time.Minute))
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The blocked front round must not swallow the pump: the summary-only round
	// behind it gets resolved instead of staying queued.
	if res.Repo != "o/back" || res.PR != 20 {
		t.Fatalf("the summary-only round behind the block must be rescued, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	back := st.Round("o/back", 20)
	if back == nil || back.Phase == PhaseQueued {
		t.Fatalf("the summary-only round must leave the queue, got %#v", back)
	}
	// Whatever the resolution, it must never have spent CodeRabbit quota: no
	// `@coderabbitai review` posted and no fire slot taken.
	for _, p := range gh.posted {
		if strings.Contains(p, cfg.ReviewCommand) {
			t.Fatalf("a summary-only round must never post the CodeRabbit command, posted=%v", gh.posted)
		}
	}
	if st.FireSlot != nil {
		t.Fatalf("a summary-only round must not take the fire slot, got %#v", st.FireSlot)
	}
}

// TestPumpSweepsQuotaFreeRoundWhileTheSlotIsHeld pins the rule the fire slot
// actually stands for: it serializes ONE account-metered review, and has no
// authority over work that spends none.
//
// Pump used to return the moment it had progressed the slot holder. Its own
// rescue scan claimed to cover "blocked or slot-busy", but the slot-busy half
// was unreachable — control never got there. So a summary-only round, whose
// CodeRabbit review is never coming at all, sat queued for as long as an
// unrelated PR's review ran.
func TestPumpSweepsQuotaFreeRoundWhileTheSlotIsHeld(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	// The slot holder: an unrelated PR whose CodeRabbit review is in flight.
	holderSHA := "1111111111111111"
	holder := ghapi.Pull{State: "open"}
	holder.Head.SHA = holderSHA
	gh.pulls[fakeKey("o/holder", 10)] = holder

	// Behind it: a summary-only PR. CodeRabbit posted only its Free-plan
	// walkthrough, which is the whole review it will ever produce.
	backSHA := "2222222222222222"
	backPull := ghapi.Pull{State: "open"}
	backPull.Head.SHA = backSHA
	gh.pulls[fakeKey("o/back", 20)] = backPull
	walkthrough := ghapi.IssueComment{
		ID:        900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/back", 20)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	// The holder is fired and holds the slot; the summary-only round is queued
	// behind it.
	seedRound(t, store, cfg, "o/holder", 10, holderSHA, PhaseFired, now.Add(-time.Minute), 701)
	if _, err := svc.Enqueue(ctx, "o/back", 20); err != nil {
		t.Fatal(err)
	}
	if st, _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	} else if st.SlotRound() == nil {
		t.Fatal("the test needs the slot held to mean anything")
	}

	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}

	st, _, _ := store.Load(ctx)
	back := st.Round("o/back", 20)
	if back == nil || back.Phase == PhaseQueued {
		t.Fatalf("the summary-only round must move while the slot is held, got %#v", back)
	}
	// The slot is not what let it move, and it did not take it.
	if slot := st.SlotRound(); slot == nil || slot.Repo != "o/holder" {
		t.Fatalf("the slot must still belong to the holder, got %#v", slot)
	}
	for _, p := range gh.posted {
		if strings.Contains(p, cfg.ReviewCommand) {
			t.Fatalf("a quota-free sweep must never post the CodeRabbit command, posted=%v", gh.posted)
		}
	}
}

func TestPumpSweepsQuotaFreeRoundWhileAnOrphanedHoldIsActive(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	frontSHA := "111111111"
	front := ghapi.Pull{State: "open"}
	front.Head.SHA = frontSHA + "abcdef0"
	gh.pulls[fakeKey("o/front", 10)] = front

	backSHA := "2222222222222222"
	back := ghapi.Pull{State: "open"}
	back.Head.SHA = backSHA
	gh.pulls[fakeKey("o/back", 20)] = back
	walkthrough := ghapi.IssueComment{
		ID:        900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/back", 20)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	seedRound(t, store, cfg, "o/front", 10, frontSHA, PhaseQueued, now.Add(-time.Minute), 0)
	if _, err := svc.Enqueue(ctx, "o/back", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		holdUntil := now.Add(cfg.InflightTimeout)
		st.FireSlotHoldUntil = &holdUntil
		frontRound := st.Round("o/front", 10)
		frontRound.SetCoCommand(dialect.CodexBotLogin, 701, now.Add(-time.Minute))
		st.PutRound(*frontRound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Repo != "o/back" || res.PR != 20 {
		t.Fatalf("the quota-free round behind an orphaned hold must be rescued, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	if round := st.Round("o/back", 20); round == nil || round.Phase == PhaseQueued {
		t.Fatalf("the quota-free round must leave the queue, got %#v", round)
	}
	for _, posted := range gh.posted {
		if strings.Contains(posted, cfg.ReviewCommand) {
			t.Fatalf("the rescue must not spend primary review quota, posted=%v", gh.posted)
		}
	}
}

// TestWaitResolvesSummaryOnlyWithoutTheQueue pins the architectural rule the
// dogfood exposed: a summary-only round is NOT a queue citizen. The Seq FIFO and
// the FireSlot exist solely to serialize CodeRabbit's account-wide review limit,
// and a CodeRabbit-Free private repo never produces a review at all — so neither
// an account block, nor another PR holding the slot, may delay it. Its
// co-reviewers are ready now; the loop must run them now.
//
// The starvation is staged at its worst: another PR HOLDS the fire slot. Pump
// now sweeps quota-free rounds even then (see
// TestPumpSweepsQuotaFreeRoundWhileTheSlotIsHeld); this pins that Wait resolves
// the round directly, without depending on a pump happening to run.
func TestWaitResolvesSummaryOnlyWithoutTheQueue(t *testing.T) {
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	cfg.PollInterval = 10 * time.Millisecond
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	// Another PR holds the fire slot: every Pump pass returns after progressing
	// it, so the FIFO below is never consulted.
	frontPull := ghapi.Pull{State: "open"}
	frontPull.Head.SHA = "1111111111111111"
	gh.pulls[fakeKey("o/front", 10)] = frontPull

	sha := "2222222222222222"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/private", 20)] = pull
	walkthrough := ghapi.IssueComment{
		ID:        900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/private", 20)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }

	seedRound(t, store, cfg, "o/front", 10, "111111111", PhaseFired, now.Add(-time.Minute), 500)
	blockedUntil := now.Add(45 * time.Minute)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		st.FireSlot = &FireSlot{Key: QueueKey("o/front", 10), Token: "seedtok", Since: now.Add(-time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A bounded context turns the pre-fix behavior (spin until the block clears)
	// into a prompt failure instead of a hung test.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, code, err := svc.Wait(ctx, "o/private", 20)
	if err != nil {
		t.Fatalf("the summary-only round must resolve without the queue, got err=%v res=%#v", err, res)
	}
	if code == 2 {
		t.Fatalf("the summary-only round must not wait out the account block, got %#v code=%d", res, code)
	}
	st, _, _ := store.Load(context.Background())
	if r := st.Round("o/private", 20); r == nil || r.Phase == PhaseQueued {
		t.Fatalf("the summary-only round must leave the queue, got %#v", r)
	}
	for _, p := range gh.posted {
		if strings.Contains(p, cfg.ReviewCommand) {
			t.Fatalf("summary-only must never post the CodeRabbit command, posted=%v", gh.posted)
		}
	}
	// The other PR keeps the slot — resolving ours never touched it.
	if st.FireSlot == nil || st.FireSlot.Key != QueueKey("o/front", 10) {
		t.Fatalf("the front PR must keep the fire slot, got %#v", st.FireSlot)
	}
}

// A rate-limit notice is evidence about the ACCOUNT, and the pump only ever
// looks at the PR it is about to fire. A notice sitting on a superseded round —
// or simply on a PR that is not next in the queue — was read here and thrown
// away, and the next fire went out inside a window the bot had already stated.
func TestFeedbackRecordsARateLimitNoticeItObserves(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "a0646f010abcdef0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	other := ghapi.Pull{State: "open"}
	other.Head.SHA = "bbbbbbbb2abcdef0"
	gh.pulls[fakeKey("o/other", 90)] = other

	notice := time.Now().UTC().Add(-time.Minute)
	rl := ghapi.IssueComment{ID: 501,
		Body:      "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**",
		CreatedAt: notice, UpdatedAt: notice}
	rl.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/carrier", 82)] = []ghapi.IssueComment{rl}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Nobody asked about this PR's round; the notice is simply there to be seen.
	if _, err := svc.Feedback(ctx, "o/carrier", 82); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil == nil {
		t.Fatal("the observed notice was thrown away; the next fire would go out inside the window")
	}

	// And the block is what the queue then decides with.
	if _, err := svc.Enqueue(ctx, "o/other", 90); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range gh.posted {
		if strings.HasSuffix(p, ":"+cfg.ReviewCommand) {
			t.Errorf("a review was requested while the account was blocked: %s", p)
		}
	}
}

func TestObservedAccountBlockPersistsANewerNoticeAtTheStandingWindow(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	standing := now.Add(time.Hour)
	oldNotice := now.Add(-2 * time.Minute)
	newNotice := now.Add(-time.Minute)
	q := AccountQuota{
		BlockedUntil:     &standing,
		RLCommentID:      100,
		RLCommentUpdated: &oldNotice,
	}
	blk := &engine.AccountBlock{
		Until: standing, CommentID: 101, CommentUpdated: newNotice,
	}
	if !observedAccountBlockChanges(q, blk) {
		t.Fatal("a distinct shorter notice was not accepted for watermarking")
	}
	st := State{Account: q}
	applyAccountBlock(&st, blk, now)
	if st.Account.RLCommentID != 101 || !st.Account.RLCommentUpdated.Equal(newNotice) {
		t.Fatalf("notice watermark = id %d updated %v, want id 101 at %s",
			st.Account.RLCommentID, st.Account.RLCommentUpdated, newNotice)
	}
}

func TestObservedAccountBlockDoesNotRollTheNoticeWatermarkBackward(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	standing := now.Add(time.Hour)
	watermark := now.Add(-time.Minute)
	older := now.Add(-2 * time.Minute)
	q := AccountQuota{
		BlockedUntil:     &standing,
		RLCommentID:      100,
		RLCommentUpdated: &watermark,
	}
	blk := &engine.AccountBlock{
		Until: standing, CommentID: 101, CommentUpdated: older,
	}
	if observedAccountBlockChanges(q, blk) {
		t.Fatal("an older notice from a different comment rolled the watermark backward")
	}

	later := standing.Add(time.Hour)
	blk.Until = later
	if !observedAccountBlockChanges(q, blk) {
		t.Fatal("an older notice's longer block window was discarded")
	}
	st := State{Account: q}
	applyAccountBlock(&st, blk, now)
	if !st.Account.BlockedUntil.Equal(later) {
		t.Fatalf("blocked until %s, want extension to %s", st.Account.BlockedUntil, later)
	}
	if st.Account.RLCommentID != q.RLCommentID || !st.Account.RLCommentUpdated.Equal(watermark) {
		t.Fatalf("older notice replaced watermark with id %d at %v",
			st.Account.RLCommentID, st.Account.RLCommentUpdated)
	}
}

// The skip rules are per-repository settings like everything else, and the
// enrollment preview answers with the resolved ones. Reading this host's
// startup configuration instead made the daemon enqueue — and spend the shared
// allowance on — pull requests the dialog had just promised would be skipped.
func TestAutoReviewScanAppliesTheRepositorysOwnSkipAuthors(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		Bot:               "coderabbitai[bot]",
		ReviewCommand:     "@coderabbitai review",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{
		{Repo: "o/app", Number: 1, Author: "renovate[bot]"},
		{Repo: "o/app", Number: 2, Author: "alice"},
	}
	for pr := 1; pr <= 2; pr++ {
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Nothing in this host's env skips renovate; the repository's own record
	// does, and that is the answer every path has to reach.
	if _, err := svc.SetSolver(ctx, "o/app", SolverChange{SkipAuthors: []string{"renovate[bot]"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Round("o/app", 1) != nil {
		t.Errorf("a repository-skipped author was enqueued anyway: %#v", st.Rounds)
	}
	if st.FiredMarker("o/app", 2) == "" {
		t.Errorf("the unaffected pull request must still be reviewed, got rounds=%#v", st.Rounds)
	}
}
