package gh

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The state blob outgrew the ETag cache's size cap, which made every state read
// a counted download. A blob addressed by hash is fetched once, whatever its
// size, and never revalidated.
func TestGitObjectsAreFetchedOnceAndNeverRevalidated(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	content := strings.Repeat("x", maxETagBody+1)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("If-None-Match") != "" {
			t.Errorf("a git object must not be revalidated, got If-None-Match=%q", r.Header.Get("If-None-Match"))
		}
		w.Header().Set("ETag", `"blob"`)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		fmt.Fprintf(w, `{"sha":%q,"encoding":"utf-8","content":%q}`, sha, content)
	}))
	defer srv.Close()

	g := NewTestClient(srv.URL, srv.Client())
	for i := range 3 {
		got, err := g.GetBlob(t.Context(), "o/r", sha)
		if err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
		if string(got) != content {
			t.Fatalf("read %d returned %d bytes, want %d", i+1, len(got), len(content))
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	replayed, err := g.send(t.Context(), http.MethodGet, srv.URL+"/repos/o/r/git/blobs/"+sha, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = replayed.Body.Close()
	if got := replayed.Header.Get("X-RateLimit-Remaining"); got != "" {
		t.Fatalf("a replayed object carried the quota header of its original fetch: %q", got)
	}
}

func TestIsGitObjectURL(t *testing.T) {
	sha := strings.Repeat("0f", 20)
	for path, want := range map[string]bool{
		"/repos/o/r/git/blobs/" + sha:                      true,
		"/repos/o/r/git/commits/" + sha:                    true,
		"/repos/o/r/git/trees/" + sha + "?recursive=1":     true,
		"/repos/o/r/git/blobs/" + strings.Repeat("0f", 32): true, // sha256 repository
		// Anything that can resolve differently later must stay revalidated.
		"/repos/o/r/git/ref/heads/crq-state-v3":        false,
		"/repos/o/r/git/trees/main":                    false,
		"/repos/o/r/git/commits/" + sha[:12]:           false,
		"/repos/o/r/commits/" + sha:                    false,
		"/repos/o/r/git/blobs/" + strings.ToUpper(sha): false,
	} {
		if got := isGitObjectURL("https://api.github.com" + path); got != want {
			t.Errorf("isGitObjectURL(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestGitObjectCacheEvictsOldestPastItsBounds(t *testing.T) {
	var cache gitObjectCache
	for i := range maxGitObjects + 5 {
		cache.put(fmt.Sprint(i), &etagEntry{body: []byte("x"), at: time.Now()}, 0)
	}
	if len(cache.entries) != maxGitObjects || cache.get("0") != nil || cache.get(fmt.Sprint(maxGitObjects+4)) == nil {
		t.Fatalf("entries = %d, oldest kept = %v", len(cache.entries), cache.get("0") != nil)
	}

	// The byte budget evicts too, but never the object just stored.
	half := make([]byte, maxGitObjectBytes/2+1)
	cache.put("big-1", &etagEntry{body: half, at: time.Now()}, 0)
	cache.put("big-2", &etagEntry{body: half, at: time.Now()}, 0)
	if cache.get("big-1") != nil || cache.get("big-2") == nil || cache.bytes > maxGitObjectBytes {
		t.Fatalf("byte budget not enforced: bytes = %d", cache.bytes)
	}
}

// The contents of an object cannot change, but the credential's right to read
// its repository can, so an entry is refetched after a while and dropped when
// the credential is replaced.
func TestGitObjectCacheForgetsOldEntriesAndOldCredentials(t *testing.T) {
	var cache gitObjectCache
	cache.put("fresh", &etagEntry{body: []byte("x"), at: time.Now()}, 0)
	cache.put("stale", &etagEntry{body: []byte("x"), at: time.Now().Add(-maxGitObjectAge - time.Second)}, 0)
	if cache.get("fresh") == nil || cache.get("stale") != nil {
		t.Fatalf("fresh kept = %v, stale kept = %v", cache.get("fresh") != nil, cache.get("stale") != nil)
	}

	t.Setenv("GITHUB_TOKEN", "replacement")
	g := NewTestClient("http://example.invalid", nil)
	started := g.gitObjects.credentialNow()
	g.gitObjects.put("object", &etagEntry{body: []byte("x"), at: time.Now()}, started)
	g.refreshToken(t.Context())
	if g.gitObjects.get("object") != nil {
		t.Fatal("an object fetched with the old credential survived its replacement")
	}
	// A fetch that was already in flight under the old credential lands late.
	g.gitObjects.put("late", &etagEntry{body: []byte("x"), at: time.Now()}, started)
	if g.gitObjects.get("late") != nil {
		t.Fatal("a fetch begun with the old credential was cached after its replacement")
	}
}

// Behind a gateway the gateway's copy is the only one: its age is what says
// when GitHub last allowed the read.
func TestGatewayClientKeepsNoGitObjects(t *testing.T) {
	sha := strings.Repeat("cd", 20)
	var calls int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-CRQ-Gateway", "1")
		fmt.Fprintf(w, `{"sha":%q,"encoding":"utf-8","content":"state"}`, sha)
	}))
	defer gateway.Close()
	client, err := NewGitHubViaServer(gateway.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.GetBlob(t.Context(), "o/r", sha); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("gateway requests = %d, want every read to reach it", got)
	}
}
