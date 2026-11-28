package gh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Every state write is a compare-and-swap that uploads the whole state, and
// every crq process on every host writes. With writes landing several times a
// minute and each upload taking seconds, most attempts lost the swap and paid
// three requests for nothing — the largest single drain on the account's quota
// once reads were cached. All of them already pass through one crq serve, so it
// hands out a short lease that lets one write proceed at a time. The swap still
// decides correctness; the lease only stops writers colliding.
const (
	// writeLockLease bounds how long one holder keeps everyone else waiting.
	// A write takes seconds; a holder that dies is waited out at most this long.
	writeLockLease = 30 * time.Second
	// writeLockWait bounds how long a writer waits for the lease before going
	// ahead without it — a collision costs requests, a stall costs the fleet.
	writeLockWait = 20 * time.Second
)

// WriteLock is the lease crq serve grants for one state write at a time.
type WriteLock struct {
	mu      sync.Mutex
	holder  string
	until   time.Time
	changed chan struct{}
}

// Acquire waits for the lease and returns the token that releases it.
func (l *WriteLock) Acquire(ctx context.Context) (string, error) {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.holder == "" || !now.Before(l.until) {
			l.holder, l.until = newLockToken(), now.Add(writeLockLease)
			token := l.holder
			l.mu.Unlock()
			return token, nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed, expires := l.changed, l.until.Sub(now)
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-changed:
		case <-time.After(expires):
		}
	}
}

// Release gives the lease up, if token still holds it.
func (l *WriteLock) Release(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token == "" || l.holder != token {
		return
	}
	l.holder = ""
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

func newLockToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ShareWriteLock makes this client take the given lease for its own state
// writes. crq serve shares the lock its endpoint hands out, so its own writes
// queue with everyone else's.
func (g *GitHub) ShareWriteLock(lock *WriteLock) { g.writeLock = lock }

// LockStateWrites takes the fleet's state-write lease for one write attempt and
// returns what releases it. It never fails: without a lease — no crq serve, an
// older one, or a wait that ran out — the write goes ahead and the
// compare-and-swap decides, as it always did.
func (g *GitHub) LockStateWrites(ctx context.Context) func() {
	ctx, cancel := context.WithTimeout(ctx, writeLockWait)
	defer cancel()
	switch {
	case g.writeLock != nil:
		token, err := g.writeLock.Acquire(ctx)
		if err != nil {
			return func() {}
		}
		return func() { g.writeLock.Release(token) }
	case g.gatewayURL != "":
		token := g.serverLock(ctx, http.MethodPost, "")
		if token == "" {
			return func() {}
		}
		return func() {
			// Released even when the caller's context is gone; the lease
			// would hold everyone else up until it ran out otherwise.
			release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			g.serverLock(release, http.MethodDelete, token)
		}
	default:
		return func() {}
	}
}

// serverLock asks crq serve for the lease (POST) or gives it back (DELETE).
func (g *GitHub) serverLock(ctx context.Context, method, token string) string {
	endpoint := strings.TrimSuffix(g.gatewayURL, "/api/github") + "/api/state-lock"
	if token != "" {
		endpoint += "?token=" + url.QueryEscape(token)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return ""
	}
	g.decorate(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || resp.StatusCode != http.StatusOK || method != http.MethodPost {
		return ""
	}
	var granted struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &granted) != nil {
		return ""
	}
	return granted.Token
}
