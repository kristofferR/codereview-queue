package state

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/kristofferR/codereview-queue/internal/gh"
)

// gitData is an in-memory stand-in for the git-data endpoints the REST store
// uses. It counts requests as "METHOD kind", because how many a read or a write
// costs is the property under test: the account shares one REST budget.
type gitData struct {
	mu      sync.Mutex
	ref     string
	commits map[string]gitDataCommit
	trees   map[string]map[string]string // tree -> path -> blob
	blobs   map[string]string
	calls   map[string]int
	// beforeSwap runs once, ahead of the next ref update, outside the lock;
	// beforeTree likewise ahead of the next tree upload.
	beforeSwap func()
	beforeTree func()
}

type gitDataCommit struct{ tree, parent string }

func newGitData() *gitData {
	return &gitData{commits: map[string]gitDataCommit{}, trees: map[string]map[string]string{}, blobs: map[string]string{}, calls: map[string]int{}}
}

func objectID(kind string, v any) string {
	raw, _ := json.Marshal(v)
	sum := sha1.Sum(append([]byte(kind), raw...))
	return hex.EncodeToString(sum[:])
}

func (d *gitData) count(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[key]
}

func (d *gitData) resetCounts() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = map[string]int{}
}

func (d *gitData) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // repos o r git kind [id...]
	if len(parts) < 5 || parts[3] != "git" {
		http.NotFound(w, r)
		return
	}
	kind := parts[4]
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)

	if r.Method == http.MethodPatch || r.Method == http.MethodPost && kind == "trees" {
		d.mu.Lock()
		hook := &d.beforeSwap
		if r.Method == http.MethodPost {
			hook = &d.beforeTree
		}
		run := *hook
		*hook = nil
		d.mu.Unlock()
		if run != nil {
			run()
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[r.Method+" "+kind]++
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && kind == "ref":
		if d.ref == "" {
			http.NotFound(w, r)
			return
		}
		reply(map[string]any{"object": map[string]string{"sha": d.ref}})
	case r.Method == http.MethodGet && kind == "commits":
		commit := d.commits[parts[5]]
		reply(map[string]any{"sha": parts[5], "tree": map[string]string{"sha": commit.tree}})
	case r.Method == http.MethodGet && kind == "trees":
		entries := []map[string]string{}
		for path, blob := range d.trees[parts[5]] {
			entries = append(entries, map[string]string{"path": path, "type": "blob", "sha": blob})
		}
		reply(map[string]any{"sha": parts[5], "tree": entries})
	case r.Method == http.MethodGet && kind == "blobs":
		reply(map[string]any{"sha": parts[5], "encoding": "utf-8", "content": d.blobs[parts[5]]})
	case r.Method == http.MethodPost && kind == "blobs":
		content, _ := in["content"].(string)
		reply(map[string]string{"sha": d.putBlob(content)})
	case r.Method == http.MethodPost && kind == "trees":
		tree := map[string]string{}
		if base, _ := in["base_tree"].(string); base != "" {
			for path, blob := range d.trees[base] {
				tree[path] = blob
			}
		}
		entries, _ := in["tree"].([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			path, _ := entry["path"].(string)
			if content, inline := entry["content"].(string); inline {
				tree[path] = d.putBlob(content)
			} else {
				tree[path], _ = entry["sha"].(string)
			}
		}
		id := objectID("tree", tree)
		d.trees[id] = tree
		reply(map[string]string{"sha": id})
	case r.Method == http.MethodPost && kind == "commits":
		commit := gitDataCommit{}
		commit.tree, _ = in["tree"].(string)
		if parents, _ := in["parents"].([]any); len(parents) > 0 {
			commit.parent, _ = parents[0].(string)
		}
		id := objectID("commit", []string{commit.tree, commit.parent})
		d.commits[id] = commit
		reply(map[string]string{"sha": id})
	case r.Method == http.MethodPost && kind == "refs":
		if d.ref != "" {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		d.ref, _ = in["sha"].(string)
		reply(map[string]string{})
	case r.Method == http.MethodPatch && kind == "refs":
		sha, _ := in["sha"].(string)
		if d.commits[sha].parent != d.ref {
			http.Error(w, `{"message":"Update is not a fast forward"}`, http.StatusUnprocessableEntity)
			return
		}
		d.ref = sha
		reply(map[string]string{})
	default:
		http.NotFound(w, r)
	}
}

func (d *gitData) putBlob(content string) string {
	id := objectID("blob", content)
	d.blobs[id] = content
	return id
}

func restStore(t *testing.T, data *gitData) *GitStateStore {
	t.Helper()
	srv := httptest.NewServer(data)
	t.Cleanup(srv.Close)
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3", Scope: []string{"owner"}, Host: "test"}
	return NewGitStateStore(cfg, gh.NewTestClient(srv.URL, srv.Client()), nil)
}

func setWarn(warn string) func(*State) error {
	return func(st *State) error {
		st.Warn = warn
		return nil
	}
}

// A write is the tree, the commit and the ref. A read at a revision this
// process has already fetched is the ref check alone: the commit, tree and blob
// behind it cannot have changed.
func TestRESTStateWriteAndRereadCost(t *testing.T) {
	data := newGitData()
	store := restStore(t, data)
	ctx := context.Background()
	if _, err := store.Update(ctx, setWarn("first")); err != nil {
		t.Fatal(err)
	}

	data.resetCounts()
	if _, err := store.Update(ctx, setWarn("second")); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"POST blobs": 0, "POST trees": 1, "POST commits": 1, "PATCH refs": 1} {
		if got := data.count(key); got != want {
			t.Errorf("a write made %d %q requests, want %d", got, key, want)
		}
	}

	if _, _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	data.resetCounts()
	st, _, err := store.Load(ctx)
	if err != nil || st.Warn != "second" {
		t.Fatalf("reloaded warn = %q, err = %v", st.Warn, err)
	}
	for key, want := range map[string]int{"GET ref": 1, "GET commits": 0, "GET trees": 0, "GET blobs": 0} {
		if got := data.count(key); got != want {
			t.Errorf("a re-read made %d %q requests, want %d", got, key, want)
		}
	}
}

func TestRESTUpdateLandsAfterLosingTheSwap(t *testing.T) {
	old := casRetryBase
	casRetryBase = time.Millisecond
	t.Cleanup(func() { casRetryBase = old })

	data := newGitData()
	store, rival := restStore(t, data), restStore(t, data)
	ctx := context.Background()
	if _, err := store.Update(ctx, setWarn("seed")); err != nil {
		t.Fatal(err)
	}

	data.mu.Lock()
	data.beforeSwap = func() {
		if _, err := rival.Update(ctx, func(st *State) error {
			st.CalibrationIssue = 7
			return nil
		}); err != nil {
			t.Errorf("rival write: %v", err)
		}
	}
	data.mu.Unlock()
	data.resetCounts()

	st, err := store.Update(ctx, setWarn("mine"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Warn != "mine" || st.CalibrationIssue != 7 {
		t.Fatalf("the retry dropped a write: warn = %q, calibration issue = %d", st.Warn, st.CalibrationIssue)
	}
	// Three tree uploads: the lost attempt, the rival's, and the retry.
	if got := data.count("POST trees"); got != 3 {
		t.Fatalf("tree uploads = %d, want 3", got)
	}
}

func TestRESTOversizedStateTakesTheBlobRoute(t *testing.T) {
	old := maxInlineState
	maxInlineState = 1
	t.Cleanup(func() { maxInlineState = old })

	data := newGitData()
	store := restStore(t, data)
	if _, err := store.Update(context.Background(), setWarn("large")); err != nil {
		t.Fatal(err)
	}
	if got := data.count("POST blobs"); got != 1 {
		t.Fatalf("blob uploads = %d, want 1", got)
	}
	st, _, err := store.Load(context.Background())
	if err != nil || st.Warn != "large" {
		t.Fatalf("reloaded warn = %q, err = %v", st.Warn, err)
	}
}

func TestRESTUpdateStopsWaitingWhenCancelled(t *testing.T) {
	old := casRetryBase
	casRetryBase = time.Hour
	t.Cleanup(func() { casRetryBase = old })

	data := newGitData()
	store, rival := restStore(t, data), restStore(t, data)
	if _, err := store.Update(context.Background(), setWarn("seed")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	data.mu.Lock()
	data.beforeSwap = func() {
		_, _ = rival.Update(context.Background(), setWarn("rival"))
		cancel()
	}
	data.mu.Unlock()
	if _, err := store.Update(ctx, setWarn("mine")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A writer that landed while this one was loading is noticed before the upload,
// for one request rather than three.
func TestRESTWriteSpendsNothingOnAnAttemptThatAlreadyLost(t *testing.T) {
	old := casRetryBase
	casRetryBase = time.Millisecond
	t.Cleanup(func() { casRetryBase = old })

	data := newGitData()
	store, rival := restStore(t, data), restStore(t, data)
	ctx := context.Background()
	if _, err := store.Update(ctx, setWarn("seed")); err != nil {
		t.Fatal(err)
	}
	first := true
	data.resetCounts()
	if _, err := store.Update(ctx, func(st *State) error {
		if first {
			first = false
			if _, err := rival.Update(ctx, setWarn("rival")); err != nil {
				t.Errorf("rival write: %v", err)
			}
		}
		st.CalibrationIssue = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The rival's write and this one's retry upload; the attempt that had
	// already lost does not.
	if got := data.count("POST trees"); got != 2 {
		t.Fatalf("tree uploads = %d, want 2", got)
	}
}
