---
name: coderabbit-queue
description: Drive autonomous CodeRabbit/Codex PR-review loops through crq without competing for the shared account-wide CodeRabbit rate limit. Use whenever you need to trigger CodeRabbit, fetch actionable bot feedback, resolve addressed review threads, run local pre-push review preflight, or keep PRs reviewed automatically.
---

# coderabbit-queue (`crq`)

CodeRabbit's PR-review limit is account-wide. Multiple agents posting `@coderabbitai review`
directly will stampede the same quota. `crq` owns that mechanical loop:

1. enqueue the PR in one FIFO queue,
2. trigger CodeRabbit only when the shared account can spend a review,
3. wait for every configured required bot (`CRQ_REQUIRED_BOTS`) on the current head,
4. emit normalized JSON findings or report convergence,
5. resolve the review threads the agent says it addressed.

## The Loop

**Call `crq next`, do exactly what `.action` says, call it again.** That is the whole agent loop.
Do not design one of your own.

```bash
crq next "$REPO" "$PR"
crq next            # inside the checkout: crq finds the PR from the remote and branch
                    # (so do feedback, loop, cancel — every command that takes a target)
```

| `.action` | what to do |
|---|---|
| `fix` | Fix `.findings[]`, validate locally, then `crq resolve` each addressed `.thread_id` (or `crq decline` with a reason). A finding with no `.thread_id` is cleared with `crq dismiss` once judged, except `source: "review_comment"`: retry it because thread lookup failed. Call again. |
| `hold` | Do NOT commit or push — a required reviewer has not answered for this head, and moving the head restarts its review (resolving threads does not). Call again at `.recheck_after`. |
| `push` | The head is released. Commit and push the accumulated fixes once. Call again. |
| `wait` | Nothing to do until `.recheck_after`. |
| `done` | Converged. Report and stop. |
| `blocked` | Needs a human; `.reason` says why (e.g. the PR was closed). |

`crq next` always exits 0 on success: read `.action`, never the exit code. It is **non-blocking and
idempotent**, and it advances the queue by one step as a side effect — so a PR in a repo outside the
autoreview fleet still progresses, and running it alongside the daemon is safe.

Before it reads feedback, `crq next` takes a two-hour renewable work claim on the PR. The autofix
daemon checks that claim in the same CAS that grants its own fix session, so it cannot start a
duplicate fix while this loop is active. `done` and `blocked` release it automatically. If you stop
working on the PR earlier, run `crq unclaim "$REPO" "$PR"`; otherwise the lease expires on its own.

Three things this deliberately takes away from you:

- **Choosing a delay.** `.recheck_after` is computed by crq from the account-quota window, the
  round's retry cooldown and the poll interval, and is never less than one poll interval away. Never
  invent one: hand the wait to `crq wait` (below), and only if your harness cannot run a background
  task, schedule a single wake at exactly `.recheck_after`. Never poll in-chat and never loop on
  `crq status` or `gh api`.
- **Deciding when to push.** `hold` vs `push` is crq's answer. It already accounts for the
  rate-limit degrade: a Codex-only round while CodeRabbit is blocked returns `push`, because the
  queued CodeRabbit review fires against whatever head exists when the window opens.
- **Keeping a process alive.** There is none. If the harness kills you mid-loop, the next `crq next`
  returns the correct action from persisted state. Nothing to re-attach, nothing to babysit.

`.local_work` separates `push` from `done`: crq checks whether the working copy holds changes the PR
head lacks. **Run `crq next` from inside the repository checkout** so that answer is accurate;
`.local_work_reason` says when it could not be determined.

## Waiting

On `wait` or `hold`, do not sleep, poll, or guess a delay. Hand the wait over and end your turn:

```bash
crq wait "$REPO" "$PR"
```

It blocks until there IS something to do (`fix`, `push`, `done`, `blocked`), prints that same JSON
and exits 0. Run it as your harness's background task — its **exit is the wake event**, so you burn
no tokens idling and never narrate a countdown.

It owns no review round. It only renews the interactive work claim, which expires after two hours if
the process disappears — just run it again (or call `crq next`) to continue. While idle it watches
the shared state ref with an authenticated conditional request. When GitHub answers that ETag check
with `304 Not Modified`, it does not count against the primary REST rate limit. Apart from renewing
the claim it is read-only in the steady state, but if nothing is advancing your PR (no round for the
head, or no daemon holding the leader lease) it drives the queue itself rather than wait for nobody,
which can request a review.

`crq next --wait` is the same wait inline, for a human at a terminal. All three share one decision
function, so they cannot disagree.

## Never Bypass crq

Never post `@coderabbitai review` directly — crq is the only trigger, because CodeRabbit's review
limit is account-wide and direct posts stampede it.

Never hand-poll the GitHub API (`gh api .../pulls/N/reviews|comments`, looping on the head) to wait
for a review or learn its outcome. That drains the shared account-wide GitHub REST quota — also spent
by the `crq autoreview` daemon and every other agent, so it exhausts fast — and competes with crq's
own polling. Use `crq next` (the loop), `crq wait` (block until actionable), `crq feedback`
(current findings, no trigger), or `crq status` (queue/quota).

Before starting, check local readiness:

```bash
crq doctor
```

`crq doctor` emits JSON covering crq config, `gh`, optional CodeRabbit CLI availability, and
`CODERABBIT_API_KEY` presence for headless local review.

## crq loop (interactive/one-shot)

`crq loop` is the older primitive: it triggers a round, blocks until feedback lands, and returns one
report with a frozen exit code (0 converged/skipped, 10 findings, 2 timeout). It remains supported
for humans and one-shot scripts.

An agent driving a PR should use `crq next` instead. `crq loop` requires the caller to interpret exit
codes, enforce the fix-first and hold-the-head rules by hand, and keep a long-lived process alive
across turns — the three things that go wrong. Use `crq next` plus `crq wait` instead.

Rate-limit degrade (default on, `CRQ_RL_CODEX_DEGRADE=0` disables): when CodeRabbit is rate-limited
and Codex demonstrably reviews the PR, crq returns Codex feedback promptly instead of waiting out the
window, and the pump posts the Codex command for blocked rounds while keeping the CodeRabbit review
queued. `crq next` folds this into its `push`/`wait` answer for you.

## User-Facing Updates

Do not send heartbeat updates while a loop is simply waiting, and do not narrate repeated stderr
lines. Report a real state change or action: a review fired, findings returned, a push, convergence,
a timeout or unexpected failure, a rate-limit window first discovered or materially changed, a
network outage or recovery, or when the user asks. If the only new information is elapsed time on the
same wait, stay silent.

## Feedback

Use this when you only need current findings and do not want to trigger a new review:

```bash
crq feedback "$REPO" "$PR"
```

`crq next` already embeds the current findings in its `fix` action, so reach for `crq feedback` only
when you want a snapshot without asking what to do about it. The output includes inline comments, GitHub review-thread IDs, collapsed/outside-diff review-body
findings, prompt-block findings, Codex issue-comment findings, severity, path, line, source URL,
commit, and bot.

`findings` is always an array. Verify each against current code and fix the bugs and flaws it
reports. It also surfaces still-open findings from earlier commits (any unresolved, non-outdated
review thread), so there is no need to audit past reviews by hand.

Review-body findings have no GitHub resolution state. Before a new review round starts, crq keeps
the newest body so failed-to-post comments are not lost after a rebase. Once a round is persisted
for the current head, body findings written before that round are suppressed; the current reviewer
must report them again. Cross-commit unresolved threads are still surfaced normally.

Parse fields defensively. Each finding has `bot`, `severity`, `title`, `body`, and `source`; `path`,
`line`, `url`, and `thread_id` are optional. Review-body/outside-diff findings often have no
resolvable `thread_id`.

## Resolving Threads

After fixing a finding that has a `thread_id`, resolve that thread **on GitHub**:

```bash
crq resolve "$THREAD_ID"
crq resolve PRRT_one PRRT_two PRRT_three   # resolve a whole round in one call
```

Thread IDs are globally unique, so no repo or PR is needed. Pass every addressed thread to one
call rather than looping a subprocess per thread.

crq keys off GitHub's resolution state: an addressed finding keeps reappearing in `crq feedback`
until its thread is resolved on GitHub. Resolve only threads you actually addressed; leave the rest open.

After a push, every thread from the previous head is **outdated**. Findings leave those out on
purpose — the code they point at is gone — so `crq feedback` no longer gives you their IDs even
though they are still open on the PR. List them instead of reaching for the GitHub API:

```bash
crq threads "$REPO" "$PR"
```

It returns every unresolved thread, outdated ones included, current ones first, each with the
`thread_id` that `crq resolve` and `crq decline` take.

For a finding you are **not** addressing, record why instead of leaving it silently open:

```bash
crq decline "$THREAD_ID" --reason "why this is declined"
```

This replies with your reason and resolves the thread. crq reads GitHub's resolution state, so a
thread left open keeps its finding actionable and `crq next` would repeat `fix` forever. The
disagreement is not lost: if the bot contests the decline, crq re-surfaces that reply as its own
finding. Pass `--keep-open` to leave it unresolved deliberately.

## Unattended Autofix

`crq watch` starts a fix session for every PR whose action is `fix` — that is the default — in a worktree crq
checked out at that head. Sessions run concurrently and off the decision loop, with **no cap by default** — fixing findings
spends no account quota, so it does not belong in a queue. `CRQ_DISPATCH_CONCURRENCY` sets one if the
machine cannot take the load. The decisions stay serial, which is what keeps the metered review in
one queue.

One command sets it up — `crq autofix install` writes the prompt, a wrapper and this platform's
service (systemd user unit, or a launchd agent on macOS), makes it survive a logout, and starts it;
`--dry-run` prints the paths and the exact invocation first. `--agent claude|codex` picks the fix
agent, and `--agent-args` carries its model and reasoning settings — crq knows how to call each
agent and nothing about which model it should use. The service inherits none of your shell, so the unit names the
config file the install read and the credential must be one the service can resolve itself
(`gh auth login`, or a token in that file). Two rules the prompt earned the hard
way — a session must stay on a detached HEAD and push by ref (`git push <head repo> HEAD:refs/heads/…`,
which for a fork PR is not `origin`), because the worktrees share one mirror and a branch checked out
in one of them makes git refuse to fetch for every PR. A session resolves or declines its current
threads before calling `crq next --wait`; only that guarded wait can issue the `push` action once
every required reviewer has answered.

Each session's output is written to `$CRQ_WORKSPACE/logs/<owner>/<name>/<pr>-<head>-<time>.log`
(last five per PR). Three dispatch attempts in a row that start nothing put `dispatch failing` on the dashboard
and the status line.

Interactive `next`, `wait`, and `loop` calls and unattended dispatch are mutually exclusive per PR.
Whichever side wins the shared CAS works. Plain `crq next` returns a conflict or wait action to the
losing caller; commands that explicitly wait block until the claim is available. Claim creation
refuses to promise this when a recently active autofix host runs a binary too old to honour work
claims, so upgrade every reported autofix host when that error appears.
## Holding a PR

To stop crq reviewing a PR — a draft you are still shaping, a branch waiting on a decision:

```bash
crq hold "$REPO" "$PR" --reason "waiting on the API decision"
crq unhold "$REPO" "$PR"
crq hold                                   # what is held
```

One write, honoured by every path that picks a round to fire, so there is no window in which a daemon
fires anyway. Creating a hold requires a live autoreview daemon that advertises hold support, so an
older standby cannot acquire the fleet lease while that daemon maintains it. It does not cancel a
review already in flight; that one is bought.

## Spent Trigger Comments

crq deletes its own `@coderabbitai review` / `@codex review` comments once the round that posted
them has progressed and the bot has answered, so a PR driven through a dozen rounds stays readable.
Set `CRQ_TIDY=1` to do this automatically under `crq autoreview`, or run it on demand:

```bash
crq tidy "$REPO" "$PR" [--dry-run]
```

It only ever removes comments **crq posted** — candidates come from the comments each round recorded
writing, never from matching text, and never one the round merely adopted (a person's request to
review is not crq's to erase). A candidate must also still read as that one-line command: crq posts
under your own account, so a recorded comment someone has since edited into a note is their words and
it stays. A candidate also has to predate the current head, because a newer
command is one crq would adopt instead of posting again; a request crq's own retry replaced is spent
either way, and an unreadable head keeps everything. Never the bots' own comments, because an
auto-generated reply can be a rate-limit or skipped-review notice that crq reads as evidence.

## Which Bots Review Which Project

```bash
crq reviewers "$REPO"                                   # who runs here, and what each costs
crq reviewers set "$REPO" --bots codex --required codex # Codex the only required co-reviewer
crq reviewers clear "$REPO"                             # back to the fleet default
```

Each reviewer reports its `budget`: `account` is serialized against the shared CodeRabbit allowance,
`none` runs immediately, outside that queue. That is the only property the queue cares about — it says
what a reviewer costs, never whether a round waits for it. `--required` alone decides that, and either
flag may be given without the other (`--bots` and `--required` update separate halves of the override).

The setting lives in the shared state ref, so the daemon and every agent read the same one.

The primary reviewer is fleet-wide: an override chooses the **co-reviewers**. Leaving the primary out
of `--required` means the round does not wait for it — not that it is never triggered, so it still
spends account quota. `--required` cannot be empty (a
round gating on nobody converges before anything runs); use `clear` to drop the override.

If the output lists `lagging_hosts`, those hosts are driving the queue with a binary that predates
per-repo overrides — they will keep using the fleet default until upgraded.

## Findings With No Thread

Review-body findings, review-skipped notices, outside-diff remarks and issue-comment findings have
no `thread_id`. `crq resolve` and `crq decline` both act on a thread, so neither can touch them — and a
finding that can never be cleared blocks every future round on that PR.

```bash
crq dismiss "$REPO" "$PR" "$FINDING_ID" --reason "why this is being set aside"
```

Finding IDs come from `.findings[].id`; they are content-derived, not GitHub node IDs, which is why
the repo and PR are required. A dismissal covers the current head only — push, and the next reviewer
has to report it again. `crq next` and `crq feedback` both report `dismissed: N` so nothing looks
silently dropped, and `crq loop` converges on the same filtered list.

Only a finding with no thread can be dismissed. One that has a `thread_id` is refused — resolve or
decline it, so the decision lands on the PR where the bot can answer it.

Judge the finding first. Dismiss is for one you have decided about, not for clearing the list.

If `crq dismiss` refuses a finding whose source is `review_comment`, crq could not read GitHub's
review threads (a GraphQL failure) and fell back to REST, which returns no thread IDs. The finding
does have a thread; retry once crq can read threads again rather than working around it. When
the notice is a SKIPPED review, narrowing the PR addresses the cause; dismissing only records that
you chose to proceed at this head.

## Turning Autofix Off Somewhere

Fixing is what watching is for, so it is on for every repository in scope. Where you do not want crq
writing code — a release branch, a repository you are hand-tuning:

```bash
crq autofix off "$REPO" --reason "hand-tuning the release branch"
crq autofix on "$REPO"          # or: crq autofix default "$REPO"
crq autofix                     # what is on, and where an answer was recorded
```

Off stops FIXING, not watching. The pull request is still observed and still reviewed, so its
feedback keeps arriving for a person to act on. The setting lives in the state ref, so it applies to
every host running autofix.

## Fleet Auto-Review

To keep all open PRs in scope reviewed while CodeRabbit native auto-review is off:

```bash
crq autoreview
crq autoreview --once
crq autoreview --no-incremental
```

Run exactly one long-lived autoreview daemon. If it is already active, do not stop, restart, or
duplicate it for a manual PR loop. `crq next`, `crq loop` and fleet autoreview all use the same
account-wide, idempotent queue entry: after a push, autoreview may enqueue the new head first and
your call only re-attaches (or vice versa). No path should post a direct CodeRabbit trigger.

For an intentionally low-risk PR that has already had enough local review, add
`<!-- crq:skip-autoreview -->` to the PR body before creating it. The marker is hidden in rendered
Markdown and prevents only fleet auto-review; an explicit `crq next`/`crq loop` still reviews the PR.

## Optional Local Preflight

If the official CodeRabbit CLI is installed, agents can run a normalized local pre-push review:

```bash
crq preflight --type uncommitted
```

Use that only to review local git changes before pushing. It does not replace `crq next`, which
coordinates queued GitHub PR review triggers and extracts GitHub PR feedback.

## Maintenance Commands

Do not use queue internals in agent loops. For diagnosis only:

```bash
crq doctor
crq status
crq debug state
crq debug refresh
crq debug enqueue "$REPO" "$PR"
crq debug pump
crq cancel "$REPO" "$PR"
```

## Required Prerequisite

CodeRabbit auto-review must be off. crq is pull-only: reviews fire through crq, not from
CodeRabbit automatically on every push.
