package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/state"
)

// stubLoader hands back one state, counting how many times it was asked.
type stubLoader struct {
	st    state.State
	err   error
	reads int
}

func (l *stubLoader) Load(context.Context) (state.State, state.Revision, error) {
	l.reads++
	return l.st, state.Revision{}, l.err
}

type countingObserver struct{ calls int }

func (o *countingObserver) Observe(context.Context, state.State, string, int) (Observation, error) {
	o.calls++
	return Observation{}, nil
}

type sequenceObserver struct {
	observations []Observation
	calls        int
}

func (o *sequenceObserver) Observe(context.Context, state.State, string, int) (Observation, error) {
	got := o.observations[o.calls]
	o.calls++
	return got, nil
}

type failingObserver struct{ calls int }

func (o *failingObserver) Observe(context.Context, state.State, string, int) (Observation, error) {
	o.calls++
	return Observation{}, errors.New("observation unavailable")
}

type countingCoster struct {
	cost  Cost
	calls int
}

type previewGuardActor struct {
	Actor
	enrollmentCalls int
}

type holdWarningActor struct {
	Actor
	warning string
}

func (a *holdWarningActor) Hold(context.Context, string, int, string) (string, error) {
	return a.warning, nil
}

func (a *holdWarningActor) Unhold(context.Context, string, int) (string, error) {
	return a.warning, nil
}

func (*holdWarningActor) Fleet(context.Context) (*FleetSettings, error) { return nil, nil }

func (*holdWarningActor) EnvSettings(state.State) []EnvSetting { return nil }

func (a *previewGuardActor) SetEnrollment(
	context.Context, string, bool, string, *int64,
) ([]string, error) {
	a.enrollmentCalls++
	return nil, nil
}

func (c *countingCoster) Cost(context.Context, state.State, string, int, Observation) (Cost, error) {
	c.calls++
	return c.cost, nil
}

func TestRefreshSuppressesOnlyTheInitialRevisionZeroLoad(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	loader := &stubLoader{st: state.New()}
	srv := New(loader, Options{Now: func() time.Time { return at }})
	srv.refresh(t.Context())
	if events := srv.events.list(); len(events) != 0 {
		t.Fatalf("initial load events = %+v, want the existing snapshot suppressed", events)
	}

	loader.st.Rev = 1
	loader.st.Enrolled = map[string]state.RepoEnrollment{
		"o/enrolled": {Enabled: true, By: "atlas", UpdatedAt: &at},
	}
	srv.refresh(t.Context())
	if events := srv.events.list(); !hasEventText(events, "Repository enrolled for review") {
		t.Fatalf("first mutation events = %+v, want the revision-zero predecessor retained", events)
	}
}

func TestRefreshUsesResolvedAllowListForRepositoryRows(t *testing.T) {
	st := state.New()
	st.Rev = 4
	srv := New(&stubLoader{st: st}, Options{
		Now:   time.Now,
		Fleet: FleetConfig{AllowRepos: []string{"startup/old"}},
		AllowReposFor: func(state.State) []string {
			return []string{"fleet/new"}
		},
	})
	srv.refresh(t.Context())
	if len(srv.last.Repos) != 1 || srv.last.Repos[0].Repo != "fleet/new" {
		t.Fatalf("repository rows = %+v, want the resolved fleet allow-list", srv.last.Repos)
	}
}

func TestRefreshedSnapshotReportsALoadFailureAfterAnAction(t *testing.T) {
	loader := &stubLoader{st: state.New()}
	srv := New(loader, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})
	if _, err := srv.refreshedSnapshot(t.Context()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	loader.err = errors.New("state ref unavailable")
	if _, err := srv.refreshedSnapshot(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "state ref unavailable") {
		t.Fatalf("failed refresh error = %v, want the state read failure", err)
	}
}

func TestActionSnapshotKeepsThePreviousViewWhenRefreshFails(t *testing.T) {
	loader := &stubLoader{st: state.New()}
	srv := New(loader, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})
	if _, err := srv.refreshedSnapshot(t.Context()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	loader.err = errors.New("state ref unavailable")
	snap, warning := srv.actionSnapshot(t.Context())
	if warning == "" || !strings.Contains(warning, "action succeeded") {
		t.Fatalf("warning = %q, want a completed-action warning", warning)
	}
	if snap.Overview.Now.IsZero() {
		t.Fatal("action response lost the last usable snapshot")
	}
}

// Before the first load returns there is no snapshot — and no error either. The
// zero Snapshot encodes its collections as null, and the client takes a 200 for
// live state and iterates them straight away, so the dashboard crashed during
// ordinary startup against a slow state read.
func TestHandlersRefuseUntilTheFirstLoadSucceeds(t *testing.T) {
	srv := New(&stubLoader{}, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	for _, path := range []string{"/api/snapshot", "/api/overview"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Host = "localhost"
		switch path {
		case "/api/snapshot":
			srv.handleSnapshot(rec, req)
		default:
			srv.handleOverview(rec, req)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d before any load, want 503 rather than a null-filled snapshot", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pr/o/r/1", nil)
	req.Host = "localhost"
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/api/pr = %d before any load, want 503 rather than a false empty PR", rec.Code)
	}

	// Health must not read as ok either: a check that passes here passes against
	// a server that has never reached the state ref.
	rec = httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", nil))
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != false {
		t.Errorf("health = %v before any load, want ok false", health)
	}

	// And the SSE stream sends nothing until there is something real to send.
	rec = httptest.NewRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	req.Host = "localhost"
	srv.handleEvents(rec, req)
	if body := rec.Body.String(); body != "" {
		t.Errorf("the stream sent %q before any load; a browser would take it for live state", body)
	}

	// Once a load lands, everything answers.
	srv.refresh(context.Background())
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot", nil)
	req.Host = "localhost"
	srv.handleSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("snapshot = %d after a successful load, want 200", rec.Code)
	}
}

func TestSetupRefreshReprobesToolsWithoutAnActor(t *testing.T) {
	loader := &stubLoader{}
	srv := New(loader, Options{
		Addr: "127.0.0.1:7777",
		Host: "test-host",
		Now:  func() time.Time { return time.Unix(0, 0).UTC() },
	})
	// A made-up cached tool proves the handler replaced the inventory rather
	// than merely returning another copy of the existing snapshot.
	srv.tools = []Tool{{Name: "definitely-not-a-real-crq-tool", Found: true}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:7777/api/setup/refresh", nil)
	req.Header.Set("X-CRQ-Dashboard", "1")
	rec := httptest.NewRecorder()
	srv.handleSetupRefresh(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Snapshot struct {
			Setup SetupView `json:"setup"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Snapshot.Setup.Tools {
		if tool.Name == "definitely-not-a-real-crq-tool" {
			t.Fatal("refresh returned the stale cached tool inventory")
		}
	}
	if loader.reads != 1 {
		t.Fatalf("state reads = %d, want one fresh snapshot read", loader.reads)
	}

	rec = httptest.NewRecorder()
	srv.handleSetupRefresh(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:7777/api/setup/refresh", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("refresh without dashboard header = %d, want 403", rec.Code)
	}
}

// A first read that FAILS is a different thing from one still running, and the
// stream is the only thing the page consumes. Sending nothing left a broken
// credential or state ref looking exactly like a slow one — the dashboard sat on
// "Reading the state ref…" indefinitely while every other handler could say why.
func TestTheStreamSaysWhyThereIsNoStateYet(t *testing.T) {
	loader := &stubLoader{err: errors.New("state ref unreadable: bad credentials")}
	srv := New(loader, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})
	srv.refresh(context.Background())

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	req.Host = "localhost"
	srv.handleEvents(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: unavailable\n") {
		t.Fatalf("stream sent %q, want a named event a snapshot reader ignores", body)
	}
	if !strings.Contains(body, "bad credentials") {
		t.Errorf("stream sent %q, want the error that explains the empty page", body)
	}
}

func TestBroadcastReplacesAQueuedStaleFrame(t *testing.T) {
	ch := make(chan []byte, 1)
	ch <- []byte("old")
	broadcastFrame([]byte("new"), []chan []byte{ch})
	if got := string(<-ch); got != "new" {
		t.Fatalf("queued frame = %q, want the latest frame", got)
	}
}

// The dashboard header stops an ordinary cross-site POST but not a DNS rebind:
// a name the attacker controls, re-pointed at 127.0.0.1, is same-origin as far
// as the browser is concerned and may set any header it likes. The name the
// request was addressed to is what still tells the two apart.
func TestActionsAreRefusedOnANameThatOnlyResolvesHere(t *testing.T) {
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Host: "atlas", AllowedHosts: []string{"crq.example.test"}})

	for _, host := range []string{
		"127.0.0.1:7777", "[::1]:7777", "192.168.1.4:7777", // an IP literal cannot be rebound
		"localhost:7777", "crq.localhost:7777",
		"atlas", "atlas.local:7777", "atlas.tail1234.ts.net", // the same machine, however it is reached
		"crq.example.test:7777", // named with --allow-host
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
		req.Host = host
		if err := srv.addressedHere(req); err != nil {
			t.Errorf("Host %q was refused: %v", host, err)
		}
	}
	for _, host := range []string{
		"", "evil.test:7777", "crq.example.test.evil.test:7777",
		// The machine is called `atlas`, and this name is not it: a zone its
		// owner controls, pointed here, is the rebinding the check is for.
		"atlas.attacker.example:7777", "atlas.evil.test",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
		req.Host = host
		if err := srv.addressedHere(req); err == nil {
			t.Errorf("Host %q was accepted; a page on that name could act on this fleet", host)
		}
	}

	// And an Origin that contradicts the Host is not a same-origin request
	// whatever it claims, even when the Host itself is one we answer to.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
	req.Host = "localhost:7777"
	req.Header.Set("Origin", "http://evil.test")
	if err := srv.addressedHere(req); err == nil {
		t.Error("a cross-origin POST to localhost was accepted")
	}
}

func TestUnsupportedPreviewIsRejectedBeforeItCanMutate(t *testing.T) {
	actor := &previewGuardActor{}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Actor: actor})
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"http://localhost:7777/api/action/enroll",
		strings.NewReader(`{"repo":"o/r","enabled":true,"preview":true}`),
	)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("action", "enroll")
	srv.handleAction(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no preview") {
		t.Fatalf("unsupported preview = %d: %s", rec.Code, rec.Body.String())
	}
	if actor.enrollmentCalls != 0 {
		t.Fatalf("enrollment calls = %d, want the request rejected before mutation", actor.enrollmentCalls)
	}
}

func TestHoldActionReturnsCommentWarning(t *testing.T) {
	loader := &stubLoader{st: state.New()}
	actor := &holdWarningActor{warning: "PR is held, but the hold comment could not be posted"}
	srv := New(loader, Options{Addr: "127.0.0.1:7777", Actor: actor})
	srv.refresh(t.Context())
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"http://localhost:7777/api/action/hold",
		strings.NewReader(`{"repo":"owner/repo","pr":12,"reason":"waiting"}`),
	)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("action", "hold")
	srv.handleAction(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), actor.warning) {
		t.Fatalf("hold response = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUnholdActionReturnsCommentWarning(t *testing.T) {
	loader := &stubLoader{st: state.New()}
	actor := &holdWarningActor{warning: "PR hold was released, but the release comment could not be posted"}
	srv := New(loader, Options{Addr: "127.0.0.1:7777", Actor: actor})
	srv.refresh(t.Context())
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"http://localhost:7777/api/action/unhold",
		strings.NewReader(`{"repo":"owner/repo","pr":12}`),
	)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("action", "unhold")
	srv.handleAction(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), actor.warning) {
		t.Fatalf("unhold response = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotStreamsAreRefusedOnANameThatOnlyResolvesHere(t *testing.T) {
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Host: "atlas"})
	srv.loaded = true

	for _, handle := range []func(http.ResponseWriter, *http.Request){
		srv.handleSnapshot,
		srv.handleOverview,
		srv.handleEvents,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.Host = "attacker.example:7777"
		handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("snapshot read on rebound host = %d, want 403", rec.Code)
		}
	}
}

func TestQuotaHeavyReadsRequireDashboardOriginProof(t *testing.T) {
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Host: "atlas"})

	for _, tc := range []struct {
		name   string
		host   string
		header string
		want   bool
	}{
		{name: "dashboard", host: "localhost:7777", header: "1", want: true},
		{name: "cross-site get", host: "localhost:7777", want: false},
		{name: "dns rebind", host: "attacker.example:7777", header: "1", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/discover?refresh=1", nil)
			req.Host = tc.host
			req.Header.Set("X-CRQ-Dashboard", tc.header)
			if got := srv.allowDashboardRead(rec, req); got != tc.want {
				t.Fatalf("allowed = %v, want %v (status %d)", got, tc.want, rec.Code)
			}
		})
	}
}

func TestPRReadRequiresDashboardOriginProofBeforeObserving(t *testing.T) {
	observer := &countingObserver{}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Observer: observer})
	srv.loaded = true
	srv.lastState = state.New()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1?refresh=1", nil)
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized PR read = %d, want 403", rec.Code)
	}
	if observer.calls != 0 {
		t.Fatalf("observer calls = %d, want none before authorization", observer.calls)
	}
}

// Rev alone cannot decide whether to push. BuildOverview derives categorical
// state from `now` — a quota block expiring, a lease lapsing, a claim going
// dead — and none of that moves Rev, so a quiet fleet stayed visibly blocked
// until an unrelated write. The render clock itself must NOT count, or every
// poll would broadcast and the change detection would mean nothing.
func TestSnapshotDigestIgnoresTheClockButNotWhatItDecides(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	blocked := now.Add(10 * time.Minute)
	st := state.New()
	st.Account.BlockedUntil = &blocked

	build := func(at time.Time) Snapshot {
		ov := BuildOverview(st, at, 0, time.Minute, func(string) []BotName { return nil }, 0, nil)
		return BuildFleet(st, FleetConfig{}, ov, nil, "testhost", at,
			func(string) []BotName { return nil }, nil, nil, nil, nil)
	}

	if a, b := snapshotDigest(build(now)), snapshotDigest(build(now.Add(time.Second))); a != b {
		t.Error("the render clock moved the digest; every poll would broadcast and nothing would be gained")
	}
	if a, b := snapshotDigest(build(now)), snapshotDigest(build(blocked.Add(time.Minute))); a == b {
		t.Error("the quota block expired without changing the digest, so nothing would be pushed")
	}
}

// The rolling-upgrade table is only as right as its idea of "newest". Compared
// as text, 2.9.0 outranks 2.10.0 the first time the fleet crosses a digit
// boundary — and then every upgraded host is warned about while the hosts still
// running the old binary read as current, which is the warning exactly
// backwards.
func TestNewestHostVersionIsPickedNumerically(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := state.State{HostReports: map[string]state.HostReport{
		"atlas": {Host: "atlas", Version: "2.9.0", At: now},
		"borg":  {Host: "borg", Version: "2.10.0", At: now},
	}}

	behind := map[string]bool{}
	for _, row := range hostTools(st, now) {
		behind[row.Host] = row.Behind
	}
	if !behind["atlas"] {
		t.Error("2.9.0 is behind 2.10.0 and must be marked so")
	}
	if behind["borg"] {
		t.Error("2.10.0 is the newest crq reporting and must not be marked behind")
	}
}

// A round's answer belongs to whichever primary produced it. CRQ_BOT is a
// setting, so a fleet that changes it leaves rounds the retired bot answered
// behind — and attributing those to whatever this process calls its primary
// showed the newly configured bot as working and the one that actually reviewed
// as silent, which is both claims backwards.
func TestAPrimarysAnswerStaysWithThePrimaryThatGaveIt(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	answered := now.Add(-time.Hour)
	st := state.State{Rounds: map[string]state.Round{
		"o/repo#1": {
			Repo: "o/repo", PR: 1, Head: "abcdef123", Phase: state.PhaseCompleted,
			PrimaryAnsweredAt: &answered, PrimaryAnsweredBy: "coderabbitai[bot]",
		},
	}}
	// The dashboard has since been configured with a different primary.
	cfg := FleetConfig{GateRepo: "o/gate", Reviewers: []ReviewerCfg{
		{Login: "macroscope[bot]", Name: "macroscope", Primary: true, Metered: true},
	}}
	running := []BotName{{Login: "macroscope[bot]", Name: "macroscope", Primary: true}}

	macroscope := func(st state.State) BotCard {
		for _, card := range botCards(st, cfg, func(string) []BotName { return running }, now) {
			if card.Login == "macroscope[bot]" {
				return card
			}
		}
		t.Fatalf("no card for the configured primary")
		return BotCard{}
	}
	if got := macroscope(st); got.LastSeen != nil || got.Status == "working" {
		t.Errorf("the new primary was credited with a review it never did: %+v", got)
	}

	// A round recorded before the login was stored has nobody to attribute it
	// to but the running primary, which is what crq assumed for every round
	// until now — so the fallback stays, and only rounds that name a bot move.
	legacy := st.Rounds["o/repo#1"]
	legacy.PrimaryAnsweredBy = ""
	st.Rounds["o/repo#1"] = legacy
	if got := macroscope(st); got.LastSeen == nil || !got.LastSeen.Equal(answered) {
		t.Errorf("an unattributed answer must still count for the running primary: %+v", got)
	}
}

func TestAPrimaryAskStaysWithThePrimaryItAddressed(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	asked := now.Add(-time.Hour)
	st := state.State{Rounds: map[string]state.Round{
		"o/repo#1": {
			Repo: "o/repo", PR: 1, Head: "abcdef123", Phase: state.PhaseReviewing,
			FiredAt: &asked, CommandID: 42,
			PostedCommands: []state.PostedCommand{{ID: 42, Bot: "coderabbitai[bot]", At: asked}},
		},
	}}
	cfg := FleetConfig{Reviewers: []ReviewerCfg{
		{Login: "macroscope[bot]", Name: "macroscope", Primary: true, Metered: true},
		{Login: "coderabbitai[bot]", Name: "coderabbit", Metered: true},
	}}
	running := []BotName{{Login: "macroscope[bot]", Name: "macroscope", Primary: true}}

	cards := botCards(st, cfg, func(string) []BotName { return running }, now)
	var current, retired BotCard
	for _, card := range cards {
		switch card.Login {
		case "macroscope[bot]":
			current = card
		case "coderabbitai[bot]":
			retired = card
		}
	}
	if current.LastAsked != nil {
		t.Errorf("new primary was credited with the retired primary's request: %+v", current)
	}
	if retired.LastAsked == nil || !retired.LastAsked.Equal(asked) {
		t.Errorf("retired primary lost the request addressed to it: %+v", retired)
	}
}

func TestBotCardsUseTheEffectiveFleetPrimary(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := FleetConfig{Reviewers: []ReviewerCfg{
		{Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true, Metered: true},
		{Login: "cursor[bot]", Name: "bugbot"},
	}}
	running := []BotName{
		{Login: "cursor[bot]", Name: "bugbot", Primary: true, Required: true},
	}
	cards := botCards(state.State{}, cfg, func(string) []BotName { return running }, now)
	primary := ""
	for _, card := range cards {
		if card.Primary {
			if primary != "" {
				t.Fatalf("multiple primary cards: %q and %q", primary, card.Login)
			}
			primary = card.Login
		}
		if card.Login == "coderabbitai[bot]" && card.Primary {
			t.Fatal("the retired startup primary is still marked primary")
		}
	}
	if primary != "cursor[bot]" {
		t.Fatalf("primary card = %q, want the effective fleet primary", primary)
	}
}

func TestBotCardsRetainConfiguredPrimaryWhenEffectiveSetIsUnavailable(t *testing.T) {
	cfg := FleetConfig{Reviewers: []ReviewerCfg{{
		Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true, Metered: true,
	}}}
	cards := botCards(state.State{}, cfg, func(string) []BotName { return nil }, time.Now())
	for _, card := range cards {
		if card.Login == "coderabbitai[bot]" {
			if !card.Primary || !card.Metered {
				t.Fatalf("configured fallback primary = %+v", card)
			}
			return
		}
	}
	t.Fatal("no card for the configured primary")
}

func TestBotCardsUseEffectiveReviewerMetadata(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := FleetConfig{Reviewers: []ReviewerCfg{{
		Login: "cursor[bot]", Name: "bugbot", Command: "old command",
		Trigger: "never", Grace: Dur(time.Minute),
	}}}
	running := []BotName{{
		Login: "cursor[bot]", Name: "bugbot", Command: "new command",
		Trigger: "always", Grace: Dur(7 * time.Minute),
	}}
	cards := botCards(state.State{}, cfg, func(string) []BotName { return running }, now)
	for _, card := range cards {
		if card.Login != "cursor[bot]" {
			continue
		}
		if card.Command != "new command" || card.Trigger != "always" || card.Grace != Dur(7*time.Minute) {
			t.Fatalf("card = %+v, want effective state-resolved metadata", card)
		}
		return
	}
	t.Fatal("no card for the effective reviewer")
}

func TestBotCardsDistinguishRegistryReviewersFromCustomGates(t *testing.T) {
	cfg := FleetConfig{Reviewers: []ReviewerCfg{
		{Login: "cursor[bot]", Name: "bugbot"},
		{Login: "sonar[bot]", Name: "sonar", Required: true},
	}}
	cards := botCards(state.State{}, cfg, func(string) []BotName { return nil }, time.Now())
	got := map[string]bool{}
	for _, card := range cards {
		got[card.Login] = card.Configurable
	}
	if !got["cursor[bot]"] {
		t.Error("registry co-reviewer is not marked configurable")
	}
	if got["sonar[bot]"] {
		t.Error("custom required login is marked as a triggerable registry co-reviewer")
	}
}

func TestBotCardsIncludeRepositoryOnlyReviewers(t *testing.T) {
	st := state.New()
	st.Repos = map[string]state.RepoReviewers{}
	st.Repos["o/special"] = state.RepoReviewers{
		Required:    []string{"cursor[bot]"},
		SetRequired: true,
	}
	botsFor := func(repo string) []BotName {
		if repo == "o/special" {
			return []BotName{{
				Login: "cursor[bot]", Name: "bugbot", Required: true,
				Command: "bugbot run", Trigger: "always", Grace: Dur(time.Minute),
			}}
		}
		return nil
	}

	for _, card := range botCards(st, FleetConfig{}, botsFor, time.Now()) {
		if card.Login != "cursor[bot]" {
			continue
		}
		if !card.Enabled || !card.Required || card.Status != "unverified" || card.RepoCount != 1 {
			t.Fatalf("repository-only reviewer card = %+v", card)
		}
		if card.Command != "bugbot run" || card.Trigger != "always" || card.Grace != Dur(time.Minute) {
			t.Fatalf("repository-only reviewer lost effective metadata: %+v", card)
		}
		return
	}
	t.Fatal("no card for the repository-only reviewer")
}

func TestBotCardsCountEffectiveReviewersInheritedByAnOverride(t *testing.T) {
	st := state.New()
	st.Repos = map[string]state.RepoReviewers{
		"o/required-only": {
			Required: []string{"coderabbitai[bot]"}, SetRequired: true,
		},
	}
	botsFor := func(string) []BotName {
		return []BotName{
			{Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true, Required: true},
			{Login: "cursor[bot]", Name: "bugbot"},
		}
	}

	for _, card := range botCards(st, FleetConfig{}, botsFor, time.Now()) {
		if card.Login == "cursor[bot]" {
			if card.RepoCount != 1 {
				t.Fatalf("inherited reviewer repo count = %d, want 1", card.RepoCount)
			}
			return
		}
	}
	t.Fatal("no card for inherited reviewer")
}

func TestCoOnlyRoundLeavesPrimaryPending(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	marks := botMarks(state.Round{FiredAt: &at, CommandID: 42, CoOnly: true}, []BotName{{
		Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true,
	}})
	if len(marks) != 1 || marks[0].Mark != "pending" {
		t.Fatalf("marks = %+v, want the uncommanded primary pending", marks)
	}
}

func TestObservationKeyIncludesEffectiveReviewers(t *testing.T) {
	base := []BotName{{Login: "coderabbitai[bot]", Primary: true, Required: true}}
	changed := []BotName{
		{Login: "coderabbitai[bot]", Primary: true, Required: true},
		{Login: "cursor[bot]", Required: true, Trigger: "always", Command: "bugbot run"},
	}
	if observationKey("o/r", 1, "abcdef123", "state", base) ==
		observationKey("o/r", 1, "abcdef123", "state", changed) {
		t.Fatal("reviewer change reused the old observation cache key")
	}
}

func TestObservationStateKeyIgnoresUnrelatedFleetRevisions(t *testing.T) {
	bots := []BotName{{Login: "coderabbitai[bot]", Primary: true, Required: true}}
	before := state.New()
	before.Rev = 7
	after := state.New()
	after.Rev = 8
	if observationKey("o/r", 1, "abcdef123", observationStateKey(before, "o/r", 1), bots) !=
		observationKey("o/r", 1, "abcdef123", observationStateKey(after, "o/r", 1), bots) {
		t.Fatal("an unrelated fleet revision invalidated the PR observation cache")
	}
}

func TestObservationStateKeyIncludesRoundState(t *testing.T) {
	queued := state.New()
	queued.Rounds[state.Key("o/r", 1)] = state.Round{
		Repo: "o/r", PR: 1, Head: "abcdef123", Phase: state.PhaseQueued,
	}
	completed := state.New()
	completed.Rounds[state.Key("o/r", 1)] = state.Round{
		Repo: "o/r", PR: 1, Head: "abcdef123", Phase: state.PhaseCompleted,
	}
	if observationStateKey(queued, "o/r", 1) == observationStateKey(completed, "o/r", 1) {
		t.Fatal("round-state change reused the old observation cache key")
	}
}

func TestPRObservationCacheSurvivesAnUnrelatedStateRevision(t *testing.T) {
	now := time.Now().UTC()
	observer := &sequenceObserver{observations: []Observation{{
		Head: "abcdef123", CheckedAt: now,
	}}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Observer: observer})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rev = 7
	srv.lastState.Rounds[state.Key("o/r", 1)] = state.Round{
		Repo: "o/r", PR: 1, Head: "abcdef123", Phase: state.PhaseCompleted,
	}

	request := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
		req.Header.Set("X-CRQ-Dashboard", "1")
		req.SetPathValue("owner", "o")
		req.SetPathValue("name", "r")
		req.SetPathValue("pr", "1")
		srv.handlePR(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
		}
	}
	request()
	srv.lastState.Rev++
	request()
	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want the unrelated revision to reuse the cached result", observer.calls)
	}
}

func TestCostKeyIncludesReviewerPricingPolicy(t *testing.T) {
	base := []BotName{{Login: "macroscope-app[bot]", Trigger: "selfheal"}}
	always := []BotName{{Login: "macroscope-app[bot]", Trigger: "always"}}
	if costKey("o/r", 1, "abcdef123", base, nil) ==
		costKey("o/r", 1, "abcdef123", always, nil) {
		t.Fatal("trigger policy change reused the old cost cache key")
	}
	primary := []BotName{{Login: "macroscope-app[bot]", Trigger: "selfheal", Primary: true}}
	if costKey("o/r", 1, "abcdef123", base, nil) ==
		costKey("o/r", 1, "abcdef123", primary, nil) {
		t.Fatal("reviewer role change reused the old cost cache key")
	}
}

func TestPRObservationCacheRejectsAnObservationForAnotherHead(t *testing.T) {
	now := time.Now().UTC()
	observer := &sequenceObserver{observations: []Observation{
		{Head: "bbbbbbbbb", CheckedAt: now},
		{Head: "ccccccccc", CheckedAt: now.Add(time.Second)},
	}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Observer: observer})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rounds = map[string]state.Round{
		"o/r#1": {Repo: "o/r", PR: 1, Head: "aaaaaaaaa", Phase: state.PhaseCompleted},
	}

	request := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
		req.Header.Set("X-CRQ-Dashboard", "1")
		req.SetPathValue("owner", "o")
		req.SetPathValue("name", "r")
		req.SetPathValue("pr", "1")
		srv.handlePR(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
		}
	}
	request()
	request()
	if observer.calls != 2 {
		t.Fatalf("observer calls = %d, want a refetch because the cached observation belonged to another head", observer.calls)
	}
}

func TestPROmitsObservationThatRacedAheadOfState(t *testing.T) {
	now := time.Now().UTC()
	observer := &sequenceObserver{observations: []Observation{
		{Head: "bbbbbbbbb", CheckedAt: now},
		{Head: "ccccccccc", CheckedAt: now.Add(time.Second)},
	}}
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Observer: observer})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rounds = map[string]state.Round{
		"o/r#1": {Repo: "o/r", PR: 1, Head: "aaaaaaaaa", Phase: state.PhaseCompleted},
	}

	for range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
		req.Header.Set("X-CRQ-Dashboard", "1")
		req.SetPathValue("owner", "o")
		req.SetPathValue("name", "r")
		req.SetPathValue("pr", "1")
		srv.handlePR(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
		}
		var view PRView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Observed != nil || !strings.Contains(view.ObserveError, "moved") {
			t.Fatalf("observed = %+v error = %q, want mixed-head observation omitted", view.Observed, view.ObserveError)
		}
	}
	if observer.calls != 2 {
		t.Fatalf("observer calls = %d, want raced observations never cached", observer.calls)
	}
}

func TestPRDoesNotPriceWithoutAnObservedHead(t *testing.T) {
	observer := &failingObserver{}
	coster := &countingCoster{cost: Cost{Head: "bbbbbbbbb"}}
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777", Observer: observer, Coster: coster,
	})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rounds = map[string]state.Round{
		"o/r#1": {Repo: "o/r", PR: 1, Head: "aaaaaaaaa", Phase: state.PhaseCompleted},
	}

	request := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
		req.Header.Set("X-CRQ-Dashboard", "1")
		req.SetPathValue("owner", "o")
		req.SetPathValue("name", "r")
		req.SetPathValue("pr", "1")
		srv.handlePR(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
		}
	}
	request()
	request()
	if coster.calls != 0 {
		t.Fatalf("cost calls = %d, want pricing skipped when no observed head and diff are available", coster.calls)
	}
}

func TestPRRejectsCostForADifferentObservedHead(t *testing.T) {
	now := time.Now().UTC()
	observer := &sequenceObserver{observations: []Observation{{Head: "aaaaaaaaa", CheckedAt: now}}}
	coster := &countingCoster{cost: Cost{Head: "bbbbbbbbb"}}
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777", Observer: observer, Coster: coster,
	})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rounds = map[string]state.Round{
		"o/r#1": {Repo: "o/r", PR: 1, Head: "aaaaaaaaa", Phase: state.PhaseCompleted},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
	}
	var view PRView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Cost != nil || !strings.Contains(view.CostError, "moved") {
		t.Fatalf("cost = %+v error = %q, want the mixed-head estimate omitted", view.Cost, view.CostError)
	}
}

func TestPRAcceptsCostForTheFullObservedHead(t *testing.T) {
	now := time.Now().UTC()
	observer := &sequenceObserver{observations: []Observation{{Head: "abcdef123", CheckedAt: now}}}
	coster := &countingCoster{cost: Cost{Head: "abcdef1234567890"}}
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777", Observer: observer, Coster: coster,
	})
	srv.loaded = true
	srv.lastState = state.New()
	srv.lastState.Rounds = map[string]state.Round{
		"o/r#1": {Repo: "o/r", PR: 1, Head: "abcdef123", Phase: state.PhaseCompleted},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1", nil)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PR read = %d: %s", rec.Code, rec.Body.String())
	}
	var view PRView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Cost == nil || view.CostError != "" {
		t.Fatalf("cost = %+v error = %q, want prefix-equivalent heads accepted", view.Cost, view.CostError)
	}
}

func TestPRStateOnlyReadSkipsGitHubObservationAndPricing(t *testing.T) {
	observer := &countingObserver{}
	coster := &countingCoster{}
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777", Observer: observer, Coster: coster,
	})
	srv.loaded = true
	srv.lastState = state.New()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:7777/api/pr/o/r/1?state_only=1", nil)
	req.Header.Set("X-CRQ-Dashboard", "1")
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PR state read = %d: %s", rec.Code, rec.Body.String())
	}
	if observer.calls != 0 || coster.calls != 0 {
		t.Fatalf("state-only read made %d observation and %d cost calls", observer.calls, coster.calls)
	}
}
