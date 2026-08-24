package gh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("github resource not found")

// ErrServerUnreachable marks a failure to reach the configured crq serve at
// all. It is the transport's fault, not one resource's: a daemon that treats it
// as a per-repository read error logs it and spins forever while its service
// manager reports it healthy. IsServerUnreachable is how a caller tells them apart.
var ErrServerUnreachable = errors.New("crq serve unreachable")

// IsServerUnreachable reports whether err is a failure to reach crq serve.
func IsServerUnreachable(err error) bool { return errors.Is(err, ErrServerUnreachable) }

type APIError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s failed: %d %s", e.Method, e.URL, e.Status, strings.TrimSpace(e.Body))
}

type Logger interface {
	Printf(string, ...any)
}

type GitHub struct {
	token          string
	tokenMu        sync.Mutex
	httpClient     *http.Client
	apiBase        string
	graphBase      string
	gatewayURL     string
	gatewayToken   string
	log            Logger
	maxRetries     int
	maxWait        time.Duration
	backoffBase    time.Duration
	networkMaxWait time.Duration
	acctTypeMu     sync.Mutex
	acctType       map[string]string // scope login -> "org:" | "user:" search qualifier
	viewerMu       sync.Mutex
	viewer         string // the token's own login, "" until looked up, "-" when unreadable
	etagMu         sync.Mutex
	etags          map[string]*etagEntry // GET URL -> last 200 response, replayed on 304
	// Server GETs for the same URL share an exact-key gate. A burst of fresh CLI
	// processes then produces one upstream 200 followed by conditional 304s
	// instead of racing several uncached requests against the shared REST
	// allowance. Unrelated URLs never wait behind one another.
	coalesceGETs bool
	getGateMu    sync.Mutex
	getGates     map[string]*requestGate
}

type requestGate struct {
	users int
	token chan struct{}
}

func (g *GitHub) acquireGETGate(ctx context.Context, requestURL string) (func(), error) {
	g.getGateMu.Lock()
	if g.getGates == nil {
		g.getGates = map[string]*requestGate{}
	}
	gate := g.getGates[requestURL]
	if gate == nil {
		gate = &requestGate{token: make(chan struct{}, 1)}
		g.getGates[requestURL] = gate
	}
	gate.users++
	g.getGateMu.Unlock()

	select {
	case gate.token <- struct{}{}:
		return func() {
			<-gate.token
			g.releaseGETGate(requestURL, gate)
		}, nil
	case <-ctx.Done():
		g.releaseGETGate(requestURL, gate)
		return nil, ctx.Err()
	}
}

func (g *GitHub) releaseGETGate(requestURL string, gate *requestGate) {
	g.getGateMu.Lock()
	defer g.getGateMu.Unlock()
	gate.users--
	if gate.users == 0 && g.getGates[requestURL] == gate {
		delete(g.getGates, requestURL)
	}
}

// etagEntry is a cached 200 GET response. GitHub serves 304 Not Modified for a
// matching If-None-Match without charging the request against the REST quota,
// which is what keeps long-running poll loops (crq loop, autoreview) from
// draining the shared 5000/hr limit.
type etagEntry struct {
	etag   string
	body   []byte
	header http.Header
}

// maxETagEntries bounds the per-process cache; polling workloads touch a small,
// stable set of URLs so anything past this is churn, not working set.
const maxETagEntries = 1024

// maxETagBody skips caching oversized responses (they'd mostly be one-off blob
// fetches, and replaying them buys nothing worth the memory).
const maxETagBody = 1 << 20

// GitHub accepts git blobs up to 100 MiB. Blob reads are base64-encoded and
// blob writes JSON-escape the UTF-8 state, so the gateway envelope needs room
// beyond the raw object limit. Requests remain authenticated and bounded.
const maxForwardBody = 256 << 20

func (g *GitHub) etagLookup(url string) *etagEntry {
	g.etagMu.Lock()
	defer g.etagMu.Unlock()
	return g.etags[url]
}

func (g *GitHub) etagStore(url string, entry *etagEntry) {
	g.etagMu.Lock()
	defer g.etagMu.Unlock()
	if g.etags == nil {
		g.etags = make(map[string]*etagEntry)
	}
	if _, exists := g.etags[url]; !exists && len(g.etags) >= maxETagEntries {
		for k := range g.etags { // evict an arbitrary entry to stay bounded
			delete(g.etags, k)
			break
		}
	}
	g.etags[url] = entry
}

// replay converts a cached entry back into the 200 response the caller would
// have seen, merging fresh 304 metadata so Link-header pagination observes
// revalidated page boundaries.
func (e *etagEntry) replay(req *http.Request, revalidated http.Header) *http.Response {
	header := http.Header{}
	if e.header != nil {
		header = e.header.Clone()
	}
	for k, values := range revalidated {
		key := http.CanonicalHeaderKey(k)
		if shouldMergeRevalidatedHeader(key) {
			header[key] = append([]string(nil), values...)
		}
	}
	e.header = header.Clone()
	if etag := e.header.Get("ETag"); etag != "" {
		e.etag = etag
	}
	return &http.Response{
		Status:        "200 OK (etag cache)",
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

func shouldMergeRevalidatedHeader(key string) bool {
	switch key {
	case "Cache-Control", "Content-Location", "Date", "ETag", "Expires", "Last-Modified", "Link", "Vary":
		return true
	}
	return strings.HasPrefix(key, "X-Ratelimit-")
}

type prefixReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (r *prefixReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *prefixReadCloser) Close() error {
	return r.closer.Close()
}

// cacheGET reads a fresh 200 GET body, remembers it under its ETag, and hands
// the caller an equivalent response. Responses without an ETag or over the size
// cap pass through uncached.
func (g *GitHub) cacheGET(url string, resp *http.Response) (*http.Response, error) {
	body := resp.Body
	b, err := io.ReadAll(io.LimitReader(body, maxETagBody+1))
	if err != nil {
		_ = body.Close()
		return nil, err
	}
	if len(b) > maxETagBody {
		resp.Body = &prefixReadCloser{
			reader: io.MultiReader(bytes.NewReader(b), body),
			closer: body,
		}
		return resp, nil
	}
	_ = body.Close()
	if etag := resp.Header.Get("ETag"); etag != "" && len(b) <= maxETagBody {
		g.etagStore(url, &etagEntry{etag: etag, body: b, header: resp.Header.Clone()})
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// LookupToken resolves a GitHub token from the environment or the gh CLI, for
// callers outside this package that need the same credential — git, which does
// not read GITHUB_TOKEN or gh's store by itself.
func LookupToken(ctx context.Context) string { return lookupToken(ctx) }

// lookupToken resolves a GitHub token from the environment or the gh CLI. gh can
// hand back a freshly-rotated OAuth token, which is why send re-runs this on a 401.
func lookupToken(ctx context.Context) string {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if token == "" {
		if out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output(); err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	return token
}

func (g *GitHub) authToken() string {
	g.tokenMu.Lock()
	defer g.tokenMu.Unlock()
	return g.token
}

// refreshToken re-resolves the token (e.g. after a 401) in case gh rotated it.
func (g *GitHub) refreshToken(ctx context.Context) {
	if t := lookupToken(ctx); t != "" {
		g.tokenMu.Lock()
		g.token = t
		g.tokenMu.Unlock()
	}
}

func NewGitHub(ctx context.Context) (*GitHub, error) {
	token := lookupToken(ctx)
	if token == "" {
		return nil, errors.New("GitHub token not found (set GITHUB_TOKEN/GH_TOKEN or run 'gh auth login')")
	}
	maxWait := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("CRQ_GITHUB_MAX_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			maxWait = d
		}
	}
	maxRetries := 6
	if v := strings.TrimSpace(os.Getenv("CRQ_GITHUB_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	// 0 = no cap: ride out an outage and keep retrying until connectivity returns
	// (only caller cancellation stops it). Set CRQ_NETWORK_MAX_WAIT to bound it.
	networkMaxWait := time.Duration(0)
	if v := strings.TrimSpace(os.Getenv("CRQ_NETWORK_MAX_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			networkMaxWait = d
		}
	}
	return &GitHub{
		token:          token,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		apiBase:        "https://api.github.com",
		graphBase:      "https://api.github.com/graphql",
		maxRetries:     maxRetries,
		maxWait:        maxWait,
		backoffBase:    2 * time.Second,
		networkMaxWait: networkMaxWait,
	}, nil
}

// NewGitHubViaServer builds a fail-closed client whose GitHub HTTP traffic is
// carried by crq serve. The caller needs no GitHub credential: the persistent
// server owns authentication, conditional-request caching, retry and backoff.
func NewGitHubViaServer(serverURL, token string) (*GitHub, error) {
	raw := strings.TrimSpace(serverURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("CRQ_SERVER_URL must be an http(s) server URL, got %q", serverURL)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("CRQ_SERVER_URL must use https for a non-loopback server, got %q", serverURL)
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	transport := baseTransport.Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	return &GitHub{
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("refusing redirect from crq serve")
			},
		},
		apiBase:        "https://api.github.com",
		graphBase:      "https://api.github.com/graphql",
		gatewayURL:     strings.TrimRight(raw, "/") + "/api/github",
		gatewayToken:   strings.TrimSpace(token),
		maxRetries:     0, // the server owns GitHub retries; a client never doubles them
		backoffBase:    2 * time.Second,
		networkMaxWait: 5 * time.Second,
	}, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SetLogger attaches a logger so rate-limit backoff/retry is visible to humans and the daemon log.
func (g *GitHub) SetLogger(l Logger) { g.log = l }

// EnableGETCoalescing makes concurrent reads of the same URL wait behind the
// first. crq serve enables it on its one persistent direct client; ordinary
// direct clients retain the existing concurrency semantics.
func (g *GitHub) EnableGETCoalescing() { g.coalesceGETs = true }

// ProbeServer verifies that the configured crq control plane is healthy and,
// when requested, accepts the writes a daemon needs. It performs no GitHub API
// operation, so installers can validate their future transport without
// spending quota or mutating shared state.
func ProbeServer(ctx context.Context, serverURL, token string, requireWrite bool) error {
	g, err := NewGitHubViaServer(serverURL, token)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	healthURL := strings.TrimSuffix(g.gatewayURL, "/api/github") + "/api/gateway/health"
	if requireWrite {
		healthURL += "?write=1"
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	g.decorate(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("crq serve health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var health struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&health); err != nil {
		return fmt.Errorf("crq serve returned an invalid health response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !health.OK {
		if health.Error == "" {
			health.Error = resp.Status
		}
		return errors.New(health.Error)
	}
	return nil
}

func (g *GitHub) request(ctx context.Context, method, path string, in, out any) error {
	body, err := marshalBody(in)
	if err != nil {
		return err
	}
	resp, err := g.send(ctx, method, g.apiBase+path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: method, URL: path, Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHub) requestPaged(ctx context.Context, path string, out any) error {
	value := reflect.ValueOf(out)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Slice {
		return errors.New("paged output must be pointer to slice")
	}
	next := g.apiBase + path
	for next != "" {
		resp, err := g.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ErrNotFound
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &APIError{Method: http.MethodGet, URL: next, Status: resp.StatusCode, Body: string(b)}
		}
		page := reflect.New(value.Elem().Type()).Interface()
		if err := json.NewDecoder(resp.Body).Decode(page); err != nil {
			resp.Body.Close()
			return err
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		value.Elem().Set(reflect.AppendSlice(value.Elem(), reflect.ValueOf(page).Elem()))
		next = nextPage(link)
	}
	return nil
}

func (g *GitHub) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := marshalBody(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	resp, envelope, err := g.graphQLResponse(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: http.MethodPost, URL: g.graphBase, Status: resp.StatusCode, Body: string(b)}
	}
	if len(envelope.Errors) > 0 {
		return errors.New(envelope.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// graphQLResponse handles GitHub's semantic HTTP-200 rate-limit response. The
// gateway uses this same path, so a short-lived remote client never becomes a
// second owner of GraphQL retry or backoff.
func (g *GitHub) graphQLResponse(ctx context.Context, body []byte) (*http.Response, graphQLEnvelope, error) {
	for attempt := 0; ; attempt++ {
		resp, err := g.send(ctx, http.MethodPost, g.graphBase, body)
		if err != nil {
			return nil, graphQLEnvelope{}, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp, graphQLEnvelope{}, nil
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxForwardBody+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, graphQLEnvelope{}, readErr
		}
		if len(payload) > maxForwardBody {
			return nil, graphQLEnvelope{}, fmt.Errorf("GitHub response exceeded %d bytes", maxForwardBody)
		}
		var envelope graphQLEnvelope
		decodeErr := json.Unmarshal(payload, &envelope)
		reset := resp.Header.Get("X-RateLimit-Reset")
		if decodeErr != nil {
			return nil, graphQLEnvelope{}, decodeErr
		}
		if len(envelope.Errors) > 0 {
			msg := envelope.Errors[0].Message
			// GraphQL reports rate limits as a 200 with a RATE_LIMITED error
			// rather than 403/429, so retry it with the same backoff as send.
			if strings.EqualFold(envelope.Errors[0].Type, "RATE_LIMITED") || strings.Contains(strings.ToLower(msg), "rate limit") {
				rl := &RateLimitError{Kind: "graphql", Method: http.MethodPost, URL: g.graphBase, Remaining: -1}
				if reset != "" {
					if epoch, perr := strconv.ParseInt(reset, 10, 64); perr == nil {
						rl.Until = time.Unix(epoch, 0)
					}
				}
				wait, known, ok := g.backoffWait(rl, attempt)
				if !ok {
					return nil, graphQLEnvelope{}, rl
				}
				if g.log != nil {
					if known {
						g.log.Printf("github graphql rate limit; waiting %s until reset", wait.Round(time.Second))
					} else {
						g.log.Printf("github graphql rate limit; backing off %s (attempt %d/%d)", wait.Round(time.Second), attempt+1, g.maxRetries)
					}
				}
				if serr := SleepCtx(ctx, wait); serr != nil {
					return nil, graphQLEnvelope{}, serr
				}
				continue
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(payload))
		resp.ContentLength = int64(len(payload))
		return resp, envelope, nil
	}
}

// UserAgent identifies crq to the GitHub API; the crq package stamps its
// version in at init time (gh cannot import crq for it).
var UserAgent = "crq"

func (g *GitHub) decorate(req *http.Request) {
	if g.gatewayURL != "" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-CRQ-Client", "1")
		if g.gatewayToken != "" {
			req.Header.Set("Authorization", "Bearer "+g.gatewayToken)
		}
		req.Header.Set("User-Agent", UserAgent)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.authToken())
	req.Header.Set("User-Agent", UserAgent)
}

func marshalBody(in any) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	return json.Marshal(in)
}

// RateLimitError is returned when GitHub rate-limits a request and crq could not
// wait it out within its retry budget. It carries the reset time so callers (and
// humans) get an actionable message instead of an opaque 403.
type RateLimitError struct {
	Method    string
	URL       string
	Kind      string // "primary", "secondary", or "graphql"
	Remaining int    // x-ratelimit-remaining when known, else -1
	Until     time.Time
}

func (e *RateLimitError) Error() string {
	if e.Until.IsZero() {
		return fmt.Sprintf("github %s rate limit hit (%s %s); retry shortly", e.Kind, e.Method, shortURL(e.URL))
	}
	wait := time.Until(e.Until).Round(time.Second)
	if wait < 0 {
		wait = 0
	}
	return fmt.Sprintf("github %s rate limit hit (%s %s); resets %s (~%s)", e.Kind, e.Method, shortURL(e.URL), e.Until.UTC().Format(time.RFC3339), wait)
}

// IsThrottled reports whether err is (or wraps) a GitHub rate-limit error.
func IsThrottled(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

// IsRecoverableRead reports a failure local to one resource that a bounded
// multi-PR preview can count as unexamined while continuing. Authentication,
// permission, validation and state errors are deliberately excluded.
func IsRecoverableRead(err error) bool {
	// Wraps a net.Error, but it is not local to one resource: nothing can be read.
	if IsServerUnreachable(err) {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var api *APIError
	if errors.As(err, &api) {
		return api.Status == http.StatusNotFound || api.Status >= 500
	}
	var network net.Error
	return errors.As(err, &network)
}

// ThrottleWait returns how long to wait before retrying a rate-limited error.
// The bool is true when err is a rate limit; the duration is 0 when GitHub gave
// no reset hint (the caller should apply its own default backoff).
func ThrottleWait(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		return 0, false
	}
	if rl.Until.IsZero() {
		return 0, true
	}
	d := time.Until(rl.Until)
	if d < 0 {
		d = 0
	}
	return d, true
}

// rateLimitFrom classifies a 403/429 response as a GitHub primary or secondary
// rate limit, or returns nil if it is an ordinary error (e.g. a permission 403).
func rateLimitFrom(resp *http.Response, body string) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	// A server-owned GraphQL throttle has no GitHub HTTP rate-limit headers:
	// GitHub returned it inside a 200 GraphQL envelope. Preserve the typed error
	// across the gateway without adding Retry-After, which would make the remote
	// client become a second retry/backoff owner.
	if kind := strings.TrimSpace(resp.Header.Get("X-CRQ-RateLimit-Kind")); kind != "" {
		return &RateLimitError{Kind: kind, Remaining: -1}
	}
	lower := strings.ToLower(body)
	// Secondary limit: honor an explicit Retry-After (seconds).
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return &RateLimitError{Kind: "secondary", Remaining: -1, Until: time.Now().Add(time.Duration(secs) * time.Second)}
		}
	}
	remaining := -1
	if r := resp.Header.Get("X-RateLimit-Remaining"); r != "" {
		if n, err := strconv.Atoi(r); err == nil {
			remaining = n
		}
	}
	resetUntil := func() time.Time {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return time.Unix(epoch, 0)
			}
		}
		return time.Time{}
	}
	// Primary limit: the quota is exhausted; wait until the window resets.
	if remaining == 0 || strings.Contains(lower, "api rate limit exceeded") {
		return &RateLimitError{Kind: "primary", Remaining: 0, Until: resetUntil()}
	}
	// Secondary/abuse limit without a Retry-After header: caller backs off.
	if strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "exceeded a secondary") || strings.Contains(lower, "abuse detection") {
		return &RateLimitError{Kind: "secondary", Remaining: remaining}
	}
	return nil
}

// send performs an HTTP request with rate-limit and failure resilience. It rides
// out internet outages by retrying transient transport errors (timeouts,
// refused/reset connections, DNS hiccups, TLS failures, short EOFs) on a backoff
// that plateaus at 30s — by default with no time cap, so it keeps probing until
// connectivity returns rather than ever failing the agent on a network drop
// (set CRQ_NETWORK_MAX_WAIT to bound it). The retry attempt is itself the probe.
// It also retries 5xx and backs off GitHub rate limits with the bounded
// maxRetries/maxWait budget. Real caller cancellation (ctx done) is never retried.
func (g *GitHub) send(ctx context.Context, method, fullURL string, body []byte) (*http.Response, error) {
	requestURL, err := g.transportURL(fullURL)
	if err != nil {
		return nil, err
	}
	attempt := 0    // bounded retries for 5xx + rate limits
	netAttempt := 0 // consecutive transient-network retries
	var offlineSince time.Time
	// Conditional GETs: replaying a cached body on 304 keeps repeated polls of
	// unchanged resources free of REST-quota cost.
	conditional := method == http.MethodGet && body == nil
	var cached *etagEntry
	if conditional {
		if g.coalesceGETs {
			release, err := g.acquireGETGate(ctx, fullURL)
			if err != nil {
				return nil, err
			}
			defer release()
		}
		cached = g.etagLookup(fullURL)
	}
	for {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, requestURL, rdr)
		if err != nil {
			return nil, err
		}
		g.decorate(req)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if cached != nil {
			req.Header.Set("If-None-Match", cached.etag)
		}
		resp, err := g.httpClient.Do(req)
		if err != nil {
			// Caller cancelled or its deadline passed: surface that, don't retry.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if isRetryableNetErr(err) {
				if offlineSince.IsZero() {
					offlineSince = time.Now()
				}
				down := time.Since(offlineSince)
				// networkMaxWait <= 0 means no cap: keep retrying until the
				// network is back (only caller cancellation, handled above, stops us).
				if g.networkMaxWait > 0 && down > g.networkMaxWait {
					if g.gatewayURL != "" {
						return nil, fmt.Errorf("%w for %s (%s %s): %w", ErrServerUnreachable, down.Round(time.Second), method, shortURL(fullURL), err)
					}
					return nil, fmt.Errorf("%s unreachable for %s (%s %s): %w", g.networkTarget(), down.Round(time.Second), method, shortURL(fullURL), err)
				}
				wait := networkRetryWait(g.backoffBase, netAttempt)
				netAttempt++
				if g.log != nil {
					capStr := "no cap"
					if g.networkMaxWait > 0 {
						capStr = g.networkMaxWait.String()
					}
					g.log.Printf("%s unreachable on %s %s (%v); retrying in %s (offline %s / cap %s)", g.networkTarget(), method, shortURL(fullURL), err, wait.Round(time.Second), down.Round(time.Second), capStr)
				}
				if serr := SleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, err
		}
		// A 5xx that crq serve did not stamp came from something in FRONT of it:
		// a reverse proxy answering for a backend that is gone. The dial
		// succeeded, so the network branch above never sees it, and a gateway
		// client takes no retries of its own — it would surface as an ordinary
		// APIError, get logged and skipped per target, and leave the daemon
		// spinning through an outage. That is the exact silence this error exists
		// to end, so it is tracked on the same offline clock: a serve restart is
		// ridden out, a serve that stays down ends the process.
		if g.gatewayURL != "" && resp.StatusCode >= 500 && resp.Header.Get("X-CRQ-Gateway") != "1" {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if offlineSince.IsZero() {
				offlineSince = time.Now()
			}
			down := time.Since(offlineSince)
			if g.networkMaxWait <= 0 || down > g.networkMaxWait {
				return nil, fmt.Errorf("%w for %s (%s %s): HTTP %d from a proxy in front of it: %s",
					ErrServerUnreachable, down.Round(time.Second), method, shortURL(fullURL), resp.StatusCode, snippet(b))
			}
			wait := networkRetryWait(g.backoffBase, netAttempt)
			netAttempt++
			if g.log != nil {
				g.log.Printf("crq serve answered by a proxy on %s %s (HTTP %d); retrying in %s (offline %s / cap %s)",
					method, shortURL(fullURL), resp.StatusCode, wait.Round(time.Second), down.Round(time.Second), g.networkMaxWait)
			}
			if serr := SleepCtx(ctx, wait); serr != nil {
				return nil, serr
			}
			continue
		}
		// A response came back: connectivity is fine, reset the offline tracker.
		if !offlineSince.IsZero() && g.log != nil {
			g.log.Printf("%s reachable again after %s offline; resuming", g.networkTarget(), time.Since(offlineSince).Round(time.Second))
		}
		netAttempt, offlineSince = 0, time.Time{}
		// Retry transient server errors (500/502/503/504).
		if isRetryableStatus(resp.StatusCode) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: HTTP %d; retrying in %s (attempt %d/%d)", method, shortURL(fullURL), resp.StatusCode, wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := SleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		// GitHub's API always returns JSON; a non-2xx with an HTML body is a
		// transient edge error (a "Bad request" / "Unicorn!" page served before the
		// request reaches the API), not a real API error — retry rather than fail.
		if resp.StatusCode >= 400 && isHTMLResponse(resp) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: HTTP %d with an HTML body (edge error); retrying in %s (attempt %d/%d)", method, shortURL(fullURL), resp.StatusCode, wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := SleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		// A 401 is often transient (a spurious GitHub error, or a gh OAuth token
		// that just rotated). Refresh the token and retry a bounded number of times
		// before surfacing it as a real auth failure.
		if resp.StatusCode == http.StatusUnauthorized {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				g.refreshToken(ctx)
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: 401 unauthorized; refreshing token and retrying in %s (attempt %d/%d)", method, shortURL(fullURL), wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := SleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		if resp.StatusCode == http.StatusNotModified && cached != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return cached.replay(req, resp.Header), nil
		}
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
			if conditional && resp.StatusCode == http.StatusOK {
				return g.cacheGET(fullURL, resp)
			}
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		rl := rateLimitFrom(resp, string(b))
		if rl == nil {
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		rl.Method, rl.URL = method, fullURL
		wait, known, ok := g.backoffWait(rl, attempt)
		if !ok {
			return nil, rl
		}
		if !known {
			attempt++ // hint-less and expired-reset backoffs consume the bounded budget
		}
		if g.log != nil {
			if known {
				g.log.Printf("github %s rate limit on %s %s; waiting %s until reset", rl.Kind, method, shortURL(fullURL), wait.Round(time.Second))
			} else {
				g.log.Printf("github %s rate limit on %s %s; backing off %s (attempt %d/%d)", rl.Kind, method, shortURL(fullURL), wait.Round(time.Second), attempt, g.maxRetries)
			}
		}
		if err := SleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (g *GitHub) networkTarget() string {
	if g.gatewayURL != "" {
		return "crq serve"
	}
	return "github"
}

// transportURL maps a GitHub endpoint onto the server gateway. Pagination
// links are absolute api.github.com URLs, so mapping happens here rather than
// only when apiBase is assembled. Refusing every other host is the fail-closed
// guarantee: a malformed link can never bypass the server and hit GitHub.
func (g *GitHub) transportURL(fullURL string) (string, error) {
	if g.gatewayURL == "" {
		return fullURL, nil
	}
	u, err := url.Parse(fullURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Host, "api.github.com") {
		return "", fmt.Errorf("refusing to bypass crq serve for %q", fullURL)
	}
	return g.gatewayURL + u.EscapedPath() + querySuffix(u.RawQuery), nil
}

func querySuffix(query string) string {
	if query == "" {
		return ""
	}
	return "?" + query
}

// ForwardResponse is one GitHub response after the persistent client's retry,
// backoff and ETag handling has run.
type ForwardResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// Forward executes one relative GitHub API request for the server gateway.
// Only a direct client may forward, which makes an accidental server->server
// proxy loop impossible.
func (g *GitHub) Forward(ctx context.Context, method, requestURI string, body []byte) (ForwardResponse, error) {
	if g.gatewayURL != "" {
		return ForwardResponse{}, errors.New("a server-backed GitHub client cannot serve as the gateway")
	}
	u, err := url.ParseRequestURI(requestURI)
	if err != nil || !strings.HasPrefix(requestURI, "/") || u.IsAbs() || u.Host != "" {
		return ForwardResponse{}, fmt.Errorf("invalid GitHub request URI %q", requestURI)
	}
	if method == http.MethodGet && len(body) == 0 {
		body = nil // preserve conditional-GET caching through the gateway
	}
	var resp *http.Response
	if u.Path == "/graphql" && method == http.MethodPost {
		resp, _, err = g.graphQLResponse(ctx, body)
	} else {
		resp, err = g.send(ctx, method, g.apiBase+requestURI, body)
	}
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return ForwardResponse{
				Status: api.Status,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   []byte(api.Body),
			}, nil
		}
		var throttled *RateLimitError
		if errors.As(err, &throttled) {
			header := http.Header{"Content-Type": []string{"application/json"}}
			if throttled.Kind == "graphql" {
				header.Set("X-CRQ-RateLimit-Kind", throttled.Kind)
			}
			if throttled.Remaining >= 0 {
				header.Set("X-RateLimit-Remaining", strconv.Itoa(throttled.Remaining))
			}
			if !throttled.Until.IsZero() {
				header.Set("X-RateLimit-Reset", strconv.FormatInt(throttled.Until.Unix(), 10))
			}
			payload, marshalErr := json.Marshal(map[string]string{"message": throttled.Error()})
			if marshalErr != nil {
				return ForwardResponse{}, marshalErr
			}
			return ForwardResponse{Status: http.StatusTooManyRequests, Header: header, Body: payload}, nil
		}
		return ForwardResponse{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxForwardBody+1))
	if err != nil {
		return ForwardResponse{}, err
	}
	if len(payload) > maxForwardBody {
		return ForwardResponse{}, fmt.Errorf("GitHub response exceeded %d bytes", maxForwardBody)
	}
	return ForwardResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: payload}, nil
}

// networkRetryWait is exponential backoff that plateaus at 30s, so during a long
// outage crq keeps probing every ~30s until connectivity returns.
func networkRetryWait(base time.Duration, attempt int) time.Duration {
	shift := attempt
	if shift > 5 {
		shift = 5
	}
	wait := base << uint(shift)
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

// backoffWait computes how long to wait before the next rate-limited retry:
// honor the reset hint when present, else exponential backoff. ok is false when
// the retry budget is exhausted (too many attempts, or a single wait exceeding
// maxWait), signalling the caller to surface the RateLimitError instead.
// maxRateLimitWait caps how long crq waits out a rate limit that has a known
// reset. GitHub REST windows reset within an hour, so this only guards against a
// bogus far-future reset header wedging the process.
const maxRateLimitWait = 75 * time.Minute

// backoffWait returns how long to wait before retrying a rate-limited request.
// A *fresh* reset hint (a GitHub primary limit, or a Retry-After still in the
// future) is waited out — the limit will clear — capped by maxRateLimitWait and
// reported as knownReset so the caller does not spend its bounded retry budget on
// it. A hint-less secondary limit, or an already-expired reset hint (a stale
// header / clock skew that would otherwise wait ~0 and never increment attempt,
// hot-looping), falls through to bounded exponential backoff and is reported as
// knownReset=false so it consumes the budget. ok is false only when the budget
// is exhausted or a fresh reset is implausibly far away.
func (g *GitHub) backoffWait(rl *RateLimitError, attempt int) (wait time.Duration, knownReset bool, ok bool) {
	if !rl.Until.IsZero() {
		until := time.Until(rl.Until) + time.Second // clock-skew buffer
		if until > maxRateLimitWait {
			return 0, false, false
		}
		if until > 0 {
			return until, true, true
		}
		// Expired reset hint: fall through to bounded backoff so a server that
		// keeps returning a stale reset can't hot-loop with attempt frozen.
	}
	if attempt >= g.maxRetries {
		return 0, false, false
	}
	wait = g.backoffBase << uint(attempt) // 2s, 4s, 8s, ... for hint-less secondary limits
	if wait > g.maxWait {
		wait = g.maxWait
	}
	return wait, false, true
}

// retryBackoff is the exponential backoff for transient network / 5xx retries,
// clamped to maxWait and bounded by maxRetries. Unlike rate-limit backoff it
// clamps (rather than gives up) so a brief outage gets the full wait.
func (g *GitHub) retryBackoff(attempt int) (time.Duration, bool) {
	if attempt >= g.maxRetries {
		return 0, false
	}
	wait := g.backoffBase << uint(attempt)
	if wait > g.maxWait {
		wait = g.maxWait
	}
	return wait, true
}

func isHTMLResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
}

// snippet trims a proxy's error page down to something an operator can read in
// one log line.
func snippet(b []byte) string {
	text := strings.Join(strings.Fields(string(b)), " ")
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isRetryableNetErr reports whether a transport error is a transient network
// failure worth retrying (timeouts, refused/reset connections, DNS hiccups, TLS
// handshake failures, short EOFs). Callers must rule out ctx cancellation first.
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"timeout", "deadline exceeded", "connection refused", "connection reset",
		"no such host", "network is unreachable", "host is unreachable",
		"tls handshake", "i/o timeout", "broken pipe", "server misbehaving",
		"temporary failure", "unexpected eof", "connection closed", "eof",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func SleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func shortURL(u string) string {
	return strings.TrimPrefix(u, "https://api.github.com")
}

func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 || !strings.Contains(sections[1], `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		return raw
	}
	return ""
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func addQuery(path string, values url.Values) string {
	if strings.Contains(path, "?") {
		return path + "&" + values.Encode()
	}
	return path + "?" + values.Encode()
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

type Pull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	// User is who opened the pull request. The listing carries it for free, and
	// the skip-author rules are applied per pull request by every path that acts
	// on one — so a caller that reads a Pull never has to ask again.
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
		// Repo is the head's repository, which differs from the base on a fork
		// PR. A contributor's checkout has a remote for THIS, not the base.
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	// Base identifies the base revision GitHub proposed to combine with Head in
	// this observation. One-pass campaigns require the finalizer to integrate it
	// and reject an already-moved base before merge. GitHub's pull-merge endpoint
	// has no expected-base parameter, so it remains an observation, not a CAS.
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	// Additions/Deletions/ChangedFiles are only populated by the single-pull
	// endpoint, not by list or search results. They cost nothing extra there,
	// and they are what a cost estimate is computed from.
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

type RepoInfo struct {
	DefaultBranch string `json:"default_branch"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ReviewComment struct {
	ID                  int64     `json:"id"`
	PullRequestReviewID int64     `json:"pull_request_review_id"`
	Body                string    `json:"body"`
	Path                string    `json:"path"`
	Line                int       `json:"line"`
	OriginalLine        int       `json:"original_line"`
	CommitID            string    `json:"commit_id"`
	OriginalCommitID    string    `json:"original_commit_id"`
	URL                 string    `json:"html_url"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	User                struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Reaction struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *GitHub) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), nil, &out)
	return out, err
}

func (g *GitHub) PatchIssue(ctx context.Context, repo string, number int, title, body string) error {
	in := map[string]string{"title": title, "body": body}
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), in, nil)
}

func (g *GitHub) CreateIssue(ctx context.Context, repo, title, body string) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repoPath(repo)), map[string]string{
		"title": title,
		"body":  body,
	}, &out)
	return out, err
}

func (g *GitHub) GetPull(ctx context.Context, repo string, pr int) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repoPath(repo), pr), nil, &out)
	return out, err
}

// MergeResult is GitHub's answer to an exact-head pull-request merge. Merged
// can be false without a transport error when GitHub refuses a stale or
// currently unmergeable head; Message carries that reason.
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// MergePull merges only sha using one of GitHub's supported merge methods.
// Supplying the head SHA is the safety boundary: a push racing the readiness
// checks makes the endpoint refuse instead of merging code the fixer did not
// release.
func (g *GitHub) MergePull(ctx context.Context, repo string, pr int, sha, method string) (MergeResult, error) {
	var out MergeResult
	err := g.request(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repoPath(repo), pr), map[string]string{
		"sha":          sha,
		"merge_method": method,
	}, &out)
	return out, err
}

func (g *GitHub) ListPulls(ctx context.Context, repo string, query url.Values) ([]Pull, error) {
	var out []Pull
	path := fmt.Sprintf("/repos/%s/pulls?per_page=100", repoPath(repo))
	if len(query) > 0 {
		path += "&" + query.Encode()
	}
	err := g.requestPaged(ctx, path, &out)
	return out, err
}

func (g *GitHub) CreatePull(ctx context.Context, repo, base, head, title, body string, draft bool) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repoPath(repo)), map[string]any{
		"base":  base,
		"head":  head,
		"title": title,
		"body":  body,
		"draft": draft,
	}, &out)
	return out, err
}

func (g *GitHub) ListIssueComments(ctx context.Context, repo string, issue int) ([]IssueComment, error) {
	var out []IssueComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repoPath(repo), issue), &out)
	return out, err
}

func (g *GitHub) GetIssueComment(ctx context.Context, repo string, commentID int64) (IssueComment, error) {
	var out IssueComment
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/comments/%d", repoPath(repo), commentID), nil, &out)
	return out, err
}

// ListIssueCommentsPage fetches a single page (GitHub returns oldest-first), so
// callers that only need the oldest comments (e.g. calibration pruning) don't
// page through thousands of them.
func (g *GitHub) ListIssueCommentsPage(ctx context.Context, repo string, issue, page, perPage int) ([]IssueComment, error) {
	var out []IssueComment
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", repoPath(repo), issue, perPage, page), nil, &out)
	return out, err
}

func (g *GitHub) DeleteIssueComment(ctx context.Context, repo string, commentID int64) error {
	return g.request(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/comments/%d", repoPath(repo), commentID), nil, nil)
}

func (g *GitHub) ListReviewComments(ctx context.Context, repo string, pr int) ([]ReviewComment, error) {
	var out []ReviewComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) ListReviews(ctx context.Context, repo string, pr int) ([]Review, error) {
	var out []Review
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) ListIssueReactions(ctx context.Context, repo string, issue int) ([]Reaction, error) {
	var out []Reaction
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/%d/reactions?per_page=100", repoPath(repo), issue), &out)
	return out, err
}

func (g *GitHub) PostIssueComment(ctx context.Context, repo string, issue int, body string) (IssueComment, error) {
	var out IssueComment
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repoPath(repo), issue), map[string]string{"body": body}, &out)
	return out, err
}

func (g *GitHub) ListCommentReactions(ctx context.Context, repo string, commentID int64) ([]Reaction, error) {
	var out []Reaction
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d/reactions?per_page=100", repoPath(repo), commentID), &out)
	return out, err
}

// EachOpenPR streams open PRs for a scope (most-recently-updated first), invoking
// fn for each. It stops early when fn returns stop=true, so a caller can bound work
// by its own post-filter scan budget without either over-fetching search pages or
// (the failure mode of a fixed pre-filter limit) stopping before excluded/gate
// results have been skipped — keeping every in-scope PR reachable.
func (g *GitHub) EachOpenPR(ctx context.Context, target string, byRepo bool, fn func(SearchPR) (stop bool, err error)) error {
	query := "type:pr state:open archived:false "
	if byRepo {
		query += "repo:" + target
	} else {
		qualifier, err := g.searchOwnerQualifier(ctx, target)
		if err != nil {
			return err
		}
		query += qualifier + target
	}
	page := 1
	for {
		values := url.Values{}
		values.Set("q", query)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		values.Set("sort", "updated")
		values.Set("order", "desc")
		var result struct {
			Items []struct {
				Number        int    `json:"number"`
				RepositoryURL string `json:"repository_url"`
				Title         string `json:"title"`
				Body          string `json:"body"`
				User          struct {
					Login string `json:"login"`
				} `json:"user"`
			} `json:"items"`
		}
		if err := g.request(ctx, http.MethodGet, "/search/issues?"+values.Encode(), nil, &result); err != nil {
			return err
		}
		if len(result.Items) == 0 {
			return nil
		}
		for _, item := range result.Items {
			repo := strings.TrimPrefix(item.RepositoryURL, "https://api.github.com/repos/")
			if repo == "" {
				continue
			}
			stop, err := fn(SearchPR{Repo: repo, Number: item.Number, Author: item.User.Login, Title: item.Title, Body: item.Body})
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
		if len(result.Items) < 100 {
			return nil
		}
		page++
	}
}

// SearchOpenPRs collects up to limit open PRs for a scope. It is a thin wrapper
// over EachOpenPR for callers that want a slice rather than a streaming callback.
func (g *GitHub) SearchOpenPRs(ctx context.Context, target string, byRepo bool, limit int) ([]SearchPR, error) {
	var out []SearchPR
	err := g.EachOpenPR(ctx, target, byRepo, func(pr SearchPR) (bool, error) {
		out = append(out, pr)
		return len(out) >= limit, nil
	})
	return out, err
}

// searchOwnerQualifier returns the issue-search owner qualifier for a non-repo
// scope: "org:" for organizations, "user:" for user accounts. GitHub's search
// distinguishes the two, so an org scope returns nothing under "user:". The lookup
// is cached per login (an account's type is stable within a run). Lookup failures
// are returned so callers do not silently search the wrong scope.
func (g *GitHub) searchOwnerQualifier(ctx context.Context, login string) (string, error) {
	g.acctTypeMu.Lock()
	if g.acctType == nil {
		g.acctType = map[string]string{}
	}
	cached, ok := g.acctType[login]
	g.acctTypeMu.Unlock()
	if ok {
		return cached, nil
	}
	var acct struct {
		Type string `json:"type"`
	}
	if err := g.request(ctx, http.MethodGet, "/users/"+login, nil, &acct); err != nil {
		return "", err
	}
	qualifier := "user:"
	if strings.EqualFold(acct.Type, "Organization") {
		qualifier = "org:"
	}
	g.acctTypeMu.Lock()
	g.acctType[login] = qualifier
	g.acctTypeMu.Unlock()
	return qualifier, nil
}

// viewerLogin is the login the token authenticates as, cached for the run, or
// "" when it cannot be read.
//
// Unreadable is a legitimate answer, not an error: a token without the scope to
// read /user can still list public repositories, and a caller asking "is this
// owner me" should get "not that I can tell" rather than a failed page.
func (g *GitHub) viewerLogin(ctx context.Context) (string, error) {
	g.viewerMu.Lock()
	cached := g.viewer
	g.viewerMu.Unlock()
	switch cached {
	case "":
	case "-":
		return "", nil
	default:
		return cached, nil
	}
	var me struct {
		Login string `json:"login"`
	}
	login := "-"
	err := g.request(ctx, http.MethodGet, "/user", nil, &me)
	switch {
	case err == nil && me.Login != "":
		login = me.Login
	case err != nil && !deniedIdentity(err):
		// A timeout, a 5xx or an exhausted quota is not an answer about this
		// token. Remembering "-" for one would keep every later repository
		// picker on /users/{owner}/repos — which omits the caller's own private
		// repositories — for the rest of the process, long after GitHub came
		// back. Nothing is cached, so the next caller asks again.
		return "", err
	}
	g.viewerMu.Lock()
	// Another first caller may have completed while this request was in
	// flight. Keep its answer rather than letting a later refusal overwrite a
	// successfully identified viewer (or vice versa).
	if g.viewer == "" || (g.viewer == "-" && login != "-") {
		g.viewer = login
	} else {
		login = g.viewer
	}
	g.viewerMu.Unlock()
	if login == "-" {
		return "", nil
	}
	return login, nil
}

// deniedIdentity reports whether an error is GitHub REFUSING to say who the
// token is, rather than failing to answer. Only the refusal is worth caching: a
// token without the scope for /user will never grow one mid-process, and a rate
// limit surfaces as its own error rather than an APIError.
func deniedIdentity(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return errors.Is(err, ErrNotFound)
	}
	switch api.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

type SearchPR struct {
	Repo   string
	Number int
	Author string
	Title  string
	Body   string
}

type gitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type Commit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
}

type Tree struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

type gitBlob struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *GitHub) GetRef(ctx context.Context, repo, ref string) (string, error) {
	var out gitRef
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/ref/heads/%s", repoPath(repo), refPath(ref)), nil, &out)
	if err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *GitHub) CreateRef(ctx context.Context, repo, ref, sha string) error {
	return g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/refs", repoPath(repo)), map[string]string{
		"ref": "refs/heads/" + ref,
		"sha": sha,
	}, nil)
}

func (g *GitHub) UpdateRef(ctx context.Context, repo, ref, sha string, force bool) error {
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repoPath(repo), refPath(ref)), map[string]any{
		"sha":   sha,
		"force": force,
	}, nil)
}

func (g *GitHub) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	var out gitBlob
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/blobs", repoPath(repo)), map[string]string{
		"content":  string(content),
		"encoding": "utf-8",
	}, &out)
	return out.SHA, err
}

func (g *GitHub) CreateTree(ctx context.Context, repo, baseTree string, entries []map[string]any) (string, error) {
	in := map[string]any{"tree": entries}
	if baseTree != "" {
		in["base_tree"] = baseTree
	}
	var out Tree
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/trees", repoPath(repo)), in, &out)
	return out.SHA, err
}

func (g *GitHub) CreateCommit(ctx context.Context, repo, message, tree string, parents []string) (string, error) {
	var out Commit
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/commits", repoPath(repo)), map[string]any{
		"message": message,
		"tree":    tree,
		"parents": parents,
	}, &out)
	return out.SHA, err
}

func (g *GitHub) GetCommit(ctx context.Context, repo, sha string) (Commit, error) {
	var out Commit
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetTree(ctx context.Context, repo, sha string) (Tree, error) {
	var out Tree
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetBlob(ctx context.Context, repo, sha string) ([]byte, error) {
	var out gitBlob
	if err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/blobs/%s", repoPath(repo), url.PathEscape(sha)), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	}
	return []byte(out.Content), nil
}

func (g *GitHub) RepoExists(ctx context.Context, repo string) (bool, error) {
	err := g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, nil)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (g *GitHub) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var out RepoInfo
	err := g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &out)
	return out, err
}

func refPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// Repo is one repository as the dashboard's repository picker needs it: enough
// to recognize and to judge whether it is worth enrolling, and nothing more.
type Repo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	// OpenIssues is GitHub's open_issues_count, which counts issues AND pull
	// requests together. There is no per-repo open-PR count without a search per
	// repository, so this is reported as what it is rather than relabelled.
	OpenIssues int       `json:"open_issues_count"`
	PushedAt   time.Time `json:"pushed_at"`
	Language   string    `json:"language"`
}

// ListOwnerRepos lists the repositories an owner has, following pagination up
// to limit. It resolves user-vs-organization the same way the PR search does, and
// through the same cache — the two ask the identical question about a login.
//
// Archived repositories are kept rather than filtered: the caller decides, and a
// picker that silently omits a repository somebody is looking for is worse than
// one that shows it greyed out.
func (g *GitHub) ListOwnerRepos(ctx context.Context, owner string, limit int) ([]Repo, error) {
	qualifier, err := g.searchOwnerQualifier(ctx, owner)
	if err != nil {
		return nil, err
	}
	viewer, err := g.viewerLogin(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	if qualifier == "org:" {
		return g.listRepos(ctx, "/orgs/"+owner+"/repos", "", limit)
	}
	if strings.EqualFold(owner, viewer) {
		return g.listRepos(ctx, "/user/repos?affiliation=owner,collaborator", owner, limit)
	}
	public, err := g.listRepos(ctx, "/users/"+owner+"/repos", "", limit)
	if err != nil {
		return nil, err
	}
	if viewer == "" {
		return public, nil
	}
	// Another personal owner needs both views. The public endpoint includes
	// repositories where the viewer has no affiliation; the authenticated list
	// adds private repositories on which the viewer collaborates.
	private, err := g.listRepos(ctx, "/user/repos?affiliation=owner,collaborator", owner, limit)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Repo, len(public)+len(private))
	for _, repo := range append(public, private...) {
		byName[strings.ToLower(repo.FullName)] = repo
	}
	out := make([]Repo, 0, len(byName))
	for _, repo := range byName {
		out = append(out, repo)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PushedAt.After(out[j].PushedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (g *GitHub) listRepos(ctx context.Context, base, filterOwner string, limit int) ([]Repo, error) {
	out := make([]Repo, 0, limit)
	maxPages := (limit + 99) / 100
	for page := 1; len(out) < limit && (filterOwner != "" || page <= maxPages); page++ {
		var batch []Repo
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		path := fmt.Sprintf("%s%sper_page=100&page=%d&sort=pushed", base, sep, page)
		if err := g.request(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		for _, repo := range batch {
			if filterOwner == "" || strings.EqualFold(ownerOfRepo(repo.FullName), filterOwner) {
				out = append(out, repo)
			}
			if len(out) == limit {
				break
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func ownerOfRepo(fullName string) string {
	owner, _, _ := strings.Cut(fullName, "/")
	return owner
}
