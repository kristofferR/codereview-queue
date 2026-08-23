package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// ErrCASConflict is returned when the state ref moved between load and write.
var ErrCASConflict = errors.New("state changed while writing")

// ErrNoChange lets a mutate closure report "nothing to persist", so Update
// returns the current state without writing a new revision.
var ErrNoChange = errors.New("state unchanged")

const (
	statePath                = "state.json"
	dashboardPath            = "dashboard.md"
	gitFallbackEnv           = "CRQ_STATE_GIT_FALLBACK"
	gitStateCacheRef         = "refs/crq/state"
	gitStateAuthorName       = "kristofferR"
	gitStateAuthorEmail      = "481270+kristofferR@users.noreply.github.com"
	gitStateTokenEnv         = "CRQ_STATE_GIT_TOKEN"
	gitStateCredentialHelper = `!f() { p=; h=; while IFS= read -r l; do case "$l" in protocol=*) p=${l#protocol=} ;; host=*) h=${l#host=} ;; esac; done; if test "$1" = get && test "$p" = https && test "$h" = github.com; then printf 'username=x-access-token\npassword=%s\n' "$CRQ_STATE_GIT_TOKEN"; fi; }; f`
)

// Logger is the minimal logging surface the store uses (for the loud
// auto-reinit line when a stale schema payload is loaded).
type Logger interface {
	Printf(string, ...any)
}

// StoreConfig carries the fields the store and dashboard need from crq's
// Config, so internal/state stays free of an import cycle back to crq.
type StoreConfig struct {
	GateRepo       string
	StateRef       string
	DashboardIssue int
	Timezone       string
	Scope          []string
	// TokenSource resolves credentials immediately before each fallback fetch
	// or push. Empty leaves authentication to the host's Git configuration.
	TokenSource func(context.Context) string
	// CoReviewers is a preformatted display string of the enabled co-reviewer
	// bots ("" hides the dashboard row, keeping co-bot-less dashboards
	// byte-identical).
	CoReviewers string
	// ResolveCoReviewers applies the current fleet record before rendering.
	// The state package owns persistence but not reviewer registry policy, so
	// crq supplies this resolver while static callers may keep CoReviewers.
	ResolveCoReviewers func(FleetDefaults) string
	// Host names this process in the state it writes, so the fleet can tell which
	// binaries are driving and what they understand (see State.NoteWriter).
	Host string
	// MinInterval is DecideFire's pacing gate. The dashboard needs it so a queue
	// entry is not advertised as ready before firing would actually accept it.
	MinInterval time.Duration
}

func (c StoreConfig) requireState() error {
	if c.GateRepo == "" {
		return errors.New("CRQ_REPO is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func (c StoreConfig) requireDashboard() error {
	if err := c.requireState(); err != nil {
		return err
	}
	if c.DashboardIssue <= 0 {
		return errors.New("CRQ_ISSUE is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

// Revision identifies the git commit/tree the loaded state came from, so the
// compare-and-swap can build the next commit on top of it.
type Revision struct {
	CommitSHA string
	TreeSHA   string
}

// StateStore is the persistence surface crq consumes. Load reads the current
// state, Update applies a mutate closure under compare-and-swap, and
// SyncDashboard mirrors the state to the dashboard issue.
type StateStore interface {
	Load(context.Context) (State, Revision, error)
	Update(context.Context, func(*State) error) (State, error)
	SyncDashboard(context.Context, State) error
}

// GitStateStore persists v4 state as state.json in a git ref, with the same
// compare-and-swap mechanism as v3 (12 retries on UpdateRef 409/422).
//
// V3 is migrated in place; still older payloads are discarded because crq is
// pre-release and they describe a world this binary cannot act on. A NEWER one
// is refused. The fleet runs mixed binary versions during a rolling deploy, so
// reinitializing there would mean the first old binary to wake up erases every
// live round the new ones are working — the whole account's queue, silently, in
// the one situation the version field exists to detect.
type GitStateStore struct {
	cfg StoreConfig
	gh  *gh.GitHub
	log Logger

	// The opt-in git path avoids GitHub's REST git-data endpoints when the
	// account's primary quota is exhausted. Each store gets a private bare
	// cache, so separate crq processes never contend on local refs or indexes.
	gitFallback  bool
	gitRemoteURL string // test seam; production derives a credential-free HTTPS URL
	gitOnce      sync.Once
	gitDir       string
	gitInitErr   error
	gitMu        sync.Mutex

	// renderCfg resolves state-backed fleet settings before rendering. It is
	// installed once by crq, which owns the bot registry needed to format them.
	renderCfg func(State) StoreConfig

	// syncMu serializes the read-then-write in SyncDashboard, so concurrent
	// syncs cannot both see a stale gate issue and both write it.
	syncMu sync.Mutex
}

func NewGitStateStore(cfg StoreConfig, client *gh.GitHub, log Logger) *GitStateStore {
	return &GitStateStore{
		cfg:         cfg,
		gh:          client,
		log:         log,
		gitFallback: strings.TrimSpace(os.Getenv(gitFallbackEnv)) == "1",
	}
}

// Close removes the process-private repository used by the git fallback.
// Normal commands and daemons call this as they exit so short-lived processes
// do not leave an unbounded trail of state history in the system temp dir.
func (s *GitStateStore) Close() error {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	if s.gitDir == "" {
		return nil
	}
	dir := s.gitDir
	s.gitDir = ""
	return os.RemoveAll(dir)
}

// SetRenderConfig installs the effective configuration used for dashboard.md
// and the gate issue.
func (s *GitStateStore) SetRenderConfig(resolve func(State) StoreConfig) {
	s.renderCfg = resolve
}

func (s *GitStateStore) renderConfig(st State) StoreConfig {
	if s.renderCfg == nil {
		return s.cfg
	}
	return s.renderCfg(st)
}

func (s *GitStateStore) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

func (s *GitStateStore) Load(ctx context.Context) (State, Revision, error) {
	if err := s.cfg.requireState(); err != nil {
		return State{}, Revision{}, err
	}
	if s.gitFallback {
		return s.loadGit(ctx)
	}
	ref, err := s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
	if errors.Is(err, gh.ErrNotFound) {
		st := s.fresh()
		return st, Revision{}, nil
	}
	if err != nil {
		return State{}, Revision{}, err
	}
	commit, err := s.gh.GetCommit(ctx, s.cfg.GateRepo, ref)
	if err != nil {
		return State{}, Revision{}, err
	}
	tree, err := s.gh.GetTree(ctx, s.cfg.GateRepo, commit.Tree.SHA)
	if err != nil {
		return State{}, Revision{}, err
	}
	var stateBlob string
	for _, item := range tree.Tree {
		if item.Path == statePath && item.Type == "blob" {
			stateBlob = item.SHA
			break
		}
	}
	rev := Revision{CommitSHA: ref, TreeSHA: commit.Tree.SHA}
	if stateBlob == "" {
		s.logf("state ref %s has no %s — reinitializing to a fresh v%d state", s.cfg.StateRef, statePath, SchemaVersion)
		return s.fresh(), rev, nil
	}
	raw, err := s.gh.GetBlob(ctx, s.cfg.GateRepo, stateBlob)
	if err != nil {
		return State{}, Revision{}, err
	}
	return s.decodeState(raw, rev)
}

// StateRef returns the current state commit through the same transport Load
// uses. crq wait type-asserts this optional method so an opted-in git fallback
// never drops back to the REST git-data endpoint while idling.
func (s *GitStateStore) StateRef(ctx context.Context) (string, error) {
	if err := s.cfg.requireState(); err != nil {
		return "", err
	}
	if !s.gitFallback {
		return s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
	}
	_, rev, err := s.loadGit(ctx)
	return rev.CommitSHA, err
}

func (s *GitStateStore) decodeState(raw []byte, rev Revision) (State, Revision, error) {
	// Peek at the schema version before a full decode. V5 is the one supported
	// migration: v6 fences v5 writers that encoded fleet policy with a
	// different shape, while preserving every live v5 round during rollout.
	var probe struct {
		Version int `json:"v"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return State{}, Revision{}, s.refuse("holds a payload that does not parse as JSON", err)
	}
	if probe.Version > SchemaVersion {
		return State{}, Revision{}, s.refuse(
			fmt.Sprintf("holds schema v%d, which this binary (v%d) does not understand", probe.Version, SchemaVersion), nil)
	}
	if probe.Version < SchemaVersion-1 {
		s.logf("state ref %s holds schema v%d (want v%d) — reinitializing to a fresh state (no migration; crq is pre-release)", s.cfg.StateRef, probe.Version, SchemaVersion)
		return s.fresh(), rev, nil
	}
	if probe.Version == SchemaVersion-1 {
		migrated, err := migrateV5State(raw)
		if err != nil {
			return State{}, Revision{}, s.refuse("holds a v5 fleet policy this binary cannot migrate", err)
		}
		raw = migrated
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		// The version says this binary should understand or migrate it and it
		// does not, so the rounds it describes are live and must not be thrown
		// away.
		return State{}, Revision{}, s.refuse("holds a v"+strconv.Itoa(probe.Version)+" payload this binary cannot decode", err)
	}
	st.Version = SchemaVersion
	st.Normalize(time.Now().UTC())
	return st, rev, nil
}

// refuse turns a state payload this binary must not touch into an actionable
// error. It names the ref and the way out, because the safe response is a human
// decision — upgrade, or deliberately reset — and never a silent erase.
func (s *GitStateStore) refuse(what string, cause error) error {
	msg := fmt.Sprintf("state ref %s %s; refusing to touch it. Upgrade crq, or reset deliberately with: git push origin --delete %s",
		s.cfg.StateRef, what, s.cfg.StateRef)
	if cause != nil {
		return fmt.Errorf("%s (%w)", msg, cause)
	}
	return errors.New(msg)
}

func (s *GitStateStore) fresh() State {
	st := New()
	st.Account.Scope = joinScope(s.cfg.Scope)
	st.Account.Source = "init"
	st.Normalize(time.Now().UTC())
	return st
}

func (s *GitStateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	const attempts = 12
	for i := 0; i < attempts; i++ {
		st, rev, err := s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		if err := mutate(&st); err != nil {
			if errors.Is(err, ErrNoChange) {
				return st, nil
			}
			return State{}, err
		}
		now := time.Now().UTC()
		st.Rev++
		st.UpdatedAt = &now
		st.NoteWriter(s.cfg.Host, WriterCaps, now)
		st.Normalize(now)
		if err := s.compareAndSwap(ctx, &st, rev); err != nil {
			if errors.Is(err, ErrCASConflict) {
				continue
			}
			return State{}, err
		}
		return st, nil
	}
	return State{}, ErrCASConflict
}

func (s *GitStateStore) compareAndSwap(ctx context.Context, st *State, rev Revision) error {
	dashboard := RenderDashboard(*st, s.renderConfig(*st))
	st.DashboardSHA = hashString(dashboard)
	stateJSON, err := json.MarshalIndent(*st, "", "  ")
	if err != nil {
		return err
	}
	if s.gitFallback {
		return s.compareAndSwapGit(ctx, st.Rev, append(stateJSON, '\n'), []byte(dashboard), rev)
	}
	stateBlob, err := s.gh.CreateBlob(ctx, s.cfg.GateRepo, append(stateJSON, '\n'))
	if err != nil {
		return err
	}
	dashboardBlob, err := s.gh.CreateBlob(ctx, s.cfg.GateRepo, []byte(dashboard))
	if err != nil {
		return err
	}
	treeSHA, err := s.gh.CreateTree(ctx, s.cfg.GateRepo, rev.TreeSHA, []map[string]any{
		{"path": statePath, "mode": "100644", "type": "blob", "sha": stateBlob},
		{"path": dashboardPath, "mode": "100644", "type": "blob", "sha": dashboardBlob},
	})
	if err != nil {
		return err
	}
	parents := []string{}
	if rev.CommitSHA != "" {
		parents = []string{rev.CommitSHA}
	}
	commitSHA, err := s.gh.CreateCommit(ctx, s.cfg.GateRepo, fmt.Sprintf("crq: state rev %d", st.Rev), treeSHA, parents)
	if err != nil {
		return err
	}
	if rev.CommitSHA == "" {
		err = s.gh.CreateRef(ctx, s.cfg.GateRepo, s.cfg.StateRef, commitSHA)
	} else {
		err = s.gh.UpdateRef(ctx, s.cfg.GateRepo, s.cfg.StateRef, commitSHA, false)
	}
	if err == nil {
		return nil
	}
	var apiErr *gh.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnprocessableEntity || apiErr.Status == http.StatusConflict) {
		return ErrCASConflict
	}
	return err
}

// loadGit reads the state through the ordinary git transport. The exact state
// ref is fetched into a process-private bare repository; no repository token is
// put in an argument, remote config, or error message.
func (s *GitStateStore) loadGit(ctx context.Context) (State, Revision, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()

	if err := s.ensureGitCache(ctx); err != nil {
		return State{}, Revision{}, err
	}
	remoteRef := s.gitRemoteRef()
	_, stderr, err := s.gitRemote(ctx, nil, nil,
		"fetch", "--quiet", "--no-tags", "origin", "+"+remoteRef+":"+gitStateCacheRef)
	if err != nil {
		if isMissingGitRemoteRef(stderr) {
			// Do not retain a stale local value if a long-lived process observes
			// that the remote state ref was deliberately removed.
			_, _, _ = s.git(ctx, nil, nil, "update-ref", "-d", gitStateCacheRef)
			return s.fresh(), Revision{}, nil
		}
		return State{}, Revision{}, gitStateError("fetching state ref", err, stderr)
	}

	commitOut, stderr, err := s.git(ctx, nil, nil, "rev-parse", "--verify", gitStateCacheRef+"^{commit}")
	if err != nil {
		return State{}, Revision{}, gitStateError("reading fetched commit", err, stderr)
	}
	commitSHA := strings.TrimSpace(string(commitOut))
	treeOut, stderr, err := s.git(ctx, nil, nil, "rev-parse", "--verify", commitSHA+"^{tree}")
	if err != nil {
		return State{}, Revision{}, gitStateError("reading fetched tree", err, stderr)
	}
	rev := Revision{CommitSHA: commitSHA, TreeSHA: strings.TrimSpace(string(treeOut))}

	raw, stderr, err := s.git(ctx, nil, nil, "show", commitSHA+":"+statePath)
	if err != nil {
		if isMissingGitPath(stderr) {
			s.logf("state ref %s has no %s — reinitializing to a fresh v%d state", s.cfg.StateRef, statePath, SchemaVersion)
			return s.fresh(), rev, nil
		}
		return State{}, Revision{}, gitStateError("reading state.json", err, stderr)
	}
	return s.decodeState(raw, rev)
}

// compareAndSwapGit creates the same two-file state commit as the REST path,
// preserving every other entry from the loaded tree. The explicit lease is the
// CAS: it rejects both an advanced ref and a ref deleted after Load. A plain
// push catches the first race but recreates a missing destination from stale
// state.
func (s *GitStateStore) compareAndSwapGit(
	ctx context.Context,
	revision int64,
	stateJSON []byte,
	dashboard []byte,
	rev Revision,
) error {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()

	if err := s.ensureGitCache(ctx); err != nil {
		return err
	}
	stateBlob, err := s.gitHashObject(ctx, stateJSON)
	if err != nil {
		return err
	}
	dashboardBlob, err := s.gitHashObject(ctx, dashboard)
	if err != nil {
		return err
	}

	indexFile, err := os.CreateTemp(s.gitDir, "state-index-*")
	if err != nil {
		return fmt.Errorf("creating git state index: %w", err)
	}
	indexPath := indexFile.Name()
	if err := indexFile.Close(); err != nil {
		_ = os.Remove(indexPath)
		return fmt.Errorf("closing git state index: %w", err)
	}
	// Git expects an absent index for read-tree --empty, not a zero-byte file.
	if err := os.Remove(indexPath); err != nil {
		return fmt.Errorf("preparing git state index: %w", err)
	}
	defer os.Remove(indexPath)
	indexEnv := []string{"GIT_INDEX_FILE=" + indexPath}

	readTreeArgs := []string{"read-tree", "--empty"}
	if rev.TreeSHA != "" {
		readTreeArgs = []string{"read-tree", rev.TreeSHA}
	}
	if _, stderr, err := s.git(ctx, indexEnv, nil, readTreeArgs...); err != nil {
		return gitStateError("reading base tree", err, stderr)
	}
	for _, entry := range []struct {
		path string
		sha  string
	}{
		{path: statePath, sha: stateBlob},
		{path: dashboardPath, sha: dashboardBlob},
	} {
		if _, stderr, err := s.git(ctx, indexEnv, nil,
			"update-index", "--add", "--cacheinfo", "100644", entry.sha, entry.path); err != nil {
			return gitStateError("updating state tree", err, stderr)
		}
	}
	treeOut, stderr, err := s.git(ctx, indexEnv, nil, "write-tree")
	if err != nil {
		return gitStateError("writing state tree", err, stderr)
	}
	treeSHA := strings.TrimSpace(string(treeOut))

	commitArgs := []string{"commit-tree", treeSHA}
	if rev.CommitSHA != "" {
		commitArgs = append(commitArgs, "-p", rev.CommitSHA)
	}
	identityEnv := []string{
		"GIT_AUTHOR_NAME=" + gitStateAuthorName,
		"GIT_AUTHOR_EMAIL=" + gitStateAuthorEmail,
		"GIT_COMMITTER_NAME=" + gitStateAuthorName,
		"GIT_COMMITTER_EMAIL=" + gitStateAuthorEmail,
	}
	message := []byte(fmt.Sprintf("crq: state rev %d\n", revision))
	commitOut, stderr, err := s.git(ctx, identityEnv, message, commitArgs...)
	if err != nil {
		return gitStateError("creating state commit", err, stderr)
	}
	commitSHA := strings.TrimSpace(string(commitOut))

	remoteRef := s.gitRemoteRef()
	lease := "--force-with-lease=" + remoteRef + ":" + rev.CommitSHA
	stdout, stderr, err := s.gitRemote(ctx, nil, nil,
		"push", "--porcelain", lease, "origin", commitSHA+":"+remoteRef)
	if err != nil {
		if isGitNonFastForward(stdout, stderr) {
			return ErrCASConflict
		}
		return gitStateError("pushing state ref", err, stderr)
	}
	_, _, _ = s.git(ctx, nil, nil, "update-ref", gitStateCacheRef, commitSHA)
	return nil
}

func (s *GitStateStore) gitHashObject(ctx context.Context, content []byte) (string, error) {
	out, stderr, err := s.git(ctx, nil, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", gitStateError("creating state blob", err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *GitStateStore) ensureGitCache(ctx context.Context) error {
	s.gitOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crq-state-git-"+strconv.Itoa(os.Getpid())+"-*")
		if err != nil {
			s.gitInitErr = fmt.Errorf("creating git state cache: %w", err)
			return
		}
		s.gitDir = dir
		if _, stderr, err := runGit(ctx, nil, nil, "init", "--bare", "--quiet", dir); err != nil {
			s.gitInitErr = gitStateError("initializing state cache", err, stderr)
			return
		}
		remote := s.gitRemoteURL
		if remote == "" {
			remote = "https://github.com/" + strings.TrimSuffix(s.cfg.GateRepo, ".git") + ".git"
		}
		if _, stderr, err := runGit(ctx, nil, nil,
			"--git-dir", dir, "remote", "add", "origin", remote); err != nil {
			s.gitInitErr = gitStateError("configuring state remote", err, stderr)
		}
	})
	return s.gitInitErr
}

func (s *GitStateStore) gitRemoteRef() string {
	name := strings.TrimPrefix(strings.TrimSpace(s.cfg.StateRef), "refs/heads/")
	return "refs/heads/" + name
}

func (s *GitStateStore) git(
	ctx context.Context,
	env []string,
	stdin []byte,
	args ...string,
) ([]byte, []byte, error) {
	fullArgs := append([]string{"--git-dir", s.gitDir}, args...)
	return runGit(ctx, env, stdin, fullArgs...)
}

// gitRemote runs a fallback network command with the same freshly resolved
// GitHub credential available to the REST transport. The secret stays in the
// child environment; argv and the bare repository contain only the helper.
func (s *GitStateStore) gitRemote(
	ctx context.Context,
	env []string,
	stdin []byte,
	args ...string,
) ([]byte, []byte, error) {
	token := ""
	if s.cfg.TokenSource != nil {
		token = strings.TrimSpace(s.cfg.TokenSource(ctx))
	}
	if token != "" {
		env = append(append([]string(nil), env...), gitStateTokenEnv+"="+token)
		args = append([]string{
			"-c", "credential.helper=",
			"-c", "credential.helper=" + gitStateCredentialHelper,
		}, args...)
	}
	return s.git(ctx, env, stdin, args...)
}

func runGit(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func gitStateError(operation string, err error, stderr []byte) error {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		return fmt.Errorf("git state %s: %w", operation, err)
	}
	return fmt.Errorf("git state %s: %w (%s)", operation, err, detail)
}

func isMissingGitRemoteRef(stderr []byte) bool {
	message := strings.ToLower(string(stderr))
	return strings.Contains(message, "couldn't find remote ref") ||
		strings.Contains(message, "remote ref does not exist")
}

func isMissingGitPath(stderr []byte) bool {
	message := strings.ToLower(string(stderr))
	return strings.Contains(message, "does not exist in") ||
		strings.Contains(message, "exists on disk, but not in")
}

func isGitNonFastForward(stdout, stderr []byte) bool {
	message := strings.ToLower(string(append(append([]byte(nil), stdout...), stderr...)))
	return strings.Contains(message, "non-fast-forward") ||
		strings.Contains(message, "[rejected]") ||
		strings.Contains(message, "fetch first") ||
		strings.Contains(message, "stale info")
}

// SyncDashboard renders the gate issue and writes it only when the issue does
// not already say exactly that.
//
// Every caller that touches state calls this, including read-mostly paths like
// Enqueue that usually report ErrNoChange — so an unconditional PATCH here made
// a plain `crq next` a write, and writes are the endpoint class that trips
// GitHub's *secondary* rate limits (the reason enqueues were batched in the
// first place).
//
// The check reads the issue rather than remembering what this process last
// wrote. A memo would be cheaper still, but it would also stop the dashboard
// ever self-healing: an issue edited by hand, or by a binary running different
// code, would stay wrong until state happened to change. Reading costs nothing
// to be right — GetIssue is a conditional GET, so an unchanged issue answers
// 304 from cache and spends no quota.
//
// A read failure is never fatal: fall through and PATCH, which is the old
// behavior.
func (s *GitStateStore) SyncDashboard(ctx context.Context, st State) error {
	// dashboard.md is committed beside state.json by compareAndSwapGit. Avoid
	// touching the issue API while this process is explicitly avoiding REST.
	if s.gitFallback {
		return nil
	}
	if err := s.cfg.requireDashboard(); err != nil {
		return err
	}
	cfg := s.renderConfig(st)
	body, err := IssueBody(st, cfg)
	if err != nil {
		return err
	}
	title := RenderTitle(st, cfg)

	// Held across read-then-write so two concurrent syncs cannot both observe a
	// stale issue and both write it.
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if issue, err := s.gh.GetIssue(ctx, s.cfg.GateRepo, s.cfg.DashboardIssue); err == nil &&
		issue.Body == body && issue.Title == title {
		return nil
	}
	return s.gh.PatchIssue(ctx, s.cfg.GateRepo, s.cfg.DashboardIssue, title, body)
}

// MemoryStore is the in-memory store used by tests and the fake-GitHub harness.
type MemoryStore struct {
	mu    sync.Mutex
	cfg   StoreConfig
	state State
	rev   int64
	now   func() time.Time
}

func NewMemoryStore(cfg StoreConfig) *MemoryStore {
	st := New()
	st.Account.Scope = joinScope(cfg.Scope)
	st.Account.Source = "init"
	st.Normalize(time.Now().UTC())
	return &MemoryStore{cfg: cfg, state: st}
}

// SetClock keeps deterministic replay state on the same clock as the service
// under test. Production memory stores use the wall clock.
func (m *MemoryStore) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func (m *MemoryStore) clock() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func (m *MemoryStore) Load(context.Context) (State, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Clone(m.state)
	st.Normalize(m.clock())
	return st, Revision{CommitSHA: fmt.Sprintf("%d", m.rev)}, nil
}

func (m *MemoryStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Clone(m.state)
	st.Normalize(m.clock())
	if err := mutate(&st); err != nil {
		if errors.Is(err, ErrNoChange) {
			return st, nil
		}
		return State{}, err
	}
	now := m.clock()
	st.Rev++
	st.UpdatedAt = &now
	st.Normalize(now)
	m.rev++
	m.state = st
	return st, nil
}

func (m *MemoryStore) SyncDashboard(context.Context, State) error { return nil }

// Clone deep-copies a State via its JSON representation, so a mutate closure
// can never scribble on the store's retained copy.
func Clone(st State) State {
	raw, _ := json.Marshal(st)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}
