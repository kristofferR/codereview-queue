package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

// Loader is the read side of the state store. Taking the interface rather than
// the store keeps this package testable without GitHub.
type Loader interface {
	Load(ctx context.Context) (state.State, state.Revision, error)
}

// Logger matches the logger the rest of crq passes around.
type Logger interface {
	Printf(format string, args ...any)
}

// Options is everything the server needs that it must not decide for itself.
type Options struct {
	Addr        string
	MinInterval time.Duration
	Inflight    time.Duration
	// Bots is the fleet's reviewer list, used only when Resolve is nil.
	Bots []BotName
	// Resolve answers which reviewers run on one repository. It takes the whole
	// state, not just that repository's override, because the answer also
	// depends on the fleet's recorded defaults — resolving from the override
	// alone showed this host's env list however the fleet had been configured.
	// An empty repo asks for the fleet's own answer.
	Resolve func(st state.State, repo string) []BotName
	// WeeklyLimit is the vendor's weekly fair-use threshold. 0 counts without
	// forecasting.
	WeeklyLimit int
	// PacingFor resolves the three settings above against the state being
	// rendered, because all three are fleet-settable: taken from startup, a
	// saved pacing or fair-use change reported itself on the settings page while
	// the queue cards and the fair-use card went on using the old numbers until
	// somebody restarted the server. Nil keeps the startup values.
	PacingFor func(st state.State) Pacing
	// Poll is how often the state ref is re-read. There is no webhook for a git
	// ref, so this is a poll by necessity; a push only happens when Rev moves.
	Poll time.Duration
	// Assets is the built SPA. Nil serves a plain page explaining how to build
	// it, so `crq serve` is still useful from a source checkout.
	Assets fs.FS
	Log    Logger
	Now    func() time.Time
	// Fleet is the configuration the settings and setup pages display.
	Fleet FleetConfig
	// FleetFor derives the effective fleet settings from the state already
	// loaded for this snapshot. Nil retains the Actor fallback for embedders.
	FleetFor func(st state.State) *FleetSettings
	// AllowReposFor resolves the shared fleet allow-list against the state being
	// rendered. Nil keeps Fleet.AllowRepos as a startup-only fallback.
	AllowReposFor func(st state.State) []string
	// Host names the machine this server runs on, so the tool list can say
	// whose PATH it describes.
	Host string
	// Token authenticates icon fetches. Repository favicons live in private
	// repos too, and the browser must never hold this.
	Token string
	// LookupToken refreshes a rotated credential after an authenticated icon
	// request is rejected. Nil keeps Token fixed.
	LookupToken func(context.Context) string
	// Gateway is the one direct GitHub transport owned by this persistent
	// process. CLI processes proxy through it so ETags and backoff are shared.
	Gateway GitHubGateway
	// GatewayToken authenticates non-loopback gateway clients. Empty is safe
	// only for the default loopback-bound server; the handler enforces that.
	GatewayToken string
	// EnrollFor resolves whether crq reviews one repository. Nil falls back to
	// reading the env lists alone, which is what the dashboard did before
	// enrollment records existed.
	EnrollFor EnrollFor
	// Discoverer lists the repositories in scope for the repo picker. Nil means
	// the picker reports that discovery is not configured rather than showing an
	// empty list, which would read as "you have no repositories".
	Discoverer Discoverer
	// Coster estimates what a round would cost. Optional: without it the PR page
	// simply shows no price, rather than a wrong one.
	Coster Coster
	// TailLog reads a bounded tail of a session log after the handler resolves
	// the current session from state. Nil means this dashboard cannot access
	// logs on its host.
	TailLog func(context.Context, string, string, int64) (LogTail, error)
	// SolverFor resolves a repository's fix-session settings. Nil leaves the
	// repository page without a solver card rather than showing env values that
	// no repository record could change.
	SolverFor SolverFor
	// Previewer prices an enrollment before it happens.
	Previewer Previewer
	// Observer supplies the per-PR findings. Optional: without it the PR page
	// still renders its state layer.
	Observer Observer
	// Actor performs writes. Nil, or ReadOnly, makes every action endpoint
	// refuse — useful when pointing a dashboard at someone else's fleet.
	Actor    Actor
	ReadOnly bool
	// AllowedHosts are extra names actions may be addressed to, beyond loopback,
	// IP literals, the bound address and this machine's own name. A reverse
	// proxy or a DNS alias is the case for it: the check exists to stop a name
	// an ATTACKER controls from being rebound at this port, and crq cannot tell
	// one of those from an alias somebody set up on purpose.
	AllowedHosts []string
}

// Pacing is the fire-pacing configuration the overview renders, resolved
// against one state snapshot.
type Pacing struct {
	MinInterval time.Duration
	Inflight    time.Duration
	WeeklyLimit int
}

// pacing resolves the pacing settings for st, falling back to the values this
// server was started with.
func (s *Server) pacing(st state.State) Pacing {
	if s.opts.PacingFor == nil {
		return Pacing{MinInterval: s.opts.MinInterval, Inflight: s.opts.Inflight, WeeklyLimit: s.opts.WeeklyLimit}
	}
	return s.opts.PacingFor(st)
}

// Server holds the latest snapshot and fans it out to connected browsers.
type Server struct {
	opts   Options
	loader Loader

	// refreshMu serializes whole refreshes. The poller and every action handler
	// call refresh, and without this a poll that started first could finish last
	// and publish ITS older state over the one an action had just written —
	// rolling the dashboard back a revision and replaying its events.
	refreshMu sync.Mutex

	mu      sync.RWMutex
	last    Snapshot
	lastRev int64
	// loaded says a load has actually produced `last`. Rev cannot answer it: a
	// state ref that has never been written has revision 0 too, and before the
	// first load loadErr is nil as well — so both handlers and the SSE stream
	// took the ZERO snapshot for live state and served collections encoded as
	// null, which the client reads straight into snap.repos.filter(...).
	loaded bool
	// digest fingerprints the last snapshot pushed, so a rebuild that changed
	// something is broadcast even when the state revision did not move. See
	// refresh.
	digest  uint64
	loadErr error
	subs    map[chan []byte]struct{}
	// tools are probed once: LookPath per refresh would be wasted work, and the
	// answer only changes when someone installs something.
	tools []Tool
	icons *Icons
	// lastState is kept so a per-PR request can render without another read of
	// the state ref; the poller already has it.
	lastState    state.State
	observer     Observer
	observations *observeCache
	actor        Actor
	events       *eventLog
	discovered   *discoverCache
	costs        *costCache
}

func New(loader Loader, opts Options) *Server {
	if opts.Poll <= 0 {
		opts.Poll = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts, loader: loader, subs: map[chan []byte]struct{}{},
		tools: LocalTools(), icons: NewIcons(opts.Token, opts.LookupToken), observer: opts.Observer,
		observations: &observeCache{}, actor: opts.Actor,
		events: newEventLog(300), discovered: &discoverCache{}, costs: &costCache{}}
}

// Run polls the state ref and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go s.watch(ctx)

	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if s.opts.Log != nil {
		s.opts.Log.Printf("control plane on http://%s", s.opts.Addr)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/gateway/health", s.handleGatewayHealth)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		mux.HandleFunc(method+" /api/github/{path...}", s.handleGitHub)
	}
	mux.HandleFunc("GET /api/icon/{kind}/{name...}", s.handleIcon)
	mux.HandleFunc("GET /api/pr/{owner}/{name}/{pr}", s.handlePR)
	mux.HandleFunc("GET /api/discover", s.handleDiscover)
	mux.HandleFunc("GET /api/enroll-preview", s.handleEnrollPreview)
	mux.HandleFunc("GET /api/autofix-log/{owner}/{name}/{pr}", s.handleAutofixLog)
	mux.HandleFunc("POST /api/setup/refresh", s.handleSetupRefresh)
	mux.HandleFunc("POST /api/action/{action}", s.handleAction)
	mux.Handle("/", s.assets())
	return mux
}

// watch re-reads the state ref and pushes a snapshot whenever Rev moves. A
// failed read never clears the last good snapshot — stale-but-labelled beats
// blank, and the error rides along so the UI can say so.
func (s *Server) watch(ctx context.Context) {
	tick := time.NewTicker(s.opts.Poll)
	defer tick.Stop()
	s.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.refresh(ctx)
		}
	}
}

func (s *Server) refresh(ctx context.Context) {
	// Held across the load as well as the publish: the event feed is derived
	// from the previous snapshot, so two refreshes that interleave would each
	// diff against a state the other is about to replace.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	st, _, err := s.loader.Load(ctx)
	if err != nil {
		s.markStale(err)
		return
	}
	now := s.opts.Now()

	// Derive events before the snapshot so the feed it carries is current.
	s.mu.RLock()
	prev := s.lastState
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		s.events.add(diffStates(prev, st, now)...)
	}

	botsFor := s.botsFor(&st)
	pace := s.pacing(st)
	ov := BuildOverview(st, now, pace.MinInterval, pace.Inflight, botsFor, pace.WeeklyLimit, s.maxAttempts(st))
	// Read alongside the snapshot rather than cached: it is a state read the
	// server has already paid for, and a settings page showing a stale default
	// is how two people overwrite each other.
	var fleet *FleetSettings
	var env []EnvSetting
	if s.opts.FleetFor != nil {
		fleet = s.opts.FleetFor(st)
	} else if s.actor != nil {
		if f, err := s.actor.Fleet(ctx); err == nil {
			fleet = f
		}
	}
	if s.actor != nil {
		env = s.actor.EnvSettings(st)
	}
	fleetConfig := s.opts.Fleet
	if s.opts.AllowReposFor != nil {
		fleetConfig.AllowRepos = s.opts.AllowReposFor(st)
	}
	snap := BuildFleet(st, fleetConfig, ov, s.tools, s.opts.Host, now, botsFor, s.opts.EnrollFor, fleet, s.opts.SolverFor, env)
	snap.Events = s.events.list()
	digest := snapshotDigest(snap)

	s.mu.Lock()
	changed := ov.Rev != s.lastRev || s.last.Stale != nil || digest != s.digest || !s.loaded
	s.last, s.lastRev, s.loadErr, s.lastState = snap, ov.Rev, nil, st
	s.loaded, s.digest = true, digest
	subs := make([]chan []byte, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	if !changed && len(subs) > 0 {
		// Countdowns tick client-side, so a rebuild that says exactly what the
		// last one said needs no push. Only the clock moved.
		return
	}
	broadcast(snap, subs)
}

// handleSetupRefresh re-probes the service's own PATH before rebuilding the
// snapshot. Ordinary state polls deliberately reuse the tool inventory;
// operators use this after installing a tool or repairing a service PATH.
func (s *Server) handleSetupRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.allowDashboardRead(w, r) {
		return
	}

	// refresh reads tools while holding refreshMu. Updating under the same lock
	// keeps this explicit probe from racing the background state poll.
	s.refreshMu.Lock()
	s.tools = LocalTools()
	s.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	s.refresh(ctx)
	snap, _, err := s.snapshot()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

// snapshotDigest fingerprints everything a browser would paint, with the render
// clock excluded.
//
// The revision alone is not enough to decide whether to push. BuildOverview
// derives categorical state from `now` — a quota block or slot hold expiring, a
// leader lease lapsing, a retry coming due, a dispatch claim or host report
// going dead — and none of that moves Rev. Client-side countdowns cannot remove
// a finished session or change a blocked headline, so a quiet fleet stayed
// visibly blocked until an unrelated write or a reload.
//
// Overview.Now is excluded precisely because it moves every poll: hashing it
// would make every rebuild look different and turn this back into an
// unconditional broadcast. Everything else `now` decides is categorical, so it
// changes only when the answer does.
func snapshotDigest(snap Snapshot) uint64 {
	snap.Overview.Now = time.Time{}
	payload, err := json.Marshal(snap)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(payload)
	return h.Sum64()
}

// markStale records that the state ref could not be read and tells everyone
// looking at the last good snapshot.
//
// Storing the error alone was not enough. The snapshot handlers only refuse
// when nothing has ever loaded, so once one load had succeeded a browser — and
// an action's post-write refresh — kept getting HTTP 200 and a state that had
// stopped being current, presented exactly as a live one. Nothing said the
// dashboard had lost the ref.
func (s *Server) markStale(err error) {
	s.mu.Lock()
	s.loadErr = err
	if !s.loaded {
		// Nothing has ever loaded, so there is no snapshot to mark — but the
		// stream is open and healthy, and a browser that only ever consumes it
		// would sit on "Reading the state ref…" for as long as the credential or
		// the ref stays broken. The error is the only thing there is to say, so
		// it is said on its own frame: a page with no snapshot can render it, and
		// a client that does not know the event ignores it.
		subs := s.subscribers()
		s.mu.Unlock()
		broadcastFrame(unavailableFrame(err), subs)
		return
	}
	if s.last.Stale == nil {
		// Since is when the ref was LOST, not when the latest retry failed:
		// re-stamping it every poll would show an outage that is always five
		// seconds old.
		s.last.Stale = &Staleness{Error: err.Error(), Since: s.opts.Now()}
	} else {
		s.last.Stale.Error = err.Error()
	}
	snap := s.last
	subs := s.subscribers()
	s.mu.Unlock()
	broadcast(snap, subs)
}

// subscribers copies the current subscriber set. The caller must hold s.mu.
func (s *Server) subscribers() []chan []byte {
	subs := make([]chan []byte, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	return subs
}

func broadcast(snap Snapshot, subs []chan []byte) {
	payload, err := json.Marshal(snap)
	if err != nil {
		return
	}
	broadcastFrame(dataFrame(payload), subs)
}

// Subscribers carry whole SSE frames rather than bare payloads, so a message
// that is not a snapshot can be sent down the same stream without the client
// having to tell them apart by inspecting one.
func dataFrame(payload []byte) []byte {
	return fmt.Appendf(nil, "data: %s\n\n", payload)
}

// unavailableFrame reports that there is no state to send yet, and why. Named
// rather than sent as data: a client reads it with an explicit listener, so one
// that predates the event simply never sees it instead of parsing an error as a
// snapshot.
func unavailableFrame(err error) []byte {
	payload, merr := json.Marshal(map[string]string{"error": firstLoadError(err)})
	if merr != nil {
		return nil
	}
	return fmt.Appendf(nil, "event: unavailable\ndata: %s\n\n", payload)
}

func broadcastFrame(frame []byte, subs []chan []byte) {
	if len(frame) == 0 {
		return
	}
	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
			// The channel holds one complete frame. Replace an unread old one
			// with the current state so a browser that catches up never paints a
			// snapshot the server already superseded.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- frame:
			default:
			}
		}
	}
}

// snapshot is the last snapshot a load produced, whether one has been produced
// at all, and the error from the most recent read.
//
// loaded is what callers must gate on, not Rev and not a nil error. Until the
// first load returns there is no snapshot and no error either, and the zero
// value serves collections as null — which the client accepts as live state and
// immediately iterates.
func (s *Server) snapshot() (Snapshot, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, s.loaded, s.loadErr
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	snap, loaded, err := s.snapshot()
	if !loaded {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": firstLoadError(err)})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	snap, loaded, err := s.snapshot()
	if !loaded {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": firstLoadError(err)})
		return
	}
	ov := snap.Overview
	ov.Stale = snap.Stale
	writeJSON(w, http.StatusOK, ov)
}

// firstLoadError says why there is nothing to serve yet: the read that failed,
// or simply that the first one has not finished.
func firstLoadError(err error) string {
	if err != nil {
		return err.Error()
	}
	return "still reading the state ref"
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap, loaded, err := s.snapshot()
	// Healthy means a load has succeeded AND the latest one did. Reporting ok
	// before the first read finished would have a health check pass against a
	// server that has never reached the state ref.
	body := map[string]any{"rev": snap.Overview.Rev, "ok": loaded && err == nil}
	if err != nil || !loaded {
		body["error"] = firstLoadError(err)
	}
	writeJSON(w, http.StatusOK, body)
}

// handleEvents streams whole snapshots. The state blob is small, so replacing
// it wholesale is simpler than diffing and makes reconnection trivially correct.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	// Send the current state immediately so a fresh tab paints without waiting
	// for the next change — but only once a load has produced one. A browser
	// connecting during a slow first read would otherwise be handed the zero
	// snapshot and take it for live state. The subscription is already in place,
	// so that tab gets the real one the moment the load lands.
	//
	// A first read that has already FAILED is said outright instead. The stream
	// itself is fine, so nothing else would ever tell this tab why it is empty.
	switch snap, loaded, loadErr := s.snapshot(); {
	case loaded:
		if payload, err := json.Marshal(snap); err == nil {
			_, _ = w.Write(dataFrame(payload))
			flusher.Flush()
		}
	case loadErr != nil:
		_, _ = w.Write(unavailableFrame(loadErr))
		flusher.Flush()
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-ch:
			_, _ = w.Write(frame)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// assets serves the embedded SPA, falling back to index.html so client-side
// routes survive a reload.
func (s *Server) assets() http.Handler {
	if s.opts.Assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, unbuiltPage)
		})
	}
	files := http.FileServer(http.FS(s.opts.Assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(s.opts.Assets, path); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
				path = ""
			}
		}
		// The bundles are content-hashed, so a name that resolves at all
		// resolves to the same bytes for ever and can be cached hard. The page
		// that NAMES them cannot: a cached index.html keeps asking for the
		// bundle it was built against, so a restarted server serves new assets
		// nobody ever requests and the dashboard silently stays a version behind.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		http.Error(w, "could not encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(payload.Bytes())
}

const unbuiltPage = `<!doctype html><meta charset="utf-8">
<title>Code Review Queue</title>
<style>body{font:14px/1.6 system-ui,sans-serif;margin:60px auto;max-width:640px;color:#1B2430}
code{background:#EEF0F3;border-radius:5px;padding:1px 6px;font-size:13px}</style>
<h1>Dashboard assets are not built</h1>
<p>The API is running — <a href="/api/overview">/api/overview</a> works — but the web app
was not compiled into this binary.</p>
<p>Build it from the repository root:</p>
<p><code>cd web &amp;&amp; bun install &amp;&amp; bun run build</code></p>
<p>then rebuild crq. The assets are embedded from <code>web/dist</code>.</p>
`

// maxAttempts binds the per-repository fix budget to one loaded state, so the
// session card can say "attempt 2 of 5" rather than leaving the reader to guess
// whether that is nearly the last one. Shared by the overview and the PR page:
// two answers to one question is how the same session reads differently
// depending on which screen you are on.
func (s *Server) maxAttempts(st state.State) func(repo string) int {
	return func(repo string) int {
		if s.opts.SolverFor == nil {
			return 0
		}
		return s.opts.SolverFor(st, repo).MaxAttempts
	}
}

// botsFor binds the reviewer resolver to one loaded state, so a row can ask
// about its own repository without another state read.
func (s *Server) botsFor(st *state.State) BotsFor {
	return func(repo string) []BotName {
		if s.opts.Resolve == nil {
			return s.opts.Bots
		}
		return s.opts.Resolve(*st, repo)
	}
}
