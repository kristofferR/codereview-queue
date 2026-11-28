package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Every writer reaches GitHub through the one server, so the lease it hands
// out is what makes writers on different hosts take turns.
func TestStateWriteLeaseIsSharedAcrossGatewayClients(t *testing.T) {
	lock := &ghapi.WriteLock{}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Gateway: &recordingGateway{}, WriteLock: lock})
	httpServer := httptest.NewServer(srv.routes())
	defer httpServer.Close()

	first, err := ghapi.NewGitHubViaServer(httpServer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ghapi.NewGitHubViaServer(httpServer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	release := first.LockStateWrites(t.Context())
	acquired := make(chan func(), 1)
	go func() { acquired <- second.LockStateWrites(t.Context()) }()
	select {
	case <-acquired:
		t.Fatal("a second client took the lease while the first held it")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case releaseSecond := <-acquired:
		releaseSecond()
	case <-time.After(2 * time.Second):
		t.Fatal("the second client was not let in after the first released the lease")
	}
}

func TestStateWriteLeaseRequiresAGatewayClient(t *testing.T) {
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", WriteLock: &ghapi.WriteLock{}})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:7777/api/state-lock", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status without the client header = %d, want 403", rec.Code)
	}
}
