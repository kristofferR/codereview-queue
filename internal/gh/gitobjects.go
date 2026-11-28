package gh

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A commit, tree or blob addressed by its full hash can never change, so one
// that has been fetched is served from memory with no request at all.
//
// The ETag cache cannot do this job. It revalidates every read, and it skips
// bodies over maxETagBody — which the state blob outgrew: base64 inflates it
// past the cap, so every state read by every crq process became a counted
// download of the whole blob, and `crq serve` alone re-read it on each poll.
const (
	// One object. Larger ones pass through uncached, as before.
	maxGitObjectBody = 32 << 20
	// The whole cache. Readers want the newest revision and a writer the one it
	// loaded moments ago, so a few revisions' worth is the working set.
	maxGitObjectBytes = 64 << 20
	maxGitObjects     = 48
	// An object's contents cannot change, but whether this credential may read
	// its repository can. Refetching after a while is what re-asks GitHub that.
	maxGitObjectAge = 10 * time.Minute
)

type gitObjectCache struct {
	mu      sync.Mutex
	entries map[string]*etagEntry
	order   []string // insertion order, oldest first
	bytes   int
	// credential counts the times the cache was emptied for a new credential,
	// so a fetch that started under the old one cannot put its object back.
	credential int
}

func (c *gitObjectCache) credentialNow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credential
}

func (c *gitObjectCache) get(url string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[url]
	if entry == nil || time.Since(entry.at) > maxGitObjectAge {
		return nil
	}
	return entry
}

func (c *gitObjectCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.order, c.bytes = nil, nil, 0
	c.credential++
}

// put stores an object fetched while the cache was at the given credential.
func (c *gitObjectCache) put(url string, entry *etagEntry, credential int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if credential != c.credential {
		return
	}
	if c.entries == nil {
		c.entries = map[string]*etagEntry{}
	}
	if old, exists := c.entries[url]; exists {
		// A refetch of an entry that aged out: same object, fresh timestamp.
		c.bytes += len(entry.body) - len(old.body)
		c.entries[url] = entry
		return
	}
	c.entries[url] = entry
	c.order = append(c.order, url)
	c.bytes += len(entry.body)
	for len(c.order) > 1 && (len(c.order) > maxGitObjects || c.bytes > maxGitObjectBytes) {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.entries[oldest].body)
		delete(c.entries, oldest)
	}
}

// response replays a cached object as the 200 it arrived as. The quota headers
// are dropped: they describe the moment of the original fetch, not this read.
func (e *etagEntry) response(req *http.Request) *http.Response {
	header := e.header.Clone()
	for key := range header {
		if strings.HasPrefix(key, "X-Ratelimit-") {
			delete(header, key)
		}
	}
	return &http.Response{
		Status:        "200 OK (git object cache)",
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

// isGitObjectURL reports a git-data read addressed by a full object hash:
// /repos/{owner}/{repo}/git/{blobs|trees|commits}/{sha}. An abbreviated hash or
// a ref name can resolve differently later, so only full hashes qualify.
func isGitObjectURL(fullURL string) bool {
	u, err := url.Parse(fullURL)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "repos" || parts[3] != "git" {
		return false
	}
	switch parts[4] {
	case "blobs", "trees", "commits":
	default:
		return false
	}
	sha := parts[5]
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
