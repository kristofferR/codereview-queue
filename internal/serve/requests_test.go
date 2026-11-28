package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type callerGateway struct{ caller string }

func (g *callerGateway) Forward(ctx context.Context, _ string, _ string, _ []byte) (GitHubResponse, error) {
	g.caller = CallerOf(ctx)
	return GitHubResponse{Status: http.StatusOK, Body: []byte(`{}`)}, nil
}

func (*callerGateway) CanWrite(context.Context, string) (bool, error) { return true, nil }

// Attribution is only as good as the name that reaches the transport: the
// handler has to carry what the client called itself through to the forward.
func TestGatewayForwardsTheCallerItWasTold(t *testing.T) {
	gateway := &callerGateway{}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: gateway})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://127.0.0.1:7777/api/github/repos/o/r/pulls/7", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("X-CRQ-Client", "1")
	req.Header.Set("X-CRQ-Caller", "autofix@host")
	rec := httptest.NewRecorder()
	srv.handleGitHub(rec, req)
	if rec.Code != http.StatusOK || gateway.caller != "autofix@host" {
		t.Fatalf("status = %d, caller = %q", rec.Code, gateway.caller)
	}
}

func TestRequestsEndpointServesTheAccountingItIsGiven(t *testing.T) {
	var asked time.Duration
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Requests: func(window time.Duration) any {
		asked = window
		return map[string]int{"counted": 42}
	}})
	get := func(srv *Server, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
		return rec
	}
	rec := get(srv, "http://127.0.0.1:7777/api/requests?window=15m")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"counted": 42`) || asked != 15*time.Minute {
		t.Fatalf("response = %d %s, window asked = %s", rec.Code, rec.Body.String(), asked)
	}
	// A server with no accounting says so rather than serving an empty report
	// that reads as "nothing was asked".
	if rec := get(New(&stubLoader{}, Options{Addr: "127.0.0.1:7777"}), "http://127.0.0.1:7777/api/requests"); rec.Code != http.StatusNotFound {
		t.Fatalf("status without accounting = %d, want 404", rec.Code)
	}
}
