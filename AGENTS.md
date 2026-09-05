# crq architecture

crq coordinates CodeRabbit/Codex PR reviews through one account-wide queue so
the fleet never double-fires a review or races the shared rate limit. This is a
map of how the code is laid out and the invariants that keep it correct. For the
CLI contract read `README.md` and `llms.txt`; for usage read `crq help`.

## Package layout

Dependency rule (Go-enforced, no cycles): `dialect ← {engine, serve}`, `engine ← crq`,
`state ← {crq, serve}`, `gh ← {state, crq}`, `workspace ← crq`. The engine does no I/O by construction.

- `internal/dialect/` — ALL bot-text knowledge, zero deps. CodeRabbit/Codex
  completion, rate-limit, paused, in-progress, failed, summary-only-plan,
  review-skipped and clean-review classifiers; finding parsers; the decline-reply verdict
  classifiers (`IsReviewFindingWithdrawn`/`IsReviewFindingRetained`) that let
  crq read a bot's rebuttal to a declined finding; SHA/severity vocabulary; the
  `Finding` type (frozen JSON tags); the typed `BotEvent`/`Classifier`. Also the
  **co-reviewer registry** (`coreviewer.go` + `bugbot.go`/`macroscope.go`): the
  static `KnownCoReviewers()` list — Codex, Cursor Bugbot, Macroscope — each
  carrying its login, config name, check-run app slug, trigger command, and its
  wording hooks (`ClassifyComment`, `ClassifyCheck`, `ResolvedInSHA`,
  `FindingDedupeKey`, `Price`, …). The only place a bot's literal wording may
  appear. `pricing.go` holds the vendors' published prices and the per-bot cost
  estimators behind `PricesCheckedAt` — money is bot knowledge like any other.
- `internal/gh/` — GitHub REST/GraphQL transport, bot-agnostic. Owns the "GitHub
  REST quota" concept under the name **Throttle** (`ThrottleWait`/`IsThrottled`).
  The only package (besides dialect) allowed to say "rate limit".
  `ListCheckRuns` fetches a ref's check runs (envelope-paged, ETag'd); matching
  them to a bot is dialect's `ClassifyCheckRun`, never gh's.
- `internal/workspace/` — reusable repository mirrors, detached PR worktrees,
  credential-safe Git execution, stale-worktree pruning, and mirror migration.
  Owns persistent filesystem and process I/O for checkouts; `crq` supplies only
  configured roots and a current-token resolver.
- `internal/serve/` — persistent control plane and dashboard. Its GitHub gateway is the only
  ordinary path to api.github.com: short-lived CLI processes proxy REST/GraphQL through it, sharing
  one ETag cache, retry/backoff owner, and same-URL GET coalescer. Command semantics stay in `crq`;
  `serve` receives narrow interfaces wired by `cmd/crq`.
- `internal/state/` — persisted schema v6: one `Round` per PR, one global
  `FireSlot`, the CodeRabbit `AccountQuota`, an `Archive` ring, and the
  per-repository records (`Repos` reviewer overrides incl. `PrimaryOff`,
  `RepoAutofix`, `Enrolled`). `WriterCaps` is a monotonic integer bumped
  whenever one of those records starts changing decisions, so a fleet running
  two binary versions can name the hosts that will ignore a new one. Round transition
  methods, durable tombstones for tidied trigger comments, the CAS store, and
  dashboard rendering. `Round.CoBots` holds per-
  co-reviewer trigger bookkeeping; Codex's entry is **dual-written** to the
  legacy `Codex*` round fields because the fleet shares one state ref across
  binary versions (`Normalize` folds them back on load). `State`, `Round` and
  every record NESTED inside them — `FireSlot`, `FleetDefaults`, `SolverSettings`,
  `RepoReviewers`, `RepoEnrollment`, `HostReport`, `ToolReport` — **round-trip unknown JSON members** (`tolerant.go`), so a
  field a newer binary added survives being read and rewritten by an older one.
  Nesting is why each needs its own: the carrier recognises the member by name
  and hands the whole object to an ordinary decoder, which drops anything inside
  it. That is what makes ordinary additions safe without another dual-write or
  schema bump. Schema v4 deliberately fenced older v3 pumping clients that
  could not enforce administrative holds. Schema v5 similarly fences v4
  writers that would erase the dispatch scheduler's model and cooldown state.
  `CoActivity` and `CoAnswers` are the unbounded per-PR indexes that preserve
  generic reviewer activity and completed-review evidence separately after the
  bounded round archive is evicted. A merge is the one outcome that retires
  them (`RetireMerged`), since a merged PR can never reopen; closed-unmerged
  PRs keep theirs, and `Normalize` skips merged PRs when it rebuilds the
  indexes from the archive so the retirement sticks across loads.
  `ReviewedHeads` is the per-PR, distinct-head ledger behind the incremental
  review circuit breaker. Closed/merged PRs clear it; an automatic budget hold
  keeps it until explicit unhold resets the next cycle.
- `internal/engine/` — PURE decision logic, `now` passed in, no ctx/gh:
  `DecideFire` (the single fire owner), `Progress` (fired/reviewing round
  transitions), `Completion` (the one "is the round done?"), `BlockingFindings`
  /`FindingsOnHead`/`Converged`, `Policy`. `coreview.go` holds the co-reviewer
  evidence/gate algebra, keyed by login and bot-shape-generic (participated,
  clean at SHA, cannot finish, was commanded) — including `DecideCoPost` and its
  `never|selfheal|always` trigger modes. `codex.go` keeps only what is genuinely
  Codex-specific (the thumbs-up quirk, the rate-limit degrade). Every rule is
  table-tested.
- `internal/crq/` — orchestration only: `service.go` (Enqueue/Pump/Wait/Cancel),
  `observe.go`, `auto.go`, `feedback.go` (Loop/Feedback assembly), `config.go`,
  `calibration`/`preflight`/`init`. Holds `Service` and wires the packages.

Vocabulary: two distinct concepts, never mixed. GitHub REST quota = **Throttle**
(gh). CodeRabbit account quota = **AccountQuota** / "account blocked" (state,
engine, crq). "rate limit" as literal text lives only in `gh` and `dialect`.

## State: one Round per PR, never deleted

```text
queued → reserved → fired → reviewing → completed
   ↑         │         │         │
   └─────────┘         ├─────────┴→ awaiting_retry ─→ (fire-eligible once RetryAt passes)
 (post failed)         └→ completed (review lands while slot held)
 any phase → abandoned (PR closed, cancelled, or superseded by a new head)
```

Transitions are methods on `Round`; illegal edges error. A round is **never
deleted** — only transitioned, or archived when a new head supersedes it. That
is the invariant that makes the spam bug unrepresentable: "we already requested a
review at this head" is a fact you'd have to destroy a record to forget, and no
transition does that. `needsReview` collapses to `r, ok := Rounds[key]; ok &&
r.Head == head → skip`. A completed round stays as the "this head was reviewed"
dedup marker. A rate-limited requeue parks the round in `awaiting_retry` (keeping
its head/attempts/history), it does not delete a fired marker.

The one exception to that skip is `Round.ReviewersChanged`: a reviewer change
requeues the repository's completed rounds, but only for PRs that are open —
marking the closed ones instead of handing Pump dead work. A marked round is
reopened by whichever enqueue path next sees the PR alive, so reopening a PR
picks up the requirements it missed while it was shut.

The global `FireSlot` allows ≤1 concurrent fire fleet-wide (CAS). A bot ack
releases the slot while the review keeps running (the round moves to
`reviewing`); the round itself stays open until `Completion` is done. The
converse does not hold: convergence alone never releases the slot. A repository
whose required set omits the primary converges as soon as its co-reviewers
answer, and completing there would hand the slot to the next PR while the
metered command is still unanswered — so a round that spent the quota stays
`fired` until the primary acknowledges or its in-flight timeout expires. Staying
`fired` holds the slot only while the round exists, and converging is what tells
the agent to push, so the loop also stamps `FireSlot.HoldUntil`: the hold belongs
to the command, not to the head it was posted at, and it outlives the supersede
that the success it reported invites. Every fire gate asks `SlotHeld`, not
"is there a round holding it".

## observe → decide → apply

One flow drives both the daemon and the loop:

1. **observe** (`crq/observe.go`) — the single place that asks GitHub "what
   happened on this PR" and reduces it to an `engine.Observation` (head, open,
   reviews, classified `BotEvent`s, adoptable commands, reactions). It also
   carries the raw reviews/comments so `Feedback` parses findings from the same
   fetch. Built once per decision.
2. **decide** (`internal/engine`) — pure. `DecideFire` consolidates every fire
   guard in order: open → head readable → head current → phase eligible →
   **primary review unavailable** → **already reviewed** → slot free → account
   quota → min interval → adopt/post. The two bold steps resolve BEFORE the
   slot and quota gates on purpose: neither spends CodeRabbit quota, so an
   unrelated PR's block or in-flight review must not delay a round whose
   primary work is already done or can never happen. Nothing else may post the
   review command. `Progress` transitions a
   fired/reviewing round. `Completion` answers convergence.
3. **apply** (`crq/service.go`) — the only effects executor: CAS state writes +
   `PostIssueComment`. `DryRun` short-circuits apply into "report, write nothing".

Daemon `Pump` = Progress on the slot round + DecideFire on the next eligible.
`crq loop` (Wait + Feedback) = the same DecideFire to fire, then `Completion` +
findings filters to converge. The wait IS the round: a fired/reviewing round with
a `WaitDeadline` is the in-flight wait. Loop exit codes are frozen: 0 converged/
skipped/held, 10 findings, 2 timeout. A hold is terminal for the run and ends
when a person lifts it or the daemon clears it after the pull request merges,
so it is never 2 — that code means the wait elapsed, and a caller scripted
against it would retry for ever.

## Adding a new bot-message format

When a bot ships a new phrasing that crq must recognise, change three things and
nothing else:

1. the matching classifier/parser in `internal/dialect` (`coderabbit.go`,
   `codex.go`, `bugbot.go`, `macroscope.go`, or `common.go`);
2. one corpus file under `internal/dialect/testdata/<bot>/` holding the real
   message, captured verbatim;
3. one row in `TestGoldenClassification` / `TestGoldenCoReviewers` /
   `TestGoldenCheckRuns` (`golden_test.go`) — the row IS the spec for how that
   file classifies.

Convergence/fire rules that consume those classifications live in
`internal/engine` and are table-tested in `engine_test.go`; orchestration stays
in `internal/crq`. Keep bot wording out of engine/state/crq.

## Adding a new co-reviewer

A **co-reviewer** is a review bot that is not the configured primary and spends
no CodeRabbit quota — so its rounds never take the `FireSlot` (a per-round
`CoBots[login].ClaimedAt` CAS claim serializes its trigger post instead). Adding
one is deliberately contained:

1. one entry in `dialect.KnownCoReviewers()` with its login, config name, check
   app slug, trigger command, and whichever wording hooks it needs;
2. its wording helpers in a new `internal/dialect/<bot>.go`;
3. corpus files + golden rows as above;
4. a default in `parseCoBots` (`internal/crq/config.go`) if it should ship on.

Nothing in `engine`/`state`/`crq` enumerates bots: they key on the login and
consume the registry's hooks. That is why a bot's identity travels as data
(`BotEvent.For`, `CheckSeen.Bot`, `Observation.Co[login]`) and never as a
per-bot event kind or verdict.
