package gh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendRetriesTransientThenSucceeds(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token") // refreshToken resolves from env, no gh exec
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable) // transient 5xx -> retry
		case 2:
			w.WriteHeader(http.StatusUnauthorized) // 401 -> refresh token + retry
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	g := &GitHub{
		token:          "test-token",
		httpClient:     srv.Client(),
		apiBase:        srv.URL,
		maxRetries:     5,
		maxWait:        time.Second,
		backoffBase:    time.Millisecond,
		networkMaxWait: time.Second,
	}
	resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("send returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retries, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts (503, 401, 200), got %d", got)
	}
}

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "opaque transport failure" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestSendRetriesHTMLEdgeErrorButNotJSON4xx(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	newGH := func(srv *httptest.Server) *GitHub {
		return &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 4, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	}

	// An HTML 400 (edge error) is retried, then succeeds.
	var calls int32
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<html><head><title>Bad request</title></head><body>Bad request</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer htmlSrv.Close()
	resp, err := newGH(htmlSrv).send(context.Background(), http.MethodPost, htmlSrv.URL+"/git/blobs", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("send returned error on retryable HTML edge error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected retry then 200 (2 calls), got status %d in %d calls", resp.StatusCode, calls)
	}

	// A genuine JSON 400 is NOT retried — returned to the caller immediately.
	var jcalls int32
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&jcalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Problems parsing JSON"}`))
	}))
	defer jsonSrv.Close()
	resp, err = newGH(jsonSrv).send(context.Background(), http.MethodPost, jsonSrv.URL+"/git/blobs", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("send should return the response, not an error, for a JSON 4xx: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || atomic.LoadInt32(&jcalls) != 1 {
		t.Fatalf("expected a single un-retried JSON 400, got status %d in %d calls", resp.StatusCode, jcalls)
	}
}

func TestIsRetryableNetErr(t *testing.T) {
	if isRetryableNetErr(nil) {
		t.Fatal("nil is not retryable")
	}
	// net.Error with Timeout() retries even when the message matches nothing.
	if !isRetryableNetErr(fakeTimeoutErr{}) {
		t.Fatal("a net.Error timeout should be retryable")
	}
	for _, msg := range []string{
		"context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
		"dial tcp: connect: connection refused",
		"read: connection reset by peer",
		"dial tcp: lookup api.github.com: no such host",
		"net/http: TLS handshake timeout",
		"unexpected EOF",
	} {
		if !isRetryableNetErr(errors.New(msg)) {
			t.Fatalf("expected retryable transport error: %q", msg)
		}
	}
	if isRetryableNetErr(errors.New("invalid json payload")) {
		t.Fatal("a non-transport error must not be retried")
	}
}

func TestNetworkRetryWaitPlateaus(t *testing.T) {
	base := 2 * time.Second
	want := map[int]time.Duration{
		0:  2 * time.Second,
		1:  4 * time.Second,
		2:  8 * time.Second,
		3:  16 * time.Second,
		4:  30 * time.Second, // 32s capped to 30s
		5:  30 * time.Second,
		20: 30 * time.Second, // plateau holds for a long outage
	}
	for attempt, exp := range want {
		if got := networkRetryWait(base, attempt); got != exp {
			t.Fatalf("networkRetryWait(attempt=%d) = %s, want %s", attempt, got, exp)
		}
	}
}

func TestBackoffWaitRidesOutKnownReset(t *testing.T) {
	g := &GitHub{maxRetries: 6, maxWait: 120 * time.Second, backoffBase: 2 * time.Second}
	// Fresh reset within the cap: wait it out (~5m), reported as a known reset so
	// the caller doesn't spend its retry budget, not clamped to maxWait.
	if w, known, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(5 * time.Minute)}, 0); !ok || !known || w < 4*time.Minute || w > 6*time.Minute {
		t.Fatalf("expected ~5m known-reset ride-out, got %s known=%v ok=%v", w, known, ok)
	}
	// Implausibly far reset: give up rather than wedge.
	if _, _, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(3 * time.Hour)}, 0); ok {
		t.Fatal("should not wait out a reset beyond the cap")
	}
	// Expired reset hint (stale header / clock skew): treat as hint-less so it
	// consumes the budget instead of hot-looping with attempt frozen at ~0 wait.
	if w, known, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(-time.Minute)}, 0); !ok || known || w != 2*time.Second {
		t.Fatalf("expected expired reset to fall through to 2s budget-consuming backoff, got %s known=%v ok=%v", w, known, ok)
	}
	if _, _, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(-time.Minute)}, 6); ok {
		t.Fatal("should give up after maxRetries on an expired reset hint")
	}
	// Hint-less secondary limit: bounded exponential backoff, capped by maxRetries.
	if w, known, ok := g.backoffWait(&RateLimitError{}, 0); !ok || known || w != 2*time.Second {
		t.Fatalf("expected 2s exponential backoff, got %s known=%v ok=%v", w, known, ok)
	}
	if _, _, ok := g.backoffWait(&RateLimitError{}, 6); ok {
		t.Fatal("should give up after maxRetries on a hint-less limit")
	}
}

func TestSearchOwnerQualifierDistinguishesOrgAndUser(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var userHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			atomic.AddInt32(&userHits, 1)
			_, _ = w.Write([]byte(`{"type":"Organization"}`))
		case "/users/alice":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	if q, err := g.searchOwnerQualifier(context.Background(), "acme"); err != nil || q != "org:" {
		t.Fatalf("an organization scope must use org:, got %q", q)
	}
	if q, err := g.searchOwnerQualifier(context.Background(), "alice"); err != nil || q != "user:" {
		t.Fatalf("a user scope must use user:, got %q", q)
	}
	// Second lookup of the same login is served from cache, not a new request.
	if q, err := g.searchOwnerQualifier(context.Background(), "acme"); err != nil || q != "org:" {
		t.Fatalf("cached org lookup mismatch: %q", q)
	}
	if got := atomic.LoadInt32(&userHits); got != 1 {
		t.Fatalf("expected the org type to be cached after one lookup, made %d", got)
	}
}

func TestEachOpenPRPropagatesOwnerQualifierFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var searchHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			http.Error(w, "temporary scope lookup failure", http.StatusForbidden)
		case "/search/issues":
			atomic.AddInt32(&searchHits, 1)
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	err := g.EachOpenPR(context.Background(), "acme", false, func(SearchPR) (bool, error) {
		t.Fatal("callback should not run when owner qualifier lookup fails")
		return true, nil
	})
	if err == nil {
		t.Fatal("expected owner qualifier lookup failure to propagate")
	}
	if got := atomic.LoadInt32(&searchHits); got != 0 {
		t.Fatalf("must not run the open-PR search with a fallback qualifier after lookup failure, search hits=%d", got)
	}
}

func TestEachOpenPRStopsAtCallerBudgetWithoutOverFetching(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var searchHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/alice" {
			_, _ = w.Write([]byte(`{"type":"User"}`))
			return
		}
		if r.URL.Path == "/search/issues" {
			atomic.AddInt32(&searchHits, 1)
			// A full page, so pagination would continue if the caller didn't stop.
			parts := make([]string, 0, 100)
			for i := 0; i < 100; i++ {
				parts = append(parts, `{"number":`+strconv.Itoa(i+1)+`,"repository_url":"https://api.github.com/repos/alice/repo"}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(parts, ",") + `]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	seen := 0
	err := g.EachOpenPR(context.Background(), "alice", false, func(SearchPR) (bool, error) {
		seen++
		return seen >= 5, nil // caller's post-filter budget
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Fatalf("EachOpenPR should stop at the caller's budget (5), saw %d", seen)
	}
	if got := atomic.LoadInt32(&searchHits); got != 1 {
		t.Fatalf("should stop within the first page, made %d search requests", got)
	}
}

func TestEachOpenPRIncludesBody(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/alice" {
			_, _ = w.Write([]byte(`{"type":"User"}`))
			return
		}
		if r.URL.Path == "/search/issues" {
			_, _ = w.Write([]byte(`{"items":[{"number":7,"repository_url":"https://api.github.com/repos/alice/repo","body":"notes\\n<!-- crq:skip-autoreview -->","user":{"login":"alice"}}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	var got SearchPR
	if err := g.EachOpenPR(context.Background(), "alice", false, func(pr SearchPR) (bool, error) {
		got = pr
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.Number != 7 || got.Author != "alice" || !strings.Contains(got.Body, "crq:skip-autoreview") {
		t.Fatalf("search PR metadata mismatch: %#v", got)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, c := range []int{500, 502, 503, 504} {
		if !isRetryableStatus(c) {
			t.Fatalf("expected status %d to be retryable", c)
		}
	}
	for _, c := range []int{200, 400, 401, 403, 404, 422, 429, 501} {
		if isRetryableStatus(c) {
			t.Fatalf("status %d must not be retryable", c)
		}
	}
}

func TestRateLimitFrom(t *testing.T) {
	mk := func(status int, hdr map[string]string) *http.Response {
		h := http.Header{}
		for k, v := range hdr {
			h.Set(k, v)
		}
		return &http.Response{StatusCode: status, Header: h}
	}

	if rateLimitFrom(mk(http.StatusOK, nil), "") != nil {
		t.Fatal("200 must not be a rate limit")
	}
	if rateLimitFrom(mk(http.StatusNotFound, nil), "") != nil {
		t.Fatal("404 must not be a rate limit")
	}

	// Plain 403 (no quota exhaustion / secondary markers) is a permission error, not a rate limit.
	if rl := rateLimitFrom(mk(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4999"}), "Resource not accessible by integration"); rl != nil {
		t.Fatalf("plain 403 must not be a rate limit: %#v", rl)
	}

	// Primary: remaining 0 + reset header.
	reset := time.Now().Add(15 * time.Minute).Unix()
	rl := rateLimitFrom(mk(http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	}), "API rate limit exceeded for user ID 1.")
	if rl == nil || rl.Kind != "primary" || rl.Until.Unix() != reset {
		t.Fatalf("primary rate limit mismatch: %#v", rl)
	}

	// Secondary with Retry-After.
	rl = rateLimitFrom(mk(http.StatusForbidden, map[string]string{"Retry-After": "30"}), "You have exceeded a secondary rate limit")
	if rl == nil || rl.Kind != "secondary" || rl.Until.IsZero() {
		t.Fatalf("secondary (retry-after) mismatch: %#v", rl)
	}

	// Secondary by body, no Retry-After: Until left zero so the caller applies exponential backoff.
	rl = rateLimitFrom(mk(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4321"}), "You have exceeded a secondary rate limit. Please wait a few minutes.")
	if rl == nil || rl.Kind != "secondary" || !rl.Until.IsZero() {
		t.Fatalf("secondary (body) mismatch: %#v", rl)
	}

	// 429 with reset is a rate limit too.
	rl = rateLimitFrom(mk(http.StatusTooManyRequests, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	}), "")
	if rl == nil || rl.Kind != "primary" {
		t.Fatalf("429 primary mismatch: %#v", rl)
	}
}

func TestSendConditionalGETReplaysCachedBodyOn304(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("conditional cache request authorization = %q, want Bearer t", got)
		}
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("first request must not be conditional, got If-None-Match=%q", r.Header.Get("If-None-Match"))
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="last"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"n":1}`))
		default:
			if got := r.Header.Get("If-None-Match"); got != `"v1"` {
				t.Errorf("expected If-None-Match %q, got %q", `"v1"`, got)
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	for i := 1; i <= 2; i++ {
		resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
		if err != nil {
			t.Fatalf("send %d returned error: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("send %d: expected 200, got %d", i, resp.StatusCode)
		}
		if string(b) != `{"n":1}` {
			t.Fatalf("send %d: body mismatch: %q", i, b)
		}
		if got := resp.Header.Get("Link"); !strings.Contains(got, "page=2") {
			t.Fatalf("send %d: Link header lost: %q", i, got)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", got)
	}
}

func TestSendConditionalGETMergesHeadersFrom304(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("first request must not be conditional, got If-None-Match=%q", r.Header.Get("If-None-Match"))
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="last"`)
			w.Header().Set("X-Cached-Only", "preserved")
			_, _ = w.Write([]byte(`{"n":1}`))
		default:
			if got := r.Header.Get("If-None-Match"); got != `"v1"` {
				t.Errorf("expected If-None-Match %q, got %q", `"v1"`, got)
			}
			w.Header().Set("Link", `<https://api.github.com/x?page=3>; rel="last"`)
			w.Header().Set("Content-Length", "999")
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("first send returned error: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("second send returned error: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after replay, got %d", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(`{"n":1}`)) {
		t.Fatalf("expected cached content length %d, got %d", len(`{"n":1}`), resp.ContentLength)
	}
	if string(b) != `{"n":1}` {
		t.Fatalf("cached body mismatch: %q", b)
	}
	if got := resp.Header.Get("Link"); !strings.Contains(got, "page=3") {
		t.Fatalf("expected 304 Link header, got %q", got)
	}
	if got := resp.Header.Get("X-Cached-Only"); got != "preserved" {
		t.Fatalf("cached-only header not preserved: %q", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", got)
	}
}

func TestSendConditionalGETRefreshesCacheOnChange(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte(`{"n":1}`))
		case 2:
			// Content changed: full 200 with a new ETag replaces the cache entry.
			w.Header().Set("ETag", `"v2"`)
			_, _ = w.Write([]byte(`{"n":2}`))
		default:
			if got := r.Header.Get("If-None-Match"); got != `"v2"` {
				t.Errorf("expected refreshed If-None-Match %q, got %q", `"v2"`, got)
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	want := []string{`{"n":1}`, `{"n":2}`, `{"n":2}`}
	for i, expected := range want {
		resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
		if err != nil {
			t.Fatalf("send %d returned error: %v", i+1, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != expected {
			t.Fatalf("send %d: expected body %q, got %q", i+1, expected, b)
		}
	}
}

func TestSendConditionalGETPreservesOversizedUncachedBody(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	largeBody := strings.Repeat("x", maxETagBody+1)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("oversized first request must not be conditional, got If-None-Match=%q", r.Header.Get("If-None-Match"))
			}
			w.Header().Set("ETag", `"large"`)
			_, _ = w.Write([]byte(largeBody))
		default:
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("oversized response must not be cached, got If-None-Match=%q", got)
			}
			w.Header().Set("ETag", `"small"`)
			_, _ = w.Write([]byte(`{"n":2}`))
		}
	}))
	defer srv.Close()

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("send returned error: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != largeBody {
		t.Fatalf("oversized body was not preserved: got %d bytes, want %d", len(b), len(largeBody))
	}

	resp, err = g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("second send returned error: %v", err)
	}
	_ = resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", got)
	}
}

func TestRequestPagedFollowsLinksThroughETagCache(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var calls int32
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch r.URL.Path {
		case "/items":
			if r.Header.Get("If-None-Match") == `"p1"` {
				w.Header().Set("ETag", `"p1"`)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"p1"`)
			w.Header().Set("Link", `<`+base+`/items2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":1}]`))
		case "/items2":
			if r.Header.Get("If-None-Match") == `"p2"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"p2"`)
			_, _ = w.Write([]byte(`[{"id":2}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	for i := 1; i <= 2; i++ {
		var out []struct {
			ID int `json:"id"`
		}
		if err := g.requestPaged(context.Background(), "/items", &out); err != nil {
			t.Fatalf("requestPaged pass %d: %v", i, err)
		}
		if len(out) != 2 || out[0].ID != 1 || out[1].ID != 2 {
			t.Fatalf("requestPaged pass %d: unexpected result %#v", i, out)
		}
	}
	// Pass 1 fetches both pages fresh; pass 2 revalidates both as 304s but must
	// still stitch the full set together from cache.
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("expected 4 upstream requests (2 fresh + 2 revalidations), got %d", got)
	}
}

// TestListCheckRunsEnvelopePagination: the check-runs endpoint wraps pages in
// a {total_count, check_runs} envelope; the loop must follow Link headers,
// concatenate pages, and surface a missing ref as ErrNotFound.
func TestListCheckRunsEnvelopePagination(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/gone/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case r.URL.Query().Get("page") == "2":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[{"id":3,"name":"Macroscope - Correctness Check","status":"completed","conclusion":"neutral","app":{"slug":"macroscopeapp"},"output":{"title":"1 issue identified (5 code objects reviewed).","summary":""}}]}`))
		default:
			w.Header().Set("Link", `<`+srvURL+r.URL.Path+`?per_page=100&page=2>; rel="next"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[{"id":1,"name":"Cursor Bugbot","status":"completed","conclusion":"success","app":{"slug":"cursor"},"output":{"title":"Bugbot Review","summary":"no issues found!"}},{"id":2,"name":"CI","status":"in_progress","conclusion":null,"completed_at":null,"app":{"slug":"github-actions"}}]}`))
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	g := &GitHub{
		token:       "test-token",
		httpClient:  srv.Client(),
		apiBase:     srv.URL,
		maxRetries:  2,
		maxWait:     time.Second,
		backoffBase: time.Millisecond,
	}
	runs, err := g.ListCheckRuns(context.Background(), "o/r", "abc123")
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(runs) != 3 || runs[0].ID != 1 || runs[2].ID != 3 {
		t.Fatalf("expected 3 runs across pages, got %+v", runs)
	}
	if runs[0].App.Slug != "cursor" || runs[0].Output.Summary != "no issues found!" {
		t.Fatalf("run fields not decoded: %+v", runs[0])
	}
	if !runs[1].CompletedAt.IsZero() || runs[1].Conclusion != "" {
		t.Fatalf("null conclusion/completed_at must decode to zero values: %+v", runs[1])
	}
	if runs[2].Name != "Macroscope - Correctness Check" {
		t.Fatalf("page 2 not appended: %+v", runs[2])
	}

	if _, err := g.ListCheckRuns(context.Background(), "o/r", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ref must map to ErrNotFound, got %v", err)
	}
}

func TestMergePullSendsExactHeadAndMethod(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var gotMethod, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"sha":"abc123","merged":true,"message":"merged"}`))
	}))
	defer srv.Close()
	g := &GitHub{
		token: "test-token", httpClient: srv.Client(), apiBase: srv.URL,
		maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond,
	}
	result, err := g.MergePull(context.Background(), "owner/repo", 17, "abc123", "squash")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged || gotMethod != http.MethodPut || gotPath != "/repos/owner/repo/pulls/17/merge" {
		t.Fatalf("result=%+v method=%s path=%s", result, gotMethod, gotPath)
	}
	if gotBody["sha"] != "abc123" || gotBody["merge_method"] != "squash" {
		t.Fatalf("merge body = %#v", gotBody)
	}
}

// TestListCheckRunsRidesETagCache: a 304 revalidation must replay the cached
// envelope instead of failing or double-charging the REST quota.
func TestListCheckRunsRidesETagCache(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	var calls, conditional int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// A second unconditional request means the ETag was not replayed. Fail
		// loudly instead of serving 200 again, which let the old assertion
		// (2 calls, same payload) pass even when caching had regressed.
		if n > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":9,"name":"Cursor Bugbot","status":"in_progress","app":{"slug":"cursor"}}]}`))
	}))
	defer srv.Close()

	g := &GitHub{
		token:       "test-token",
		httpClient:  srv.Client(),
		apiBase:     srv.URL,
		maxRetries:  2,
		maxWait:     time.Second,
		backoffBase: time.Millisecond,
	}
	for i := 0; i < 2; i++ {
		runs, err := g.ListCheckRuns(context.Background(), "o/r", "abc123")
		if err != nil {
			t.Fatalf("ListCheckRuns pass %d: %v", i, err)
		}
		if len(runs) != 1 || runs[0].ID != 9 {
			t.Fatalf("pass %d: got %+v", i, runs)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 HTTP calls (200 then 304), got %d", got)
	}
	if got := atomic.LoadInt32(&conditional); got != 1 {
		t.Fatalf("the second call must carry If-None-Match, got %d conditional requests", got)
	}
}

// The repository picker offers what is in scope, and /users/{owner}/repos lists
// only PUBLIC repositories whoever asks. A personal account is the ordinary
// case for the private workflow, so the picker showed a partial list — or an
// empty one — while claiming to show everything in scope. Only the
// authenticated-user endpoint answers for private repositories owned by the
// token and for private repositories it collaborates on under another user.
func TestListOwnerReposUsesTheAuthenticatedEndpointForPersonalAccounts(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case "/users/alice", "/users/bob":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		case "/user/repos":
			paths = append(paths, r.URL.String())
			_, _ = w.Write([]byte(`[
				{"full_name":"alice/private","private":true},
				{"full_name":"bob/private","private":true}
			]`))
		case "/users/bob/repos":
			paths = append(paths, r.URL.String())
			_, _ = w.Write([]byte(`[
				{"full_name":"bob/public"},
				{"full_name":"bob/private","private":false}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	own, err := g.ListOwnerRepos(context.Background(), "alice", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].FullName != "alice/private" || !own[0].Private {
		t.Fatalf("repos = %+v, want the token's own private repository", own)
	}
	if len(paths) != 1 || !strings.Contains(paths[0], "affiliation=owner,collaborator") ||
		!strings.Contains(paths[0], "per_page=100") {
		t.Fatalf("requested %q, want the authenticated endpoint with both query halves intact", paths)
	}

	// Another personal owner is filtered from the authenticated list, which
	// includes private repositories on which the viewer collaborates.
	other, err := g.ListOwnerRepos(context.Background(), "bob", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Fatalf("repos = %+v, want the public and private collaborator repositories", other)
	}
	byName := map[string]Repo{}
	for _, repo := range other {
		byName[repo.FullName] = repo
	}
	if !byName["bob/private"].Private || byName["bob/public"].Private {
		t.Fatalf("repos = %+v, want deduplicated public plus authenticated-private metadata", other)
	}
	if len(paths) != 3 ||
		!strings.HasPrefix(paths[1], "/users/bob/repos?per_page=100") ||
		!strings.HasPrefix(paths[2], "/user/repos?affiliation=owner,collaborator") {
		t.Fatalf("requested %q, want both public and authenticated endpoints for another personal owner", paths)
	}
}

func TestListOwnerReposCanReadOneSentinelPastTenPages(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case "/users/alice":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		case "/user/repos":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			pages = max(pages, page)
			count := 100
			if page == 11 {
				count = 1
			}
			repos := make([]Repo, 0, count)
			for i := 0; i < count; i++ {
				repos = append(repos, Repo{FullName: "alice/repo-" + strconv.Itoa((page-1)*100+i)})
			}
			_ = json.NewEncoder(w).Encode(repos)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	repos, err := g.ListOwnerRepos(context.Background(), "alice", 1001)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1001 || pages != 11 {
		t.Fatalf("repos=%d pages=%d, want the one-row sentinel from page 11", len(repos), pages)
	}
}

func TestListOwnerReposPaginatesPastNonOwnerRows(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case "/users/alice":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		case "/user/repos":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			pages = max(pages, page)
			if page == 1 {
				repos := make([]Repo, 100)
				for i := range repos {
					repos[i].FullName = "other/repo-" + strconv.Itoa(i)
				}
				_ = json.NewEncoder(w).Encode(repos)
				return
			}
			_ = json.NewEncoder(w).Encode([]Repo{{FullName: "alice/private-old"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	repos, err := g.ListOwnerRepos(context.Background(), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "alice/private-old" || pages != 2 {
		t.Fatalf("repos=%+v pages=%d, want the owner match from page 2", repos, pages)
	}
}

// "crq cannot read who this token is" is cached for the process, because a
// token without the scope will not grow one. A 500 or a timeout is not that
// answer — caching it kept every later repository picker on the public-only
// listing, which omits the caller's own private repositories, long after GitHub
// came back.
func TestViewerLoginRetriesAfterATransientFailureButNotARefusal(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var down int32 = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		if atomic.LoadInt32(&down) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"login":"kristofferR"}`))
	}))
	defer srv.Close()

	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 1, maxWait: time.Millisecond, backoffBase: time.Millisecond, networkMaxWait: time.Millisecond}
	if got, err := g.viewerLogin(context.Background()); err == nil || got != "" {
		t.Fatalf("viewerLogin = %q, err %v, want the transient failure propagated", got, err)
	}
	atomic.StoreInt32(&down, 0)
	if got, err := g.viewerLogin(context.Background()); err != nil || got != "kristofferR" {
		t.Errorf("viewerLogin = %q, err %v, want the identity once GitHub answers again", got, err)
	}

	// A refusal IS an answer, and asking again every time would spend quota on
	// a question whose answer cannot change within the process.
	var denials int32
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&denials, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer denied.Close()
	d := &GitHub{token: "t", httpClient: denied.Client(), apiBase: denied.URL, maxRetries: 1, maxWait: time.Millisecond, backoffBase: time.Millisecond, networkMaxWait: time.Millisecond}
	if got, err := d.viewerLogin(context.Background()); err != nil || got != "" {
		t.Fatalf("viewerLogin = %q, err %v, want none from a refused token", got, err)
	}
	// Counted from here: send retries a 401 once with a refreshed token, so the
	// question is whether a SECOND call asks again, not how many requests the
	// first one took.
	asked := atomic.LoadInt32(&denials)
	if got, err := d.viewerLogin(context.Background()); err != nil || got != "" {
		t.Fatalf("viewerLogin = %q, err %v, want none from a refused token", got, err)
	}
	if got := atomic.LoadInt32(&denials); got != asked {
		t.Errorf("asked %d more times, want the refusal remembered", got-asked)
	}
}

func TestViewerLoginDoesNotOverwriteConcurrentIdentity(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseDenied := make(chan struct{})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			close(firstEntered)
			<-secondEntered
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case 2:
			close(secondEntered)
			<-releaseDenied
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 1, maxWait: time.Millisecond, backoffBase: time.Millisecond, networkMaxWait: time.Millisecond}

	var firstLogin string
	var firstErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstLogin, firstErr = g.viewerLogin(t.Context())
	}()
	<-firstEntered
	secondResult := make(chan struct {
		login string
		err   error
	}, 1)
	go func() {
		login, err := g.viewerLogin(t.Context())
		secondResult <- struct {
			login string
			err   error
		}{login, err}
	}()
	wg.Wait()
	close(releaseDenied)
	second := <-secondResult

	if firstErr != nil || firstLogin != "alice" {
		t.Fatalf("successful viewer lookup = %q, %v", firstLogin, firstErr)
	}
	if second.err != nil || second.login != "alice" {
		t.Fatalf("concurrent refusal returned %q, %v; want cached identity", second.login, second.err)
	}
	if got, err := g.viewerLogin(t.Context()); err != nil || got != "alice" {
		t.Fatalf("cached viewer = %q, %v; successful identity was overwritten", got, err)
	}
}

func TestViewerLoginConcurrentIdentityReplacesRefusal(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	successEntered := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			close(successEntered)
			<-releaseSuccess
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 1, maxWait: time.Millisecond, backoffBase: time.Millisecond, networkMaxWait: time.Millisecond}

	successResult := make(chan struct {
		login string
		err   error
	}, 1)
	go func() {
		login, err := g.viewerLogin(t.Context())
		successResult <- struct {
			login string
			err   error
		}{login, err}
	}()
	<-successEntered
	if login, err := g.viewerLogin(t.Context()); err != nil || login != "" {
		t.Fatalf("concurrent refusal = %q, %v; want an unreadable identity", login, err)
	}
	close(releaseSuccess)
	success := <-successResult

	if success.err != nil || success.login != "alice" {
		t.Fatalf("successful viewer lookup = %q, %v", success.login, success.err)
	}
	if got, err := g.viewerLogin(t.Context()); err != nil || got != "alice" {
		t.Fatalf("cached viewer = %q, %v; successful identity did not replace refusal", got, err)
	}
}
