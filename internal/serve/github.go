package serve

import (
	"context"
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"strings"
)

// GitHubGateway is the direct, persistent GitHub transport supplied by the crq
// process. Keeping this as an interface avoids teaching the dashboard package
// anything about GitHub's API client.
type GitHubGateway interface {
	Forward(ctx context.Context, method, requestURI string, body []byte) (GitHubResponse, error)
}

type GitHubResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// handleGitHub is the control-plane boundary: every short-lived crq process
// sends its GitHub HTTP request here, and only this long-lived process reaches
// api.github.com. That gives the whole fleet one ETag cache and one backoff
// owner without moving command semantics or local-work inspection to a remote
// machine.
func (s *Server) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if s.opts.Gateway == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the GitHub gateway is not configured"})
		return
	}
	if !s.authorizeGateway(w, r) {
		return
	}
	if s.opts.ReadOnly && r.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this server is read-only"})
		return
	}

	const maxRequestBody = 16 << 20
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "GitHub request body is too large"})
		return
	}
	requestURI := "/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		requestURI += "?" + r.URL.RawQuery
	}
	result, err := s.opts.Gateway.Forward(r.Context(), r.Method, requestURI, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	for key, values := range result.Header {
		if hopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := result.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(result.Body)
}

func (s *Server) handleGatewayHealth(w http.ResponseWriter, r *http.Request) {
	if s.opts.Gateway == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "the GitHub gateway is not configured"})
		return
	}
	if !s.authorizeGateway(w, r) {
		return
	}
	snap, loaded, err := s.snapshot()
	body := map[string]any{"ok": loaded && err == nil, "rev": snap.Overview.Rev}
	if err != nil || !loaded {
		body["error"] = firstLoadError(err)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) authorizeGateway(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-CRQ-Client") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing crq client header"})
		return false
	}
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return false
	}
	if !s.gatewayAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "the GitHub gateway requires CRQ_SERVER_TOKEN for non-loopback clients",
		})
		return false
	}
	return true
}

func (s *Server) gatewayAuthorized(r *http.Request) bool {
	want := strings.TrimSpace(s.opts.GatewayToken)
	if want == "" {
		return remoteIsLoopback(r.RemoteAddr) && hostIsLoopback(r.Host)
	}
	scheme, got, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got = strings.TrimSpace(got)
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func hostIsLoopback(raw string) bool {
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		host = raw
	}
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func remoteIsLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func hopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}
