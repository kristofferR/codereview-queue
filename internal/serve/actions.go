package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Actor performs the mutations the dashboard offers. Every one is a thin mirror
// of a CLI verb and goes through the same Service method, so the dashboard can
// never become a second way to change state with different rules.
type Actor interface {
	Hold(ctx context.Context, repo string, pr int, reason string) (warning string, err error)
	Unhold(ctx context.Context, repo string, pr int) (warning string, err error)
	Prioritize(ctx context.Context, repo string, pr int) error
	Cancel(ctx context.Context, repo string, pr int) error
	SetAutofix(ctx context.Context, repo string, enabled bool, reason string) error
	ClearAutofix(ctx context.Context, repo string) error
	// SetEnrollment decides whether crq reviews a repository at all, and
	// ClearEnrollment hands it back to the hosts' env files. Like the reviewer
	// override, they report the hosts that will not honour the record.
	SetEnrollment(ctx context.Context, repo string, enabled bool, reason string, expectedRev *int64) (lagging []string, err error)
	ClearEnrollment(ctx context.Context, repo string, expectedRev *int64) error
	// Fleet reads the recorded defaults; SetFleet applies a change, or with
	// preview reports what it WOULD do and writes nothing. A fleet save reaches
	// every repository that has not overridden the setting, so the preview is
	// not a nicety — it is how someone finds out that adding a required reviewer
	// reopens nineteen completed rounds before they click.
	Fleet(ctx context.Context) (*FleetSettings, error)
	// EnvSettings is every individual setting with its effective value and the
	// layer that decided it. Pure: it reads the state it is handed.
	EnvSettings(st state.State) []EnvSetting
	// SetEnv previews or records one fleet-wide setting by its env name.
	SetEnv(ctx context.Context, key, value string, unset bool, expectedRev *int64, preview bool) (FleetImpact, error)
	// SetSolver records how one repository's fix sessions run, or with an empty
	// repo the fleet default every repository inherits.
	SetSolver(ctx context.Context, repo string, change SolverChange) error
	SetFleet(ctx context.Context, change FleetChange, preview bool) (FleetImpact, error)
	// SetReviewers returns the hosts that will not honour the override, so the
	// UI can say so rather than reporting a save that some daemon ignores.
	SetReviewers(
		ctx context.Context,
		repo string,
		coBots, required []string,
		primary *bool,
		expectedRev *int64,
		preview bool,
	) (lagging []string, impact FleetImpact, err error)
	ClearReviewers(ctx context.Context, repo string, expectedRev *int64, preview bool) (FleetImpact, error)

	// The three ways a finding stops blocking. They are distinct on purpose:
	// resolving says it was handled, declining says it was considered and
	// rejected (and posts that reasoning back), dismissing accounts for a
	// finding GitHub gives no way to close.
	ResolveThreads(ctx context.Context, threadIDs []string) error
	DeclineThreads(ctx context.Context, threadIDs []string, reason string, resolve bool) error
	DismissFindings(ctx context.Context, repo string, pr int, ids []string, reason string) error
}

type actionRequest struct {
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	Reason  string `json:"reason"`
	Enabled *bool  `json:"enabled"`
	// ExpectedRev binds a confirmed enrollment preview to the state it priced.
	ExpectedRev *int64 `json:"expected_rev,omitempty"`
	// CoBots and Required are the whole intended sets, not a delta: a save that
	// sent only changes could not express "explicitly none".
	CoBots   []string `json:"cobots"`
	Required []string `json:"required"`
	// Primary is nil to leave the metered reviewer's switch alone, or points at
	// whether it runs on this repository at all.
	Primary *bool `json:"primary"`
	Clear   bool  `json:"clear"`
	// Fleet carries a fleet-defaults change, raw so its own pointer fields keep
	// "not chosen" distinct from "chosen to be zero".
	Fleet json.RawMessage `json:"fleet"`
	// Preview asks what a change would do without making it.
	Preview bool `json:"preview"`
	// Solver carries a fix-session change, raw so its pointer fields keep "not
	// chosen" distinct from "chosen to be empty".
	Solver json.RawMessage `json:"solver"`
	// Key/Value address one setting by its environment-variable name.
	Key   string `json:"key"`
	Value string `json:"value"`

	ThreadIDs  []string `json:"thread_ids"`
	FindingIDs []string `json:"finding_ids"`
	// KeepOpen declines a finding without resolving its thread, for when the
	// disagreement is worth leaving visible.
	KeepOpen bool `json:"keep_open"`
}

// handleAction runs one action and returns the refreshed snapshot, so the UI
// never has to guess whether the write landed — it sees the new state or an
// error, and nothing in between.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if s.actor == nil || s.opts.ReadOnly {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this dashboard is read-only",
		})
		return
	}
	// A custom header a browser cannot set cross-origin without a preflight.
	// The server is unauthenticated on a tailnet, so this is what stops a page
	// on another site from posting to it behind your back.
	if r.Header.Get("X-CRQ-Dashboard") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing dashboard header"})
		return
	}
	// The header is not enough on its own, because a DNS rebind makes the
	// attacker's page same-origin: a name they control, re-pointed at 127.0.0.1,
	// reaches this server with any header it likes and no preflight at all. The
	// one thing that still tells the two apart is the name the request was
	// addressed to, so this checks that it is a name this dashboard answers to.
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	var req actionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	// Fleet settings are the one action with no repository: they are the answer
	// for every repository that has not overridden them.
	req.Repo = strings.TrimSpace(req.Repo)
	action := r.PathValue("action")
	if req.Preview && action != "fleet" && action != "env" && action != "reviewers" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this action has no preview"})
		return
	}
	fleetWide := action == "fleet" || action == "env" || (action == "solver" && req.Repo == "")
	if !fleetWide && (req.Repo == "" || !strings.Contains(req.Repo, "/")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/name"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var (
		err     error
		warning string
	)
	switch action {
	case "hold":
		// The reason is the record: every screen that shows a hold shows why,
		// and an unexplained hold is the one nobody dares lift.
		if strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.needPR(req, func() error {
			warning, err = s.actor.Hold(ctx, req.Repo, req.PR, req.Reason)
			return err
		})
	case "unhold":
		err = s.needPR(req, func() error {
			warning, err = s.actor.Unhold(ctx, req.Repo, req.PR)
			return err
		})
	case "prioritize":
		err = s.needPR(req, func() error { return s.actor.Prioritize(ctx, req.Repo, req.PR) })
	case "cancel":
		err = s.needPR(req, func() error { return s.actor.Cancel(ctx, req.Repo, req.PR) })
	case "autofix":
		if req.Enabled == nil {
			err = s.actor.ClearAutofix(ctx, req.Repo) // back to the fleet default
		} else {
			err = s.actor.SetAutofix(ctx, req.Repo, *req.Enabled, req.Reason)
		}
	case "reviewers":
		if req.Clear {
			impact, clearErr := s.actor.ClearReviewers(ctx, req.Repo, req.ExpectedRev, req.Preview)
			if writeImpact(w, impact, clearErr, req.Preview) {
				return
			}
			snap, warning := s.actionSnapshot(ctx)
			response := map[string]any{"snapshot": snap, "impact": impact}
			if warning != "" {
				response["warning"] = warning
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
		// A nil list was not sent at all (a save that only flips the primary
		// switch), which is different from a sent-but-empty one. The service
		// re-checks the RESOLVED set either way — this is the early, specific
		// error for the case the UI can produce directly.
		if req.Required != nil && len(req.Required) == 0 {
			// Convergence would never be reachable, so refuse here rather than
			// letting a round wait for a set nothing can satisfy.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "at least one reviewer must be required, or convergence can never happen",
			})
			return
		}
		var lagging []string
		var impact FleetImpact
		lagging, impact, err = s.actor.SetReviewers(
			ctx, req.Repo, req.CoBots, req.Required, req.Primary, req.ExpectedRev, req.Preview,
		)
		if writeImpact(w, impact, err, req.Preview) {
			return
		}
		if err == nil && len(lagging) > 0 {
			s.writeActionSnapshot(w, ctx, "saved, but these hosts run an older binary and will ignore it: "+
				strings.Join(hostsOf(lagging), ", "))
			return
		}
	case "enroll":
		if req.Clear {
			err = s.actor.ClearEnrollment(ctx, req.Repo, req.ExpectedRev)
			break
		}
		if req.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled must be true or false"})
			return
		}
		// The service refuses an unexplained removal too; asking here means the
		// dialog can say so before the round trip.
		if !*req.Enabled && strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required to stop reviewing a repository"})
			return
		}
		var lagging []string
		lagging, err = s.actor.SetEnrollment(ctx, req.Repo, *req.Enabled, req.Reason, req.ExpectedRev)
		if err == nil && len(lagging) > 0 {
			s.writeActionSnapshot(w, ctx, "saved, but these hosts run an older binary and decide from their own env alone: "+
				strings.Join(hostsOf(lagging), ", "))
			return
		}
	case "fleet":
		var change FleetChange
		if err := json.Unmarshal(req.Fleet, &change); len(req.Fleet) > 0 && err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed fleet change"})
			return
		}
		impact, ferr := s.actor.SetFleet(ctx, change, req.Preview)
		if writeImpact(w, impact, ferr, req.Preview) {
			return
		}
		snap, warning := s.actionSnapshot(ctx)
		response := map[string]any{"snapshot": snap, "impact": impact}
		if warning != "" {
			response["warning"] = warning
		}
		writeJSON(w, http.StatusOK, response)
		return
	case "env":
		if strings.TrimSpace(req.Key) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no setting named"})
			return
		}
		impact, envErr := s.actor.SetEnv(ctx, req.Key, req.Value, req.Clear, req.ExpectedRev, req.Preview)
		if writeImpact(w, impact, envErr, req.Preview) {
			return
		}
		snap, warning := s.actionSnapshot(ctx)
		response := map[string]any{"snapshot": snap, "impact": impact}
		if warning != "" {
			response["warning"] = warning
		}
		writeJSON(w, http.StatusOK, response)
		return
	case "solver":
		var change SolverChange
		if err := json.Unmarshal(req.Solver, &change); len(req.Solver) > 0 && err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed solver change"})
			return
		}
		// An empty repo is the fleet default, which is why this action is one of
		// the two that does not require one.
		err = s.actor.SetSolver(ctx, req.Repo, change)
	case "resolve":
		if len(req.ThreadIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no thread given"})
			return
		}
		err = s.actor.ResolveThreads(ctx, req.ThreadIDs)
	case "decline":
		if len(req.ThreadIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no thread given"})
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			// The reason is posted to the pull request as a reply, so an empty
			// one would leave a bare rejection on someone else's review.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.actor.DeclineThreads(ctx, req.ThreadIDs, req.Reason, !req.KeepOpen)
	case "dismiss":
		if len(req.FindingIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no finding given"})
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.needPR(req, func() error {
			return s.actor.DismissFindings(ctx, req.Repo, req.PR, req.FindingIDs, req.Reason)
		})
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errBadPR) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	// Re-read immediately rather than waiting for the next poll: the person who
	// clicked is watching, and a stale answer reads as a failed click.
	s.writeActionSnapshot(w, ctx, warning)
}

// writeImpact owns the common preview contract: a preview returns only the
// consequence estimate, while a failed preview or save returns the mutation
// error. It reports whether the response is complete.
func writeImpact(w http.ResponseWriter, impact FleetImpact, err error, preview bool) bool {
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return true
	}
	if preview {
		// A preview writes nothing, so returning a snapshot would read as a save
		// that silently failed to change it.
		writeJSON(w, http.StatusOK, map[string]any{"impact": impact})
		return true
	}
	return false
}

// writeActionSnapshot reports a completed mutation even if its confirming read
// failed. Repeating a non-idempotent action such as decline could otherwise
// post duplicate replies. The previous snapshot remains useful while the next
// poll retries the read, and the warning tells the client it may be stale.
func (s *Server) writeActionSnapshot(w http.ResponseWriter, ctx context.Context, warning string) {
	snap, refreshWarning := s.actionSnapshot(ctx)
	if refreshWarning != "" {
		if warning != "" {
			warning += " "
		}
		warning += refreshWarning
	}
	if warning == "" {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap, "warning": warning})
}

func (s *Server) actionSnapshot(ctx context.Context) (Snapshot, string) {
	snap, err := s.refreshedSnapshot(ctx)
	if err == nil {
		return snap, ""
	}
	return snap, err.Error()
}

func (s *Server) refreshedSnapshot(ctx context.Context) (Snapshot, error) {
	s.refresh(ctx)
	snap, loaded, err := s.snapshot()
	if err != nil {
		return snap, fmt.Errorf("action succeeded, but refreshing state failed: %w", err)
	}
	if !loaded {
		return Snapshot{}, errors.New("action succeeded, but refreshing state produced no snapshot")
	}
	return snap, nil
}

// addressedHere reports whether an action request was addressed to this
// dashboard, rather than to a name that merely resolves to it.
//
// This is the anti-rebinding check, and it is about NAMES. An address bar
// holding an IP literal cannot be rebound — the browser dials it, and the origin
// is the address itself — so those pass, as does loopback by its own name. Every
// other name has to be one this host answers to: the address the server was
// asked to bind, the machine's own hostname — itself, or its short name in a
// zone nobody outside can publish in, so `mac.local` and a tailnet's
// `mac.tailnet.ts.net` are the same machine — or one named with --allow-host for
// a proxy or an alias crq cannot know about.
//
// An Origin, when the browser sends one, must agree with it: same-origin is the
// whole claim being made, and a request that contradicts itself is not one.
func (s *Server) addressedHere(r *http.Request) error {
	host := hostname(r.Host)
	if host == "" {
		return errors.New("refusing an action with no Host: crq serve answers only to its own address")
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(hostname(u.Host), host) {
			return fmt.Errorf("refusing an action sent from %s to %s: the two must be the same dashboard", origin, r.Host)
		}
	}
	if s.answersTo(host) {
		return nil
	}
	return fmt.Errorf("refusing an action addressed to %q: crq serve is unauthenticated, so it acts only on its own address — "+
		"a name that resolves here is how a page on another site reaches a local dashboard. Start it with --allow-host %s if that name is yours",
		host, host)
}

// answersTo reports whether host names this server.
func (s *Server) answersTo(host string) bool {
	switch {
	case net.ParseIP(host) != nil:
		return true
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return true
	}
	for _, allowed := range s.opts.AllowedHosts {
		if strings.EqualFold(hostname(strings.TrimSpace(allowed)), host) {
			return true
		}
	}
	// The bound address and this machine's own name, exactly as configured — and
	// the short forms of it: a host is reached as `mac`, `mac.local` and
	// `mac.<tailnet>.ts.net` interchangeably, and all of those are the same
	// machine answering.
	for _, own := range []string{hostname(s.opts.Addr), hostname(s.opts.Host)} {
		if own == "" || own == "0.0.0.0" || own == "::" {
			continue
		}
		if host == own {
			return true
		}
		if short := firstLabel(own); short != "" && (host == short || isOwnNameIn(host, short)) {
			return true
		}
	}
	return false
}

// localZones are the suffixes this machine's own short name may be reached
// under. They are the zones nobody outside can publish a record in: mDNS on the
// local link, a home network's own domain, and a Tailscale tailnet.
//
// The list is what makes the name check a check at all. Comparing first labels
// alone accepted ANY zone sharing this machine's name — `mac.attacker.example`
// for a machine called `mac` — and that name is registrable, points wherever its
// owner says, and is same-origin with the page doing the pointing. It defeated
// the whole anti-rebinding check for any host whose name an attacker can guess,
// which is most of them. A name in some other zone is a proxy or an alias crq
// cannot know about, and --allow-host is how it is told.
var localZones = []string{"local", "lan", "home.arpa", "internal"}

// isOwnNameIn reports whether host is this machine's short name in one of the
// zones above — `mac.local` — or in a tailnet, which puts its own name between
// the machine and the zone: `mac.tail1234.ts.net`.
func isOwnNameIn(host, own string) bool {
	rest, ok := strings.CutPrefix(host, own+".")
	if !ok {
		return false
	}
	for _, zone := range localZones {
		if rest == zone {
			return true
		}
	}
	return strings.HasSuffix(rest, ".ts.net")
}

// hostname is the lowercased name in an authority, without its port.
func hostname(authority string) string {
	authority = strings.TrimSpace(authority)
	if h, _, err := net.SplitHostPort(authority); err == nil {
		authority = h
	}
	return strings.ToLower(strings.Trim(authority, "[]"))
}

func firstLabel(host string) string {
	name, _, _ := strings.Cut(hostname(host), ".")
	return name
}

func (s *Server) needPR(req actionRequest, run func() error) error {
	if req.PR <= 0 {
		return errBadPR
	}
	return run()
}

var errBadPR = errPR("a pull request number is required")

type errPR string

func (e errPR) Error() string { return string(e) }

// hostsOf reduces writer keys ("host=X pid=… run=…") to the machine names a
// person would act on.
func hostsOf(writers []string) []string {
	out := make([]string, 0, len(writers))
	seen := map[string]bool{}
	for _, w := range writers {
		h := state.WriterHost(w)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// FleetChange mirrors crq.FleetChange on the wire. Pointers throughout so a
// form that posts its whole state cannot overwrite a setting another host
// changed a second earlier.
type FleetChange struct {
	CoBots         []string `json:"cobots"`
	Required       []string `json:"required"`
	MinInterval    *string  `json:"min_interval"`
	WeeklyLimit    *int     `json:"weekly_limit"`
	AutofixDefault *bool    `json:"autofix_default"`
	ExpectedRev    *int64   `json:"expected_rev,omitempty"`
	Clear          bool     `json:"clear"`
}

// SolverChange mirrors crq.SolverChange on the wire.
type SolverChange struct {
	Models           []string `json:"models"`
	Model            *string  `json:"model"`
	Effort           *string  `json:"effort"`
	Prompt           *string  `json:"prompt"`
	MaxAttempts      *int     `json:"max_attempts"`
	Severities       []string `json:"severities"`
	AskMode          *string  `json:"ask_mode"`
	Forks            *bool    `json:"forks"`
	SkipAuthors      []string `json:"skip_authors"`
	OnePass          *bool    `json:"one_pass"`
	MergeMethod      *string  `json:"merge_method"`
	UnsetModels      bool     `json:"unset_models,omitempty"`
	UnsetEffort      bool     `json:"unset_effort,omitempty"`
	UnsetPrompt      bool     `json:"unset_prompt,omitempty"`
	UnsetSeverities  bool     `json:"unset_severities,omitempty"`
	UnsetAskMode     bool     `json:"unset_ask_mode,omitempty"`
	UnsetForks       bool     `json:"unset_forks,omitempty"`
	UnsetSkipAuthors bool     `json:"unset_skip_authors,omitempty"`
	UnsetOnePass     bool     `json:"unset_one_pass,omitempty"`
	UnsetMerge       bool     `json:"unset_merge,omitempty"`
	Clear            bool     `json:"clear"`
}
