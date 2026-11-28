package gh

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A request that GitHub refused for an exhausted allowance says when the
// allowance returns. Until then every other request against it is refused the
// same way, so they are answered here instead of being sent: asking again is
// what GitHub's own guidance says not to do, and through the gateway it turned
// one empty quota into a refused upstream request per waiting client.
const (
	// throttleProbe is how often one request is let through anyway. The reset
	// GitHub reports is not always the moment service returns — two backends
	// have been seen answering with different counters — so the remembered
	// throttle is a reason to stop asking, not a promise about when to resume.
	throttleProbe = 30 * time.Second
	// throttleRecheck bounds one sleep while waiting out a known reset, so a
	// waiter notices that a probe got through.
	throttleRecheck = time.Minute
)

type throttleMemo struct {
	mu      sync.Mutex
	buckets map[string]*throttledBucket
}

type throttledBucket struct {
	kind    string
	until   time.Time
	noted   time.Time
	probeAt time.Time
}

// throttleWait is one call's dealings with rate limits. A GraphQL call shares
// one across its HTTP attempts and the limits it finds inside their envelopes.
type throttleWait struct {
	attempt   int           // bounded retries taken, shared with the 5xx retries
	waited    time.Duration // slept for throttles so far
	announced time.Time     // the reset last logged
	sentAt    time.Time     // when the latest HTTP attempt went out
}

// quotaBucket names the GitHub allowance a request draws on. They are separate
// counters with separate resets: an exhausted search allowance says nothing
// about the core one.
func quotaBucket(fullURL string) string {
	path := fullURL
	if u, err := url.Parse(fullURL); err == nil {
		path = u.Path
	}
	switch {
	case path == "/graphql":
		return "graphql"
	case strings.HasPrefix(path, "/search/"):
		return "search"
	}
	return "core"
}

// throttled answers a request from the remembered throttle, or returns nil when
// the request should be sent — because nothing is remembered, the reset has
// passed, or this request is the periodic probe.
func (g *GitHub) throttled(bucket, method, fullURL string) *RateLimitError {
	now := time.Now()
	g.throttles.mu.Lock()
	defer g.throttles.mu.Unlock()
	memo := g.throttles.buckets[bucket]
	if memo == nil {
		return nil
	}
	if !now.Before(memo.until) {
		delete(g.throttles.buckets, bucket)
		return nil
	}
	if !now.Before(memo.probeAt) {
		memo.probeAt = now.Add(throttleProbe)
		return nil
	}
	rl := &RateLimitError{Kind: memo.kind, Method: method, URL: fullURL, Remaining: -1, Until: memo.until}
	if memo.kind == "primary" {
		rl.Remaining = 0
	}
	return rl
}

// noteThrottle remembers an exhausted allowance. Only a limit with a believable
// reset qualifies, and never a secondary one: that is about one burst of
// requests and clears in seconds.
func (g *GitHub) noteThrottle(bucket string, rl *RateLimitError) {
	if rl.Kind == "secondary" || rl.Until.IsZero() {
		return
	}
	now := time.Now()
	if !rl.Until.After(now) || rl.Until.Sub(now) > maxRateLimitWait {
		return
	}
	g.throttles.mu.Lock()
	defer g.throttles.mu.Unlock()
	if g.throttles.buckets == nil {
		g.throttles.buckets = map[string]*throttledBucket{}
	}
	g.throttles.buckets[bucket] = &throttledBucket{kind: rl.Kind, until: rl.Until, noted: now, probeAt: now.Add(throttleProbe)}
}

// clearThrottle forgets a throttle once GitHub answers a request sent after it
// was noted. A request already in flight when the allowance ran out says
// nothing about whether it has come back.
func (g *GitHub) clearThrottle(bucket string, sentAt time.Time) {
	g.throttles.mu.Lock()
	defer g.throttles.mu.Unlock()
	if memo := g.throttles.buckets[bucket]; memo != nil && !memo.noted.After(sentAt) {
		delete(g.throttles.buckets, bucket)
	}
}

// rideOutThrottle sleeps for a throttle this client may wait out, and returns
// the error itself when the caller should hear about it instead: the retry
// budget is spent, the reset is implausibly far off, or the call has waited as
// long as this client may hold one request.
func (g *GitHub) rideOutThrottle(ctx context.Context, rl *RateLimitError, tw *throttleWait) error {
	wait, known, ok := g.backoffWait(rl, tw.attempt)
	if !ok || (g.maxThrottleWait > 0 && tw.waited+wait > g.maxThrottleWait) {
		return rl
	}
	if !known {
		tw.attempt++ // hint-less and expired-reset backoffs consume the bounded budget
	}
	if g.log != nil {
		switch {
		case !known:
			g.log.Printf("github %s rate limit on %s %s; backing off %s (attempt %d/%d)", rl.Kind, rl.Method, shortURL(rl.URL), wait.Round(time.Second), tw.attempt, g.maxRetries)
		case !rl.Until.Equal(tw.announced):
			// Said once per reset, not once per recheck.
			g.log.Printf("github %s rate limit on %s %s; waiting %s until reset", rl.Kind, rl.Method, shortURL(rl.URL), wait.Round(time.Second))
		}
	}
	tw.announced = rl.Until
	// A remembered throttle keeps answering for the rest of the wait, so waking
	// early costs nothing and notices a probe that got through. One that is not
	// remembered has nothing to stop the retry reaching GitHub early.
	if known && wait > throttleRecheck && g.throttleRemembered(rl) {
		wait = throttleRecheck
	}
	tw.waited += wait
	return SleepCtx(ctx, wait)
}

// throttleRemembered reports whether this very throttle is the one remembered
// for its allowance — not merely that one is. A burst limit met while an
// exhausted allowance is remembered is still a wait nothing else enforces.
func (g *GitHub) throttleRemembered(rl *RateLimitError) bool {
	g.throttles.mu.Lock()
	defer g.throttles.mu.Unlock()
	memo := g.throttles.buckets[quotaBucket(rl.URL)]
	return memo != nil && memo.kind == rl.Kind && memo.until.Equal(rl.Until)
}
