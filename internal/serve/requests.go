package serve

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type callerKey struct{}

// CallerOf is who a gateway request was forwarded for, as that client named
// itself ("autofix@host"), or "" when it did not say.
func CallerOf(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey{}).(string)
	return caller
}

func withCaller(r *http.Request) context.Context {
	caller := strings.TrimSpace(r.Header.Get("X-CRQ-Caller"))
	if caller == "" {
		return r.Context()
	}
	return context.WithValue(r.Context(), callerKey{}, caller)
}

// handleRequests reports what this server asked of GitHub recently, by caller
// and endpoint. It is how an exhausted quota gets explained from counts rather
// than guessed at from traffic volume.
//
// Authorized like the gateway it describes: the report names the fleet's hosts
// and what each was doing, and its only reader is the crq command line.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeGateway(w, r) {
		return
	}
	if s.opts.Requests == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this server keeps no request accounting"})
		return
	}
	// An unparseable or absent window means the whole log.
	window, _ := time.ParseDuration(r.URL.Query().Get("window"))
	writeJSON(w, http.StatusOK, s.opts.Requests(window))
}
