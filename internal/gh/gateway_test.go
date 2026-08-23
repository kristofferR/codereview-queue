package gh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerClientRoutesRESTGraphQLAndPagination(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("X-CRQ-Client") != "1" || r.Header.Get("Authorization") != "Bearer gateway-secret" {
			t.Errorf("gateway headers = client %q auth %q", r.Header.Get("X-CRQ-Client"), r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/github/repos/o/r/pulls/7":
			_, _ = io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc"}}`)
		case "/api/github/graphql":
			_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"kris"}}}`)
		case "/api/github/repos/o/r/issues/7/comments":
			if r.URL.Query().Get("page") == "2" {
				_, _ = io.WriteString(w, `[{"id":2,"body":"second"}]`)
				return
			}
			w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues/7/comments?per_page=100&page=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"id":1,"body":"first"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewGitHubViaServer(srv.URL, "gateway-secret")
	if err != nil {
		t.Fatal(err)
	}
	pull, err := client.GetPull(t.Context(), "o/r", 7)
	if err != nil || pull.Number != 7 {
		t.Fatalf("pull = %+v, err = %v", pull, err)
	}
	var graph struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(t.Context(), `query { viewer { login } }`, nil, &graph); err != nil || graph.Viewer.Login != "kris" {
		t.Fatalf("graphql = %+v, err = %v", graph, err)
	}
	comments, err := client.ListIssueComments(t.Context(), "o/r", 7)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments = %+v, err = %v", comments, err)
	}
	if got := strings.Join(paths, "\n"); strings.Contains(got, "api.github.com") || !strings.Contains(got, "page=2") {
		t.Fatalf("gateway paths = %s", got)
	}
}

func TestPersistentGatewaySharesETagsAcrossFreshClients(t *testing.T) {
	var mu sync.Mutex
	var conditions []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conditions = append(conditions, r.Header.Get("If-None-Match"))
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"pull-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"pull-v1"`)
		_, _ = io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc"}}`)
	}))
	defer upstream.Close()

	direct := NewTestClient(upstream.URL, upstream.Client())
	direct.EnableGETCoalescing()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/api/github")
		body, _ := io.ReadAll(r.Body)
		result, err := direct.Forward(r.Context(), r.Method, uri, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range result.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}))
	defer proxy.Close()

	for range 2 {
		client, err := NewGitHubViaServer(proxy.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		pull, err := client.GetPull(t.Context(), "o/r", 7)
		if err != nil || pull.Head.SHA != "abc" {
			t.Fatalf("pull = %+v, err = %v", pull, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conditions) != 2 || conditions[0] != "" || conditions[1] != `"pull-v1"` {
		t.Fatalf("upstream conditional requests = %q, want first fresh and second revalidated", conditions)
	}
}

func TestPersistentGatewayCoalescesConcurrentFreshGETs(t *testing.T) {
	var mu sync.Mutex
	fresh, total := 0, 0
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		total++
		conditional := r.Header.Get("If-None-Match") != ""
		if !conditional {
			fresh++
		}
		mu.Unlock()
		if conditional {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		select {
		case <-firstStarted:
		default:
			close(firstStarted)
			<-releaseFirst
		}
		w.Header().Set("ETag", `"pull-v1"`)
		_, _ = io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc"}}`)
	}))
	defer upstream.Close()

	direct := NewTestClient(upstream.URL, upstream.Client())
	direct.EnableGETCoalescing()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/api/github")
		result, err := direct.Forward(r.Context(), r.Method, uri, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range result.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}))
	defer proxy.Close()

	errCh := make(chan error, 2)
	for range 2 {
		go func() {
			client, err := NewGitHubViaServer(proxy.URL, "")
			if err == nil {
				_, err = client.GetPull(t.Context(), "o/r", 7)
			}
			errCh <- err
		}()
	}
	<-firstStarted
	close(releaseFirst)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if fresh != 1 || total != 2 {
		t.Fatalf("upstream requests = %d fresh / %d total, want one paid response and one conditional revalidation", fresh, total)
	}
}

func TestPersistentGatewayDoesNotBlockDifferentURLsThatSharedALegacyStripe(t *testing.T) {
	firstPR, secondPR := 1, 0
	var upstreamURL string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, fmt.Sprintf("/%d", firstPR)) {
			close(firstStarted)
			<-releaseFirst
		}
		_, _ = fmt.Fprintf(w, `{"number":%d,"state":"open","head":{"sha":"abc"}}`, firstPR)
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL

	firstURL := upstreamURL + fmt.Sprintf("/repos/o/r/pulls/%d", firstPR)
	for candidate := 2; candidate < 10_000; candidate++ {
		candidateURL := upstreamURL + fmt.Sprintf("/repos/o/r/pulls/%d", candidate)
		if legacyURLStripe(firstURL) == legacyURLStripe(candidateURL) {
			secondPR = candidate
			break
		}
	}
	if secondPR == 0 {
		t.Fatal("could not find a URL colliding under the old 64-stripe coalescer")
	}

	direct := NewTestClient(upstream.URL, upstream.Client())
	direct.EnableGETCoalescing()
	firstDone := make(chan error, 1)
	go func() {
		_, err := direct.GetPull(t.Context(), "o/r", firstPR)
		firstDone <- err
	}()
	<-firstStarted

	secondDone := make(chan error, 1)
	go func() {
		_, err := direct.GetPull(t.Context(), "o/r", secondPR)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			close(releaseFirst)
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		close(releaseFirst)
		<-firstDone
		t.Fatal("an unrelated URL waited behind the blocked GET")
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func legacyURLStripe(value string) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)
	hash := offset
	for i := 0; i < len(value); i++ {
		hash ^= uint64(value[i])
		hash *= prime
	}
	return hash % 64
}

func TestPersistentGatewayOwnsGraphQLRateLimitBackoff(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"errors":[{"type":"RATE_LIMITED","message":"rate limit exceeded"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"kris"}}}`)
	}))
	defer upstream.Close()

	direct := NewTestClient(upstream.URL, upstream.Client())
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/api/github")
		body, _ := io.ReadAll(r.Body)
		result, err := direct.Forward(r.Context(), r.Method, uri, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}))
	defer proxy.Close()

	client, err := NewGitHubViaServer(proxy.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(t.Context(), `query { viewer { login } }`, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Viewer.Login != "kris" || calls != 2 {
		t.Fatalf("GraphQL = %+v after %d upstream calls, want one server-owned retry", out, calls)
	}
}

func TestGatewayPreservesAnExhaustedGraphQLThrottle(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"errors":[{"type":"RATE_LIMITED","message":"rate limit exceeded"}]}`)
	}))
	defer upstream.Close()

	direct := NewTestClient(upstream.URL, upstream.Client())
	direct.maxRetries = 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/api/github")
		body, _ := io.ReadAll(r.Body)
		result, err := direct.Forward(r.Context(), r.Method, uri, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range result.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}))
	defer proxy.Close()

	client, err := NewGitHubViaServer(proxy.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	err = client.GraphQL(t.Context(), `query { viewer { login } }`, nil, nil)
	var throttled *RateLimitError
	if !errors.As(err, &throttled) || throttled.Kind != "graphql" {
		t.Fatalf("gateway GraphQL throttle = %T %v", err, err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want no client-owned retry", calls)
	}
}

func TestServerClientFailsClosedWhenServerIsUnavailable(t *testing.T) {
	client, err := NewGitHubViaServer("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.GetPull(ctx, "o/r", 7); err == nil {
		t.Fatal("an unavailable crq server must fail instead of falling back to GitHub")
	}
}

func TestServerClientRejectsRedirects(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls++
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()

	client, err := NewGitHubViaServer(redirect.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetPull(t.Context(), "o/r", 7); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("redirect error = %v", err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls)
	}
}

func TestGatewayForwardsStateBlobResponsesBeyondTheOldLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), (16<<20)+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	direct := NewTestClient(upstream.URL, upstream.Client())
	result, err := direct.Forward(t.Context(), http.MethodGet, "/large-state-blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Body) != len(payload) {
		t.Fatalf("forwarded body = %d bytes, want %d", len(result.Body), len(payload))
	}
}

func TestServerClientRejectsInvalidURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:7777", "ftp://localhost/crq", "http://user@localhost", "http://crq.example.test"} {
		if _, err := NewGitHubViaServer(raw, ""); err == nil {
			t.Errorf("server URL %q was accepted", raw)
		}
	}
}

func TestServerClientAllowsPlainHTTPOnlyOnLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:7777",
		"http://localhost.:7777",
		"http://127.0.0.1:7777",
		"http://[::1]:7777",
		"https://crq.example.test",
	} {
		if _, err := NewGitHubViaServer(raw, "gateway-secret"); err != nil {
			t.Errorf("server URL %q was rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://crq.example.test",
		"http://192.0.2.1:7777",
		"http://[2001:db8::1]:7777",
		"http://127.0.0.1.example.test:7777",
	} {
		if _, err := NewGitHubViaServer(raw, "gateway-secret"); err == nil || !strings.Contains(err.Error(), "must use https") {
			t.Errorf("server URL %q error = %v, want a non-loopback HTTPS refusal", raw, err)
		}
	}
}
