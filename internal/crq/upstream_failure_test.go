package crq

import (
	"context"
	"errors"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// gatewayFailure is what a daemon sees when GitHub fails a request the gateway
// forwarded: an API error with a 5xx status, from a server that is reachable.
var gatewayFailure = &ghapi.APIError{Method: "GET", URL: "/repos/o/gate/git/ref/heads/crq-state-v3", Status: 502, Body: `{"error":"context canceled"}`}

// upstreamFailingStore fails chosen reads and writes with a gateway failure and
// reports each call that got past them.
type upstreamFailingStore struct {
	StateStore
	failLoads   int // the first N loads fail
	failUpdate  int // this Update call fails (1-based)
	loads       int
	updates     int
	afterFailed func()
}

func (s *upstreamFailingStore) Load(ctx context.Context) (State, Revision, error) {
	s.loads++
	if s.loads <= s.failLoads {
		return State{}, Revision{}, gatewayFailure
	}
	if s.failLoads > 0 && s.afterFailed != nil {
		s.afterFailed()
	}
	return s.StateStore.Load(ctx)
}

func (s *upstreamFailingStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	s.updates++
	if s.updates == s.failUpdate {
		return State{}, gatewayFailure
	}
	if s.failUpdate > 0 && s.updates > s.failUpdate && s.afterFailed != nil {
		s.afterFailed()
	}
	return s.StateStore.Update(ctx, mutate)
}

// A watcher that exits over a GitHub 5xx is restarted by its service manager
// straight into the same failure. The next pass is the retry.
func TestWatchKeepsRunningThroughAnUpstreamFailure(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &upstreamFailingStore{StateStore: NewMemoryStore(cfg), failLoads: 2, afterFailed: cancel}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	observeOnly := false
	err := svc.Watch(ctx, WatchOptions{Interval: time.Millisecond, Dispatch: &observeOnly}, func(WatchEvent) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch returned %v, want it to run until cancelled", err)
	}
	if store.loads < 3 {
		t.Fatalf("state loads = %d, want a pass after the two failed ones", store.loads)
	}

	// A one-shot run still reports it: exiting 0 would call an unscanned fleet clean.
	once := &upstreamFailingStore{StateStore: NewMemoryStore(cfg), failLoads: 1}
	err = NewService(cfg, newFakeGitHub(), once, nil).Watch(context.Background(),
		WatchOptions{Once: true, Interval: time.Millisecond, Dispatch: &observeOnly}, func(WatchEvent) error { return nil })
	if !ghapi.IsUpstreamFailure(err) {
		t.Fatalf("one-shot Watch returned %v, want the upstream failure", err)
	}
}

func TestAutoReviewKeepsRunningWhenALeaseWriteFailsUpstream(t *testing.T) {
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	cfg.LeaderTTL = time.Minute
	cfg.AutoReviewPoll = time.Millisecond
	cfg.AutoReviewMaxScan = 10
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Update 1 is the host report; update 2 is the first lease claim.
	store := &upstreamFailingStore{StateStore: NewMemoryStore(cfg), failUpdate: 2, afterFailed: cancel}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Incremental: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AutoReview returned %v, want it to run until cancelled", err)
	}
	if store.updates <= store.failUpdate {
		t.Fatalf("updates = %d, want the lease claimed again after the failed write", store.updates)
	}

	once := &upstreamFailingStore{StateStore: NewMemoryStore(cfg), failUpdate: 2}
	err := NewService(cfg, newFakeGitHub(), once, nil).AutoReview(context.Background(), AutoOptions{Once: true, Incremental: true})
	if !ghapi.IsUpstreamFailure(err) {
		t.Fatalf("one-shot AutoReview returned %v, want the upstream failure", err)
	}
}

// cancelledRenewalStore fails the first lease RENEWAL the way a shutdown that
// lands mid-write does. The claim before it, and everything after, go through.
type cancelledRenewalStore struct {
	StateStore
	fired bool
}

func (s *cancelledRenewalStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	return s.StateStore.Update(ctx, func(st *State) error {
		before := st.Leader
		err := mutate(st)
		if err == nil && !s.fired && before != nil && st.Leader != nil && st.Leader != before && st.Leader.Token == before.Token {
			s.fired = true
			return context.Canceled
		}
		return err
	})
}

// A lease is keyed by token and a restart draws a new one, so a daemon that
// exits without handing its lease back stands by behind its own dead self.
func TestAutoReviewHandsBackItsLeaseWhenARenewalIsCancelled(t *testing.T) {
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	cfg.LeaderTTL = time.Minute
	cfg.AutoReviewPoll = time.Millisecond
	cfg.AutoReviewMaxScan = 10
	store := &cancelledRenewalStore{StateStore: NewMemoryStore(cfg)}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.AutoReview(context.Background(), AutoOptions{Incremental: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AutoReview returned %v, want the cancelled renewal", err)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Leader != nil {
		t.Fatalf("the lease from the earlier pass was left behind: %#v", st.Leader)
	}
}
