package state

import (
	"strings"
	"testing"
	"time"
)

const nothingQueued = "_Nothing queued._"

func ptime(t time.Time) *time.Time { return &t }

// coolingRound is the shape that used to vanish from the dashboard: a waiting
// round whose own RetryAt has not passed yet.
func coolingRound(repo string, pr int, seq int64, now time.Time, retryIn time.Duration) Round {
	return Round{
		Repo:       repo,
		PR:         pr,
		Head:       "a9c688a1c",
		Seq:        seq,
		Phase:      PhaseAwaitingRetry,
		Attempts:   1,
		EnqueuedAt: now.Add(-10 * time.Minute),
		FiredAt:    ptime(now.Add(-9 * time.Minute)),
		RetryAt:    ptime(now.Add(retryIn)),
		ByHost:     "cachyos",
	}
}

func queuedRound(repo string, pr int, seq int64, now time.Time) Round {
	return Round{
		Repo:       repo,
		PR:         pr,
		Head:       "beefbeef1",
		Seq:        seq,
		Phase:      PhaseQueued,
		EnqueuedAt: now.Add(-time.Minute),
		ByHost:     "cachyos",
	}
}

func stateWith(rounds ...Round) State {
	st := New()
	for _, r := range rounds {
		st.PutRound(r)
	}
	return st
}

// queueSection returns just the "## ⏳ Queue" section of a rendered dashboard,
// so a match cannot come from the in-flight table or the requested history.
func queueSection(t *testing.T, out string) string {
	t.Helper()
	_, after, ok := strings.Cut(out, "## ⏳ Queue")
	if !ok {
		t.Fatalf("no queue section:\n%s", out)
	}
	before, _, _ := strings.Cut(after, "\n## ")
	return before
}

// A queue whose rounds are all cooling down must not render as an empty queue:
// that is the bug this design exists to prevent (a reader concluded their
// enqueue had been dropped).
func TestRenderDashboardCoolingDownOnly(t *testing.T) {
	now := time.Now().UTC()
	a := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	b := coolingRound("kristofferr/ha-adjustable-bed", 481, 2, now, 12*time.Minute)
	st := stateWith(a, b)

	out := RenderDashboard(st, StoreConfig{})
	if strings.Contains(out, nothingQueued) {
		t.Fatalf("cooling-down rounds rendered as an empty queue:\n%s", out)
	}
	if !strings.Contains(out, "## ⏳ Queue — 2 waiting") {
		t.Errorf("queue heading does not count cooling-down rounds:\n%s", out)
	}
	q := queueSection(t, out)
	for _, want := range []string{
		"kristofferr/ha-adjustable-bed#480",
		"https://github.com/kristofferr/ha-adjustable-bed/pull/480",
		"`a9c688a1c`",
		fmtStamp(a.RetryAt, time.UTC), // absolute ready time, not a relative "in 11m"
		WaitCoolingDown,
		"`cachyos`",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("queue section missing %q:\n%s", want, q)
		}
	}
	// The header must say when the front of the queue opens.
	if !strings.Contains(out, "2 queued — next at "+fmtStamp(a.RetryAt, time.UTC)) {
		t.Errorf("header does not report the next ready time:\n%s", out)
	}

	if got, want := RenderTitle(st, StoreConfig{}), "🐰 crq — 2 queued"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

// The pacing floor is a fleet setting, and this issue is written by whichever
// host happens to save state. Rendering it from that process's own startup
// configuration advertised a "next at" — and a queue order — that Pump, which
// resolves the setting from the record, was never going to follow.
func TestRenderDashboardPacesFromTheRecordedInterval(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("owner/repo", 7, 1, now))
	fired := now.Add(-time.Minute)
	st.LastFired = &fired
	st.Fleet.MinInterval = "2h"

	out := RenderDashboard(st, StoreConfig{MinInterval: time.Second})
	ready := fmtStamp(ptime(fired.Add(2*time.Hour)), time.UTC)
	if !strings.Contains(out, "next at "+ready) {
		t.Errorf("header does not pace from the recorded interval (want %s):\n%s", ready, out)
	}
	// And the generic layer answers for it when nothing typed does, exactly as
	// the configuration resolves it.
	st.Fleet.MinInterval = ""
	st.Fleet.Env = map[string]string{"CRQ_MIN_INTERVAL": "30m"}
	out = RenderDashboard(st, StoreConfig{MinInterval: time.Second})
	ready = fmtStamp(ptime(fired.Add(30*time.Minute)), time.UTC)
	if !strings.Contains(out, "next at "+ready) {
		t.Errorf("header ignores the fleet's env layer (want %s):\n%s", ready, out)
	}
}

// Guard against over-correcting: a genuinely empty state keeps its empty-state
// text and its idle title.
func TestRenderDashboardEmpty(t *testing.T) {
	st := New()

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, nothingQueued) {
		t.Errorf("empty state lost its empty-state text:\n%s", out)
	}
	if !strings.Contains(out, "## 🔬 In flight — 0\n\n_None._") {
		t.Errorf("empty state lost its empty in-flight section:\n%s", out)
	}
	if got, want := RenderTitle(st, StoreConfig{}), "🐰 crq — idle"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

func TestExpiredRoundDoesNotRenderAsInFlight(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(Round{
		Repo: "owner/repo", PR: 12, Head: "abcdef123", Phase: PhaseExpired,
		EnqueuedAt: now.Add(-time.Hour), WaitDeadline: ptime(now.Add(-time.Minute)),
	})

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, "## 🔬 In flight — 0\n\n_None._") {
		t.Fatalf("expired round still rendered in flight:\n%s", out)
	}
	if got := RenderTitle(st, StoreConfig{}); got != "🐰 crq — idle" {
		t.Fatalf("expired terminal marker title = %q, want idle", got)
	}
}

// The queue is ordered the way rounds will actually fire: ready rounds first
// (by Seq), then by when each window opens — regardless of Seq.
func TestQueueOrdersByReadyThenSeq(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		coolingRound("kristofferr/a", 1, 1, now, 20*time.Minute), // lowest Seq, latest window
		coolingRound("kristofferr/b", 2, 2, now, 5*time.Minute),
		queuedRound("kristofferr/c", 3, 3, now),                  // ready now, highest Seq
		coolingRound("kristofferr/d", 4, 4, now, -2*time.Minute), // window already open
	)

	q := st.Queue(now, 0)
	var got []int
	for _, e := range q {
		got = append(got, e.PR)
	}
	// Ready now: c (Seq 3) and d (Seq 4, elapsed RetryAt) — Seq order among them.
	// Then b (+5m), then a (+20m).
	want := []int{3, 4, 2, 1}
	if len(got) != len(want) {
		t.Fatalf("Queue returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Queue order = %v, want %v", got, want)
		}
	}
	// That d (an ELAPSED RetryAt) sorts among the ready rounds rather than after
	// the cooling ones is the point: an expired window is not a wait.
	//
	// Only the front reports its own gate; everything behind it reports being
	// behind, because when it starts depends on when the front finishes.
	if q[0].Why != "" || !q[0].ReadyAt.IsZero() {
		t.Errorf("the front is ready now: %+v", q[0])
	}
	for i := 1; i < len(q); i++ {
		if !q[i].ReadyAt.IsZero() {
			t.Errorf("q[%d] must carry no time, got %v", i, q[i].ReadyAt)
		}
	}
	// The ready follower has nothing of its own holding it, so it is purely behind.
	if q[1].Why != WaitBehind {
		t.Errorf("q[1].Why = %q, want %q", q[1].Why, WaitBehind)
	}
}

// The account-wide quota block gates firing too, so a round whose own RetryAt
// falls inside a longer block must display the block's end, not its own — the
// dashboard must not promise a time DecideFire will refuse.
func TestQueueAccountBlockDominatesRetryAt(t *testing.T) {
	now := time.Now().UTC()
	r := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	st := stateWith(r)
	blockedUntil := now.Add(44 * time.Minute)
	st.Account.BlockedUntil = &blockedUntil

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue returned %d entries, want 1", len(q))
	}
	if !q[0].ReadyAt.Equal(blockedUntil.UTC()) {
		t.Errorf("ReadyAt = %s, want the account block end %s", q[0].ReadyAt, blockedUntil.UTC())
	}
	if q[0].Why != WaitAccountBlocked {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitAccountBlocked)
	}

	out := queueSection(t, RenderDashboard(st, StoreConfig{}))
	if !strings.Contains(out, fmtStamp(&blockedUntil, time.UTC)) {
		t.Errorf("queue does not show the account-block end:\n%s", out)
	}
	if strings.Contains(out, fmtStamp(r.RetryAt, time.UTC)) {
		t.Errorf("queue shows the round's own RetryAt, which the fire gate would refuse:\n%s", out)
	}
}

// A round waiting only because another PR holds the fire slot is ready now; the
// slot is what it waits on.
func TestQueueSlotBusy(t *testing.T) {
	now := time.Now().UTC()
	holder := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseReserved, EnqueuedAt: now.Add(-2 * time.Minute),
		ReservedAt: ptime(now.Add(-time.Minute)), Token: "tok", ByHost: "cachyos",
	}
	st := stateWith(holder, queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: Key(holder.Repo, holder.PR), Token: "tok"}

	q := st.Queue(now, 0)
	if len(q) != 1 || q[0].PR != 2 {
		t.Fatalf("Queue = %+v, want only the queued round", q)
	}
	if q[0].Why != WaitSlotBusy {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitSlotBusy)
	}
}

// In flight (reserved/fired/reviewing) and Queue (queued/awaiting_retry)
// partition Active(), so every active round is on the dashboard exactly once.
// The old single "Feedback wait" row hid every reviewing round past the first
// and any reserved round whose fire slot had been cleared.
func TestRenderDashboardPartitionsActiveRounds(t *testing.T) {
	now := time.Now().UTC()
	fired := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseFired, EnqueuedAt: now.Add(-5 * time.Minute),
		FiredAt: ptime(now.Add(-4 * time.Minute)), ByHost: "cachyos",
	}
	reviewing := Round{
		Repo: "kristofferr/b", PR: 2, Head: "bbbbbbbb2", Seq: 2,
		Phase: PhaseReviewing, EnqueuedAt: now.Add(-3 * time.Minute),
		FiredAt: ptime(now.Add(-2 * time.Minute)), ByHost: "cachyos",
	}
	reserved := Round{ // slot-less reserved round (FireSlot cleared by Normalize)
		Repo: "kristofferr/c", PR: 3, Head: "cccccccc3", Seq: 3,
		Phase: PhaseReserved, EnqueuedAt: now.Add(-time.Minute),
		ReservedAt: ptime(now), ByHost: "cachyos",
	}
	completed := Round{ // not active: the reviewed-head dedup marker
		Repo: "kristofferr/e", PR: 5, Head: "eeeeeeee5", Seq: 5,
		Phase: PhaseCompleted, EnqueuedAt: now.Add(-time.Hour), ByHost: "cachyos",
	}
	st := stateWith(fired, reviewing, reserved, completed,
		coolingRound("kristofferr/d", 4, 4, now, 30*time.Minute),
		queuedRound("kristofferr/f", 6, 6, now))

	out := RenderDashboard(st, StoreConfig{})
	// The "Recently requested" history lists fired rounds regardless of phase, so
	// it must not count as accounting for a live round.
	live, _, _ := strings.Cut(out, "## 📨 Recently requested")
	inFlight, queue, ok := strings.Cut(live, "## ⏳ Queue")
	if !ok {
		t.Fatalf("no queue section:\n%s", out)
	}
	for _, r := range st.Rounds {
		key := Key(r.Repo, r.PR)
		inA, inB := strings.Contains(inFlight, key), strings.Contains(queue, key)
		if !r.Active() {
			if inA || inB {
				t.Errorf("inactive round %s rendered as live work", key)
			}
			continue
		}
		if inA == inB {
			t.Errorf("active round %s is in %d sections, want exactly 1:\n%s", key, btoi(inA)+btoi(inB), live)
		}
	}
	if !strings.Contains(out, "## 🔬 In flight — 3") {
		t.Errorf("want 3 in-flight rounds:\n%s", out)
	}
	if !strings.Contains(out, "## ⏳ Queue — 2 waiting") {
		t.Errorf("want 2 waiting rounds:\n%s", out)
	}
}

func TestRenderDashboardShowsHeldRound(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/held", 7, 1, now),
		queuedRound("kristofferr/ready", 8, 2, now),
	)
	st.Hold("kristofferr/held", 7, "waiting on a decision", "operator", now)

	out := RenderDashboard(st, StoreConfig{})
	if strings.Contains(out, "### 🟢 Idle") {
		t.Errorf("held work rendered as idle:\n%s", out)
	}
	if !strings.Contains(out, "## ⏸ Held — 1") ||
		!strings.Contains(out, "kristofferr/held#7") ||
		!strings.Contains(out, "waiting on a decision") {
		t.Errorf("held round or reason missing from dashboard:\n%s", out)
	}
	line := StatusLine(st, StoreConfig{})
	if !strings.Contains(line, "next #8") || !strings.Contains(line, "1 held") {
		t.Errorf("mixed ready and held work missing from status line: %q", line)
	}
	heldOnly := stateWith(queuedRound("kristofferr/held", 7, 1, now))
	heldOnly.Hold("kristofferr/held", 7, "waiting on a decision", "operator", now)
	if title := RenderTitle(heldOnly, StoreConfig{}); !strings.Contains(title, "1 held") {
		t.Errorf("held-only work missing from dashboard title: %q", title)
	}
	if line := StatusLine(heldOnly, StoreConfig{}); !strings.Contains(line, "1 held") {
		t.Errorf("held-only work missing from status line: %q", line)
	}

	retrying := stateWith(coolingRound("kristofferr/retrying", 9, 1, now, time.Hour))
	retrying.Hold("kristofferr/retrying", 9, "paused retry", "operator", now)
	retryingDashboard := RenderDashboard(retrying, StoreConfig{})
	if strings.Contains(retryingDashboard, "### 🟢 Idle") ||
		!strings.Contains(retryingDashboard, "## ⏸ Held — 1") ||
		!strings.Contains(retryingDashboard, "awaiting_retry") {
		t.Errorf("held awaiting-retry round missing from dashboard:\n%s", retryingDashboard)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The in-flight table carries each round's co-reviewer trigger marks.
func TestRenderDashboardInFlightTriggers(t *testing.T) {
	now := time.Now().UTC()
	r := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseReviewing, EnqueuedAt: now.Add(-3 * time.Minute),
		FiredAt: ptime(now.Add(-2 * time.Minute)), WaitDeadline: ptime(now.Add(18 * time.Minute)),
		ByHost: "cachyos",
	}
	r.SetCoCommand("chatgpt-codex-connector", 42, now.Add(-2*time.Minute))
	st := stateWith(r)

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, "chatgpt-codex-connector ✓") {
		t.Errorf("in-flight row missing trigger marks:\n%s", out)
	}
	if !strings.Contains(out, fmtStamp(r.WaitDeadline, time.UTC)) {
		t.Errorf("in-flight row missing the wait deadline:\n%s", out)
	}
}

// The ready column honours CRQ_TZ via the shared fmtStamp helper.
func TestRenderDashboardQueueHonoursTimezone(t *testing.T) {
	now := time.Now().UTC()
	r := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	st := stateWith(r)

	loc, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	out := RenderDashboard(st, StoreConfig{Timezone: "Europe/Oslo"})
	if !strings.Contains(out, fmtStamp(r.RetryAt, loc)) {
		t.Errorf("ready time not rendered in Europe/Oslo:\n%s", out)
	}
}

// The dashboard's "ready" column is a promise about when firing will accept a
// round, so every gate DecideFire applies has to be reflected. These three cases
// each rendered "ready: now" for a round the fire gate would have refused.

// Pacing: after a fire, DecideFire rejects everything until LastFired +
// MinInterval. Excluding it as "sub-minute churn" was wrong twice over — it is an
// absolute boundary, so it does not churn between renders, and CRQ_MIN_INTERVAL
// can be configured far beyond 90s.
func TestQueueReflectsThePacingGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	fired := now.Add(-30 * time.Second)
	st.LastFired = &fired

	q := st.Queue(now, 90*time.Second)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if want := fired.Add(90 * time.Second); !q[0].ReadyAt.Equal(want) {
		t.Errorf("ReadyAt = %v, want the pacing boundary %s", q[0].ReadyAt, want)
	}
	if q[0].Why != WaitPacing {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitPacing)
	}

	// Once the interval has elapsed it stops being a gate at all.
	if q := st.Queue(now.Add(2*time.Minute), 90*time.Second); len(q) != 1 || !q[0].ReadyAt.IsZero() {
		t.Errorf("an elapsed interval must not gate: %+v", q)
	}
}

// A slot held by another PR has no knowable release time. Leaving ReadyAt empty
// rendered as "now", which is the one thing that is definitely false.
func TestQueueDoesNotClaimSlotBlockedRoundsAreReadyNow(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/b#2", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase = PhaseFired
		r.Token = "tok"
		return r
	}()

	for _, e := range st.Queue(now, 0) {
		if e.Why != WaitSlotBusy {
			continue
		}
		if !e.ReadyAt.IsZero() {
			t.Errorf("a slot-blocked entry must not carry a ready time, got %s", e.ReadyAt)
		}
	}
}

// A reserved round has not posted its command, so no feedback can be coming —
// and with no FireSlot behind it, Pump cannot advance it either. Reporting a
// feedback wait sent the reader looking for a review nobody requested.
func TestRenderNamesAStrandedReservation(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase = PhaseReserved
		reserved := now.Add(-time.Minute)
		r.ReservedAt = &reserved
		return r
	}()

	got := RenderDashboard(st, StoreConfig{})
	if strings.Contains(got, "Awaiting feedback") {
		t.Error("a reserved round with no fire slot is not a feedback wait")
	}
	if !strings.Contains(got, "Stranded reservation") {
		t.Errorf("the stranded reservation must be named, got:\n%s", got)
	}
}

// Gates compose: the ready time is the latest of them, and the reason is
// whichever one binds. Taking the first that matched let a short cooldown hide a
// long account block, advertising a time firing would refuse for hours.
func TestQueueTakesTheLatestBindingGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(coolingRound("kristofferr/a", 1, 1, now, time.Minute))
	blocked := now.Add(2 * time.Hour)
	st.Account.BlockedUntil = &blocked

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if !q[0].ReadyAt.Equal(blocked.UTC()) {
		t.Errorf("ReadyAt = %v, want the dominating account block %s", q[0].ReadyAt, blocked.UTC())
	}
	if q[0].Why != WaitAccountBlocked {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitAccountBlocked)
	}

	// And the other way round: a pacing boundary beyond a short cooldown binds.
	st2 := stateWith(coolingRound("kristofferr/b", 2, 2, now, time.Second))
	fired := now
	st2.LastFired = &fired
	q2 := st2.Queue(now, 10*time.Minute)
	if len(q2) != 1 || q2[0].Why != WaitPacing || !q2[0].ReadyAt.Equal(fired.Add(10*time.Minute).UTC()) {
		t.Errorf("pacing must bind past a shorter cooldown, got %+v", q2)
	}
}

// Pacing is serial, so a round behind the front cannot start until the front
// fires — but crq does not know when that is, and a time derived from render
// time would give an unchanged state a different DashboardSHA every minute (the
// churn that argued against surfacing pacing at all). So the queue says "behind
// an earlier round" rather than naming a time it cannot know.
func TestQueueNamesRoundsBehindTheFrontWithoutInventingATime(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/a", 1, 1, now),
		queuedRound("kristofferr/b", 2, 2, now),
		queuedRound("kristofferr/c", 3, 3, now),
	)

	q := st.Queue(now, 90*time.Second)
	if len(q) != 3 {
		t.Fatalf("Queue = %d entries, want 3", len(q))
	}
	if !q[0].ReadyAt.IsZero() || q[0].Why != "" {
		t.Errorf("the front is ready now with no gate, got ready=%v why=%q", q[0].ReadyAt, q[0].Why)
	}
	for i := 1; i < len(q); i++ {
		if !q[i].ReadyAt.IsZero() {
			t.Errorf("entry %d invented a ready time %s; only absolute gates may be shown", i, q[i].ReadyAt)
		}
		if q[i].Why != WaitBehind {
			t.Errorf("entry %d Why = %q, want %q", i, q[i].Why, WaitBehind)
		}
	}

	// Stability is the point: the same state rendered a minute later must produce
	// the same queue, or every render becomes a dashboard write.
	later := st.Queue(now.Add(time.Minute), 90*time.Second)
	for i := range q {
		if !later[i].ReadyAt.Equal(q[i].ReadyAt) || later[i].Why != q[i].Why {
			t.Errorf("entry %d changed with render time: %v/%q -> %v/%q",
				i, q[i].ReadyAt, q[i].Why, later[i].ReadyAt, later[i].Why)
		}
	}
}

// Order past the front is not knowable, so the queue stops asserting it. What it
// must still get right is WHICH round is next and that the rest are not ranked:
// slot release comes from the bot acknowledging or from the in-flight timeout, so
// a round cooling now can overtake a ready one with a higher Seq.
func TestQueueRanksOnlyTheFront(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/a", 1, 1, now),                 // ready, lowest Seq
		coolingRound("kristofferr/b", 2, 2, now, 5*time.Minute), // cooling
		queuedRound("kristofferr/c", 3, 3, now),                 // ready, higher Seq
	)

	q := st.Queue(now, 90*time.Second)
	if q[0].PR != 1 {
		t.Errorf("front = %d, want the lowest-Seq ready round", q[0].PR)
	}
	if !q[0].ReadyAt.IsZero() || q[0].Why != "" {
		t.Errorf("the front is ready now, got ready=%v why=%q", q[0].ReadyAt, q[0].Why)
	}
	// Behind the front: no time, whatever the reason. A round with a gate of its
	// own still reports it; one with nothing holding it is purely behind.
	for i := 1; i < len(q); i++ {
		if !q[i].ReadyAt.IsZero() {
			t.Errorf("entry %d must be untimed, got %v", i, q[i].ReadyAt)
		}
	}
	if q[1].Why != WaitBehind {
		t.Errorf("the ready follower has nothing of its own holding it, got %q", q[1].Why)
	}
	if q[2].Why != WaitCoolingDown {
		t.Errorf("the cooling round should still say so, got %q", q[2].Why)
	}

	// And the table must not number them.
	rendered := RenderDashboard(st, StoreConfig{})
	if strings.Contains(rendered, "| 2 | [") || strings.Contains(rendered, "| 3 | [") {
		t.Errorf("only the front may carry a position:\n%s", rendered)
	}
}

// A slot held by another PR has no knowable release time, so it outranks every
// projected timestamp — otherwise a cooldown or pacing boundary is presented as a
// promise while the round cannot fire at all.
func TestQueueLetsSlotBusyDominateATimedGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(coolingRound("kristofferr/a", 1, 1, now, 5*time.Minute), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/b#2", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase, r.Token = PhaseFired, "tok"
		return r
	}()

	for _, e := range st.Queue(now, 0) {
		if e.Why != WaitSlotBusy {
			t.Errorf("entry %d: Why = %q, want %q while the slot is held", e.PR, e.Why, WaitSlotBusy)
		}
		if !e.ReadyAt.IsZero() {
			t.Errorf("entry %d: ReadyAt = %s, want no promised time", e.PR, e.ReadyAt)
		}
	}
}

// In flight is ordered by fire time, so an older reviewing round would hide a
// later stranded reservation — and that pairing is the normal shape.
func TestRenderFindsAStrandedReservationBehindAReviewingRound(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	fired := now.Add(-10 * time.Minute)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.FiredAt = PhaseReviewing, &fired
		return r
	}()
	reserved := now.Add(-time.Minute)
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()

	got := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(got, "Stranded reservation on kristofferr/b#2") {
		t.Errorf("the stranded reservation must be named even behind a reviewing round, got:\n%s", got)
	}
}

// The title is what a human sees first, so it must not contradict the body: a
// reserved round whose slot was cleared cannot be awaiting feedback.
func TestRenderTitleNamesAStrandedReservation(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	reserved := now.Add(-time.Minute)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()

	got := RenderTitle(st, StoreConfig{})
	if strings.Contains(got, "awaiting feedback") {
		t.Errorf("title claims a feedback wait for a stranded reservation: %q", got)
	}
	if !strings.Contains(got, "stranded") {
		t.Errorf("title = %q, want it to name the stranded reservation", got)
	}
}

// An account block that outlasts a cooldown makes both rounds eligible the moment
// it clears, and NextEligible then takes the lowest Seq. Simulating from render
// time instead put the ready higher-Seq round first.
func TestQueueOrdersFromWhenFiringResumes(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		coolingRound("kristofferr/a", 1, 1, now, time.Hour), // lower Seq, cooling
		queuedRound("kristofferr/b", 2, 2, now),             // ready now, higher Seq
	)
	blocked := now.Add(2 * time.Hour)
	st.Account.BlockedUntil = &blocked

	q := st.Queue(now, 90*time.Second)
	if len(q) != 2 {
		t.Fatalf("Queue = %d entries, want 2", len(q))
	}
	if q[0].PR != 1 {
		t.Errorf("Queue order = [%d %d], want [1 2]: both are eligible when the block clears, so Seq decides",
			q[0].PR, q[1].PR)
	}
}

// The model clears the ready time when a gate's end is unknowable; the renderer
// must not turn that zero back into "now" beside a "slot busy" reason.
func TestRenderShowsUnknownRatherThanNowForAnUnknowableGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/b#2", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase, r.Token = PhaseFired, "tok"
		return r
	}()

	got := RenderDashboard(st, StoreConfig{})
	if strings.Contains(got, "now | slot busy") {
		t.Errorf("a slot-blocked round must not read as ready now:\n%s", got)
	}
	if !strings.Contains(got, "unknown | slot busy") {
		t.Errorf("want an unknown ready time beside the slot-busy reason, got:\n%s", got)
	}
}

// Sharing the front's boundary is not a later time: after the front fires, pacing
// pushes the next round out by another interval, so repeating that timestamp
// advertises a moment firing will refuse.
func TestQueueDoesNotRepeatTheFrontsBoundaryForFollowers(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	blocked := now.Add(time.Hour)
	st.Account.BlockedUntil = &blocked

	q := st.Queue(now, 90*time.Second)
	if !q[0].ReadyAt.Equal(blocked.UTC()) {
		t.Errorf("the front is gated by the account block, got %v", q[0].ReadyAt)
	}
	// The follower keeps its reason — the block is real — but loses the time,
	// because when it actually starts depends on the round ahead of it.
	if !q[1].ReadyAt.IsZero() {
		t.Errorf("the follower must carry no time: ready=%v", q[1].ReadyAt)
	}
	if q[1].Why != WaitAccountBlocked {
		t.Errorf("the follower should still report why it waits, got %q", q[1].Why)
	}
}

// A stranded reservation cannot be advanced by Pump at all, so a quota window or
// another PR's review — both of which clear by themselves — must not hide it.
func TestStrandedReservationOutranksTransientStates(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	reserved := now.Add(-time.Minute)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()
	blocked := now.Add(time.Hour)
	st.Account.BlockedUntil = &blocked

	if got := RenderDashboard(st, StoreConfig{}); !strings.Contains(got, "Stranded reservation") {
		t.Errorf("an account block must not hide a stranded reservation:\n%s", got)
	}
	if got := RenderTitle(st, StoreConfig{}); !strings.Contains(got, "stranded") {
		t.Errorf("title = %q, want the stranded reservation named", got)
	}
}

// Every normal fire passes through PhaseReserved while its slot is held and the
// command is posted. Reporting that as stranded turns the happy path into a loud
// false alarm, now that stranded outranks every other state.
func TestReservedRoundHoldingTheSlotIsNotStranded(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	reserved := now.Add(-time.Second)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.ReservedAt, r.Token = PhaseReserved, &reserved, "tok"
		return r
	}()
	st.FireSlot = &FireSlot{Key: "kristofferr/a#1", Token: "tok", Since: reserved}

	if got := RenderDashboard(st, StoreConfig{}); strings.Contains(got, "Stranded") {
		t.Errorf("a mid-fire reservation must not read as stranded:\n%s", got)
	}
	if got := RenderTitle(st, StoreConfig{}); strings.Contains(got, "stranded") {
		t.Errorf("title = %q, want no stranded claim mid-fire", got)
	}

	// Once the slot is gone it really is stranded.
	st.FireSlot = nil
	if got := RenderDashboard(st, StoreConfig{}); !strings.Contains(got, "Stranded reservation") {
		t.Errorf("a reservation with no slot must still be named:\n%s", got)
	}
}

// A reserved round has no FiredAt yet, so ordering the in-flight table by
// enqueue time put a PR reserved seconds ago ahead of reviews fired much earlier.
func TestInFlightOrdersAReservationByWhenItReserved(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/old", 1, 1, now), queuedRound("kristofferr/new", 2, 2, now))
	firedLongAgo := now.Add(-time.Hour)
	st.Rounds["kristofferr/old#1"] = func() Round {
		r := st.Rounds["kristofferr/old#1"]
		r.Phase, r.FiredAt = PhaseReviewing, &firedLongAgo
		return r
	}()
	enqueuedEvenEarlier := now.Add(-2 * time.Hour)
	justReserved := now.Add(-time.Second)
	st.Rounds["kristofferr/new#2"] = func() Round {
		r := st.Rounds["kristofferr/new#2"]
		r.Phase, r.EnqueuedAt, r.ReservedAt = PhaseReserved, enqueuedEvenEarlier, &justReserved
		return r
	}()

	got := inFlightRounds(st)
	if len(got) != 2 || got[0].PR != 1 {
		t.Errorf("in flight = %v, want the earlier-fired review first", []int{got[0].PR, got[1].PR})
	}
}

// While another PR holds the slot, which round fires next depends on whose
// cooldown has elapsed when it releases — and that moment is unknown, so every
// order including Seq is a guess. The table must not number them.
func TestQueueTableDropsPositionsWhileTheSlotIsHeld(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(coolingRound("kristofferr/a", 1, 1, now, time.Hour), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/c#3", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/c#3"] = Round{
		Repo: "kristofferr/c", PR: 3, Head: "ccccccccc", Seq: 3,
		Phase: PhaseFired, Token: "tok", EnqueuedAt: now.Add(-2 * time.Minute), FiredAt: &now,
	}

	got := RenderDashboard(st, StoreConfig{})
	if strings.Contains(got, "| 1 | [kristofferr/a") || strings.Contains(got, "| 2 | [kristofferr/b") {
		t.Errorf("positions must not be claimed while the slot is held:\n%s", got)
	}
}

// A round degraded to its co-reviewers spends no account quota, so DecideFire
// resolves it before the quota gate. Promising it cannot proceed until the window
// closes describes a wait the next observation can end immediately.
func TestQueueDoesNotBlockACoOnlyRoundOnTheAccountWindow(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.CoOnly = true
		return r
	}()
	blocked := now.Add(2 * time.Hour)
	st.Account.BlockedUntil = &blocked

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if q[0].Why == WaitAccountBlocked || !q[0].ReadyAt.IsZero() {
		t.Errorf("a co-only round must not wait on the account window: ready=%v why=%q", q[0].ReadyAt, q[0].Why)
	}
}

// A co-only round waits for nothing the queue serializes: no account quota, no
// fire slot, and DecideFire resolves it before either gate. Exempting it
// gate-by-gate missed a different spot three rounds running — the account window,
// then the ordering, then the follower pass — so this asserts the whole exemption
// at once, with every queue-wide gate active simultaneously.
func TestCoOnlyRoundIsExemptFromEveryQueueGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/metered", 1, 1, now), // lower Seq, ordinary round
		queuedRound("kristofferr/free", 2, 2, now),    // higher Seq, co-only
	)
	st.Rounds["kristofferr/free#2"] = func() Round {
		r := st.Rounds["kristofferr/free#2"]
		r.CoOnly = true
		return r
	}()
	// Every gate the queue imposes on ITS OWN behalf, at once. The fire slot is
	// deliberately not among them: it stops everything, co-only included, because
	// Pump returns as soon as it sees a holder (see
	// TestHeldSlotStopsFreeRunningRoundsToo).
	blocked := now.Add(2 * time.Hour)
	st.Account.BlockedUntil = &blocked
	fired := now
	st.LastFired = &fired

	q := st.Queue(now, 90*time.Second)
	if len(q) != 2 {
		t.Fatalf("Queue = %d entries, want 2", len(q))
	}
	free := q[0]
	if free.PR != 2 {
		t.Fatalf("the co-only round must lead: got #%d — actionable work cannot sit behind a slot it never needs", free.PR)
	}
	if !free.ReadyAt.IsZero() || free.Why != "" {
		t.Errorf("the co-only round is ready now, got ready=%v why=%q", free.ReadyAt, free.Why)
	}
	// The ordinary round is still governed by all of it.
	if q[1].Why != WaitAccountBlocked && q[1].Why != WaitPacing {
		t.Errorf("the metered round must report a queue gate, got %q", q[1].Why)
	}
}

// A retry keeps the previous attempt's FiredAt as history, and Reserve does not
// clear it — so a reserved round would print an earlier attempt's timestamp as
// though the current command had gone out. If the post then hangs, that
// misleading value is what stays on the dashboard beside a stranded reservation.
func TestInFlightPrintsNoFireTimeForAReservation(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	previous := now.Add(-3 * time.Hour)
	reserved := now.Add(-time.Second)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.FiredAt, r.ReservedAt, r.Attempts = PhaseReserved, &previous, &reserved, 1
		return r
	}()

	if at := firedTimeOf(st.Rounds["kristofferr/a#1"]); at != nil {
		t.Errorf("a reservation reports no fire time for this attempt, got %s", at)
	}
	// And a round that really did fire still reports it.
	fired := st.Rounds["kristofferr/a#1"]
	fired.Phase = PhaseFired
	if at := firedTimeOf(fired); at == nil || !at.Equal(previous) {
		t.Errorf("a fired round must still report its fire time, got %v", at)
	}
}

// A held slot stops everything, free-running rounds included — not because they
// need the slot, but because Pump returns as soon as it sees a holder, so the
// quota-free path that would advance them is never reached.
func TestHeldSlotStopsFreeRunningRoundsToo(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/free", 1, 1, now))
	st.Rounds["kristofferr/free#1"] = func() Round {
		r := st.Rounds["kristofferr/free#1"]
		r.CoOnly = true
		return r
	}()
	st.FireSlot = &FireSlot{Key: "kristofferr/other#9", Token: "tok", Since: now.Add(-time.Minute)}
	fired := now.Add(-time.Minute)
	st.Rounds["kristofferr/other#9"] = Round{
		Repo: "kristofferr/other", PR: 9, Head: "999999999", Seq: 9,
		Phase: PhaseFired, Token: "tok", EnqueuedAt: now.Add(-2 * time.Minute), FiredAt: &fired,
	}

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if q[0].Why != WaitSlotBusy {
		t.Errorf("a co-only round must report the held slot, got %q — the daemon cannot advance it either", q[0].Why)
	}
	if strings.Contains(RenderDashboard(st, StoreConfig{}), "| 1 | [kristofferr/free") {
		t.Error("nothing may be numbered while the slot is held")
	}
}

func TestOrphanedSlotHoldLeavesFreeRunningRoundsReady(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/metered", 1, 1, now),
		queuedRound("kristofferr/free", 2, 2, now),
	)
	st.Rounds["kristofferr/free#2"] = func() Round {
		r := st.Rounds["kristofferr/free#2"]
		r.CoOnly = true
		return r
	}()
	until := now.Add(time.Hour)
	st.FireSlot = &FireSlot{
		Key: "kristofferr/gone#9", Token: "old", Since: now.Add(-time.Minute),
		HoldUntil: &until,
	}
	st.FireSlotHoldUntil = &until

	q := st.Queue(now, 0)
	if len(q) != 2 {
		t.Fatalf("Queue = %d entries, want 2", len(q))
	}
	if q[0].PR != 2 || q[0].Why != "" || !q[0].ReadyAt.IsZero() {
		t.Fatalf("quota-free round = %+v, want it ready ahead of the orphaned hold", q[0])
	}
	if q[1].PR != 1 || q[1].Why != WaitSlotBusy {
		t.Fatalf("metered round = %+v, want it blocked by the orphaned hold", q[1])
	}
}

// The status line answers "is it still going?" continuously, so a session never
// has to spend a tool call and a paragraph on it. Each state must read at a
// glance, and it must never claim a next PR the queue itself will not name.
func TestStatusLine(t *testing.T) {
	now := time.Now().UTC()

	if got := StatusLine(New(), StoreConfig{}); !strings.Contains(got, "idle") {
		t.Errorf("empty state = %q, want idle", got)
	}

	ready := stateWith(queuedRound("kristofferr/a", 7, 1, now))
	if got := StatusLine(ready, StoreConfig{}); !strings.Contains(got, "next #7") {
		t.Errorf("a ready queue should name what is next, got %q", got)
	}

	// Blocked: the countdown is the useful part.
	blockedState := stateWith(queuedRound("kristofferr/a", 7, 1, now))
	until := now.Add(30 * time.Minute)
	blockedState.Account.BlockedUntil = &until
	got := StatusLine(blockedState, StoreConfig{})
	if !strings.Contains(got, "blocked") {
		t.Errorf("blocked state = %q, want the block named", got)
	}
	if strings.Contains(got, "next #") {
		t.Errorf("nothing can fire while blocked, so no next may be claimed: %q", got)
	}

	// Stranded outranks everything, as on the dashboard.
	strandedState := stateWith(queuedRound("kristofferr/a", 7, 1, now))
	reserved := now.Add(-time.Minute)
	strandedState.Rounds["kristofferr/a#7"] = func() Round {
		r := strandedState.Rounds["kristofferr/a#7"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()
	stranded := StatusLine(strandedState, StoreConfig{})
	if !strings.Contains(stranded, "stranded") {
		t.Errorf("stranded state = %q, want it named first", stranded)
	}
	// Precedence, not just presence: a stranded round outranks every state that
	// clears by itself, so none of those may share the line.
	for _, lower := range []string{"blocked", "idle", "next #", "reviewing"} {
		if strings.Contains(stranded, lower) {
			t.Errorf("stranded line %q must not also report %q", stranded, lower)
		}
	}

	// A stranded reservation with a backlog behind it: the earlier precedence
	// check only proved this when the queue happened to be empty, and "stranded
	// ... next #8" reads as though the queue is moving.
	strandedBacklog := stateWith(
		queuedRound("kristofferr/a", 7, 1, now),
		queuedRound("kristofferr/a", 8, 2, now),
	)
	strandedBacklog.Rounds["kristofferr/a#7"] = func() Round {
		r := strandedBacklog.Rounds["kristofferr/a#7"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()
	if got := StatusLine(strandedBacklog, StoreConfig{}); strings.Contains(got, "next #") {
		t.Errorf("stranded line %q must not point past the stranded round", got)
	}

	// A quota-free round stays actionable while the account window is shut — that
	// is the whole point of Queue's exemption — so the line must not call it
	// blocked and then name what is next in the same breath.
	blockedButReady := stateWith(func() Round {
		r := queuedRound("kristofferr/a", 9, 1, now)
		r.CoOnly = true
		return r
	}())
	blockedButReady.Account.BlockedUntil = &until
	line := StatusLine(blockedButReady, StoreConfig{})
	if strings.Contains(line, "blocked") && strings.Contains(line, "next #") {
		t.Errorf("status line %q says blocked and names a next PR at once", line)
	}

	// It is a status LINE: anything multi-line corrupts the bar it renders into.
	for _, line := range []string{
		StatusLine(New(), StoreConfig{}),
		StatusLine(ready, StoreConfig{}),
		StatusLine(blockedState, StoreConfig{}),
		stranded,
	} {
		if strings.ContainsAny(line, "\r\n") {
			t.Errorf("status line contains a newline: %q", line)
		}
	}
}

// The issue is ONE artifact the whole fleet reads, so how its times are rendered
// is a fleet answer. Taken from the rendering host's startup environment alone,
// a timezone saved from the settings page was reported as in force while every
// sync went on writing the zone of whichever machine happened to run it.
func TestRenderDashboardPrefersTheFleetTimezone(t *testing.T) {
	now := time.Now().UTC()
	r := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	st := stateWith(r)
	st.Fleet.Env = map[string]string{"CRQ_TZ": "Asia/Tokyo"}

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	out := RenderDashboard(st, StoreConfig{Timezone: "Europe/Oslo"})
	if !strings.Contains(out, fmtStamp(r.RetryAt, tokyo)) {
		t.Errorf("ready time not rendered in the fleet's zone:\n%s", out)
	}

	// A zone the fleet records but this binary cannot load falls through to the
	// host's own rather than silently rendering everything in UTC.
	oslo, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	st.Fleet.Env["CRQ_TZ"] = "Middle/Earth"
	out = RenderDashboard(st, StoreConfig{Timezone: "Europe/Oslo"})
	if !strings.Contains(out, fmtStamp(r.RetryAt, oslo)) {
		t.Errorf("an unloadable fleet zone must fall back to the host's:\n%s", out)
	}
}

func TestCoReviewerCellListsOnlyCoBotOverrides(t *testing.T) {
	st := New()
	st.SetRepoOverride("owner/primary-only", RepoReviewers{PrimaryOff: true})
	st.SetRepoOverride("owner/required-only", RepoReviewers{SetRequired: true})
	st.SetRepoOverride("owner/cobots", RepoReviewers{SetCoBots: true})

	got := coReviewerCell(st, "codex")
	if !strings.Contains(got, "owner/cobots") {
		t.Fatalf("co-reviewer cell omitted the co-bot override: %q", got)
	}
	for _, unrelated := range []string{"owner/primary-only", "owner/required-only"} {
		if strings.Contains(got, unrelated) {
			t.Fatalf("co-reviewer cell lists unrelated override %q: %q", unrelated, got)
		}
	}
}
