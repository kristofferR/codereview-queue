package serve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxGatewayRequestBody = 256 << 20

// GitHubGateway is the direct, persistent GitHub transport supplied by the crq
// process. Keeping this as an interface avoids teaching the dashboard package
// anything about GitHub's API client.
type GitHubGateway interface {
	Forward(ctx context.Context, method, requestURI string, body []byte) (GitHubResponse, error)
	CanWrite(ctx context.Context, repo string) (bool, error)
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

	// The server's ordinary 60-second ReadTimeout protects small endpoints. A
	// declared large gateway body gets a proportional authenticated extension,
	// still bounded, so the supported state size does not become a slow-client
	// denial-of-service path.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(gatewayBodyReadTimeout(r.ContentLength)))
	// GitHub accepts 100 MiB git blobs. A UTF-8 blob request JSON-escapes the
	// state content, so leave enough bounded room for the largest supported blob.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGatewayRequestBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "GitHub request body is too large"})
		return
	}
	requestURI, ok := githubRequestURI(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid GitHub gateway path"})
		return
	}
	if s.opts.ReadOnly && !githubRequestReadOnly(r.Method, requestURI, body) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this server is read-only"})
		return
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

func gatewayBodyReadTimeout(contentLength int64) time.Duration {
	const (
		minimum = 60 * time.Second
		maximum = 45 * time.Minute
		// 128 KiB/s lets the full gateway body arrive in about 35 minutes while
		// still expiring a stalled authenticated upload.
		minimumBytesPerSecond = 128 << 10
	)
	if contentLength <= 0 {
		return minimum
	}
	seconds := (contentLength + minimumBytesPerSecond - 1) / minimumBytesPerSecond
	timeout := time.Duration(seconds) * time.Second
	if timeout < minimum {
		return minimum
	}
	if timeout > maximum {
		return maximum
	}
	return timeout
}

func githubRequestURI(r *http.Request) (string, bool) {
	requestURI, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/github")
	if !ok || !strings.HasPrefix(requestURI, "/") {
		return "", false
	}
	if r.URL.RawQuery != "" {
		requestURI += "?" + r.URL.RawQuery
	}
	return requestURI, true
}

func githubRequestReadOnly(method, requestURI string, body []byte) bool {
	if method == http.MethodGet {
		return true
	}
	path, _, _ := strings.Cut(requestURI, "?")
	return method == http.MethodPost && path == "/graphql" && graphQLBodyReadOnly(body)
}

func graphQLBodyReadOnly(body []byte) bool {
	var request struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Query) == "" {
		return false
	}
	tokens, ok := graphQLTokens(request.Query)
	if !ok {
		return false
	}
	seenOperation := false
	for i := 0; i < len(tokens); {
		token := tokens[i]
		if token.kind == '{' {
			seenOperation = true
			i, ok = skipGraphQLSelection(tokens, i)
			if !ok {
				return false
			}
			continue
		}
		if token.kind != 'n' {
			return false
		}
		switch token.text {
		case "mutation":
			return false
		case "query", "subscription":
			seenOperation = true
		case "fragment":
		default:
			return false
		}
		i, ok = findAndSkipGraphQLSelection(tokens, i+1)
		if !ok {
			return false
		}
	}
	return seenOperation
}

// The gateway needs only the top-level operation definitions, not a GraphQL
// AST. Strings, comments and nested selection sets are skipped so a mutation
// cannot hide behind a fragment or a second operation in the same document.
type graphQLToken struct {
	kind byte
	text string
}

func graphQLTokens(query string) ([]graphQLToken, bool) {
	tokens := make([]graphQLToken, 0, 32)
	for i := 0; i < len(query); {
		switch c := query[i]; {
		case c == '#':
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
		case c == '"':
			next, ok := skipGraphQLString(query, i)
			if !ok {
				return nil, false
			}
			i = next
		case isGraphQLNameStart(c):
			start := i
			for i++; i < len(query) && isGraphQLNameContinue(query[i]); i++ {
			}
			tokens = append(tokens, graphQLToken{kind: 'n', text: query[start:i]})
		case strings.ContainsRune("{}()[]", rune(c)):
			tokens = append(tokens, graphQLToken{kind: c})
			i++
		default:
			i++
		}
	}
	return tokens, true
}

func skipGraphQLString(query string, start int) (int, bool) {
	if strings.HasPrefix(query[start:], `"""`) {
		for i := start + 3; i+2 < len(query); i++ {
			if query[i] == '\\' {
				i++
				continue
			}
			if strings.HasPrefix(query[i:], `"""`) {
				return i + 3, true
			}
		}
		return 0, false
	}
	for i := start + 1; i < len(query); i++ {
		if query[i] == '\\' {
			i++
			continue
		}
		if query[i] == '"' {
			return i + 1, true
		}
	}
	return 0, false
}

func findAndSkipGraphQLSelection(tokens []graphQLToken, start int) (int, bool) {
	parenDepth, bracketDepth := 0, 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i].kind {
		case '(':
			parenDepth++
		case ')':
			parenDepth--
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
		case '{':
			if parenDepth == 0 && bracketDepth == 0 {
				return skipGraphQLSelection(tokens, i)
			}
			var ok bool
			i, ok = skipGraphQLSelection(tokens, i)
			if !ok {
				return 0, false
			}
			i--
		}
		if parenDepth < 0 || bracketDepth < 0 {
			return 0, false
		}
	}
	return 0, false
}

func skipGraphQLSelection(tokens []graphQLToken, start int) (int, bool) {
	depth := 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i].kind {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func isGraphQLNameStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isGraphQLNameContinue(c byte) bool {
	return isGraphQLNameStart(c) || c >= '0' && c <= '9'
}

func (s *Server) handleGatewayHealth(w http.ResponseWriter, r *http.Request) {
	if s.opts.Gateway == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "the GitHub gateway is not configured"})
		return
	}
	if !s.authorizeGateway(w, r) {
		return
	}
	if r.URL.Query().Get("write") == "1" && s.opts.ReadOnly {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "the GitHub gateway is read-only"})
		return
	}
	if r.URL.Query().Get("write") == "1" {
		if strings.TrimSpace(s.opts.Fleet.GateRepo) == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "the gate repository is not configured"})
			return
		}
		canWrite, err := s.opts.Gateway.CanWrite(r.Context(), s.opts.Fleet.GateRepo)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "could not verify GitHub write access: " + err.Error()})
			return
		}
		if !canWrite {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "the GitHub credential cannot write the gate repository"})
			return
		}
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
