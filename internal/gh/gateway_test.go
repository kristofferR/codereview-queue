package gh

import (
	"context"
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

func TestServerClientRejectsInvalidURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:7777", "ftp://localhost/crq", "http://user@localhost"} {
		if _, err := NewGitHubViaServer(raw, ""); err == nil {
			t.Errorf("server URL %q was accepted", raw)
		}
	}
}
