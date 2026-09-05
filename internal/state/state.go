// Package state defines crq's persisted schema v6: one Round per tracked PR,
// a single global fire slot, and the CodeRabbit account quota. A Round is
// never deleted, only transitioned (or archived when superseded by a new
// head) — the invariant that makes "forgot we already requested a review at
// this head" unrepresentable. That amnesia — a rate-limited requeue deleting
// the fired marker — is what let the daemon spam `@coderabbitai review` 19
// times on one PR in a day.
package state

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Phase is a Round's position in its lifecycle. Legal transitions are owned
// by the methods on Round; everything else must go through them.
//
//	queued → reserved → fired → reviewing → completed
//	   ↑         │         │         ├───────────→ expired
//	   └─────────┘         ├─────────┴→ awaiting_retry (→ fire-eligible again)
//	 (post failed)         └→ completed (review lands while slot held)
//	 any → abandoned (PR closed, cancelled, or superseded by a new head)
type Phase string

const (
	PhaseQueued        Phase = "queued"         // waiting for a fire slot
	PhaseReserved      Phase = "reserved"       // slot held, command not yet posted
	PhaseFired         Phase = "fired"          // command posted (or adopted), review pending
	PhaseReviewing     Phase = "reviewing"      // bot acknowledged; slot released, review runs
	PhaseAwaitingRetry Phase = "awaiting_retry" // throttled or timed out; may re-fire at RetryAt
	PhaseCompleted     Phase = "completed"      // every required bot reviewed this head
	PhaseExpired       Phase = "expired"        // feedback deadline elapsed; same-head dedup marker
	PhaseAbandoned     Phase = "abandoned"      // closed, cancelled, or superseded
)

// Round is one review cycle for a repo#pr at a specific head. RetryAt is the
// per-head cooldown that survives every transition: an awaiting_retry round
// refuses to fire again before it, no matter how many daemon passes observe
// "no bot review at head" in the meantime.
type Round struct {
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	Head     string `json:"head"` // 9-char short SHA
	Seq      int64  `json:"seq"`
	Phase    Phase  `json:"phase"`
	Attempts int    `json:"attempts,omitempty"` // fire attempts for this head

	EnqueuedAt time.Time  `json:"enqueued_at"`
	ReservedAt *time.Time `json:"reserved_at,omitempty"`
	FiredAt    *time.Time `json:"fired_at,omitempty"`

	// CommandID is the review-command comment that fired this round (posted or
	// adopted). It anchors completion-reply pairing to this round.
	CommandID int64 `json:"command_id,omitempty"`

	// CodexCommandID is the `@codex review` comment crq posted/adopted for this
	// round (0 when Codex was not fired). It suppresses re-posting the Codex
	// command on a retry of the same head.
	//
	// Legacy twin of CoBots["chatgpt-codex-connector"].CommandID: the fleet
	// shares one state ref across binary versions, so Codex bookkeeping is
	// dual-written (accessors mirror into these fields, Normalize folds them
	// back into CoBots) — old binaries must keep seeing codex_command_id.
	CodexCommandID int64 `json:"codex_command_id,omitempty"`

	// CoBots is the per-round co-reviewer bookkeeping, keyed by normalized
	// login (no "[bot]" suffix). Bots other than Codex exist only here — old
	// binaries never fired them, so ignoring the map is correct for them.
	// Mutate only through Co/SetCoCommand/ClaimCo/ClearCoClaim.
	CoBots map[string]CoBotRound `json:"cobots,omitempty"`
	// Title is the pull request's own title, recorded when the round is
	// enqueued. Every queue and in-flight list showed "repo#141" and nothing
	// about what that pull request was for, and fetching a title per row would
	// be a request per row — so it is stored once, with the round that needs it.
	// Stale after a rename, which is a price worth paying for a list that says
	// what it is listing.
	Title string `json:"title,omitempty"`

	// CoOnly marks a round that reached its fired/reviewing state WITHOUT crq
	// requesting a primary review — a co-reviewer-only trigger, or a bounded
	// co-review wait. FiredAt does double duty as the evidence-floor anchor and
	// as "we asked for a review", and those two meanings diverged the moment
	// co-reviewer-only rounds existed: the dashboard's requested-reviews table
	// filled with repos crq had never asked CodeRabbit about. Anything counting
	// CodeRabbit requests must skip these.
	CoOnly bool `json:"co_only,omitempty"`

	// Dispatch is the claim held by a watcher running a fix for this round. It
	// exists so two watchers cannot both spawn a session for the same PR, and so
	// a session that dies does not hold the round forever — the claim is
	// heartbeated, and one older than DispatchTTL is free to take over.
	Dispatch *DispatchClaim `json:"dispatch,omitempty"`
	// DispatchHoldPhase/DispatchHoldRetryAt preserve the queue state hidden by a
	// live dispatch. While held, a queued round is mirrored into
	// awaiting_retry with a heartbeat-extended RetryAt. Older binaries already
	// honor that phase/window even though they do not understand Dispatch, so a
	// rolling deployment cannot fire a review for code a new watcher is fixing.
	// These are top-level round members so tolerant old binaries preserve them.
	DispatchHoldPhase   Phase      `json:"dispatch_hold_phase,omitempty"`
	DispatchHoldRetryAt *time.Time `json:"dispatch_hold_retry_at,omitempty"`
	// PostedCommands are the trigger comments crq itself WROTE for this round,
	// including the ones a retry replaced. Two things need it. A retry posts a
	// new command — the bot answered the previous one, usually with a rate-limit
	// notice, so it can never be adopted again — and without this record nothing
	// knows the old comment exists, which is why a throttled PR collects a
	// column of identical review requests. And CommandID alone cannot say who
	// wrote a comment: a round records an ADOPTED command there just the same,
	// so treating it as crq's own is how tidying would erase a person's request
	// to review.
	PostedCommands []PostedCommand `json:"posted_commands,omitempty"`
	// Dismissed maps the ID of a finding an agent explicitly accounted for to the
	// reason given. GitHub offers no way to close these: a review-body finding, a
	// review-skipped notice and an outside-diff remark all lack a review thread,
	// so `crq resolve` and `crq decline` have nothing to act on and fix-first
	// can never be satisfied — the round repeats `fix` forever and no new review is ever
	// requested for the head.
	//
	// Scoped to this round on purpose. Finding IDs are content-derived, so a
	// dismissal that outlived the head would silently swallow the same finding
	// when the next reviewer reports it again. Superseding the round drops these
	// with it, which is the same rule body findings already follow: the current
	// reviewer must report it again.
	Dismissed map[string]string `json:"dismissed,omitempty"`

	// PrimaryAnsweredAt is when crq OBSERVED the primary produce head evidence,
	// the same fact CoBots[login].AnsweredAt records for a co-reviewer.
	//
	// It cannot be derived from the phase. A required set that omits the primary
	// completes as soon as the co-reviewers answer and the primary merely
	// acknowledges the metered command, so reading a completed round as proof
	// the primary reviewed labelled a reviewer as working on the strength of its
	// own acknowledgement. FiredAt is crq's command going out and says even less.
	//
	// Not in CoBots despite being the same fact: several fire and completion
	// rules walk that map as "the co-reviewers", and the primary appearing there
	// would quietly join them.
	PrimaryAnsweredAt *time.Time `json:"primary_answered_at,omitempty"`
	// PrimaryAnsweredBy is WHICH primary produced that evidence. CRQ_BOT is a
	// setting, and a fleet that changes it leaves rounds answered by the retired
	// one behind: attributing them to whatever the reading process calls its
	// primary shows the new bot as working and the old one as silent, which is
	// the two claims exactly backwards. The identity travels as data here for
	// the same reason it does in CoBots.
	PrimaryAnsweredBy string `json:"primary_answered_by,omitempty"`
	// PrimarySettled marks a completed round reopened solely to restore a lost
	// co-reviewer gate. The primary side of that round is final: its metered
	// request may have produced only an acknowledgement because the primary was
	// not required, and its trigger comment may already have been tidied.
	PrimarySettled bool `json:"primary_settled,omitempty"`

	// RetryAt is the earliest time this head may fire again (awaiting_retry).
	RetryAt *time.Time `json:"retry_at,omitempty"`

	// CodexClaimedAt reserves the self-heal Codex post: it is CAS-set before the
	// network post so two unserialized sweepers cannot both post `@codex review`
	// for the same round. A stale claim (the poster died mid-flight) expires and
	// may be re-claimed.
	CodexClaimedAt *time.Time `json:"codex_claimed_at,omitempty"`
	// CodexCommandedAt records when crq posted this round's Codex command. It
	// can precede FiredAt (a deferred post while queued behind a rate-limit
	// window or busy slot), and Codex evidence binds from it — otherwise a
	// SHA-less Codex answer delivered before the delayed CodeRabbit fire
	// would be ignored by the completion cutoff and never re-requested.
	CodexCommandedAt *time.Time `json:"codex_commanded_at,omitempty"`

	// LastAttemptAt is the adoption cutoff: command comments older than the
	// most recent failed/abandoned attempt must not be adopted as this round's
	// fire.
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`

	// WaitDeadline bounds how long a fired/reviewing round is waited on before
	// the round is retried or surfaced as timed out.
	WaitDeadline *time.Time `json:"wait_deadline,omitempty"`

	// ReviewersChanged marks a completed round whose effective reviewer set
	// changed while its pull request was closed. Requeueing a closed PR's round
	// would hand Pump dead work ahead of every live round, so the change is
	// recorded on the marker instead: if the PR is ever reopened, enqueue reopens
	// the round rather than treating it as "this head was reviewed". Reopen
	// clears it — the reopened round answers under the current requirements.
	ReviewersChanged bool `json:"reviewers_changed,omitempty"`
	// ForceCoReviewers names newly enabled or required self-heal reviewers that
	// need one immediate trigger on an existing round. Once their command is
	// recorded, normal per-bot dedupe makes the force harmless.
	ForceCoReviewers []string `json:"force_co_reviewers,omitempty"`

	Token string `json:"token,omitempty"` // reservation token (CAS race detection)
	// ByHost identifies the PROCESS that reserved this round, in the writer form
	// "host=<name> pid=<n> run=<id>" — the key NoteWriter records capabilities under, so
	// LaggingWriters can ask whether the process driving a fire understands the
	// configuration it is firing from. The dashboard shows the machine name.
	ByHost string `json:"by_host,omitempty"`
	Note   string `json:"note,omitempty"` // human-readable reason for the last transition

	// unknown carries JSON members this binary has no field for, so a newer
	// binary's additions survive being read and rewritten here. Unexported, so
	// it is never a member itself. See tolerant.go.
	unknown unknownFields
}

// PostedCommand is one trigger comment crq posted, with the reviewer it was
// addressed to and when it landed — the two facts a later cleanup needs to
// decide whether that reviewer has read it yet.
type PostedCommand struct {
	ID  int64     `json:"id"`
	Bot string    `json:"bot,omitempty"`
	At  time.Time `json:"at,omitempty"`
}

// CoBotRound is one co-reviewer's bookkeeping inside a Round: the trigger
// comment crq posted/adopted for it, and the CAS claim that serializes the
// pending post (the per-round concurrency guard — co-reviewer fires never
// take the global FireSlot).
type CoBotRound struct {
	CommandID   int64      `json:"command_id,omitempty"`
	CommandedAt *time.Time `json:"commanded_at,omitempty"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	// SeenActiveAt is durable proof that crq has observed this reviewer act on
	// the pull request. Unlike AnsweredAt it carries across supersede, so a bot
	// whose clean output exists only as a previous head's check run remains
	// eligible for self-heal when it silently misses the next head.
	SeenActiveAt *time.Time `json:"seen_active_at,omitempty"`
	// ActivityCarried says SeenActiveAt came from another head. The timestamp
	// alone cannot prove that during a supersede race: the old-head observation
	// may be recorded on the replacement after it was enqueued.
	ActivityCarried bool `json:"activity_carried,omitempty"`
	// AnsweredAt is when crq FIRST observed this bot produce head evidence — a
	// review, a clean summary at the SHA, a completed check run.
	//
	// The three fields above are all crq's own bookkeeping: what it posted and
	// what it claimed the right to post. None of them says the bot did
	// anything, and treating them as evidence of that is how a bot nobody has
	// an account for reads as working — crq asks, records that it asked, and
	// nothing ever answers. Only this field is about the bot.
	AnsweredAt *time.Time `json:"answered_at,omitempty"`

	// unknown carries members a newer binary wrote inside this record. Round's
	// carrier cannot: it sees "cobots" as a member it knows and hands the map
	// to the ordinary decoder, which drops anything inside the values. See
	// tolerant.go.
	unknown unknownFields
}

func (c CoBotRound) empty() bool {
	return c.CommandID == 0 && c.CommandedAt == nil && c.ClaimedAt == nil &&
		c.SeenActiveAt == nil && !c.ActivityCarried && c.AnsweredAt == nil &&
		len(c.unknown) == 0
}

// codexCoBotKey is dialect.CodexBotLogin under coBotKey. The literal is
// repeated here because state stays stdlib-only; state_test pins the two in
// sync.
const codexCoBotKey = "chatgpt-codex-connector"

// coBotKey mirrors dialect.NormalizeBotName: CoBots keys carry no "[bot]"
// suffix regardless of which login spelling the caller observed.
func coBotKey(login string) string { return strings.TrimSuffix(login, "[bot]") }

// Co returns login's bookkeeping for this round (zero value when none).
func (r *Round) Co(login string) CoBotRound {
	return r.CoBots[coBotKey(login)]
}

// setCo stores login's entry copy-on-write (Rounds are copied by value while
// the map inside would otherwise be shared) and dual-writes Codex's entry
// into the legacy fields old binaries read.
func (r *Round) setCo(login string, c CoBotRound) {
	key := coBotKey(login)
	m := make(map[string]CoBotRound, len(r.CoBots)+1)
	for k, v := range r.CoBots {
		m[k] = v
	}
	if c.empty() {
		delete(m, key)
	} else {
		m[key] = c
	}
	if len(m) == 0 {
		m = nil
	}
	r.CoBots = m
	if key == codexCoBotKey {
		r.CodexCommandID = c.CommandID
		r.CodexCommandedAt = c.CommandedAt
		r.CodexClaimedAt = c.ClaimedAt
	}
}

// SetCoCommand records the trigger comment posted/adopted for login this
// round and releases its claim.
func (r *Round) SetCoCommand(login string, commandID int64, at time.Time) {
	c := r.Co(login)
	c.CommandID = commandID
	t := at.UTC()
	c.CommandedAt = &t
	c.ClaimedAt = nil
	r.setCo(login, c)
}

// ClaimCo reserves the pending trigger post for login: CAS-set before the
// network post so two unserialized sweepers cannot both post the command for
// the same round. A stale claim (the poster died mid-flight) expires by age
// and may be re-claimed.
func (r *Round) ClaimCo(login string, now time.Time) {
	c := r.Co(login)
	t := now.UTC()
	c.ClaimedAt = &t
	r.setCo(login, c)
}

// NoteCoAnswer records that login was OBSERVED producing head evidence. The
// FIRST such observation wins: a round is one head, so the evidence does not
// change, and re-stamping it with each sweep's clock would both rewrite state
// on every pass and make a bot that answered days ago read as freshly active.
// SeenActiveAt is initialized beside it and then carried to later heads.
func (r *Round) NoteCoAnswer(login string, at time.Time) {
	c := r.Co(login)
	if c.AnsweredAt != nil && c.SeenActiveAt != nil && !c.ActivityCarried {
		return
	}
	t := at.UTC()
	if c.AnsweredAt == nil {
		c.AnsweredAt = &t
	}
	if c.SeenActiveAt == nil {
		c.SeenActiveAt = &t
	}
	c.ActivityCarried = false
	r.setCo(login, c)
}

// NoteCoParticipation records activity bound to this round without claiming
// the reviewer finished. Unlike NoteCoActivity, this evidence came from the
// current head and must not be restored as historical activity after the
// round's bounded wait completes.
func (r *Round) NoteCoParticipation(login string, at time.Time) {
	c := r.Co(login)
	if c.AnsweredAt != nil {
		return
	}
	if c.SeenActiveAt != nil && !c.ActivityCarried {
		return
	}
	t := at.UTC()
	c.SeenActiveAt = &t
	c.ActivityCarried = false
	r.setCo(login, c)
}

// NoteCoActivity records durable evidence that login has acted on this pull
// request without claiming it answered this round. It is used when an
// observation races a supersede: the old head's evidence still proves the
// reviewer is active, but cannot satisfy the replacement round's head gate.
func (r *Round) NoteCoActivity(login string, at time.Time) {
	c := r.Co(login)
	if c.AnsweredAt != nil {
		return
	}
	if c.SeenActiveAt != nil {
		return
	}
	t := at.UTC()
	c.SeenActiveAt = &t
	c.ActivityCarried = true
	r.setCo(login, c)
}

// NotePrimaryAnswer records that login, the primary at the time, was OBSERVED
// producing head evidence. Same terms as NoteCoAnswer sets for a co-reviewer:
// the FIRST observation wins, since a round is one head and re-stamping it
// would rewrite state on every sweep. The login is stored with it so a later
// reader is not left inferring who answered from its own configuration.
func (r *Round) NotePrimaryAnswer(login string, at time.Time) {
	if r.PrimaryAnsweredAt != nil {
		return
	}
	t := at.UTC()
	r.PrimaryAnsweredAt = &t
	r.PrimaryAnsweredBy = login
}

// ClearCoClaim releases login's claim without recording a command.
func (r *Round) ClearCoClaim(login string) {
	c := r.Co(login)
	c.ClaimedAt = nil
	r.setCo(login, c)
}

// foldLegacyCodex folds the legacy per-round Codex fields into CoBots on
// load. Every writer keeps the legacy fields current for Codex (old binaries
// write only them, new ones dual-write), so they are authoritative in BOTH
// directions: they overwrite the mirror entry, and an empty legacy set clears
// a stale mirror (a writer zeroed the legacy fields directly).
// Authoritative about what they CARRY, which is the three trigger fields.
// AnsweredAt has no legacy counterpart — it is what crq observed the bot do,
// not bookkeeping about crq's own post — so no writer of any version can state
// it there. Overwriting the whole entry therefore erased it on every load, and
// Codex alone read as a bot that never answers.
func (r *Round) foldLegacyCodex() {
	prev := r.Co(codexCoBotKey)
	r.setCo(codexCoBotKey, CoBotRound{
		CommandID: r.CodexCommandID, CommandedAt: r.CodexCommandedAt, ClaimedAt: r.CodexClaimedAt,
		SeenActiveAt: prev.SeenActiveAt, ActivityCarried: prev.ActivityCarried,
		AnsweredAt: prev.AnsweredAt, unknown: prev.unknown,
	})
}

// inferCoOnly backfills CoOnly for rounds written before the flag existed. The
// evidence is already in the round: a co-reviewer-only fire recorded one of its
// OWN trigger comments as the round's CommandID, whereas a real fire records
// the primary review command — two different comments, so the inference cannot
// mislabel a genuine request.
func (r *Round) inferCoOnly() {
	if r.CoOnly || r.FiredAt == nil {
		return
	}
	// A bounded co-review wait anchors FiredAt without ever posting a command,
	// so a fire with no CommandID is one by construction: every real fire
	// records the command it posted or adopted.
	if r.CommandID == 0 {
		r.CoOnly = true
		return
	}
	// A co-reviewer-only fire recorded one of its OWN trigger comments as the
	// round's CommandID; a real fire records the primary review command, a
	// different comment, so this cannot mislabel a genuine request.
	for _, c := range r.CoBots {
		if c.CommandID == r.CommandID {
			r.CoOnly = true
			return
		}
	}
}

// FireSlot is the single global in-flight reservation: at most one review
// command may be getting posted at a time, fleet-wide.
type FireSlot struct {
	Key   string    `json:"key"` // repo#pr holding the slot
	Token string    `json:"token"`
	Since time.Time `json:"since"`
	// HoldUntil keeps the slot taken after the round holding it went away with
	// its metered command still unacknowledged — a head advance archives the
	// round, and the command it posted does not stop being in flight because of
	// that. Set to the end of that command's in-flight window, so the hold is
	// bounded by the same deadline Progress would have applied.
	HoldUntil *time.Time `json:"hold_until,omitempty"`

	// unknown carries JSON members this binary has no field for, so a newer
	// binary's additions survive being read and rewritten here. See tolerant.go.
	unknown unknownFields
}

// AccountQuota is the CodeRabbit account-wide review quota (NOT the GitHub
// REST quota — that is internal/gh's Throttle). Set only from classified
// CodeRabbit comments.
type AccountQuota struct {
	Scope        string     `json:"scope,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	Remaining    *int       `json:"remaining,omitempty"`
	Source       string     `json:"source,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	CalibAskedAt *time.Time `json:"calib_asked_at,omitempty"`
	// RLCommentID/RLCommentUpdated identify the rate-limit comment whose "next
	// review available in" window produced the current block. CodeRabbit edits a
	// single rate-limit comment in place instead of posting a new one, so its
	// UpdatedAt advances past every later fire; tracking it lets a re-observed
	// edit reuse the standing block instead of being counted as a fresh event
	// that extends the window on every bounce.
	RLCommentID      int64      `json:"rl_comment_id,omitempty"`
	RLCommentUpdated *time.Time `json:"rl_comment_updated,omitempty"`
	// Fires is when metered reviews were requested, trimmed to a rolling two
	// weeks. See firelog.go: it exists to forecast the vendor's WEEKLY fair-use
	// throttle, which crq can already recognise but never see coming.
	Fires []time.Time `json:"fires,omitempty"`
	// FiresFrom is when the log's COVERAGE starts, which is not the same as its
	// oldest surviving entry: trimming keeps the blob small and would otherwise
	// keep moving the start forward, telling a quiet fleet its fully observed
	// week is only a floor. Recorded once and moved only when the entry cap
	// genuinely discards history the rolling week needs.
	FiresFrom *time.Time `json:"fires_from,omitempty"`

	// unknown carries members a newer binary wrote inside this record. State's
	// carrier cannot: it sees "account" as a member it knows and hands the whole
	// object to the ordinary decoder. See tolerant.go.
	unknown unknownFields
}

type LeaderLease struct {
	Owner        string    `json:"owner"`
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Capabilities []string  `json:"capabilities,omitempty"`
}

// HasCapability reports whether the active daemon understands a state feature.
func (l LeaderLease) HasCapability(want string) bool {
	for _, capability := range l.Capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

// LeaderCapabilityLease dual-writes the active leader's capabilities at the
// top level. Older schema-v3 binaries preserve unknown top-level members, while
// they drop unknown members nested inside LeaderLease. The token prevents a
// preserved capability list from being attributed to a different leader.
type LeaderCapabilityLease struct {
	Token        string   `json:"token"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func (l LeaderCapabilityLease) HasCapability(want string) bool {
	for _, capability := range l.Capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

// State is schema v6. It persists as state.json in the existing git state ref;
// v4 is migrated in place so its live rounds survive the compatibility fence.
type State struct {
	Version int   `json:"v"` // 5
	Rev     int64 `json:"rev"`
	NextSeq int64 `json:"next_seq"`

	Rounds   map[string]Round `json:"rounds"`
	FireSlot *FireSlot        `json:"fire_slot,omitempty"`
	// FireSlotHoldUntil is the top-level compatibility mirror of
	// FireSlot.HoldUntil. Binaries predating nested FireSlot tolerance discard
	// hold_until and clear an orphaned slot during Normalize, but their State
	// tolerance carries this unknown top-level member. New binaries can therefore
	// still recover the hold after an older writer rewrites the shared state.
	FireSlotHoldUntil *time.Time `json:"fire_slot_hold_until,omitempty"`
	// FireSlotHoldLastFired is the real pacing anchor replaced while the hold
	// is dual-written into LastFired for binaries that do not understand the
	// top-level mirror. Current binaries restore it when the hold ends.
	FireSlotHoldLastFired *time.Time   `json:"fire_slot_hold_last_fired,omitempty"`
	LastFired             *time.Time   `json:"last_fired,omitempty"`
	Account               AccountQuota `json:"account"`
	Leader                *LeaderLease `json:"leader,omitempty"`

	// LeaderCapabilities is outside Leader so old binaries' tolerant state
	// writer carries it across lease renewals and unrelated state mutations.
	LeaderCapabilities *LeaderCapabilityLease `json:"leader_capabilities,omitempty"`

	// CalibrationIssue overrides the configured calibration PR/issue when the
	// original hit GitHub's hard 2500-comment cap and crq rotated to a fresh
	// one. Persisted in the shared state so the whole fleet uses the new issue.
	CalibrationIssue int `json:"calibration_issue,omitempty"`

	// Holds are the PRs crq must not fire a review for, keyed by "owner/name#pr".
	//
	// Holding used to take two commands that could not be one: the skip marker
	// stops fleet auto-review from enqueueing, `crq cancel` stops the pump, and
	// between the two a daemon fired anyway. A hold is one fact, in the state
	// every firing path already reads.
	Holds map[string]Hold `json:"holds,omitempty"`
	// HoldTokens identify the specific hold operation that owns its PR notice.
	// Keeping these at the state top level lets older tolerant writers preserve
	// them while a newer process is still posting that notice.
	HoldTokens map[string]string `json:"hold_tokens,omitempty"`
	// Writers records which hosts have written this state and what they can do,
	// so a feature that only SOME binaries understand can say so instead of
	// pretending agreement. Sharing a ref stops an old binary erasing a new
	// field; it does not make that binary act on it.
	Writers map[string]WriterSeen `json:"writers,omitempty"`

	// Repos holds per-repository reviewer overrides, keyed by normalized
	// "owner/name".
	//
	// It lives HERE, in the shared state ref, rather than in a .crq.yaml the
	// repository carries. The daemon has no checkout of any repository it
	// reviews, so an in-repo file would be invisible to it or cost a REST fetch
	// per PR — and a daemon and an agent reading different configurations while
	// writing one shared state ref is a new class of divergence. Both already
	// read this ref, so both cannot disagree about it.
	Repos map[string]RepoReviewers `json:"repos,omitempty"`

	// Archive keeps recently finished rounds (superseded, closed, cancelled)
	// for the dashboard and debugging. Bounded by ArchiveMax.
	Archive []Round `json:"archive,omitempty"`
	// CoActivity keeps the latest observed activity for each co-reviewer on a
	// pull request. Unlike Archive, it is not bounded: a closed PR may be
	// reopened after its historical rounds have been evicted, and a silent
	// check-only reviewer still needs to be eligible for self-heal then.
	CoActivity map[string]map[string]time.Time `json:"co_activity,omitempty"`
	// CoAnswers keeps durable proof that each co-reviewer completed a review on
	// the pull request. Generic activity is deliberately not enough for the
	// one-pass review cap: an in-progress, auxiliary, or failed check did not
	// deliver a review.
	CoAnswers map[string]map[string]time.Time `json:"co_answers,omitempty"`
	// ReviewedHeads is the PR-wide review-round ledger. A round itself is scoped
	// to one head and is archived whenever a fixer pushes, so its attempt count
	// cannot stop a long review/fix loop. Keeping the distinct reviewed heads
	// here lets the scheduler bound that loop without mistaking repeated
	// observations of one review for new work. An explicitly empty entry is a
	// reset cycle; Normalize must not reconstruct it from the archive.
	ReviewedHeads map[string][]string `json:"reviewed_heads,omitempty"`

	// Autofix records the watcher's dispatch health. It is separate from Warn
	// because Warn is cleared by the next successful fire — a dispatcher that
	// has been failing for hours would be wiped by unrelated progress, which is
	// exactly how the failure stayed invisible.
	Autofix *AutofixHealth `json:"autofix,omitempty"`
	// AutofixByHost keeps each host's failure streak independent. Autofix above is the
	// fleet summary dual-written for older binaries during rolling upgrades.
	AutofixByHost map[string]AutofixHealth `json:"autofix_by_host,omitempty"`
	// Dispatches holds live PR-level claims outside the bounded round archive.
	// A session may still be resolving threads after its push superseded the
	// claimed round, and archive eviction must not admit a second session.
	Dispatches map[string]DispatchClaim `json:"dispatches,omitempty"`
	// WorkClaims are short-lived PR-level leases held by interactive `crq next`,
	// `crq wait`, and `crq loop` callers. Autofix dispatch consults them in its
	// claiming CAS, so an unattended session cannot start work an agent has
	// already taken. They are independent of the head because one interactive
	// loop owns the PR across its fix, push, and re-review cycle.
	WorkClaims map[string]WorkClaim `json:"work_claims,omitempty"`
	// Fleet is what every repository inherits, recorded once for the whole fleet
	// rather than in each host's env file. See fleet.go.
	Fleet FleetDefaults `json:"fleet,omitempty"`
	// HostReports is what each machine says about itself: its crq version and
	// the tools it can reach. See hosts.go.
	HostReports map[string]HostReport `json:"host_reports,omitempty"`
	// RepoSolver is how a fix session runs, per repository. See solver.go.
	RepoSolver map[string]SolverSettings `json:"repo_solver,omitempty"`
	// OnePass is the fixer-to-merger hand-off for repository-scoped one-pass
	// campaigns. See onepass.go.
	OnePass map[string]OnePassProgress `json:"one_pass,omitempty"`
	// OnePassEvidence preserves the campaign's reviewer identities and consumed
	// review marker across reviewer changes. See onepass.go.
	OnePassEvidence map[string]OnePassEvidence `json:"one_pass_evidence,omitempty"`
	// Enrolled answers "does crq review this project at all?" per repository,
	// so the decision lives with the fleet rather than in one host's env file.
	// Absent means the hosts' CRQ_REPOS/CRQ_EXCLUDE decide, as before.
	Enrolled map[string]RepoEnrollment `json:"enrolled,omitempty"`
	// RepoAutofix answers "may crq fix pull requests here?" per repository. Absent
	// means the default, which is yes — see AutofixEnabled.
	RepoAutofix map[string]RepoAutofixSwitch `json:"repo_autofix,omitempty"`
	// TidiedCommands are durable tombstones for trigger comments Tidy removed.
	// Unlike Archive, they are not bounded: GitHub can deliver a delayed reply
	// for as long as the PR conversation exists, and command/reply FIFO pairing
	// still needs the deleted command's chronological position.
	TidiedCommands map[string][]PostedCommand `json:"tidied_commands,omitempty"`
	// TidyReactionCursors rotate the bounded scan of unanswered Codex commands
	// so old candidates are not reread on every housekeeping pass and newer
	// candidates are not starved.
	TidyReactionCursors map[string]int64 `json:"tidy_reaction_cursors,omitempty"`

	Warn         string     `json:"warn,omitempty"`
	UpdatedAt    *time.Time `json:"wrote_at,omitempty"`
	DashboardSHA string     `json:"dashboard_sha,omitempty"`

	// unknown carries top-level JSON members this binary has no field for. See
	// tolerant.go.
	unknown unknownFields
}

// LeaderHasCapability reports whether the current lease holder advertises a
// capability, accepting either the nested representation or its rolling-
// deployment-safe top-level copy.
func (s State) LeaderHasCapability(want string) bool {
	if s.Leader == nil {
		return false
	}
	if s.Leader.HasCapability(want) {
		return true
	}
	return s.LeaderCapabilities != nil &&
		s.LeaderCapabilities.Token == s.Leader.Token &&
		s.LeaderCapabilities.HasCapability(want)
}

const SchemaVersion = 6

// WriterCaps is what THIS binary understands. Bump it when a state field starts
// changing decisions, so a fleet running two versions can tell.
const WriterCaps = 17

// CapsExpiredRounds is the capability needed to persist PhaseExpired. Older
// daemons do not recognise that terminal same-head marker and could otherwise
// enqueue another paid review for a round a newer daemon retired.
const CapsExpiredRounds = 16

// CapsReviewRoundBudget is the capability both review schedulers and autofix
// watchers need before a PR-wide review budget can be relied on. Older review
// schedulers keep posting new-head commands, while older fixers keep producing
// the heads that invite them.
const CapsReviewRoundBudget = 17

// CapsRepoOverrides is the capability that makes per-repository reviewer
// overrides safe to act on.
const CapsRepoOverrides = 1

// CapsPrimaryOff is the capability that makes RepoReviewers.PrimaryOff safe to
// act on. A host below it still fires the primary there — so turning the
// primary off has to say which hosts will not honour it, exactly as the
// override itself does.
//
// Worse here than elsewhere, which is why the warning matters: a binary from
// before RepoReviewers round-tripped unknown members does not merely ignore the
// switch, it ERASES it on its next write, and the repository silently resumes
// metered primary reviews on every host. Nothing can be done about a binary
// already in the field; the tolerant decoding in tolerant.go is what stops the
// next one from doing it.
const CapsPrimaryOff = 2

// CapsEnrollment is the capability that makes State.Enrolled safe to act on. A
// host below it decides from its own env alone, so a repository enrolled here
// is invisible to it and one turned off here keeps being reviewed by it.
const CapsEnrollment = 3

// CapsFleetDefaults is the capability that makes State.Fleet safe to act on. A
// host below it keeps deciding from its own env, so a default recorded here is
// simply not applied there.
const CapsFleetDefaults = 4

// CapsSolver is the capability that makes every solver setting safe to act on.
// A host below it either runs every fix session with its own install-time
// settings or cannot distinguish an explicitly empty repository prompt from
// inheritance, so the dashboard must name it before claiming the saved answer
// applies fleet-wide.
const CapsSolver = 8

// CapsOnePass is the capability required by both autoreview and autofix hosts
// to honour the one-review cap and the fixer-to-merge hand-off.
const CapsOnePass = 15

// CapsPreflightSkipBlocked is the capability that makes the shared
// CRQ_PREFLIGHT_SKIP_BLOCKED policy safe to act on. Older hosts run the local
// CLI even when the fleet has recorded an account block, so operators need to
// see them before relying on the shared skip.
const CapsPreflightSkipBlocked = 10

// CapsWorkClaims is the capability an autofix watcher needs to honour
// interactive PR ownership. Claim creation refuses to promise exclusivity
// while a recently active autofix host predates it.
const CapsWorkClaims = 9

// CapsDispatchClarification is the capability that makes a head-scoped
// clarification marker terminal for autofix dispatch. Older watchers preserve
// the marker but may still launch another session for the same question.
const CapsDispatchClarification = 7

// writerTTL is how long a host counts as still active for capability purposes.
const writerTTL = 30 * time.Minute

// WriterSeen is one host's last write.
type WriterSeen struct {
	Caps int       `json:"caps"`
	At   time.Time `json:"at"`
}

// NoteWriter records that a process wrote this state with the given
// capabilities. The key identifies the PROCESS, not the machine: a new CLI and
// an old daemon on one host is the ordinary upgrade, and keying by hostname
// would let the CLI's write vouch for the daemon that has not been upgraded.
func (s *State) NoteWriter(host string, caps int, now time.Time) {
	if host == "" {
		return
	}
	if s.Writers == nil {
		s.Writers = map[string]WriterSeen{}
	}
	s.Writers[host] = WriterSeen{Caps: caps, At: now.UTC()}
	// Bounded: a host that has not written in a day is not part of the fleet's
	// current behaviour, and the state ref is not an audit log.
	for name, seen := range s.Writers {
		if now.Sub(seen.At) > 24*time.Hour {
			delete(s.Writers, name)
		}
	}
}

// LaggingWriters names the hosts that are DRIVING this queue — holding the
// leader lease or the fire slot — without having announced the capability
// needed for caps.
//
// It answers the question a shared config field cannot: "will everyone who acts
// on this actually honour it?" An old binary loads an unknown field, writes it
// back untouched, and keeps deciding from its own fleet-wide configuration.
func (s *State) LaggingWriters(caps int, now time.Time) []string {
	acting := map[string]bool{}
	// The leader identifies itself as "host=<name> pid=<n> run=<id>", which is
	// exactly the process identity capabilities are recorded under — the run
	// component is what keeps a restart into a reused pid from inheriting them.
	if s.Leader != nil && s.Leader.ExpiresAt.After(now) && strings.TrimSpace(s.Leader.Owner) != "" {
		acting[s.Leader.Owner] = true
	}
	if slot := s.SlotRound(); slot != nil && slot.ByHost != "" {
		acting[slot.ByHost] = true
	}
	var out []string
	for host := range acting {
		if seen, ok := s.Writers[host]; ok && seen.Caps >= caps && now.Sub(seen.At) <= writerTTL {
			continue
		}
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

// LaggingRoleWriters is LaggingWriters plus every host reporting one of the
// given roles with an older capability set.
//
// The acting set LaggingWriters builds is the leader and the fire-slot owner —
// the processes that drive a REVIEW. A setting consumed by something else has no
// entry there: the autofix watcher holds neither lease, so a solver record's
// lagging list came back empty while an old watcher went on dispatching
// install-time model, fork and attempt values. A host's self-report is what
// names it, since it records the binary's capabilities beside the roles running
// there.
//
// Reports are named by machine and writers by process ("host=blue pid=4711
// run=1a2b"), so a machine lagging in both registers is listed once, under the
// writer identity that says which process it is.
//
// Capabilities are asked of the ROLE, not of the record: a machine mid-upgrade
// runs one service on each build, and the record's own value is whichever wrote
// last. Reading it let a fresh `serve` heartbeat vouch for an old watcher.
func (s *State) LaggingRoleWriters(caps int, now time.Time, roles ...string) []string {
	out := s.LaggingWriters(caps, now)
	named := map[string]bool{}
	for _, writer := range out {
		named[hostName(writer)] = true
	}
	for host, report := range s.HostReports {
		if named[host] {
			continue
		}
		for _, role := range roles {
			if report.CapsFor(role) < caps && report.RolesFresh([]string{role}, now, HostReportTTL) {
				named[host] = true
				out = append(out, host)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// RepoReviewers overrides which reviewers run on one repository. A nil slice
// means "no override, use the fleet default"; an empty non-nil slice means
// "none here" — the difference is why these are pointers to slices in JSON
// terms and why SetRepoReviewers takes explicit values.
type RepoReviewers struct {
	// CoBots are the co-reviewer logins enabled here, replacing the fleet list.
	CoBots []string `json:"cobots,omitempty"`
	// Required are the logins that gate convergence here.
	Required []string `json:"required,omitempty"`
	// PrimaryOff turns the metered primary off for this repository: crq never
	// posts its review command here, never spends the account quota or the fire
	// slot on it, and never waits for its review. The co-reviewers resolve the
	// round alone. There is no "set" companion because false IS the default —
	// the primary runs unless a repository says otherwise.
	PrimaryOff bool `json:"primary_off,omitempty"`
	// SetCoBots/SetRequired record whether each list was set at all, so
	// "explicitly none" survives a JSON round trip that drops empty slices.
	SetCoBots   bool       `json:"set_cobots,omitempty"`
	SetRequired bool       `json:"set_required,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	By          string     `json:"by,omitempty"`

	// unknown carries JSON members this binary has no field for. State
	// recognises "repos" and hands each value to an ordinary decoder, so the
	// top-level carrier never sees a member added INSIDE one of these records —
	// an older binary that knows the map but not PrimaryOff would drop the
	// switch on its next write and the repository would silently resume metered
	// primary reviews. See tolerant.go.
	unknown unknownFields
}

// RepoOverride returns the override for repo, and whether one exists.
func (s *State) RepoOverride(repo string) (RepoReviewers, bool) {
	ov, ok := s.Repos[normalizeRepoKey(repo)]
	return ov, ok
}

// SetRepoOverride records repo's reviewer override, replacing any earlier one.
func (s *State) SetRepoOverride(repo string, ov RepoReviewers) {
	if s.Repos == nil {
		s.Repos = map[string]RepoReviewers{}
	}
	s.Repos[normalizeRepoKey(repo)] = ov
}

// ClearRepoOverride drops repo's override, returning it to the fleet default.
func (s *State) ClearRepoOverride(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

func normalizeRepoKey(repo string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(repo), ".git"))
}

// ArchiveMax bounds the finished-rounds ring. Active rounds are never
// evicted — only Archive is trimmed — so a live "already fired at this head"
// marker cannot be lost to an eviction cap.
const ArchiveMax = 50

func Key(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), pr)
}

func New() State {
	return State{Version: SchemaVersion, Rounds: map[string]Round{}}
}

// --- Round transitions -----------------------------------------------------

type TransitionError struct {
	From, To Phase
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal round transition %s → %s", e.From, e.To)
}

func (r *Round) illegal(to Phase) error { return &TransitionError{From: r.Phase, To: to} }

// Reserve takes the fire slot for this round: queued (or retry-eligible
// awaiting_retry) → reserved. writer is the reserving process's writer id (see
// ByHost), not a bare hostname.
func (r *Round) Reserve(token, writer string, now time.Time) error {
	if r.Phase != PhaseQueued && !r.retryEligible(now) {
		return r.illegal(PhaseReserved)
	}
	r.Phase = PhaseReserved
	r.Token = token
	r.ByHost = writer
	t := now.UTC()
	r.ReservedAt = &t
	r.Note = ""
	return nil
}

// Fire records the posted (or adopted) review command: reserved → fired.
// Adoption of an already-posted command fires straight from queued.
func (r *Round) Fire(commandID int64, at time.Time) error {
	if r.Phase != PhaseReserved && r.Phase != PhaseQueued {
		return r.illegal(PhaseFired)
	}
	r.Phase = PhaseFired
	r.CommandID = commandID
	t := at.UTC()
	r.FiredAt = &t
	r.Attempts++
	r.Note = ""
	return nil
}

// ReleaseToQueue returns a reservation that never posted: reserved → queued.
// The attempt still counts and LastAttemptAt moves, so a stale command comment
// from before the failure cannot be adopted later.
func (r *Round) ReleaseToQueue(reason string, now time.Time) error {
	if r.Phase != PhaseReserved {
		return r.illegal(PhaseQueued)
	}
	r.Phase = PhaseQueued
	r.Token = ""
	r.ReservedAt = nil
	r.Attempts++
	t := now.UTC()
	r.LastAttemptAt = &t
	r.Note = reason
	return nil
}

// Reopen puts a completed round back in the queue because its effective
// reviewer set changed.
//
// A completed round is the "this head was reviewed" dedup marker, so a newly
// required reviewer would otherwise strand the PR: convergence reports it
// pending while enqueue keeps skipping the head, and no eligible round exists to
// trigger it. An optional reviewer also needs an active round for its trigger,
// self-heal and bounded participation wait. This is the one transition that
// reopens a finished round, and it keeps the head, the attempts, the settled
// primary side and the co-reviewer bookkeeping — what changed is who runs, not
// what happened. A caller whose effective primary changed clears that
// settlement explicitly.
//
// LastAttemptAt is deliberately left alone: it is the adoption floor for a
// FAILED attempt, and moving it would discard a newly required co-reviewer's own
// unanswered trigger comment as too old to adopt — so crq would post that bot a
// second request for the very round the reopen exists to let it answer.
func (r *Round) Reopen() error {
	if r.Phase != PhaseCompleted {
		return r.illegal(PhaseQueued)
	}
	r.Phase = PhaseQueued
	r.Token = ""
	r.ReservedAt = nil
	r.WaitDeadline = nil
	r.RetryAt = nil
	r.ReviewersChanged = false
	r.Note = "reviewer configuration changed"
	return nil
}

// ReopenForRestoredActivity puts a completed round back in the queue when a
// rolling-upgrade repair restores reviewer activity it had lost.
func (r *Round) ReopenForRestoredActivity() error {
	if err := r.Reopen(); err != nil {
		return err
	}
	r.PrimarySettled = true
	r.Note = "restored reviewer activity"
	return nil
}

// ForceCoReviewer reports whether a reviewer-change reopen granted login its
// one immediate trigger. Bot names are normalized like CoBots keys.
func (r *Round) ForceCoReviewer(login string) bool {
	key := coBotKey(login)
	for _, candidate := range r.ForceCoReviewers {
		if coBotKey(candidate) == key {
			return true
		}
	}
	return false
}

// Acknowledge records that the bot has seen the fired command (reaction,
// in-progress summary, or other non-terminal reply): fired → reviewing. The
// fire slot may be released; the round itself stays open until Complete.
func (r *Round) Acknowledge() error {
	if r.Phase == PhaseReviewing {
		return nil // idempotent: acks arrive repeatedly while a review runs
	}
	if r.Phase != PhaseFired {
		return r.illegal(PhaseReviewing)
	}
	r.Phase = PhaseReviewing
	r.Note = ""
	return nil
}

// AwaitRetry parks the round until retryAt: fired|reviewing|reserved →
// awaiting_retry. This REPLACES the v2 "delete the fired marker and requeue"
// path — the round keeps its head, attempts, and fire history, so the next
// daemon pass sees "already requested, waiting" instead of "never fired".
func (r *Round) AwaitRetry(retryAt time.Time, reason string, now time.Time) error {
	switch r.Phase {
	case PhaseFired, PhaseReviewing, PhaseReserved:
	default:
		return r.illegal(PhaseAwaitingRetry)
	}
	r.Phase = PhaseAwaitingRetry
	t := retryAt.UTC()
	r.RetryAt = &t
	n := now.UTC()
	r.LastAttemptAt = &n
	r.Token = ""
	r.ReservedAt = nil
	r.Note = reason
	return nil
}

// AwaitCoReview bounds a co-review wait: the configured primary bot already
// reviewed the head, but a gating co-bot (Codex) has not — so crq waits for it,
// bounded by deadline, WITHOUT posting a command or holding the fire slot. Legal
// from queued|awaiting_retry|fired|reviewing → reviewing. FiredAt is the wait
// anchor: the primary review already stands in as the fire, so it is set to now
// only when no fire was recorded. Token/ReservedAt are cleared (no slot is held);
// CodexCommandID is left as-is so an existing Codex command is not re-posted.
func (r *Round) AwaitCoReview(deadline, anchor time.Time) error {
	switch r.Phase {
	case PhaseQueued, PhaseAwaitingRetry, PhaseFired, PhaseReviewing:
	default:
		return r.illegal(PhaseReviewing)
	}
	r.Phase = PhaseReviewing
	dl := deadline.UTC()
	r.WaitDeadline = &dl
	if r.FiredAt == nil {
		// The anchor is the wait's evidence floor (Completion ignores SHA-less
		// co-bot summaries before FiredAt). Callers pass the adopted co-bot
		// command's time when one exists — anchoring at observation time would
		// hide an answer posted between that command and this pump.
		t := anchor.UTC()
		r.FiredAt = &t
	}
	r.Token = ""
	r.ReservedAt = nil
	r.Note = "awaiting codex co-review"
	return nil
}

// Complete finishes the round: fired|reviewing → completed. A completed round
// stays in Rounds (it IS the "this head was reviewed" dedup marker) until a
// new head supersedes it or the PR closes.
func (r *Round) Complete() error {
	if r.Phase != PhaseFired && r.Phase != PhaseReviewing {
		return r.illegal(PhaseCompleted)
	}
	if r.WaitDeadline != nil {
		for _, co := range r.CoBots {
			if co.AnsweredAt == nil && co.ActivityCarried {
				r.PrimarySettled = true
				break
			}
		}
	}
	r.Phase = PhaseCompleted
	r.ForceCoReviewers = nil
	r.Note = ""
	return nil
}

// Expire retires a reviewing round whose durable feedback deadline elapsed
// before every required reviewer answered. It remains in Rounds as the
// same-head dedup marker: losing an interactive waiter must never buy another
// review of unchanged code.
func (r *Round) Expire(reason string) error {
	if r.Phase != PhaseReviewing {
		return r.illegal(PhaseExpired)
	}
	r.Phase = PhaseExpired
	r.Token = ""
	r.ReservedAt = nil
	r.ForceCoReviewers = nil
	r.Note = reason
	return nil
}

// Dedupe completes a not-yet-fired round because the configured bot already
// reviewed its head independently (an adopted review, not a fire crq made): a
// queued (or retry-eligible) round → completed. The completed round stays as
// the "this head was reviewed" dedup marker without recording a fictitious
// fire (FiredAt stays nil).
func (r *Round) Dedupe(now time.Time) error {
	if r.Phase != PhaseQueued && r.Phase != PhaseReserved && !r.retryEligible(now) {
		return r.illegal(PhaseCompleted)
	}
	r.Phase = PhaseCompleted
	r.Token = ""
	r.ReservedAt = nil
	r.ForceCoReviewers = nil
	r.Note = "bot already reviewed head"
	return nil
}

// Abandon ends the round from any phase (PR closed/merged, cancelled, or
// superseded by a new head). The caller archives it via State.EndRound.
func (r *Round) Abandon(reason string) {
	r.Phase = PhaseAbandoned
	r.Token = ""
	r.Note = reason
}

func (r *Round) retryEligible(now time.Time) bool {
	if r.Phase != PhaseAwaitingRetry || r.RetryAt == nil || now.Before(*r.RetryAt) {
		return false
	}
	// A dead dispatch loses its live claim after DispatchTTL, but it must not
	// erase a longer cooldown the round was already serving. ReleaseDispatch
	// restores this value for an orderly exit; this check preserves it when the
	// watcher dies before releasing.
	return r.DispatchHoldRetryAt == nil || !now.Before(*r.DispatchHoldRetryAt)
}

// FireEligible reports whether Pump may consider this round for firing now.
//
// A round a fix session holds is not: that session is replacing the very code a
// review would be about, and its push moves the head minutes later. Firing there
// spends the account's metered review on a head nobody will look at again — and
// the claim is the only record of it, since the round stays queued throughout.
// Leaving the round OUT of the queue rather than refusing it at the fire gate is
// what keeps every other PR moving while one is being fixed; a claim nobody
// heartbeats expires (DispatchTTL), so this can never park a round for good.
func (r *Round) FireEligible(now time.Time) bool {
	if r.DispatchHeld(now) {
		return false
	}
	return r.Phase == PhaseQueued || r.retryEligible(now)
}

// Active reports whether the round still occupies its PR slot (i.e. is not
// finished). Completed and expired rounds are NOT active but still occupy
// Rounds as same-head dedup markers.
func (r *Round) Active() bool {
	switch r.Phase {
	case PhaseQueued, PhaseReserved, PhaseFired, PhaseReviewing, PhaseAwaitingRetry:
		return true
	}
	return false
}

// --- State operations ------------------------------------------------------

// Round returns the current round for repo#pr, or nil.
func (s *State) Round(repo string, pr int) *Round {
	if s.Rounds == nil {
		return nil
	}
	r, ok := s.Rounds[Key(repo, pr)]
	if !ok {
		return nil
	}
	return &r
}

// PutRound stores r as the current round for its PR.
func (s *State) PutRound(r Round) {
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	s.rememberCoActivity(r)
	s.Rounds[Key(r.Repo, r.PR)] = r
}

// MoveToFront gives the current round the lowest sequence in the state.
//
// Normal rounds have positive sequences. Explicitly prioritized rounds use
// negative ones, which keeps the existing queue contract understood by older
// binaries while also letting autofix distinguish operator priority from
// ordinary FIFO order.
func (s *State) MoveToFront(repo string, pr int) bool {
	r := s.Round(repo, pr)
	if r == nil || (r.Phase != PhaseQueued && r.Phase != PhaseAwaitingRetry) {
		return false
	}
	minSeq := int64(0)
	for _, other := range s.Rounds {
		if other.Seq < minSeq {
			minSeq = other.Seq
		}
	}
	r.Seq = minSeq - 1
	s.PutRound(*r)
	return true
}

// NewRound begins a round for a head with no current round. It refuses to
// clobber an existing round — supersede via EndRound first — so "two rounds
// for one PR" cannot happen by accident. Durable co-reviewer activity is
// restored from the per-PR index (and archived rounds written before that
// index existed) so reopening a PR does not forget a silent check-only reviewer
// that worked on an earlier head.
func (s *State) NewRound(repo string, pr int, head string, now time.Time) (*Round, error) {
	key := Key(repo, pr)
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	if cur, ok := s.Rounds[key]; ok {
		return nil, fmt.Errorf("round already exists for %s@%s (%s)", key, cur.Head, cur.Phase)
	}
	if s.ReviewedHeads == nil {
		s.ReviewedHeads = map[string][]string{}
	}
	if _, initialized := s.ReviewedHeads[key]; !initialized {
		s.ReviewedHeads[key] = []string{}
	}
	s.NextSeq++
	r := Round{
		Repo:       strings.ToLower(repo),
		PR:         pr,
		Head:       head,
		Seq:        s.NextSeq,
		Phase:      PhaseQueued,
		EnqueuedAt: now.UTC(),
	}
	s.carryCoActivity(&r)
	s.Rounds[key] = r
	return &r, nil
}

// NoteReviewedHead records one delivered reviewer round for a pull request.
// It is idempotent because the same GitHub evidence is observed by several
// queue paths and on every subsequent poll.
func (s *State) NoteReviewedHead(repo string, pr int, head string) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	if s.ReviewedHeads == nil {
		s.ReviewedHeads = map[string][]string{}
	}
	key := Key(repo, pr)
	for _, seen := range s.ReviewedHeads[key] {
		if seen == head {
			return false
		}
	}
	s.ReviewedHeads[key] = append(s.ReviewedHeads[key], head)
	return true
}

// ReviewRoundCount reports how many distinct heads received a review in the
// current budget cycle.
func (s *State) ReviewRoundCount(repo string, pr int) int {
	return len(s.ReviewedHeads[Key(repo, pr)])
}

// ResetReviewBudget starts a fresh review-round cycle while preserving the
// explicit entry that prevents Normalize from rebuilding old archive history.
func (s *State) ResetReviewBudget(repo string, pr int) {
	if s.ReviewedHeads == nil {
		s.ReviewedHeads = map[string][]string{}
	}
	s.ReviewedHeads[Key(repo, pr)] = []string{}
}

// ClearReviewBudget retires a PR's ledger when the PR can no longer continue.
func (s *State) ClearReviewBudget(repo string, pr int) {
	delete(s.ReviewedHeads, Key(repo, pr))
}

// NoteMerged is the abandon reason recorded when the pull request merged. A
// merge is the one terminal outcome: GitHub never reopens a merged pull
// request, so evidence kept for a possible reopen can be retired with it.
const NoteMerged = "merged"

const legacyNoteMerged = "pr merged"

// Merged reports whether the round ended because its pull request merged.
func (r Round) Merged() bool {
	return r.Phase == PhaseAbandoned && (r.Note == NoteMerged || r.Note == legacyNoteMerged)
}

// RetireMerged forgets what crq keeps per pull request once it merged: the
// review-round ledger and the co-reviewer activity and answer indexes. Those
// indexes are unbounded precisely so a closed PR can reopen with its evidence
// intact; a merged one cannot, so keeping them would only grow the state ref
// for ever. Matching archive entries are marked first so Normalize cannot
// rebuild the retired indexes. Reports whether anything changed.
func (s *State) RetireMerged(repo string, pr int) bool {
	key := Key(repo, pr)
	_, ledger := s.ReviewedHeads[key]
	_, activity := s.CoActivity[key]
	_, answers := s.CoAnswers[key]
	changed := ledger || activity || answers
	for i := range s.Archive {
		round := &s.Archive[i]
		if Key(round.Repo, round.PR) == key && !round.Merged() {
			round.Abandon(NoteMerged)
			changed = true
		}
	}
	delete(s.ReviewedHeads, key)
	delete(s.CoActivity, key)
	delete(s.CoAnswers, key)
	return changed
}

// mergedKeys lists the pull requests whose archived rounds record a merge.
func (s *State) mergedKeys() map[string]bool {
	merged := map[string]bool{}
	for i := range s.Archive {
		if s.Archive[i].Merged() {
			merged[Key(s.Archive[i].Repo, s.Archive[i].PR)] = true
		}
	}
	return merged
}

func (s *State) rememberCoActivity(r Round) {
	key := Key(r.Repo, r.PR)
	for login, co := range r.CoBots {
		login = coBotKey(login)
		if co.AnsweredAt != nil {
			s.rememberCoAnswer(key, login, *co.AnsweredAt)
		}
		seenAt := co.SeenActiveAt
		if co.AnsweredAt != nil && (seenAt == nil || seenAt.Before(*co.AnsweredAt)) {
			seenAt = co.AnsweredAt
		}
		if seenAt == nil {
			continue
		}
		if s.CoActivity == nil {
			s.CoActivity = map[string]map[string]time.Time{}
		}
		activity := s.CoActivity[key]
		if activity == nil {
			activity = map[string]time.Time{}
			s.CoActivity[key] = activity
		}
		seen := seenAt.UTC()
		if previous, ok := activity[login]; !ok || previous.Before(seen) {
			activity[login] = seen
		}
	}
}

func (s *State) rememberCoAnswer(key, login string, at time.Time) {
	if s.CoAnswers == nil {
		s.CoAnswers = map[string]map[string]time.Time{}
	}
	answers := s.CoAnswers[key]
	if answers == nil {
		answers = map[string]time.Time{}
		s.CoAnswers[key] = answers
	}
	answered := at.UTC()
	login = coBotKey(login)
	if previous, ok := answers[login]; !ok || previous.Before(answered) {
		answers[login] = answered
	}
}

// RememberCoAnswer records completed-review evidence when its source round was
// superseded before the observation could be written back to that round.
func (s *State) RememberCoAnswer(repo string, pr int, login string, at time.Time) {
	s.rememberCoAnswer(Key(repo, pr), login, at)
}

// CoReviewerAnswered reports durable completed-review evidence for login on
// this pull request. The round scans retain compatibility with state written
// before the unbounded answer index existed.
func (s *State) CoReviewerAnswered(repo string, pr int, login string) bool {
	key, login := Key(repo, pr), coBotKey(login)
	if answers := s.CoAnswers[key]; !answers[login].IsZero() {
		return true
	}
	if round := s.Round(repo, pr); round != nil && round.Co(login).AnsweredAt != nil {
		return true
	}
	for i := range s.Archive {
		round := &s.Archive[i]
		if Key(round.Repo, round.PR) == key && round.Co(login).AnsweredAt != nil {
			return true
		}
	}
	return false
}

// RememberCoActivity folds a round's reviewer activity into the durable per-PR
// index. Callers that mutate an archived round in place need this because
// PutRound, EndRound, and Normalize do not observe that mutation.
func (s *State) RememberCoActivity(r Round) {
	s.rememberCoActivity(r)
}

func carryCoActivity(next *Round, activity map[string]time.Time) bool {
	restored := false
	for login, seenAt := range activity {
		current := next.Co(login)
		if current.SeenActiveAt != nil && !current.SeenActiveAt.Before(seenAt) {
			// The activity may already be present because a newer writer carried
			// it before an older writer completed this replacement round. Its
			// timestamp predating the round preserves that provenance even when
			// there is nothing left to copy from the activity index.
			if current.AnsweredAt == nil && (current.ActivityCarried ||
				!next.EnqueuedAt.IsZero() && current.SeenActiveAt.Before(next.EnqueuedAt)) {
				restored = true
			}
			continue
		}
		seen := seenAt.UTC()
		current.SeenActiveAt = &seen
		current.ActivityCarried = current.AnsweredAt == nil || current.AnsweredAt.Before(seenAt)
		next.setCo(login, current)
		if current.AnsweredAt == nil || current.AnsweredAt.Before(seenAt) {
			restored = true
		}
	}
	return restored
}

func (s *State) carryCoActivity(next *Round) bool {
	key := Key(next.Repo, next.PR)
	if activity, ok := s.CoActivity[key]; ok {
		return carryCoActivity(next, activity)
	}
	changed := false
	for i := len(s.Archive) - 1; i >= 0; i-- {
		previous := s.Archive[i]
		if Key(previous.Repo, previous.PR) != key {
			continue
		}
		activity := make(map[string]time.Time, len(previous.CoBots))
		for login, co := range previous.CoBots {
			seenAt := co.SeenActiveAt
			if seenAt == nil {
				// Rounds written before SeenActiveAt existed still carry the
				// head-scoped observation that originally proved activity.
				seenAt = co.AnsweredAt
			}
			if seenAt == nil {
				continue
			}
			activity[login] = seenAt.UTC()
		}
		if carryCoActivity(next, activity) {
			changed = true
		}
	}
	return changed
}

// PreviewRound builds the round NewRound would create without changing state.
// Read-only decisions use it before enqueue so durable reviewer activity is
// visible on the same pass that first observes a replacement head.
func (s *State) PreviewRound(repo string, pr int, head string, now time.Time) Round {
	r := Round{
		Repo:       strings.ToLower(repo),
		PR:         pr,
		Head:       head,
		Phase:      PhaseQueued,
		EnqueuedAt: now.UTC(),
	}
	s.carryCoActivity(&r)
	return r
}

// EndRound abandons the current round (superseded/closed/cancelled) and moves
// it to the archive. The PR has no round afterwards.
func (s *State) EndRound(repo string, pr int, reason string) {
	key := Key(repo, pr)
	r, ok := s.Rounds[key]
	if !ok {
		return
	}
	r.Abandon(reason)
	s.rememberCoActivity(r)
	delete(s.Rounds, key)
	s.Archive = append(s.Archive, r)
	if len(s.Archive) > ArchiveMax {
		s.Archive = s.Archive[len(s.Archive)-ArchiveMax:]
	}
}

// Supersede replaces the round for repo#pr with a fresh queued round at the
// new head, archiving the old one. It is the ONLY way a round's head changes.
func (s *State) Supersede(repo string, pr int, head string, now time.Time) (*Round, error) {
	s.EndRound(repo, pr, "superseded by "+head)
	return s.NewRound(repo, pr, head, now)
}

// SlotRound returns the round currently holding the fire slot, or nil. A slot
// whose round vanished or moved on is stale and is reported as nil (the
// caller clears it).
func (s *State) SlotRound() *Round {
	if s.FireSlot == nil {
		return nil
	}
	r, ok := s.Rounds[s.FireSlot.Key]
	if !ok || (r.Phase != PhaseReserved && r.Phase != PhaseFired) || r.Token != s.FireSlot.Token {
		return nil
	}
	return &r
}

// SlotHeld reports whether the fire slot is taken — the question every fire gate
// actually asks. A live round holds it; so does an orphaned hold, left behind
// when the round that posted a still-unacknowledged metered command was
// superseded by a new head. Without the second case the expected push after a
// round that converged without its primary would free the slot for a second
// concurrent metered command.
func (s *State) SlotHeld(now time.Time) bool {
	if s.FireSlot != nil && s.SlotRound() != nil {
		return true
	}
	if s.FireSlotHoldUntil != nil && s.FireSlotHoldUntil.After(now) {
		return true
	}
	return s.FireSlot != nil && s.FireSlot.HoldUntil != nil && s.FireSlot.HoldUntil.After(now)
}

// HoldSlotUntil keeps the current fire slot held past the round that owns it.
// The caller sets the deadline, since only it knows the in-flight window the
// command this slot was taken for is bounded by.
func (s *State) HoldSlotUntil(until time.Time) {
	if s.FireSlot == nil {
		return
	}
	u := until.UTC()
	before := s.LastFired
	if s.FireSlotHoldUntil != nil && s.LastFired != nil &&
		s.LastFired.Equal(*s.FireSlotHoldUntil) {
		before = s.FireSlotHoldLastFired
	}
	s.FireSlot.HoldUntil = &u
	s.FireSlotHoldUntil = &u
	s.FireSlotHoldLastFired = before
	// The previous binary preserves the top-level mirror but does not read it.
	// It does read LastFired, so a future pacing anchor keeps its Pump from
	// posting another metered review. Its MinInterval may conservatively extend
	// the wait; a current binary restores the real anchor below.
	if s.LastFired == nil || s.LastFired.Before(u) {
		s.LastFired = &u
	}
}

// ClearSlotHold removes the compatibility hold and restores the pacing anchor
// it temporarily replaced. If another writer fired after the deadline,
// LastFired no longer equals the synthetic value and is left untouched.
func (s *State) ClearSlotHold() {
	if s.FireSlotHoldUntil != nil && s.LastFired != nil &&
		s.LastFired.Equal(*s.FireSlotHoldUntil) {
		s.LastFired = s.FireSlotHoldLastFired
	}
	s.FireSlotHoldUntil = nil
	s.FireSlotHoldLastFired = nil
}

// NextEligible returns the fire-eligible round with the lowest Seq, or nil.
func (s *State) NextEligible(now time.Time) *Round {
	var best *Round
	for key := range s.Rounds {
		r := s.Rounds[key]
		// Held PRs are skipped HERE, in the one place a round is chosen to fire,
		// rather than at each caller. An exemption that has to be remembered at
		// every site is one that will be missed at one of them.
		if !r.FireEligible(now) || s.isHeld(r) {
			continue
		}
		if best == nil || r.Seq < best.Seq {
			c := r
			best = &c
		}
	}
	return best
}

// QueuedRounds returns every fire-eligible round ordered by Seq (dashboard).
func (s *State) QueuedRounds(now time.Time) []Round {
	var out []Round
	for _, r := range s.Rounds {
		if r.FireEligible(now) && !s.isHeld(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// DispatchTTL is how long a dispatch claim survives without a heartbeat. A fix
// session outlives any single poll, so the claim is refreshed while one runs;
// this bounds how long a crashed watcher blocks the next attempt.
const DispatchTTL = 10 * time.Minute

// DispatchClaim records who is running a fix for a round, and how many attempts
// this head has had.
type DispatchClaim struct {
	Host string `json:"host"`
	// OnePass binds this session to the campaign policy that granted it. A
	// repository setting can change while the process runs; completion must not
	// reinterpret an older ordinary session as the campaign's merge hand-off.
	OnePass bool `json:"one_pass,omitempty"`
	// OnePassCampaign prevents a fixer finishing after one-pass was disabled
	// from recreating a hand-off in a later campaign.
	OnePassCampaign string `json:"one_pass_campaign,omitempty"`
	// Token distinguishes two claims from the same host, so a restarted watcher
	// cannot heartbeat or release the claim of the process it replaced.
	Token     string    `json:"token"`
	At        time.Time `json:"at"`
	Heartbeat time.Time `json:"heartbeat"`
	// Attempts counts dispatches in THIS head's current attempt cycle. It
	// survives release; the cycle resets after a cooldown or on a new head.
	Attempts int `json:"attempts,omitempty"`
	// Model is the model selected for this claim. ModelCursor points at the next
	// ranked fallback, so ordinary failures rotate rather than hammering one
	// model. UnavailableModels parks provider/model outages until their retry
	// time without spending the head's fix-attempt budget.
	Model             string               `json:"model,omitempty"`
	ModelCursor       int                  `json:"model_cursor,omitempty"`
	UnavailableModels map[string]time.Time `json:"unavailable_models,omitempty"`
	LastFailure       string               `json:"last_failure,omitempty"`
	// Clarification is a head-scoped terminal stop requested by the agent. It
	// is separate from an administrative hold because standalone watch/autofix
	// has no autoreview leader capable of enforcing those holds.
	Clarification string `json:"clarification,omitempty"`
	// AttemptResetAt makes the safety bound a cooldown rather than a permanent
	// dead letter. Exhaustions lengthens repeated cooldowns, capped at a day.
	AttemptResetAt *time.Time `json:"attempt_reset_at,omitempty"`
	Exhaustions    int        `json:"exhaustions,omitempty"`
	// Findings is how many the session set out to fix, and Log is where its
	// output is going. Both are for the reader: "attempt 2" says nothing about
	// whether that is nearly the last one, and a session with no visible log is
	// one you cannot check on.
	Findings int    `json:"findings,omitempty"`
	Log      string `json:"log,omitempty"`

	// unknown carries members a newer binary wrote inside this record. The maps
	// holding claims are recognised by name, so only the claim itself can carry
	// one. See tolerant.go.
	unknown unknownFields
}

// DispatchHeld reports whether a live claim exists.
func (r *Round) DispatchHeld(now time.Time) bool {
	return r.Dispatch != nil && !r.Dispatch.Heartbeat.IsZero() && now.UTC().Sub(r.Dispatch.Heartbeat) < DispatchTTL
}

// ArchivedDispatchHeld reports whether a round of repo#pr that has already been
// archived still holds a live dispatch claim.
//
// A session's own push is what supersedes the round it was fixing, and the claim
// is archived with it while the session is still running — resolving threads,
// declining others. The current round is then a fresh one carrying no claim at
// all, so asking it alone answers "nobody is fixing this" about a pull request
// somebody is very much still fixing, and a second session is launched into the
// same worktree generation's work.
func (s *State) ArchivedDispatchHeld(repo string, pr int, now time.Time) bool {
	key := Key(repo, pr)
	if claim, ok := s.Dispatches[key]; ok && claim.Live(now) {
		return true
	}
	for i := range s.Archive {
		r := &s.Archive[i]
		if Key(r.Repo, r.PR) == key && r.DispatchHeld(now) {
			return true
		}
	}
	return false
}

// LiveDispatchUntil reports when the latest live unattended claim for repo#pr
// expires. A session can move its claim into the archive when it pushes, so
// callers coordinating work must consider the current round, mirror, and
// archived rounds together.
func (s *State) LiveDispatchUntil(repo string, pr int, now time.Time) (time.Time, bool) {
	var until time.Time
	consider := func(claim *DispatchClaim) {
		if claim == nil || !claim.Live(now) {
			return
		}
		expires := claim.Heartbeat.Add(DispatchTTL)
		if expires.After(until) {
			until = expires
		}
	}

	key := Key(repo, pr)
	if claim, ok := s.Dispatches[key]; ok {
		consider(&claim)
	}
	if round := s.Round(repo, pr); round != nil {
		consider(round.Dispatch)
	}
	for i := range s.Archive {
		round := &s.Archive[i]
		if Key(round.Repo, round.PR) == key {
			consider(round.Dispatch)
		}
	}
	return until, !until.IsZero()
}

// OwnsLiveDispatch reports whether token is the unattended session currently
// entitled to work on repo#pr. The claim may be on the current round, its
// top-level mirror, or an archived round after the session pushed a new head.
func (s *State) OwnsLiveDispatch(repo string, pr int, token string, now time.Time) bool {
	if token == "" {
		return false
	}
	key := Key(repo, pr)
	if claim, ok := s.Dispatches[key]; ok && claim.Token == token && claim.Live(now) {
		return true
	}
	if round := s.Round(repo, pr); round != nil && round.Dispatch != nil &&
		round.Dispatch.Token == token && round.DispatchHeld(now) {
		return true
	}
	for i := range s.Archive {
		round := &s.Archive[i]
		if Key(round.Repo, round.PR) == key && round.Dispatch != nil &&
			round.Dispatch.Token == token && round.DispatchHeld(now) {
			return true
		}
	}
	return false
}

// HeartbeatArchivedDispatch refreshes the archived claim owned by token. A
// successful session archives its own round when its push moves the head, but
// it still owns the PR until thread resolution and the process both finish.
func (s *State) HeartbeatArchivedDispatch(repo string, pr int, token string, now time.Time) (ok, taken bool) {
	key := Key(repo, pr)
	if claim, exists := s.Dispatches[key]; exists {
		if claim.Token == token {
			// The CAS update containing this heartbeat also serializes a safe
			// reacquisition after an extended state outage. If a replacement
			// watcher won first, its different live token is observed below.
			claim.Heartbeat = now.UTC()
			s.Dispatches[key] = claim
			return true, false
		}
		if claim.Live(now) {
			return false, true
		}
	}
	for i := range s.Archive {
		r := &s.Archive[i]
		if Key(r.Repo, r.PR) != key {
			continue
		}
		if refreshed, byOther := r.HeartbeatDispatch(token, now); refreshed {
			if s.Dispatches == nil {
				s.Dispatches = map[string]DispatchClaim{}
			}
			s.Dispatches[key] = *r.Dispatch
			return true, false
		} else if byOther {
			taken = true
		}
	}
	return false, taken
}

// ReleaseArchivedDispatch drops a claim this token owns on an archived round, so
// a session that has finished stops holding the next one out for the rest of the
// TTL. It reports whether anything was released.
func (s *State) ReleaseArchivedDispatch(repo string, pr int, token string) bool {
	key := Key(repo, pr)
	released := false
	if claim, ok := s.Dispatches[key]; ok && claim.Token == token {
		delete(s.Dispatches, key)
		released = true
	}
	for i := range s.Archive {
		r := &s.Archive[i]
		if Key(r.Repo, r.PR) == key && r.ReleaseDispatch(token) {
			released = true
		}
	}
	return released
}

// MarkArchivedDispatchUnavailable records a provider outage on the archived
// claim owned by token. A session can outlive the head it started on, so the
// current round is not necessarily the claim owner.
func (s *State) MarkArchivedDispatchUnavailable(repo string, pr int, token string, until time.Time, reason string) bool {
	key := Key(repo, pr)
	marked := false
	if claim, ok := s.Dispatches[key]; ok && claim.Token == token {
		claim.markUnavailable(until, reason)
		s.Dispatches[key] = claim
		marked = true
	}
	for i := range s.Archive {
		r := &s.Archive[i]
		if Key(r.Repo, r.PR) == key && r.MarkDispatchUnavailable(token, until, reason) {
			marked = true
		}
	}
	return marked
}

// RememberDispatch stores a newly granted claim independently of its round.
func (s *State) RememberDispatch(repo string, pr int, claim DispatchClaim) {
	if s.Dispatches == nil {
		s.Dispatches = map[string]DispatchClaim{}
	}
	s.Dispatches[Key(repo, pr)] = claim
}

// OnePassDispatch reports whether token owns a claim created by one-pass mode.
// The independent claim map survives the session pushing and archiving its
// round, while the round scans preserve compatibility with older state shapes.
func (s *State) OnePassDispatch(repo string, pr int, token string) bool {
	onePass, _ := s.OnePassDispatchCampaign(repo, pr, token)
	return onePass
}

// OnePassDispatchCampaign returns the campaign identity recorded when token's
// one-pass claim was granted.
func (s *State) OnePassDispatchCampaign(repo string, pr int, token string) (bool, string) {
	key := Key(repo, pr)
	if claim, ok := s.Dispatches[key]; ok && claim.Token == token {
		return claim.OnePass, claim.OnePassCampaign
	}
	if round := s.Round(repo, pr); round != nil && round.Dispatch != nil && round.Dispatch.Token == token {
		return round.Dispatch.OnePass, round.Dispatch.OnePassCampaign
	}
	for i := range s.Archive {
		round := &s.Archive[i]
		if Key(round.Repo, round.PR) == key && round.Dispatch != nil && round.Dispatch.Token == token {
			return round.Dispatch.OnePass, round.Dispatch.OnePassCampaign
		}
	}
	return false, ""
}

// Live reports whether a session is still behind this claim: a heartbeat within
// DispatchTTL. It is the same predicate dispatch ownership uses, exported so a
// reader — the dashboard — cannot show a crashed watcher's claim as a running
// session for ever.
func (c DispatchClaim) Live(now time.Time) bool {
	return !c.Heartbeat.IsZero() && now.UTC().Sub(c.Heartbeat) < DispatchTTL
}

// ClaimDispatch takes this round's dispatch claim, or reports why it cannot. A
// claim past its TTL is taken over, keeping the attempt count: that session died,
// but its attempt still happened.
func (r *Round) ClaimDispatch(host, token string, now time.Time, maxAttempts int) (bool, string) {
	return r.ClaimDispatchModels(host, token, now, maxAttempts, nil)
}

// ClaimDispatchModels claims a fix session and selects the first currently
// available model in ranking order. An empty ranking is one selectable entry:
// the agent's own default.
func (r *Round) ClaimDispatchModels(host, token string, now time.Time, maxAttempts int, models []string) (bool, string) {
	now = now.UTC()
	attempts := 0
	cursor := 0
	unavailable := map[string]time.Time{}
	lastFailure := ""
	var attemptResetAt *time.Time
	exhaustions := 0
	var unknown unknownFields
	if r.Dispatch != nil {
		if r.DispatchHeld(now) {
			return false, "another watcher is already fixing this round"
		}
		if r.Dispatch.Clarification != "" {
			return false, "autofix needs clarification: " + r.Dispatch.Clarification
		}
		attempts = r.Dispatch.Attempts
		cursor = r.Dispatch.ModelCursor
		lastFailure = r.Dispatch.LastFailure
		attemptResetAt = r.Dispatch.AttemptResetAt
		exhaustions = r.Dispatch.Exhaustions
		unknown = r.Dispatch.unknown
		for model, until := range r.Dispatch.UnavailableModels {
			if until.After(now) {
				unavailable[model] = until.UTC()
			}
		}
	}
	if maxAttempts > 0 && attempts >= maxAttempts {
		resetAt := r.Dispatch.At.Add(dispatchAttemptCooldown(exhaustions))
		if attemptResetAt != nil {
			resetAt = attemptResetAt.UTC()
		}
		if now.Before(resetAt) {
			return false, fmt.Sprintf(
				"%d dispatch attempts already made in this cycle; retries resume at %s",
				attempts, resetAt.Format(time.RFC3339),
			)
		}
		attempts = 0
		attemptResetAt = nil
		exhaustions++
	}
	if len(models) == 0 {
		models = []string{""}
	}
	if cursor < 0 || cursor >= len(models) {
		cursor = 0
	}
	selected, selectedAt := "", -1
	var earliest time.Time
	for step := 0; step < len(models); step++ {
		i := (cursor + step) % len(models)
		model := strings.TrimSpace(models[i])
		if until, blocked := unavailable[model]; blocked {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
			continue
		}
		selected, selectedAt = model, i
		break
	}
	if selectedAt < 0 {
		return false, fmt.Sprintf("all configured models are temporarily unavailable until %s", earliest.Format(time.RFC3339))
	}
	r.beginDispatchHold(now)
	r.Dispatch = &DispatchClaim{
		Host: host, Token: token, At: now, Heartbeat: now,
		Attempts: attempts + 1, Model: selected,
		ModelCursor:       (selectedAt + 1) % len(models),
		UnavailableModels: unavailable, LastFailure: lastFailure,
		AttemptResetAt: attemptResetAt, Exhaustions: exhaustions,
		unknown: unknown,
	}
	if maxAttempts > 0 && r.Dispatch.Attempts >= maxAttempts {
		resetAt := now.Add(dispatchAttemptCooldown(exhaustions))
		r.Dispatch.AttemptResetAt = &resetAt
	}
	return true, ""
}

func dispatchAttemptCooldown(exhaustions int) time.Duration {
	cooldown := time.Hour
	for i := 0; i < exhaustions && cooldown < 24*time.Hour; i++ {
		cooldown *= 2
	}
	if cooldown > 24*time.Hour {
		return 24 * time.Hour
	}
	return cooldown
}

// MarkDispatchUnavailable records a provider/model outage against the selected
// claim and refunds its attempt. It deliberately leaves ModelCursor advanced,
// so the next claim tries the next ranked model.
func (r *Round) MarkDispatchUnavailable(token string, until time.Time, reason string) bool {
	if r.Dispatch == nil || r.Dispatch.Token != token {
		return false
	}
	r.Dispatch.markUnavailable(until, reason)
	return true
}

func (c *DispatchClaim) markUnavailable(until time.Time, reason string) {
	if c.UnavailableModels == nil {
		c.UnavailableModels = map[string]time.Time{}
	}
	c.UnavailableModels[c.Model] = until.UTC()
	c.LastFailure = reason
	if c.Attempts > 0 {
		c.Attempts--
	}
	c.AttemptResetAt = nil
}

// beginDispatchHold mirrors the new dispatch exclusion into queue state every
// older binary already knows. The original cooldown is restored on release.
func (r *Round) beginDispatchHold(now time.Time) {
	if r.DispatchHoldPhase == "" && (r.Phase == PhaseQueued || r.Phase == PhaseAwaitingRetry) {
		r.DispatchHoldPhase = r.Phase
		if r.RetryAt != nil {
			at := r.RetryAt.UTC()
			r.DispatchHoldRetryAt = &at
		}
	}
	if r.DispatchHoldPhase == "" || (r.Phase != PhaseQueued && r.Phase != PhaseAwaitingRetry) {
		return
	}
	until := now.UTC().Add(DispatchTTL)
	r.Phase = PhaseAwaitingRetry
	r.RetryAt = &until
}

// HeartbeatDispatch refreshes a claim this token owns.
//
// The two ways it can fail are NOT the same thing, and treating them alike is
// what made a session destroy its own work. taken is true only when somebody
// else holds a live claim — the case where continuing would put two sessions in
// one worktree. A claim that is merely GONE means the round was superseded,
// which is what a session's own push does: it moves the head, the round is
// archived, and the fresh round carries no claim. Killing the session there ends
// it between pushing and resolving, every time it succeeds.
func (r *Round) HeartbeatDispatch(token string, now time.Time) (ok, taken bool) {
	if r.Dispatch == nil {
		return false, false
	}
	if r.Dispatch.Token != token {
		return false, r.DispatchHeld(now)
	}
	r.Dispatch.Heartbeat = now.UTC()
	r.beginDispatchHold(now)
	return true, false
}

// ReleaseDispatch drops a claim this token owns, keeping the attempt count.
func (r *Round) ReleaseDispatch(token string) bool {
	if r.Dispatch == nil || r.Dispatch.Token != token {
		return false
	}
	r.Dispatch.Heartbeat = time.Time{}
	r.Dispatch.Token = ""
	if r.DispatchHoldPhase != "" {
		// An archived round was abandoned when it was superseded. Never revive it
		// into the queue just because its session finished.
		if r.Phase == PhaseAwaitingRetry {
			r.Phase = r.DispatchHoldPhase
			r.RetryAt = r.DispatchHoldRetryAt
		}
		r.DispatchHoldPhase = ""
		r.DispatchHoldRetryAt = nil
	}
	return true
}

// MarkDispatchClarification ends token's live claim while retaining a terminal
// reason on this head. A new head gets a new Round and may dispatch normally.
func (r *Round) MarkDispatchClarification(token, question string) bool {
	if r.Dispatch == nil || r.Dispatch.Token != token {
		return false
	}
	r.Dispatch.Clarification = strings.TrimSpace(question)
	if r.Dispatch.Attempts > 0 {
		r.Dispatch.Attempts--
	}
	r.Dispatch.AttemptResetAt = nil
	return r.ReleaseDispatch(token)
}

// AutofixUnhealthyAfter is how many consecutive dispatch attempts may fail to
// start a session before crq says so out loud. One failure is a transient; three
// in a row is a dispatcher that is not working.
//
// Attempts, not passes: sessions outlive the pass that started them, so their
// outcomes arrive one at a time and any success resets the count.
const AutofixUnhealthyAfter = 3

// AutofixHealthTTL is how long a host that reports no dispatch outcome remains
// part of the fleet health summary. A retired or renamed host cannot report its
// own recovery, so retaining it forever would make one historical failure the
// permanent fleet verdict.
const AutofixHealthTTL = 24 * time.Hour

// AutofixHealth is the watcher's dispatch record: whether fix sessions are
// actually starting.
//
// A dispatch failure used to be a line in a log nobody read, so a wedged git
// mirror stopped every session for hours while the queue looked busy. Counting
// it here puts it on the dashboard and in the status line instead.
type AutofixHealth struct {
	Host                string     `json:"host,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
}

// Unhealthy reports whether dispatch has failed enough times in a row to be
// worth someone's attention.
func (d *AutofixHealth) Unhealthy() bool {
	return d != nil && d.ConsecutiveFailures >= AutofixUnhealthyAfter
}

// NoteDispatch records one dispatch attempt's outcome: whether a session
// started, and the reason if it did not.
func (s *State) NoteDispatch(host string, started bool, reason string, now time.Time) {
	if s.AutofixByHost == nil {
		s.AutofixByHost = map[string]AutofixHealth{}
		if s.Autofix != nil && s.Autofix.Host != "" {
			s.AutofixByHost[s.Autofix.Host] = *s.Autofix
		}
	}
	at := now.UTC()
	health := s.AutofixByHost[host]
	health.Host = host
	if started {
		health.ConsecutiveFailures = 0
		health.LastError = ""
		health.LastSuccessAt = &at
	} else {
		health.ConsecutiveFailures++
		health.LastError = reason
		health.LastFailureAt = &at
	}
	s.AutofixByHost[host] = health
	s.summarizeAutofix(now)
}

func (s *State) summarizeAutofix(now time.Time) {
	var summary *AutofixHealth
	for host, health := range s.AutofixByHost {
		latest := health.LastFailureAt
		if timeAfter(health.LastSuccessAt, latest) {
			latest = health.LastSuccessAt
		}
		if latest != nil && !latest.Add(AutofixHealthTTL).After(now.UTC()) {
			delete(s.AutofixByHost, host)
			continue
		}
		candidate := health
		if summary == nil ||
			candidate.ConsecutiveFailures > summary.ConsecutiveFailures ||
			(candidate.ConsecutiveFailures == summary.ConsecutiveFailures &&
				(timeAfter(candidate.LastFailureAt, summary.LastFailureAt) ||
					(timeEqual(candidate.LastFailureAt, summary.LastFailureAt) &&
						candidate.Host < summary.Host))) {
			summary = &candidate
		}
	}
	s.Autofix = summary
}

func timeAfter(a, b *time.Time) bool {
	return a != nil && (b == nil || a.After(*b))
}

func timeEqual(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// Hold records why a PR is held out of the review queue.
type Hold struct {
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
}

// Hold marks repo#pr as not to be reviewed, replacing any earlier hold.
func (s *State) Hold(repo string, pr int, reason, by string, now time.Time) {
	s.HoldWithToken(repo, pr, reason, by, now, "")
}

// HoldWithToken records a hold and the operation that created it. A caller
// posting a notice can use the token to tell whether that notice was superseded
// while GitHub was processing it.
func (s *State) HoldWithToken(repo string, pr int, reason, by string, now time.Time, token string) {
	if s.Holds == nil {
		s.Holds = map[string]Hold{}
	}
	key := holdKey(repo, pr)
	s.Holds[key] = Hold{Reason: reason, By: by, At: now.UTC()}
	if token == "" {
		delete(s.HoldTokens, key)
		return
	}
	if s.HoldTokens == nil {
		s.HoldTokens = map[string]string{}
	}
	s.HoldTokens[key] = token
}

// Unhold releases a hold, reporting whether one was there.
func (s *State) Unhold(repo string, pr int) bool {
	key := holdKey(repo, pr)
	if _, ok := s.Holds[key]; !ok {
		return false
	}
	delete(s.Holds, key)
	delete(s.HoldTokens, key)
	return true
}

// HeldPR reports whether repo#pr is held, and why.
func (s *State) HeldPR(repo string, pr int) (Hold, bool) {
	h, ok := s.Holds[holdKey(repo, pr)]
	return h, ok
}

// HoldToken reports the operation that created the current hold, if it was
// created by a token-aware writer.
func (s *State) HoldToken(repo string, pr int) string {
	return s.HoldTokens[holdKey(repo, pr)]
}

func (s *State) isHeld(r Round) bool {
	_, held := s.HeldPR(r.Repo, r.PR)
	return held
}

func holdKey(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", normalizeRepoKey(repo), pr)
}

// RecordPosted remembers a trigger comment crq WROTE for this round, addressed
// to bot. Call it only where crq actually posted: an adopted command is not
// crq's to record, and later cleanup trusts this list as proof of authorship.
//
// Bounded, so a PR that retries all day cannot grow its round without limit.
func (r *Round) RecordPosted(bot string, id int64, at time.Time) {
	const maxPosted = 50
	if id == 0 {
		return
	}
	for _, have := range r.PostedCommands {
		if have.ID == id {
			return
		}
	}
	// Copy-on-write, like setCo: Rounds are passed around by value, so appending
	// in place could write this entry into a sibling copy's backing array.
	posted := make([]PostedCommand, len(r.PostedCommands), len(r.PostedCommands)+1)
	copy(posted, r.PostedCommands)
	posted = append(posted, PostedCommand{ID: id, Bot: bot, At: at.UTC()})
	if len(posted) > maxPosted {
		posted = posted[len(posted)-maxPosted:]
	}
	r.PostedCommands = posted
}

// RecordTidied remembers a trigger before Tidy removes it from GitHub. It is
// idempotent because a CAS retry may apply the same mutation more than once.
func (s *State) RecordTidied(repo string, pr int, commands ...PostedCommand) {
	if len(commands) == 0 {
		return
	}
	if s.TidiedCommands == nil {
		s.TidiedCommands = map[string][]PostedCommand{}
	}
	key := Key(repo, pr)
	have := make(map[int64]bool, len(s.TidiedCommands[key])+len(commands))
	for _, command := range s.TidiedCommands[key] {
		have[command.ID] = true
	}
	for _, command := range commands {
		if command.ID == 0 || have[command.ID] {
			continue
		}
		command.At = command.At.UTC()
		s.TidiedCommands[key] = append(s.TidiedCommands[key], command)
		have[command.ID] = true
	}
}

// ForgetTidied removes tombstones for comments Tidy ultimately kept or failed
// to delete. A present GitHub comment remains the source of truth for those.
func (s *State) ForgetTidied(repo string, pr int, ids ...int64) {
	key := Key(repo, pr)
	if len(ids) == 0 || len(s.TidiedCommands[key]) == 0 {
		return
	}
	remove := make(map[int64]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}
	kept := s.TidiedCommands[key][:0]
	for _, command := range s.TidiedCommands[key] {
		if !remove[command.ID] {
			kept = append(kept, command)
		}
	}
	if len(kept) == 0 {
		delete(s.TidiedCommands, key)
		if len(s.TidiedCommands) == 0 {
			s.TidiedCommands = nil
		}
		return
	}
	s.TidiedCommands[key] = kept
}

// Dismiss records a finding ID as accounted for. Returns false when it was
// already dismissed, so a caller can tell a repeat from a new decision.
func (r *Round) Dismiss(id, reason string) bool {
	if id == "" || r.IsDismissed(id) {
		return false
	}
	if r.Dismissed == nil {
		r.Dismissed = map[string]string{}
	}
	r.Dismissed[id] = reason
	return true
}

// IsDismissed reports whether this round already accounted for the finding.
func (r *Round) IsDismissed(id string) bool {
	_, ok := r.Dismissed[id]
	return ok
}

// Queue-entry wait reasons. A waiting round is held by exactly one of these
// (or by nothing, in which case it is next up).
const (
	WaitCoolingDown    = "cooling down"    // this round's own RetryAt has not passed
	WaitAccountBlocked = "account blocked" // the CodeRabbit account quota window dominates
	WaitSlotBusy       = "slot busy"       // another PR's review holds the fire slot
	WaitPacing         = "pacing"          // LastFired + CRQ_MIN_INTERVAL has not passed
	WaitBehind         = "behind an earlier round"
)

// QueueEntry is one round waiting for a fire, plus when it can actually fire
// and what is holding it. It is a VIEW over Rounds + Account — never persisted,
// so it carries no schema obligations.
type QueueEntry struct {
	Round
	// ReadyAt is the earliest time this round may fire. Zero means "now".
	ReadyAt time.Time
	// Why is the gate holding it: one of the Wait* constants, or "" when the
	// round is simply next up.
	Why string
}

// roundReadyAt is a waiting round's OWN not-before time: RetryAt while it is
// cooling down, zero (ready now) while it is merely queued.
func roundReadyAt(r Round) time.Time {
	if r.Phase == PhaseAwaitingRetry && r.RetryAt != nil {
		return r.RetryAt.UTC()
	}
	return time.Time{}
}

// Queue returns every round waiting for a fire — queued AND awaiting_retry — in
// the order they will actually reach the slot: by ready time, then by Seq.
//
// There is only one queue. A cooling-down round is not a different species of
// work, it is a queued round with a not-before time, and rendering it as a
// separate list left "nothing queued" and "two PRs parked until 00:07Z" looking
// identical. Ordering by (ReadyAt, Seq) reproduces what firing actually does: a
// round whose window has not opened cannot precede a ready one, and among ready
// rounds NextEligible takes the lowest Seq.
//
// ReadyAt folds in the account-wide quota block, because DecideFire gates on it
// too — showing a round's own RetryAt alone promises a time the fire gate will
// not honour once CodeRabbit extends the window. This is the same max() that
// AccountBlockedUntil computes for the wait path.
//
// minInterval is folded in too. It is DecideFire's pacing gate, so leaving it out
// rendered a round "ready: now" that firing would refuse for up to another
// CRQ_MIN_INTERVAL — 90s by default and configurable far longer. It is an
// absolute boundary (LastFired + minInterval), not a countdown, so surfacing it
// does not churn DashboardSHA between renders.
func (s *State) Queue(now time.Time, minInterval time.Duration) []QueueEntry {
	var blocked time.Time
	if s.Account.BlockedUntil != nil && s.Account.BlockedUntil.After(now) {
		blocked = s.Account.BlockedUntil.UTC()
	}
	liveSlotBusy := s.SlotRound() != nil
	orphanSlotBusy := !liveSlotBusy && s.SlotHeld(now)
	// The pacing gate applies to whichever round fires next, so it bounds every
	// entry's earliest possible start.
	var paced time.Time
	if minInterval > 0 && s.LastFired != nil {
		if at := s.LastFired.Add(minInterval).UTC(); at.After(now) {
			paced = at
		}
	}

	// Split by whether the round is waiting for anything the queue serializes.
	//
	// A co-only round is not. It spends no account quota, takes no fire slot, and
	// DecideFire resolves it before either gate — so the account window, the slot,
	// and its position behind other rounds are all irrelevant to it. Exempting it
	// gate-by-gate was tried and missed a different spot three times running; the
	// partition makes the exemption structural, so a gate added later cannot
	// silently apply to work it does not govern.
	var queued, freeRunning []QueueEntry
	for _, r := range s.Rounds {
		if r.Phase != PhaseQueued && r.Phase != PhaseAwaitingRetry {
			continue
		}
		// A round a fix session holds is not waiting its turn either: the claim
		// is what keeps it out of the queue, and showing it invites somebody to
		// wonder why nothing is taking it.
		if r.DispatchHeld(now) {
			continue
		}

		// A held round is not waiting its turn — nothing will take it — so
		// showing it in the queue tells the reader the opposite of the truth.
		if s.isHeld(r) {
			continue
		}
		e := QueueEntry{Round: r}
		// Its own cooldown binds either way: that is the round's, not the queue's.
		if own := roundReadyAt(r); own.After(now) {
			e.ReadyAt, e.Why = own, WaitCoolingDown
		}
		if r.CoOnly {
			freeRunning = append(freeRunning, e)
			continue
		}
		// Every remaining gate is a lower bound on when firing will accept this
		// round, so the ready time is the LATEST of them and the reason is
		// whichever binds. Taking the first that matched let a one-minute cooldown
		// hide a two-hour account block.
		for _, g := range []struct {
			at  time.Time
			why string
		}{{blocked, WaitAccountBlocked}, {paced, WaitPacing}} {
			if g.at.After(now) && g.at.After(e.ReadyAt) {
				e.ReadyAt, e.Why = g.at, g.why
			}
		}
		queued = append(queued, e)
	}

	// A live slot stops EVERYTHING, free-running rounds included.
	//
	// Not because they need the slot — they do not — but because Pump returns as
	// soon as it sees a slot holder, so the quota-free path that would advance
	// them is never reached while one is held. The exemption above is about the
	// account window, which genuinely does not apply to them; claiming they are
	// ready here would promise action the daemon cannot take until the holder is
	// acknowledged. (An agent's own `crq next` can still resolve such a round
	// directly, which is why this describes the queue rather than forbidding it.)
	if liveSlotBusy {
		for i := range queued {
			queued[i].ReadyAt, queued[i].Why = time.Time{}, WaitSlotBusy
		}
		for i := range freeRunning {
			freeRunning[i].ReadyAt, freeRunning[i].Why = time.Time{}, WaitSlotBusy
		}
	} else if orphanSlotBusy {
		// An orphaned bounded hold still blocks another metered fire, but Pump
		// deliberately scans past it for quota-free work. Reflect that split:
		// ordinary rounds wait on the slot while co-only rounds remain ready.
		for i := range queued {
			queued[i].ReadyAt, queued[i].Why = time.Time{}, WaitSlotBusy
		}
	}

	// One list, ordered by readiness then Seq — which is NextEligible's own rule
	// among rounds that are eligible together. Concatenating the groups instead
	// put a cooling co-only round ahead of work that could fire immediately.
	out := append(freeRunning, queued...)
	// An empty ReadyAt means two different things and they must not sort alike:
	// nothing is holding this round, or something is holding it whose end is
	// unknowable (a slot another PR holds). Rank them apart, or a slot-blocked
	// round sorts as if it were ready and can take the front.
	rank := func(e QueueEntry) int {
		switch {
		case e.ReadyAt.IsZero() && e.Why == "":
			return 0 // fire-eligible now
		case !e.ReadyAt.IsZero():
			return 1 // waiting until a known time
		default:
			return 2 // waiting on something with no knowable end
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		if !out[i].ReadyAt.Equal(out[j].ReadyAt) {
			return out[i].ReadyAt.Before(out[j].ReadyAt)
		}
		return out[i].Seq < out[j].Seq
	})

	// Say which round is next ONLY when one is eligible now — then it is the
	// lowest-Seq ready round, exactly what NextEligible picks. With everything
	// still cooling, which one fires depends on when a pump happens to run: if
	// none runs between two retry times, both are eligible at the next pass and
	// the lower Seq wins, not the earlier window. So no front is claimed, and the
	// soonest opening is reported without saying whose it is.
	//
	// Only that front carries a time. Anything behind it starts when the front
	// finishes, which is the unknowable part, so naming its own gate there would
	// state a lower bound as if it were the answer.
	front := len(out) > 0 && rank(out[0]) == 0
	for i := range out {
		if front && i == 0 {
			continue // the front is the one round whose readiness is knowable
		}
		if !front && i == 0 {
			continue // nothing is eligible; the soonest opening is still worth naming
		}
		// Drop the TIME but keep the reason. A round's own gate is real and worth
		// reporting — "slot busy", "account blocked" — it just is not a start time,
		// because the round ahead has to finish first. Only a round with nothing of
		// its own holding it is purely behind.
		out[i].ReadyAt = time.Time{}
		if out[i].Why == "" {
			out[i].Why = WaitBehind
		}
	}
	return out
}

// Normalize repairs invariants after load: map init, expired retry windows
// (awaiting_retry with a passed RetryAt is simply fire-eligible; nothing to
// do), and a FireSlot no round holds and no orphaned hold keeps alive.
func (s *State) Normalize(now time.Time) {
	s.normalize(now)
}

// normalize reports whether it retired merged-PR evidence while repairing the
// loaded state. The git store uses that signal to persist this specific repair
// even when the requested mutation is otherwise a no-op.
func (s *State) normalize(now time.Time) (retiredMergedEvidence bool) {
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	if s.ReviewedHeads == nil {
		s.ReviewedHeads = map[string][]string{}
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.Autofix != nil && s.Autofix.Host != "" {
		if s.AutofixByHost == nil {
			s.AutofixByHost = map[string]AutofixHealth{}
		}
		if _, ok := s.AutofixByHost[s.Autofix.Host]; !ok {
			s.AutofixByHost[s.Autofix.Host] = *s.Autofix
		}
	}
	if s.AutofixByHost != nil {
		s.summarizeAutofix(now)
	}
	// Dispatches is the cross-archive ownership index, not attempt history.
	// Once a claim's heartbeat expires scheduling already treats it as free, so
	// retaining it can only grow state and let an older dashboard render a dead
	// session forever. The round (current or archived) retains the attempt
	// counters used by a replacement claim.
	for key, claim := range s.Dispatches {
		if !claim.Live(now) {
			delete(s.Dispatches, key)
		}
	}
	// An interrupted interactive loop must not dead-letter a PR. Unlike a
	// running dispatch there is no background process to heartbeat while an
	// agent edits, so the claim carries a generous fixed expiry and is renewed
	// by next/wait/loop as expiry approaches.
	for key, claim := range s.WorkClaims {
		if !claim.Live(now) {
			delete(s.WorkClaims, key)
		}
	}
	// Fold both hold representations before repairing the slot. The top-level
	// mirror may be the only copy left after a pre-FireSlot-tolerance binary
	// normalized and rewrote an orphaned hold.
	if s.FireSlot != nil && s.FireSlot.HoldUntil != nil &&
		(s.FireSlotHoldUntil == nil || s.FireSlotHoldUntil.Before(*s.FireSlot.HoldUntil)) {
		u := s.FireSlot.HoldUntil.UTC()
		s.FireSlotHoldUntil = &u
	}
	if s.FireSlotHoldUntil != nil && s.FireSlot != nil &&
		(s.FireSlot.HoldUntil == nil || s.FireSlot.HoldUntil.Before(*s.FireSlotHoldUntil)) {
		u := s.FireSlotHoldUntil.UTC()
		s.FireSlot.HoldUntil = &u
	}
	if !s.SlotHeld(now) {
		s.FireSlot = nil
		s.ClearSlotHold()
	}
	// Rebuild the per-PR indexes from the archive for state written by a binary
	// that did not keep them — except for merged PRs, whose indexes were retired
	// on purpose and must not be resurrected from their own archived rounds.
	merged := s.mergedKeys()
	for key := range merged {
		if _, ok := s.ReviewedHeads[key]; ok {
			retiredMergedEvidence = true
		}
		if _, ok := s.CoActivity[key]; ok {
			retiredMergedEvidence = true
		}
		if _, ok := s.CoAnswers[key]; ok {
			retiredMergedEvidence = true
		}
		delete(s.ReviewedHeads, key)
		delete(s.CoActivity, key)
		delete(s.CoAnswers, key)
	}
	for i := range s.Archive {
		s.Archive[i].foldLegacyCodex()
		s.Archive[i].inferCoOnly()
		if merged[Key(s.Archive[i].Repo, s.Archive[i].PR)] {
			continue
		}
		s.rememberCoActivity(s.Archive[i])
	}
	if len(s.Archive) > ArchiveMax {
		s.Archive = s.Archive[len(s.Archive)-ArchiveMax:]
	}
	for key, r := range s.Rounds {
		r.foldLegacyCodex()
		r.inferCoOnly()
		if merged[key] {
			s.Rounds[key] = r
			continue
		}
		s.rememberCoActivity(r)
		// During a rolling upgrade, an older writer can archive a round while
		// preserving SeenActiveAt as an unknown member, then create its
		// replacement without copying it. Repair that replacement on load, unless
		// its restored-activity wait already completed and consumed the gate.
		if s.carryCoActivity(&r) && r.Phase == PhaseCompleted && !r.PrimarySettled {
			_ = r.ReopenForRestoredActivity()
		}
		s.Rounds[key] = r
	}
	// Bootstrap the PR-wide ledger once when upgrading existing live rounds.
	// Presence, including an empty slice, means the cycle was already
	// initialized or deliberately reset and must not be rebuilt.
	for key, current := range s.Rounds {
		if merged[key] {
			continue
		}
		if _, initialized := s.ReviewedHeads[key]; initialized {
			continue
		}
		s.ReviewedHeads[key] = []string{}
		for _, archived := range s.Archive {
			if Key(archived.Repo, archived.PR) == key && roundHasReviewEvidence(archived) {
				s.NoteReviewedHead(archived.Repo, archived.PR, archived.Head)
			}
		}
		if roundHasReviewEvidence(current) {
			s.NoteReviewedHead(current.Repo, current.PR, current.Head)
		}
	}
	return retiredMergedEvidence
}

func roundHasReviewEvidence(r Round) bool {
	if r.PrimaryAnsweredAt != nil {
		return true
	}
	for _, co := range r.CoBots {
		if co.AnsweredAt != nil {
			return true
		}
	}
	return false
}

// --- round-native views consumed by crq's Wait/Loop ------------------------

// waitingHead returns the head a fired/reviewing round is currently waiting on
// (the wait IS the round), or "" when repo#pr has no active wait. Loop and Wait
// use it to tell "a review is in flight for this head" from "start a new round".
func (st *State) WaitingHead(repo string, pr int) string {
	r := st.Round(repo, pr)
	if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
		return ""
	}
	return r.Head
}

// roundWaitDeadline returns the wait deadline of the fired/reviewing round at
// head, if one is set. It is the wall-clock bound Loop polls against.
func (st *State) RoundWaitDeadline(repo string, pr int, head string) (time.Time, bool) {
	r := st.Round(repo, pr)
	if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) || r.WaitDeadline == nil {
		return time.Time{}, false
	}
	return r.WaitDeadline.UTC(), true
}

// containsActive reports whether repo#pr has a round still occupying its slot
// (queued through awaiting_retry) — the v2 State.Contains for the queue/inflight.
func (st *State) ContainsActive(repo string, pr int) bool {
	r := st.Round(repo, pr)
	return r != nil && r.Active()
}

// firedMarker returns the head for which repo#pr has already been requested and
// must not be re-fired without a new head — the v2 Fired[key] dedupe. A
// completed/expired round, or one still fired/reviewing, is such a marker; a
// parked awaiting_retry round is not (Pump re-fires it once RetryAt passes).
func (st *State) FiredMarker(repo string, pr int) string {
	r := st.Round(repo, pr)
	if r == nil {
		return ""
	}
	switch r.Phase {
	case PhaseFired, PhaseReviewing, PhaseCompleted, PhaseExpired:
		return r.Head
	}
	return ""
}

// accountBlockedUntil returns the latest active block preventing repo#pr@head
// from firing: the account-wide quota block or this round's own retry window
// (the v2 feedbackBlockedUntil over Blocked + per-head Cooldown).
func (st *State) AccountBlockedUntil(repo string, pr int, head string, now time.Time) (time.Time, bool) {
	var until time.Time
	if st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now) {
		until = st.Account.BlockedUntil.UTC()
	}
	if r := st.Round(repo, pr); r != nil && r.Phase == PhaseAwaitingRetry && r.Head == head && r.RetryAt != nil && r.RetryAt.After(now) && r.RetryAt.After(until) {
		until = r.RetryAt.UTC()
	}
	return until, !until.IsZero()
}
