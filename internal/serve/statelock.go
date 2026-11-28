package serve

import (
	"context"
	"net/http"
	"time"
)

// WriteLocker grants the lease for one state write at a time. See the
// WriteLock in internal/gh, which cmd/crq supplies.
type WriteLocker interface {
	Acquire(ctx context.Context) (string, error)
	Release(token string)
}

// stateLockWait bounds one request's wait for the lease. A writer that gets
// no answer in time goes ahead without it, and the swap decides.
const stateLockWait = 20 * time.Second

// handleStateLock grants the state-write lease to a gateway client, waiting
// for it if another write holds it. Authorized like the gateway: the clients
// that write state are the ones that reach GitHub through it.
func (s *Server) handleStateLock(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeGateway(w, r) {
		return
	}
	if s.opts.WriteLock == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this server hands out no state-write lease"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), stateLockWait)
	defer cancel()
	token, err := s.opts.WriteLock.Acquire(ctx)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the state-write lease stayed busy"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleStateUnlock(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeGateway(w, r) {
		return
	}
	if s.opts.WriteLock != nil {
		s.opts.WriteLock.Release(r.URL.Query().Get("token"))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"released": true})
}
