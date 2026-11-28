package gh

import (
	"context"
	"testing"
	"time"
)

func TestWriteLockLetsOneHolderAtATime(t *testing.T) {
	var lock WriteLock
	first, err := lock.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := lock.Acquire(waiting); err == nil {
		t.Fatal("a second holder was let in while the first still held the lease")
	}

	lock.Release("not-the-token")
	got := make(chan string, 1)
	go func() {
		token, _ := lock.Acquire(t.Context())
		got <- token
	}()
	select {
	case <-got:
		t.Fatal("a release with the wrong token freed the lease")
	case <-time.After(30 * time.Millisecond):
	}
	lock.Release(first)
	select {
	case token := <-got:
		if token == "" || token == first {
			t.Fatalf("second token = %q", token)
		}
	case <-time.After(time.Second):
		t.Fatal("releasing the lease did not wake the writer waiting for it")
	}
}

// A holder that dies is waited out, not waited for.
func TestWriteLockLeaseRunsOut(t *testing.T) {
	var lock WriteLock
	if _, err := lock.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	lock.mu.Lock()
	lock.until = time.Now().Add(20 * time.Millisecond)
	lock.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := lock.Acquire(ctx); err != nil {
		t.Fatalf("an expired lease was not taken over: %v", err)
	}
}

// Without a lease to take, a write simply goes ahead.
func TestLockStateWritesNeverBlocksAClientWithNoLease(t *testing.T) {
	g := NewTestClient("http://example.invalid", nil)
	started := time.Now()
	g.LockStateWrites(t.Context())()
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("a client with no lease to take waited for one")
	}
}
