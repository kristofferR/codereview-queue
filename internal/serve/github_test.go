package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type recordingGateway struct {
	calls  int
	method string
	uri    string
	body   string
	result GitHubResponse
	err    error
}

func (g *recordingGateway) Forward(_ context.Context, method, uri string, body []byte) (GitHubResponse, error) {
	g.calls++
	g.method, g.uri, g.body = method, uri, string(body)
	return g.result, g.err
}

func gatewayRequest(t *testing.T, srv *Server, method, rawURL, remote, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, rawURL, strings.NewReader(`{"value":1}`))
	req.RemoteAddr = remote
	req.SetPathValue("path", strings.TrimPrefix(req.URL.Path, "/api/github/"))
	req.Header.Set("X-CRQ-Client", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.handleGitHub(rec, req)
	return rec
}

func TestGitHubGatewayForwardsLoopbackRequests(t *testing.T) {
	gateway := &recordingGateway{result: GitHubResponse{
		Status: http.StatusCreated,
		Header: http.Header{"X-RateLimit-Remaining": []string{"4999"}},
		Body:   []byte(`{"ok":true}`),
	}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway})
	rec := gatewayRequest(t, srv, http.MethodPost,
		"http://127.0.0.1:7777/api/github/repos/o/r/issues?labels=bug", "127.0.0.1:4321", "")

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("gateway response = %d %s", rec.Code, rec.Body.String())
	}
	if gateway.calls != 1 || gateway.method != http.MethodPost || gateway.uri != "/repos/o/r/issues?labels=bug" || gateway.body != `{"value":1}` {
		t.Fatalf("forwarded = calls=%d method=%q uri=%q body=%q", gateway.calls, gateway.method, gateway.uri, gateway.body)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "4999" {
		t.Fatalf("rate-limit header = %q, want it preserved", got)
	}
}

func TestGitHubGatewayPreservesEscapedPaths(t *testing.T) {
	gateway := &recordingGateway{result: GitHubResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"object":{"sha":"abc"}}`),
	}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway})
	httpServer := httptest.NewServer(srv.routes())
	defer httpServer.Close()

	client, err := ghapi.NewGitHubViaServer(httpServer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := client.GetRef(t.Context(), "o/r", "state%prod#draft")
	if err != nil || sha != "abc" || gateway.uri != "/repos/o/r/git/ref/heads/state%25prod%23draft" {
		t.Fatalf("escaped gateway ref = SHA %q, URI %q, err %v", sha, gateway.uri, err)
	}
}

func TestServerRoutesGitHubClientsThroughTheGateway(t *testing.T) {
	gateway := &recordingGateway{result: GitHubResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"number":7,"state":"open","head":{"sha":"abc"}}`),
	}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway})
	httpServer := httptest.NewServer(srv.routes())
	defer httpServer.Close()

	client, err := ghapi.NewGitHubViaServer(httpServer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	pull, err := client.GetPull(t.Context(), "o/r", 7)
	if err != nil || pull.Head.SHA != "abc" {
		t.Fatalf("pull = %+v, err = %v", pull, err)
	}
	if gateway.calls != 1 || gateway.uri != "/repos/o/r/pulls/7" {
		t.Fatalf("gateway calls = %d, URI = %q", gateway.calls, gateway.uri)
	}
}

func TestGitHubGatewayRequiresClientHeader(t *testing.T) {
	gateway := &recordingGateway{}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://127.0.0.1:7777/api/github/repos/o/r", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	rec := httptest.NewRecorder()
	srv.handleGitHub(rec, req)

	if rec.Code != http.StatusForbidden || gateway.calls != 0 {
		t.Fatalf("missing-header response = %d, calls = %d", rec.Code, gateway.calls)
	}
}

func TestGitHubGatewayRequiresATokenOffLoopback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		provided   string
		want       int
		calls      int
	}{
		{name: "no token configured", want: http.StatusUnauthorized},
		{name: "wrong token", configured: "secret", provided: "wrong", want: http.StatusUnauthorized},
		{name: "matching token", configured: "secret", provided: "secret", want: http.StatusOK, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &recordingGateway{result: GitHubResponse{Status: http.StatusOK, Body: []byte(`{}`)}}
			srv := New(&stubLoader{}, Options{
				Addr: "0.0.0.0:7777", Host: "atlas", AllowedHosts: []string{"crq.example.test"},
				Gateway: gateway, GatewayToken: tc.configured,
			})
			rec := gatewayRequest(t, srv, http.MethodGet,
				"http://crq.example.test:7777/api/github/repos/o/r", "192.0.2.4:4321", tc.provided)
			if rec.Code != tc.want || gateway.calls != tc.calls {
				t.Fatalf("response = %d, calls = %d; want %d, %d", rec.Code, gateway.calls, tc.want, tc.calls)
			}
		})
	}
}

func TestGitHubGatewayRequiresTheBearerScheme(t *testing.T) {
	gateway := &recordingGateway{}
	srv := New(&stubLoader{}, Options{
		Addr: "0.0.0.0:7777", Gateway: gateway, GatewayToken: "secret",
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://192.0.2.1:7777/api/github/repos/o/r", nil)
	req.RemoteAddr = "192.0.2.4:4321"
	req.SetPathValue("path", "repos/o/r")
	req.Header.Set("X-CRQ-Client", "1")
	req.Header.Set("Authorization", "secret")
	rec := httptest.NewRecorder()
	srv.handleGitHub(rec, req)

	if rec.Code != http.StatusUnauthorized || gateway.calls != 0 {
		t.Fatalf("non-bearer request = %d, calls = %d; want it rejected", rec.Code, gateway.calls)
	}
}

func TestGitHubGatewayDoesNotTrustALoopbackReverseProxyWithoutAToken(t *testing.T) {
	gateway := &recordingGateway{}
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777", AllowedHosts: []string{"crq.example.test"}, Gateway: gateway,
	})
	rec := gatewayRequest(t, srv, http.MethodGet,
		"http://crq.example.test/api/github/repos/o/r", "127.0.0.1:4321", "")
	if rec.Code != http.StatusUnauthorized || gateway.calls != 0 {
		t.Fatalf("proxied request = %d, calls = %d; want a server token", rec.Code, gateway.calls)
	}
}

func TestReadOnlyServerRefusesGitHubMutations(t *testing.T) {
	gateway := &recordingGateway{result: GitHubResponse{Status: http.StatusOK, Body: []byte(`{}`)}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway, ReadOnly: true})
	rec := gatewayRequest(t, srv, http.MethodPost,
		"http://127.0.0.1:7777/api/github/repos/o/r/issues", "127.0.0.1:4321", "")
	if rec.Code != http.StatusForbidden || gateway.calls != 0 {
		t.Fatalf("read-only mutation = %d, calls = %d", rec.Code, gateway.calls)
	}

	rec = gatewayRequest(t, srv, http.MethodGet,
		"http://127.0.0.1:7777/api/github/repos/o/r", "127.0.0.1:4321", "")
	if rec.Code != http.StatusOK || gateway.calls != 1 {
		t.Fatalf("read-only GET = %d, calls = %d", rec.Code, gateway.calls)
	}
}

func TestReadOnlyServerAllowsGraphQLQueriesButRejectsMutations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int
		calls int
	}{
		{
			name:  "named query with object default",
			query: `query Review($filter: Filter = {name: "mutation"}) { repository { name } }`,
			want:  http.StatusOK, calls: 1,
		},
		{
			name: "fragment before query",
			query: `# mutation in a comment
fragment mutation on Repository { name }
query Read { repository { ...mutation } }`,
			want: http.StatusOK, calls: 1,
		},
		{name: "anonymous query", query: `{ viewer { login } }`, want: http.StatusOK, calls: 1},
		{name: "mutation", query: `mutation Resolve { resolveReviewThread(input: {}) { clientMutationId } }`, want: http.StatusForbidden},
		{name: "mutation after query", query: `query Read { viewer { login } } mutation Write { deleteThing }`, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &recordingGateway{result: GitHubResponse{Status: http.StatusOK, Body: []byte(`{"data":{}}`)}}
			srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway, ReadOnly: true})
			body, err := json.Marshal(map[string]string{"query": tc.query})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"http://127.0.0.1:7777/api/github/graphql", strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:4321"
			req.Header.Set("X-CRQ-Client", "1")
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)

			if rec.Code != tc.want || gateway.calls != tc.calls {
				t.Fatalf("response = %d, calls = %d; want %d, %d", rec.Code, gateway.calls, tc.want, tc.calls)
			}
		})
	}
}
