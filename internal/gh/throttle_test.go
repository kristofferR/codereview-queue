package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// exhaustedUpstream refuses core requests as an exhausted primary allowance
// until `open` is set, and counts what reaches it per path.
type exhaustedUpstream struct {
	reset time.Time
	open  atomic.Bool
	core  atomic.Int32
	other atomic.Int32
}

func (u *exhaustedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/search/issues" {
		u.other.Add(1)
		_, _ = w.Write([]byte(`{"items":[]}`))
		return
	}
	u.core.Add(1)
	if u.open.Load() {
		_, _ = w.Write([]byte(`{"number":7}`))
		return
	}
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(u.reset.Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for user ID 1."}`))
}

func gatewayClient(t *testing.T, upstream http.Handler) *GitHub {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	g := NewTestClient(srv.URL, srv.Client())
	g.LimitThrottleWait(10 * time.Second)
	return g
}

// An exhausted allowance is the caller's to wait out, so the gateway says so at
// once — and says it to everyone after the first without asking GitHub again.
func TestGatewayAnswersAnExhaustedAllowanceWithoutHoldingOrReasking(t *testing.T) {
	upstream := &exhaustedUpstream{reset: time.Now().Add(20 * time.Minute)}
	g := gatewayClient(t, upstream)

	for i := range 3 {
		started := time.Now()
		result, err := g.Forward(t.Context(), http.MethodGet, "/repos/o/r/pulls/7", nil)
		if err != nil {
			t.Fatalf("forward %d: %v", i+1, err)
		}
		if result.Status != http.StatusTooManyRequests ||
			result.Header.Get("X-RateLimit-Remaining") != "0" ||
			result.Header.Get("X-RateLimit-Reset") != strconv.FormatInt(upstream.reset.Unix(), 10) {
			t.Fatalf("forward %d = %d %v, want a 429 carrying the reset", i+1, result.Status, result.Header)
		}
		if held := time.Since(started); held > 2*time.Second {
			t.Fatalf("forward %d held the request for %s", i+1, held)
		}
	}
	if got := upstream.core.Load(); got != 1 {
		t.Fatalf("upstream saw %d refused requests, want 1", got)
	}

	// Another allowance is another counter.
	if _, err := g.SearchOpenPRs(t.Context(), "o/r", true, 10); err != nil {
		t.Fatalf("search during a core throttle: %v", err)
	}
	if got := upstream.other.Load(); got != 1 {
		t.Fatalf("search requests = %d, want 1", got)
	}
}

// The reported reset is not trusted as the moment service returns: one request
// is let through periodically, and an answer ends the throttle for everyone.
func TestThrottleProbeEndsTheThrottleEarly(t *testing.T) {
	upstream := &exhaustedUpstream{reset: time.Now().Add(20 * time.Minute)}
	g := gatewayClient(t, upstream)
	if _, err := g.GetPull(t.Context(), "o/r", 7); !IsThrottled(err) {
		t.Fatalf("err = %v, want a throttle", err)
	}

	// Probe due, GitHub still refusing: one request goes out, the throttle stays.
	g.throttles.buckets["core"].probeAt = time.Now().Add(-time.Second)
	for range 2 {
		if _, err := g.GetPull(t.Context(), "o/r", 7); !IsThrottled(err) {
			t.Fatalf("err = %v, want a throttle", err)
		}
	}
	if got := upstream.core.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want the original and one probe", got)
	}

	upstream.open.Store(true)
	g.throttles.buckets["core"].probeAt = time.Now().Add(-time.Second)
	for range 2 {
		if pull, err := g.GetPull(t.Context(), "o/r", 7); err != nil || pull.Number != 7 {
			t.Fatalf("pull = %+v, err = %v", pull, err)
		}
	}
}

// A client with no wait limit rides out the reset, as before, and stops as soon
// as its caller does.
func TestClientWaitsOutAKnownResetUntilCancelled(t *testing.T) {
	upstream := &exhaustedUpstream{reset: time.Now().Add(20 * time.Minute)}
	srv := httptest.NewServer(upstream)
	defer srv.Close()
	g := NewTestClient(srv.URL, srv.Client())

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_, err := g.GetPull(ctx, "o/r", 7)
	if !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if got := upstream.core.Load(); got != 1 {
		t.Fatalf("upstream requests while waiting = %d, want 1", got)
	}
}

func TestQuotaBucket(t *testing.T) {
	for url, want := range map[string]string{
		"https://api.github.com/graphql":                      "graphql",
		"https://api.github.com/search/issues?q=type%3Apr":    "search",
		"https://api.github.com/repos/o/r/pulls/7":            "core",
		"https://api.github.com/repos/o/search/pulls":         "core",
		"https://api.github.com/repos/o/r/git/blobs/abcdef01": "core",
	} {
		if got := quotaBucket(url); got != want {
			t.Errorf("quotaBucket(%s) = %q, want %q", url, got, want)
		}
	}
}

// GraphQL reports an exhausted allowance inside a 200, which never reaches the
// REST classifier. It is remembered all the same.
func TestGraphQLExhaustionIsRememberedToo(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	var calls atomic.Int32
	g := gatewayClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	}))

	for i := range 3 {
		result, err := g.Forward(t.Context(), http.MethodPost, "/graphql", []byte(`{"query":"{viewer{login}}"}`))
		if err != nil {
			t.Fatalf("forward %d: %v", i+1, err)
		}
		if result.Status != http.StatusTooManyRequests || result.Header.Get("X-CRQ-RateLimit-Kind") != "graphql" ||
			result.Header.Get("X-RateLimit-Reset") != strconv.FormatInt(reset.Unix(), 10) {
			t.Fatalf("forward %d = %d %v, want a graphql 429 carrying the reset", i+1, result.Status, result.Header)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream saw %d refused queries, want 1", got)
	}
}

// The gateway hands a wait it will not sit through to its client, so the client
// has to learn how long that wait is — for every kind of limit.
func TestGatewayClientLearnsTheResetOfAForwardedThrottle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}))
	defer upstream.Close()
	direct := NewTestClient(upstream.URL, upstream.Client())
	direct.LimitThrottleWait(10 * time.Second)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, err := direct.Forward(r.Context(), r.Method, strings.TrimPrefix(r.URL.RequestURI(), "/api/github"), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range result.Header {
			w.Header()[key] = values
		}
		w.Header().Set("X-CRQ-Gateway", "1")
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}))
	defer proxy.Close()

	client, err := NewGitHubViaServer(proxy.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	client.LimitThrottleWait(time.Millisecond) // report the wait instead of taking it
	_, err = client.GetPull(t.Context(), "o/r", 7)
	var throttled *RateLimitError
	if !errors.As(err, &throttled) || throttled.Kind != "secondary" {
		t.Fatalf("err = %T %v, want a secondary throttle", err, err)
	}
	if wait, _ := ThrottleWait(err); wait < 50*time.Second || wait > 61*time.Second {
		t.Fatalf("client was told to wait %s, want GitHub's 60s", wait)
	}
}

// A request already in flight when the allowance ran out says nothing about
// whether it has come back.
func TestThrottleIsNotClearedByAnAnswerSentBeforeIt(t *testing.T) {
	g := NewTestClient("http://example.invalid", nil)
	sentBefore := time.Now().Add(-time.Second)
	g.noteThrottle("core", &RateLimitError{Kind: "primary", Until: time.Now().Add(time.Minute)})

	g.clearThrottle("core", sentBefore)
	if g.throttled("core", http.MethodGet, "/x") == nil {
		t.Fatal("an answer to an earlier request cleared the throttle")
	}
	g.clearThrottle("core", time.Now())
	if g.throttled("core", http.MethodGet, "/x") != nil {
		t.Fatal("an answer to a later request must clear the throttle")
	}
}

// The limit is on the whole call, not on each sleep: a run of short waits must
// not hold a forwarded request open indefinitely.
func TestThrottleWaitLimitIsCumulative(t *testing.T) {
	g := NewTestClient("http://example.invalid", nil)
	g.LimitThrottleWait(10 * time.Second)
	rl := &RateLimitError{Kind: "secondary", Until: time.Now().Add(2 * time.Second)}
	if err := g.rideOutThrottle(t.Context(), rl, &throttleWait{waited: 9 * time.Second}); !IsThrottled(err) {
		t.Fatalf("err = %v, want the throttle returned once the call has waited its limit", err)
	}
}

// A secondary limit can arrive inside a 200 as well. The reset header beside it
// is the primary window's, so it is neither waited for nor remembered: doing
// either stalls every GraphQL caller for up to an hour over a burst limit.
func TestGraphQLSecondaryLimitIsNotTakenForAnExhaustedAllowance(t *testing.T) {
	var calls atomic.Int32
	g := gatewayClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(40*time.Minute).Unix(), 10))
		w.Header().Set("Retry-After", "60")
		_, _ = w.Write([]byte(`{"errors":[{"message":"You have exceeded a secondary rate limit."}]}`))
	}))

	for i := range 2 {
		err := g.GraphQL(t.Context(), `query { viewer { login } }`, nil, nil)
		var throttled *RateLimitError
		if !errors.As(err, &throttled) || throttled.Kind != "secondary" {
			t.Fatalf("query %d: err = %T %v, want a secondary throttle", i+1, err, err)
		}
		if wait, _ := ThrottleWait(err); wait > 61*time.Second {
			t.Fatalf("query %d: told to wait %s, want Retry-After, not the primary reset", i+1, wait)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream queries = %d, want both sent: a burst limit is not remembered", got)
	}
}

// Waking early is only free when a remembered throttle answers the retry. A
// burst limit is not remembered, so its wait is taken whole.
func TestOnlyARememberedThrottleIsRecheckedEarly(t *testing.T) {
	g := NewTestClient("http://example.invalid", nil)
	until := time.Now().Add(2 * time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the sleep returns at once; what was asked of it is in tw.waited

	exhausted := &RateLimitError{Kind: "primary", URL: "https://api.github.com/repos/o/r", Until: until.Add(time.Hour / 2)}
	g.noteThrottle("core", exhausted)
	remembered := throttleWait{}
	_ = g.rideOutThrottle(ctx, exhausted, &remembered)
	if remembered.waited != throttleRecheck {
		t.Fatalf("a remembered throttle slept %s, want a recheck after %s", remembered.waited, throttleRecheck)
	}

	// Met by a probe while that allowance is still remembered: a different
	// limit, with a wait of its own.
	burst := throttleWait{}
	_ = g.rideOutThrottle(ctx, &RateLimitError{Kind: "secondary", URL: "https://api.github.com/repos/o/r", Until: until}, &burst)
	if burst.waited < 110*time.Second {
		t.Fatalf("a burst limit's wait was cut to %s", burst.waited)
	}
}
