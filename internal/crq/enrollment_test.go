package crq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// TestEnrollmentPrecedence pins the order the whole feature rests on. Getting it
// wrong is not a cosmetic bug: too permissive and crq reviews a repository
// somebody deliberately kept it out of, too strict and the dashboard's Off
// button silently does nothing.
func TestEnrollmentPrecedence(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/allowed": true, "o/fought-over": true}
	cfg.ExcludeRepos = map[string]bool{"o/killed": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	// A repository named by neither list, on a host that HAS an allow-list, is
	// off — the allow-list is the whole statement of what this host looks at.
	if v, _ := svc.Enrollment(ctx, "o/unknown"); v.Enabled || v.Source != "off" {
		t.Errorf("unknown repo = %+v, want off", v)
	}
	if v, _ := svc.Enrollment(ctx, "o/allowed"); !v.Enabled || v.Source != "env" {
		t.Errorf("allowed repo = %+v, want enabled by env", v)
	}
	if v, _ := svc.Enrollment(ctx, "o/killed"); v.Enabled || v.Source != "excluded" {
		t.Errorf("excluded repo = %+v, want excluded", v)
	}
	if v, _ := svc.Enrollment(ctx, cfg.GateRepo); v.Enabled {
		t.Errorf("gate repo = %+v, want excluded: it holds crq's own state", v)
	}

	// CRQ_EXCLUDE is a per-host kill switch and shared state does not override
	// it, so the write is refused rather than recorded and ignored.
	if _, err := svc.SetEnrollment(ctx, "o/killed", true, ""); err == nil {
		t.Error("enrolling an env-excluded repo must be refused, not silently recorded")
	}

	// Turning one off is the direction that needs a reason, because it makes a
	// repository disappear from every queue.
	if _, err := svc.SetEnrollment(ctx, "o/fought-over", false, ""); err == nil {
		t.Error("removing without a reason must be refused")
	}

	// A record beats env in BOTH directions. An Off that only tells you which
	// file to edit on another machine is not a switch.
	view, err := svc.SetEnrollment(ctx, "o/fought-over", false, "archived")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.Source != "state" || !view.EnvConflict || !view.ClearEnables {
		t.Errorf("view = %+v, want off by record with the env disagreement reported", view)
	}

	// And it enrolls a repository env never mentioned — which is not a
	// conflict, it is the feature working.
	view, err = svc.SetEnrollment(ctx, "o/added", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.EnvConflict {
		t.Errorf("view = %+v, want enabled with no conflict reported", view)
	}

	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targets, scoped := svc.scanTargets(st)
	if scoped {
		t.Error("a host with an allow-list must search by repository, not owner-wide")
	}
	want := map[string]bool{"o/allowed": true, "o/added": true}
	if len(targets) != len(want) {
		t.Fatalf("scan targets = %v, want exactly %v", targets, want)
	}
	for _, repo := range targets {
		if !want[repo] {
			t.Errorf("scan targets = %v, want %v — a repository turned off must not be searched", targets, want)
		}
	}

	// default hands it back to env.
	if view, err = svc.ClearEnrollment(ctx, "o/fought-over"); err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.Source != "env" {
		t.Errorf("view = %+v, want the env answer back", view)
	}
}

func TestOutOfScopeOffRecordDoesNotClaimClearingWillEnableIt(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{}
	cfg.Scope = []string{"in-scope"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	view, err := svc.SetEnrollment(ctx, "outside/repo", false, "not reviewed here")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.ClearEnables {
		t.Fatalf("view = %+v, want clearing to remain off because the owner is outside CRQ_SCOPE", view)
	}
}

func TestSetEnrollmentHonorsFleetExcludePolicy(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "o/gate",
		"CRQ_HOST": "testhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_EXCLUDE": "o/excluded"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/excluded", true, ""); err == nil {
		t.Fatal("enrolling a repository excluded by fleet policy succeeded")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Enrollment("o/excluded"); ok {
		t.Fatal("rejected fleet-excluded enrollment persisted a record")
	}
}

func TestSetEnrollmentRejectsInvalidRepositorySlugs(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, repo := range []string{"owner/..", "owner/name?bad", "bad_owner/name", "owner/name/extra"} {
		if _, err := svc.SetEnrollment(ctx, repo, true, ""); err == nil {
			t.Errorf("SetEnrollment(%q) succeeded, want invalid slug rejected", repo)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repos := st.EnrolledRepos(); len(repos) != 0 {
		t.Fatalf("invalid enrollment records persisted: %v", repos)
	}
}

// A host with no allow-list searches its whole CRQ_SCOPE. Records must not
// narrow that to themselves, or enrolling one repository would silently stop
// every other one from being scanned.
func TestEnrollmentDoesNotNarrowAScopeWideHost(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	if v, _ := svc.Enrollment(ctx, "o/anything"); !v.Enabled || v.Source != "scope" {
		t.Errorf("view = %+v, want enabled by scope", v)
	}
	if _, err := svc.SetEnrollment(ctx, "o/one", true, ""); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if targets, scoped := svc.scanTargets(st); len(targets) != 0 || !scoped {
		t.Errorf("scan targets = %v (scoped %v), want none and scope mode so the pass still searches the whole scope", targets, scoped)
	}

	// It must still WIDEN it. A repository enrolled from outside CRQ_SCOPE is
	// reported as enrolled by every screen, and no owner-wide search can reach
	// it — so it has to be named as a target of its own or it is enrolled,
	// counted, and never enqueued.
	if _, err := svc.SetEnrollment(ctx, "elsewhere/added", true, ""); err != nil {
		t.Fatal(err)
	}
	if st, _, err = svc.store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	targets, scoped := svc.scanTargets(st)
	if !scoped {
		t.Error("an out-of-scope enrollment must not turn the scope search off")
	}
	if len(targets) != 1 || targets[0] != "elsewhere/added" {
		t.Errorf("scan targets = %v, want only the out-of-scope enrollment named", targets)
	}
	// The off direction still works there: the per-PR gate reads the record.
	if _, err := svc.SetEnrollment(ctx, "o/noisy", false, "too busy"); err != nil {
		t.Fatal(err)
	}
	st, _, err = svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if svc.reviewsRepo(st, "o/noisy") {
		t.Error("a repository turned off must not be reviewed, scope-wide host or not")
	}
	if !svc.reviewsRepo(st, "o/other") {
		t.Error("turning one repository off must not affect the rest of the scope")
	}
}

func TestEnrollmentAndScanUseFleetResolvedPolicy(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":  "owner/gate",
		"CRQ_SCOPE": "startup-owner",
		"CRQ_REPOS": "startup-owner/old",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fleet.Env = map[string]string{
			"CRQ_SCOPE":   "fleet-owner",
			"CRQ_REPOS":   "fleet-owner/new,fleet-owner/excluded",
			"CRQ_EXCLUDE": "fleet-owner/excluded",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, store, nil)
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if view := svc.enrollmentOf(st, "startup-owner/old"); view.Enabled {
		t.Fatalf("startup-only repository remained enabled: %+v", view)
	}
	if view := svc.enrollmentOf(st, "fleet-owner/excluded"); view.Enabled || view.Source != "excluded" {
		t.Fatalf("fleet exclusion was ignored: %+v", view)
	}
	if _, err := svc.SetEnrollment(ctx, "fleet-owner/excluded", true, ""); err == nil {
		t.Fatal("enrolling a fleet-excluded repository must be refused")
	}
	if st, _, err = store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Enrollment("fleet-owner/excluded"); ok {
		t.Fatal("fleet-excluded repository was persisted")
	}
	if targets, scoped := svc.scanTargets(st); scoped || len(targets) != 1 || targets[0] != "fleet-owner/new" {
		t.Fatalf("scan targets = %v scoped=%v, want only the fleet-resolved repository", targets, scoped)
	}
	enrollments, err := svc.Enrollments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, view := range enrollments {
		listed = append(listed, view.Repo)
	}
	if strings.Join(listed, ",") != "fleet-owner/excluded,fleet-owner/new" {
		t.Fatalf("enrollments = %v, want the fleet-resolved allow-list", listed)
	}
	if _, _, err := svc.ScopeRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gh.owners, ",") != "fleet-owner" {
		t.Fatalf("discovery owners = %v, want the fleet-resolved scope", gh.owners)
	}
}

// The record round-trips members a newer binary added, but only if the toggle
// EDITS it. Building a replacement from scratch — which is what every save did —
// erased them on the next CAS, so an older binary flipping a switch silently
// unset whatever setting the newer one had recorded beside it.
func TestTogglingEnrollmentKeepsANewerBinarysMembers(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// Seeded as a newer binary would have left it.
	if _, err := store.Update(ctx, func(st *State) error {
		var foreign State
		if err := json.Unmarshal([]byte(`{"v":4,"rev":1,"next_seq":1,"account":{"scope":"o"},
		  "enrolled":{"o/repo":{"enabled":true,"future_enroll_flag":{"until":"2030-01-01"}}}}`), &foreign); err != nil {
			return err
		}
		st.Enrolled = foreign.Enrolled
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollment(ctx, "o/repo", false, "paused for the quarter"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := st.Enrollment("o/repo")
	if !ok || rec.Enabled || rec.Reason != "paused for the quarter" {
		t.Fatalf("record = %+v, want the switch actually thrown", rec)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	enrolled, _ := back["enrolled"].(map[string]any)
	own, _ := enrolled["o/repo"].(map[string]any)
	carried, _ := own["future_enroll_flag"].(map[string]any)
	if carried == nil || carried["until"] != "2030-01-01" {
		t.Errorf("toggling the switch erased a member this binary does not know: %#v", own)
	}
}

// Enrolling a repository is the one click in the product that spends money, so
// the dialog's rule is that an unknown price must never read as a free one. A
// per-PR pricing call that fails — a spent REST quota, an unreadable diff — used
// to be skipped silently, and a backlog nothing could be priced for was
// summarised as having "no per-review cost".
func TestEnrollSummaryNeverPricesAnUnknownAsFree(t *testing.T) {
	none := enrollSummary(EnrollImpact{Open: 4, Eligible: 4, Unpriced: 4})
	if strings.Contains(none, "no per-review cost") {
		t.Errorf("summary = %q, want an unpriced backlog reported as unknown", none)
	}
	if !strings.Contains(none, "could not") {
		t.Errorf("summary = %q, want it to say the cost could not be read", none)
	}
	// A partly priced backlog states both: the money it knows about, and how
	// many pull requests are not in that number.
	partial := enrollSummary(EnrollImpact{Open: 4, Eligible: 4, Low: 1, High: 2, Unpriced: 2})
	if !strings.Contains(partial, "$1.00–$2.00") || !strings.Contains(partial, "2 that could not be priced") {
		t.Errorf("summary = %q, want the known cost and the unpriced count", partial)
	}
	// And a fully priced free backlog still says so.
	free := enrollSummary(EnrollImpact{Open: 2, Eligible: 2})
	if !strings.Contains(free, "no per-review cost") {
		t.Errorf("summary = %q, want a genuinely free backlog unchanged", free)
	}
}

// Turning a repository off has to remove the work already queued for it, not
// merely stop new scans finding more. Pump chooses from Rounds through
// NextEligible, which asks nothing about enrollment — so a queued round kept its
// place and spent the shared allowance on a metered review minutes after
// somebody stopped the repository.
func TestDisablingEnrollmentDropsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/stopped#7"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Enqueue(ctx, "o/stopped", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err != nil {
		t.Fatal(err)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/stopped", 7); round != nil {
		t.Fatalf("round = %+v, want it archived rather than left fire-eligible", round)
	}
	if next := st.NextEligible(svc.clock()); next != nil {
		t.Errorf("next eligible = %+v, want nothing for a repository that was turned off", next)
	}
	// The round is archived, never deleted: it says why it stopped.
	if len(st.Archive) != 1 || st.Archive[0].Phase != PhaseAbandoned ||
		!strings.Contains(st.Archive[0].Note, "turned off") {
		t.Errorf("archive = %+v, want the round kept with the reason it ended", st.Archive)
	}

	// Turning it back on enqueues the head again — an off switch somebody can
	// undo has to hand the repository back the way it found it.
	if _, err := svc.SetEnrollment(ctx, "o/stopped", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/stopped", 7); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	if round := st.Round("o/stopped", 7); round == nil || round.Phase != PhaseQueued {
		t.Errorf("round = %+v, want a fresh queued round once the repository is back on", round)
	}
}

func TestSetEnrollmentPersistsInDryRun(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	cfg.DryRun = true
	store := NewMemoryStore(cfg)
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	if _, err := store.Update(ctx, func(st *State) error {
		_, err := st.NewRound("o/stopped", 7, "abcdef123", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	view, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.Source != "state" || view.Reason != "stop reviewing" {
		t.Fatalf("dry-run enrollment = %+v, want the persisted disabled enrollment", view)
	}

	after, afterRevision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision == revision {
		t.Fatalf("dry-run did not change state revision: before=%+v after=%+v", revision, afterRevision)
	}
	if enrollment, ok := after.Enrollment("o/stopped"); !ok || enrollment.Enabled ||
		enrollment.Reason != "stop reviewing" {
		t.Fatalf("dry-run enrollment = %+v (present %v), want the explicit configuration persisted", enrollment, ok)
	}
	if after.Round("o/stopped", 7) != nil {
		t.Fatal("dry-run left the disabled repository's queued round active")
	}
}

func TestDisablingEnrollmentRefusesAnArchivedTriggerClaim(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now()
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", now)
		if err != nil {
			return err
		}
		round.ClaimCo("cursor[bot]", now)
		st.PutRound(*round)
		_, err = st.Supersede("o/stopped", 7, "fedcba987", now.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err == nil {
		t.Fatal("turning the repository off succeeded while an archived trigger claim was still posting")
	}
}

func TestDisablingEnrollmentRefusesAnArchivedPrimaryPostClaim(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", now)
		if err != nil {
			return err
		}
		if err := round.Reserve("posting", cfg.WriterID(), now); err != nil {
			return err
		}
		st.PutRound(*round)
		_, err = st.Supersede("o/stopped", 7, "fedcba987", now.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err == nil {
		t.Fatal("turning the repository off succeeded while an archived primary trigger was still posting")
	}
}

func TestExpiredArchivedTriggerClaimDoesNotBlockEnrollment(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", now.Add(-triggerClaimTTL-time.Minute))
		if err != nil {
			return err
		}
		round.ClaimCo("cursor[bot]", now.Add(-triggerClaimTTL-time.Second))
		st.PutRound(*round)
		_, err = st.Supersede("o/stopped", 7, "fedcba987", now.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err != nil {
		t.Fatalf("expired archived trigger claim still blocked enrollment: %v", err)
	}
}

func TestArchivedTriggerClaimClearsWhenItsPostFinishes(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	cfg.CoBots = []CoBotConfig{{Login: "cursor[bot]", Command: "@cursor review"}}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now()
	var claimed Round
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", now)
		if err != nil {
			return err
		}
		round.ClaimCo("cursor[bot]", now)
		st.PutRound(*round)
		claimed = *round
		_, err = st.Supersede("o/stopped", 7, "fedcba987", now.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	svc.fireCoTrigger(ctx, cfg, claimed, "cursor[bot]")
	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err != nil {
		t.Fatalf("turning the repository off after the archived poster finished: %v", err)
	}
}

func TestDisabledEnrollmentDoesNotRetryAnInflightRound(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()

	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", now.Add(-time.Hour))
		if err != nil {
			return err
		}
		round.Phase = PhaseFired
		round.FiredAt = &now
		st.PutRound(*round)
		st.FireSlot = &FireSlot{Key: QueueKey(round.Repo, round.PR), Token: round.Token, Since: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err != nil {
		t.Fatal(err)
	}

	blocked := now.Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		round := st.Round("o/stopped", 7)
		return svc.applyTransition(st, round, engine.Transition{
			Outcome: engine.OutRetry,
			Reason:  dialect.ReasonRateLimited,
			RetryAt: blocked,
			Blocked: &engine.AccountBlock{Until: blocked},
		}, now, cfg)
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/stopped", 7); round != nil {
		t.Fatalf("disabled round = %+v, want it archived instead of retryable", round)
	}
	if len(st.Archive) != 1 || st.Archive[0].Repo != "o/stopped" ||
		st.Archive[0].PR != 7 || st.Archive[0].Phase != PhaseAbandoned {
		t.Fatalf("archive = %+v, want the disabled in-flight round retained as abandoned", st.Archive)
	}
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(blocked) {
		t.Fatalf("account block = %v, want the purchased response's global quota evidence retained", st.Account.BlockedUntil)
	}
}

func TestClearedEnrollmentDoesNotRetryOutsideTheEnvAllowlist(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/other": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()

	if _, err := svc.SetEnrollment(ctx, "o/adopted", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/adopted", 7, "abcdef123", now.Add(-time.Hour))
		if err != nil {
			return err
		}
		round.Phase = PhaseFired
		round.FiredAt = &now
		st.PutRound(*round)
		st.FireSlot = &FireSlot{Key: QueueKey(round.Repo, round.PR), Token: round.Token, Since: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClearEnrollment(ctx, "o/adopted"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(ctx, func(st *State) error {
		round := st.Round("o/adopted", 7)
		return svc.applyTransition(st, round, engine.Transition{
			Outcome: engine.OutRetry,
			Reason:  "review failed",
			RetryAt: now.Add(time.Minute),
		}, now, cfg)
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/adopted", 7); round != nil {
		t.Fatalf("cleared round = %+v, want it archived instead of retryable", round)
	}
	if len(st.Archive) != 1 || st.Archive[0].Phase != PhaseAbandoned {
		t.Fatalf("archive = %+v, want the cleared in-flight round retained as abandoned", st.Archive)
	}
}

func TestDisablingEnrollmentRefusesAClaimedTriggerPost(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/stopped", 7, "abcdef123", time.Now())
		if err != nil {
			return err
		}
		round.Phase = PhaseReserved
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err == nil {
		t.Fatal("turning the repository off succeeded while its review trigger was being posted")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment, ok := st.Enrollment("o/stopped"); ok && !enrollment.Enabled {
		t.Fatal("the rejected off action still changed enrollment")
	}
}

// Clearing a record hands the repository back to this host's env, which need
// not list it: a record that said ON becomes an effective OFF without
// SetEnrollment ever being called. Pump asks Rounds, not enrollment, so the
// queued work has to go the same way it does when the switch is thrown.
func TestClearingEnrollmentIntoAnExcludingEnvDropsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	// Nonempty and WITHOUT o/adopted: the record is the only thing enrolling it.
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/adopted#3"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/adopted", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/adopted", 3); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ClearEnrollment(ctx, "o/adopted")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled {
		t.Fatalf("view = %+v, want the env's answer, which omits this repository", view)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/adopted", 3); round != nil {
		t.Errorf("round = %+v, want it archived rather than left fire-eligible", round)
	}
	if next := st.NextEligible(svc.clock()); next != nil {
		t.Errorf("next eligible = %+v, want nothing for a repository nothing enrolls now", next)
	}
}

func TestClearingEnrollmentIntoAnExcludingEnvRefusesAClaimedTrigger(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/adopted", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		round, err := st.NewRound("o/adopted", 3, "abcdef123", time.Now())
		if err != nil {
			return err
		}
		round.Phase = PhaseReserved
		st.PutRound(*round)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ClearEnrollment(ctx, "o/adopted"); err == nil {
		t.Fatal("clear succeeded while its review trigger was being posted")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment, ok := st.Enrollment("o/adopted"); !ok || !enrollment.Enabled {
		t.Fatalf("rejected clear changed enrollment: %+v (present %v)", enrollment, ok)
	}
}

// The converse: clearing a record for a repository this host's env DOES list
// leaves it enrolled, so its queued work must survive untouched.
func TestClearingEnrollmentBackIntoEnvKeepsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/listed#5"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/listed", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/listed", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClearEnrollment(ctx, "o/listed"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/listed", 5); round == nil || round.Phase != PhaseQueued {
		t.Errorf("round = %+v, want the queued round untouched: env still enrolls this repository", round)
	}
}

// An allow-list with every entry switched off is not the same as no allow-list.
// Treating both as "search CRQ_SCOPE owner-wide" made every pass walk the whole
// organisation's open-PR result set for the per-PR gate to reject each row —
// the shared REST quota spent by a host with nothing left to review.
func TestAPassWithAnAllowListButNoActiveRepositorySearchesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/elsewhere", Number: 1, Title: "t"}}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	if _, err := svc.SetEnrollment(ctx, "o/listed", false, "archived"); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if targets, scoped := svc.scanTargets(st); len(targets) != 0 || scoped {
		t.Fatalf("scan targets = %v (scoped %v), want none and NOT scope mode", targets, scoped)
	}
	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	if gh.searches != 0 {
		t.Errorf("searched %d time(s); a host with no eligible repository has nothing to look for", gh.searches)
	}
}

// The off switch abandons a repository's pending rounds, and every SCAN path
// honours it — but Enqueue is the manual path, and Pump asks nothing about
// enrollment. A `crq next` or `crq loop` run afterwards recreated the round and
// spent a metered review on a repository somebody had deliberately stopped.
func TestEnqueueRefusesARepositoryTurnedOff(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/stopped#7"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "archived"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Enqueue(ctx, "o/stopped", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Held || !strings.Contains(result.Reason, "archived") {
		t.Errorf("result = %+v, want it refused with the reason the record carries", result)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/stopped", 7); round != nil {
		t.Errorf("round = %+v, want none: a manual enqueue must not undo the off switch", round)
	}

	// A repository this host's env simply does not list is NOT turned off. A
	// manual run against one is the ordinary way `crq next` is used, and
	// refusing it would break every repository outside the fleet's allow-list.
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh.pulls["o/unlisted#8"] = pull
	other := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	if result, err := other.Enqueue(ctx, "o/unlisted", 8); err != nil || result.Held {
		t.Errorf("result = %+v, err = %v, want a manual enqueue on an unlisted repository to work", result, err)
	}
}

// The preview is a dialog somebody is waiting in front of, and each pull
// request it examines costs a head read and a review list from the same GitHub
// quota the queue runs on. Unbounded, opening it for a repository with hundreds
// of open pull requests spent hundreds of requests and left review processing
// throttled — so the examining stops, and what it stopped short of is reported
// rather than silently counted as nothing.
func TestPreviewEnrollBoundsItsPerPullRequestReads(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	const open = maxExamined + 10
	for i := 1; i <= open; i++ {
		gh.searchPRs = append(gh.searchPRs, ghapi.SearchPR{Repo: "o/busy", Number: i, Author: "kristofferR"})
		var pull ghapi.Pull
		pull.State = "open"
		pull.Number = i
		pull.Head.SHA = fmt.Sprintf("%016d", i)
		gh.pulls[fakeKey("o/busy", i)] = pull
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	impact, err := svc.PreviewEnroll(ctx, "o/busy")
	if err != nil {
		t.Fatal(err)
	}
	if impact.Open != open {
		t.Errorf("open = %d, want every open pull request still counted (the listing is one search)", impact.Open)
	}
	if impact.Eligible > maxExamined {
		t.Errorf("eligible = %d, want no more than the %d examined", impact.Eligible, maxExamined)
	}
	if impact.Unexamined != open-maxExamined {
		t.Errorf("unexamined = %d, want the %d the bound stopped short of", impact.Unexamined, open-maxExamined)
	}
	if !strings.Contains(impact.Summary, "at least") || !strings.Contains(impact.Summary, "not examined") {
		t.Errorf("summary = %q, want the counts stated as floors", impact.Summary)
	}
	// The reads themselves are what the bound exists for: an assertion on the
	// reported number alone would pass against a preview that read everything
	// and then truncated its own answer.
	if got := gh.reviewPolls(); got > maxExamined {
		t.Errorf("review reads = %d, want at most %d", got, maxExamined)
	}
}

func TestPreviewEnrollUsesTheOnePassReviewPredicate(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = buildReviewers(cfg.Bot, cfg.ReviewCommand, cfg.RequiredBots, nil, false)
	repo, pr, head := "o/campaign", 7, "abcdef1234567890"
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr, Author: "kristofferR"}}
	var pull ghapi.Pull
	pull.State, pull.Head.SHA = "open", head
	gh.pulls[fakeKey(repo, pr)] = pull
	review := ghapi.Review{CommitID: "1111111111111111", State: "COMMENTED", Body: "reviewed"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	on := true
	if _, err := svc.SetSolver(ctx, repo, SolverChange{OnePass: &on}); err != nil {
		t.Fatal(err)
	}

	impact, err := svc.PreviewEnroll(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if impact.Eligible != 0 || impact.Metered != 0 {
		t.Fatalf("one-pass preview = %+v, want the older review to consume the campaign cap", impact)
	}
}

// The preview quotes a price for a BACKLOG, and enrolling spends the account's
// included reviews down as it works through it. Pricing every pull request
// against the same unchanged count told an operator with one review left that a
// whole backlog was included — the one way this dialog can talk somebody into a
// bill it never checked for.
func TestPreviewEnrollSpendsTheAllowanceAcrossTheBacklog(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = buildReviewers(cfg.Bot, cfg.ReviewCommand, cfg.RequiredBots, nil, false)
	gh := newFakeGitHub()
	for i := 1; i <= 3; i++ {
		gh.searchPRs = append(gh.searchPRs, ghapi.SearchPR{Repo: "o/backlog", Number: i, Author: "kristofferR"})
		var pull ghapi.Pull
		pull.State = "open"
		pull.Number = i
		pull.Head.SHA = fmt.Sprintf("%016d", i)
		pull.ChangedFiles = 4
		gh.pulls[fakeKey("o/backlog", i)] = pull
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		remaining := 1
		st.Account.Remaining = &remaining
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	impact, err := svc.PreviewEnroll(ctx, "o/backlog")
	if err != nil {
		t.Fatal(err)
	}
	if impact.Eligible != 3 || impact.Metered != 3 {
		t.Fatalf("impact = %+v, want three eligible metered reviews", impact)
	}
	// One review left covers the first pull request. The other two are past the
	// allowance, and crq has not learned whether usage-based reviews are on —
	// so they are unknown rather than free.
	if impact.Unpriced != 2 {
		t.Errorf("unpriced = %d, want the two beyond the one included review", impact.Unpriced)
	}
	if !strings.Contains(impact.Summary, "could not") {
		t.Errorf("summary = %q, want it to say the price past the allowance is unread", impact.Summary)
	}
}

func TestPreviewEnrollSpendsAllowanceWhenPricingReadFails(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Reviewers = buildReviewers(cfg.Bot, cfg.ReviewCommand, cfg.RequiredBots, nil, false)
	gh := newFakeGitHub()
	for i := 1; i <= 2; i++ {
		gh.searchPRs = append(gh.searchPRs, ghapi.SearchPR{Repo: "o/backlog", Number: i, Author: "kristofferR"})
		var pull ghapi.Pull
		pull.State, pull.Number, pull.Head.SHA = "open", i, fmt.Sprintf("%016d", i)
		pull.ChangedFiles = 4
		gh.pulls[fakeKey("o/backlog", i)] = pull
	}
	// reviewNeeded reads each pull once. Fail only the second read for #1,
	// which is the pricing read after eligibility has already been established.
	gh.pullErrOnRead[fakeKey("o/backlog", 1)] = 2
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		remaining := 1
		st.Account.Remaining = &remaining
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	impact, err := NewService(cfg, gh, store, nil).PreviewEnroll(ctx, "o/backlog")
	if err != nil {
		t.Fatal(err)
	}
	if impact.Metered != 2 || impact.Unpriced != 2 {
		t.Fatalf("impact = %+v, want failed #1 to spend the allowance before #2 is priced", impact)
	}
}

func TestScopeReposUsesASentinelBeforeReportingTruncation(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"owner"}
	gh := newFakeGitHub()
	for i := 0; i < scopeRepoLimit; i++ {
		gh.ownerRepos = append(gh.ownerRepos, ghapi.Repo{FullName: fmt.Sprintf("owner/repo-%04d", i)})
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	repos, truncated, err := svc.ScopeRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != scopeRepoLimit || len(truncated) != 0 {
		t.Fatalf("repos=%d truncated=%v, want an exact-limit complete listing", len(repos), truncated)
	}

	gh.ownerRepos = append(gh.ownerRepos, ghapi.Repo{FullName: "owner/one-too-many"})
	repos, truncated, err = svc.ScopeRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != scopeRepoLimit || len(truncated) != 1 || truncated[0] != "owner" {
		t.Fatalf("repos=%d truncated=%v, want the sentinel removed and owner marked", len(repos), truncated)
	}
}

func TestSetEnrollmentAtRejectsAStalePreview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	impact, err := svc.PreviewEnroll(ctx, "o/new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetEnrollment("o/other", RepoEnrollment{Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetEnrollmentAt(ctx, "o/new", true, "", &impact.Rev); err == nil ||
		!strings.Contains(err.Error(), "preview enrollment again") {
		t.Fatalf("stale enrollment save error = %v, want preview-again refusal", err)
	}
}

func TestClearEnrollmentAtRejectsAStaleEnablingPreview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/re-enabled": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if _, err := svc.SetEnrollment(ctx, "o/re-enabled", false, "paused"); err != nil {
		t.Fatal(err)
	}
	impact, err := svc.PreviewEnroll(ctx, "o/re-enabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.CalibrationIssue++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClearEnrollmentAt(ctx, "o/re-enabled", &impact.Rev); err == nil ||
		!strings.Contains(err.Error(), "preview enrollment again") {
		t.Fatalf("stale clear error = %v, want preview-again refusal", err)
	}
	view, err := svc.Enrollment(ctx, "o/re-enabled")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || !view.ClearEnables {
		t.Fatalf("view = %+v, want the off record retained with its enabling-clear warning", view)
	}
}

func TestPreviewEnrollDoesNotSpendCodeRabbitAllowanceForRegistryPrimary(t *testing.T) {
	ctx := context.Background()
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":       "o/gate",
		"CRQ_BOT":        "chatgpt-codex-connector[bot]",
		"CRQ_REVIEW_CMD": "@codex review",
		"CRQ_COBOTS":     "",
		"CRQ_REPOS":      "o/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/repo", Number: 1, Author: "kristofferR"}}
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 1, "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	impact, err := svc.PreviewEnroll(ctx, "o/repo")
	if err != nil {
		t.Fatal(err)
	}
	if impact.Eligible != 1 || impact.Metered != 0 {
		t.Fatalf("impact = %+v, want one eligible review that spends no CodeRabbit allowance", impact)
	}
}
