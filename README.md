<h1 align="center">🐰 crq — CodeRabbit review queue</h1>

<p align="center"><b>Stop your AI agents from fighting over one CodeRabbit rate limit.</b></p>

<p align="center">
One shared queue for your whole CodeRabbit account, so parallel agents request reviews
<b>in an orderly line — one at a time, only when there's capacity</b> — instead of stampeding.
On top of the queue, crq drives the whole review round: trigger, wait, normalize every finding to
JSON, resolve the threads you fixed, and tell you when the PR has converged.
</p>

---

## The problem (in plain English)

You've got several pull requests moving at once — yours and your AI agents' — each looping:
*fix code → push → ask CodeRabbit to review → read feedback → repeat.* To ask for a review, you
post `@coderabbitai review` on the PR.

Here's the catch: **CodeRabbit's review limit is per *account/organization*, not per PR.** On the
Pro plan, once you've been reviewing a lot, the refill rate drops — at the slowest tier it's
effectively **one review at a time, hours apart**, shared across *all* your PRs.

So your agents collide:

- 🔁 **They spam while blocked.** Agent A's PR is "available in 3 hours," but Agent B has no idea —
  it keeps posting `@coderabbitai review` on *its* PR and getting rate-limited too. Noise everywhere.
- 🐎 **They stampede when the window opens.** The moment one slot frees up, every agent fires at
  once. One wins; the rest waste the slot and trip the limit again.
- 🤷 **Nobody knows the real state.** Each agent only sees its own PR's stale countdown, but the
  limit is account-wide — so that countdown is usually wrong.

The result: wasted quota, redundant requests, and reviews landing in a random order.

## What crq does

`crq` puts **one queue in front of your whole account.** You don't post `@coderabbitai review`
yourself anymore — you ask `crq`, and `crq`:

- 🧠 **Knows the real limit** — it *asks CodeRabbit directly* (`@coderabbitai rate limit`, which
  doesn't cost a review) instead of guessing from a stale comment.
- 🚦 **Serializes everything** — compare-and-swap on a single git ref means reviews fire **one at a
  time, in FIFO order, only when the account is actually unblocked.** No stampede, no spam.
- 📊 **Shows you the line** — a live GitHub issue is the dashboard: who's queued, what's in flight,
  recent history, and when the next slot opens.
- 🌍 **Works across machines** — agents on your laptop, a server, and a CI box share one queue and
  send GitHub API work through one persistent `crq serve` control plane.
- 🔁 **Drives the round** — `crq next` doesn't just trigger; it tracks CodeRabbit *and* Codex on the
  current head and answers with the one thing to do now: fix these findings, hold the head, push,
  wait until this time, or done.

One agent changes one line — `gh pr comment ... @coderabbitai review` becomes `crq next <repo> <pr>` —
and the chaos is gone.

> ## ⚠️ Required: turn OFF CodeRabbit auto-review
>
> crq can only control *when* reviews happen if CodeRabbit isn't reviewing on its own.
> **If auto-review is enabled, CodeRabbit reviews every push automatically — bypassing crq's
> queue entirely and spending your shared rate limit outside it.** That defeats the whole point.
>
> So crq's model is **pull, not push** — reviews fire *only* when crq (or you) explicitly post
> `@coderabbitai review`. Disable auto-review before relying on crq:
>
> - **Account / org-wide (recommended):** CodeRabbit dashboard → your organization →
>   **Settings → Review → Automatic Review** → turn it **off** (also disable incremental/auto
>   reviews). This is the setting that matters most.
> - **Or per repository:** commit a `.coderabbit.yaml` with:
>
>   ```yaml
>   reviews:
>     auto_review:
>       enabled: false
>   ```
>
> To have reviews happen automatically across your PRs, use **`crq autoreview`** instead — it does
> the same job, rate-coordinated. See [Review all your PRs automatically](#review-all-your-prs-automatically).

## How it works

```text
   agent A ─┐
   agent B ─┼─► crq next <repo> <pr> ─► crq serve
   agent C ─┘                              │
                           ┌───────────────┴───────────────┐
                           ▼                               ▼
               typed state in a git ref          PR comments + reviews
               compare-and-swap + FIFO    ◄────► GitHub API ◄────► CodeRabbit
```

Durable queue state lives in one small **gate repo** (private is fine), with one
`crq serve` process acting as the control plane:

| Piece | What it is |
|-------|-----------|
| 🔒 **State ref** | The typed queue state is JSON stored in a git ref (`CRQ_STATE_REF`, default `crq-state-v3`), updated with optimistic **compare-and-swap** — a new commit is written only if the ref hasn't moved, so concurrent callers across machines never corrupt the queue. No database or service account. |
| 🌐 **Control plane** | `crq serve` owns the GitHub credential, REST/GraphQL retries, backoff, and the shared ETag cache. CLI commands fail closed through it instead of each process spending GitHub quota independently. |
| 📊 **Dashboard issue** | A tracking **issue** renders the live state below a hidden machine-readable block: status, the queue, in-flight review, recently requested review commands, and the current quota — every PR linked. The issue **title** is a one-glance status (`🐰 crq — 2 queued`). |
| 🐰 **Calibration PR** | A throwaway draft PR where crq asks `@coderabbitai rate limit` to read your real quota *without spending a review*. crq prunes its own probe comments so the PR never hits GitHub's 2500-comment cap. |

---

## Quick start

**1. Install.** The machine running `crq serve` needs [`gh`](https://cli.github.com/) logged in, or
`GITHUB_TOKEN`/`GH_TOKEN` set:

```bash
curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
```

The installer drops a prebuilt binary into `~/.local/bin` when a release asset exists, otherwise
builds from source (needs [Go](https://go.dev/dl/)), and installs the Codex skill into
`${CODEX_HOME:-$HOME/.codex}/skills/coderabbit-queue`. Set `CRQ_INSTALL_SKILL=0` to skip the skill,
or `CRQ_SKILL_DIR=/custom/path/coderabbit-queue` to install it elsewhere.

<details>
<summary>Manual install (build from source)</summary>

```bash
git clone https://github.com/kristofferR/coderabbit-queue.git
cd coderabbit-queue
go test ./...
# Build to a temp file and rename — never `go build -o` straight onto an existing
# crq: overwriting an already-executed binary in place (same inode) leaves macOS's
# cached code-signature stale, and every later run dies with "Killed: 9".
go build -trimpath -ldflags "-s -w" -o ~/.local/bin/crq.new ./cmd/crq   # ensure ~/.local/bin is on $PATH
mv -f ~/.local/bin/crq.new ~/.local/bin/crq
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
rm -rf "${CODEX_HOME:-$HOME/.codex}/skills/coderabbit-queue"
cp -R "skills/coderabbit-queue" "${CODEX_HOME:-$HOME/.codex}/skills/coderabbit-queue"
crq version
```

The top-level `./crq` is a dev launcher that runs `go run ./cmd/crq`.
</details>

**2. Create your queue** (one private repo holds the state ref + dashboard + calibration PR):

```bash
gh repo create YOURUSER/crq-state --private --add-readme
export CRQ_REPO=YOURUSER/crq-state
crq init
```

`crq init` opens the calibration PR and dashboard issue and prints the `export CRQ_*` lines to save.
Drop them into `~/.config/crq/env` — crq sources that file automatically, so every machine just needs
the same handful of lines:

```bash
mkdir -p ~/.config/crq
cat > ~/.config/crq/env <<'EOF'
export CRQ_REPO=YOURUSER/crq-state
export CRQ_ISSUE=2
export CRQ_CAL_PR=1
export CRQ_SCOPE=YOURUSER
export CRQ_STATE_REF=crq-state-v3
EOF
```

> **One-time:** make sure CodeRabbit is installed on the gate repo (so it can answer
> `@coderabbitai rate limit` on the calibration PR). If your CodeRabbit covers "all repositories"
> you're already done; otherwise add `crq-state` in the CodeRabbit dashboard.
>
> crq posts calibration comments *as you*, which re-subscribes you to the gate repo. Set it to
> **Watch ▾ → Ignore** on GitHub so the machine-only calibration PR never emails you.

**3. Start the control plane:**

```bash
crq serve install
crq doctor
```

By default every command uses `http://127.0.0.1:7777`. For one server shared across machines, set
`CRQ_SERVER_URL` on the clients and the same `CRQ_SERVER_TOKEN` on the server and clients, then bind
the server on the private network with `crq serve install --addr 0.0.0.0:7777 --allow-host <name>`.
Every non-loopback endpoint must use HTTPS through a reverse proxy; plain HTTP is accepted only for
`localhost` and loopback IP addresses. The token is optional only on loopback.

Every GitHub-backed command fails closed when the server is unavailable. `crq --direct <command>`
is the explicit recovery path for an operator; agents and daemons should never use it.

**4. Use it.** In any review loop, replace this:

```bash
gh pr comment "$PR" --repo "$REPO" --body "@coderabbitai review"   # ❌ competes with other agents
```

with this:

```bash
crq next "$REPO" "$PR"   # ✅ tells you the one thing to do next
```

`crq next` gets the PR in line, fires the review exactly once when CodeRabbit has capacity, and
answers with a single instruction: `fix` (findings are attached), `hold` (a required reviewer is
still pending — don't move the head), `push`, `wait` until a time crq computed, `done`, or `blocked`.
Call it, do what it says, call it again. It is non-blocking and idempotent, so there is no per-PR
process to babysit. It also takes a two-hour renewable work claim on the PR before reading feedback.
The unattended autofix daemon checks that claim in the same atomic update that starts a fix session,
so it cannot duplicate work an interactive agent has already taken. `done` and `blocked` release the
claim; use `crq unclaim` when abandoning a loop early.

`crq loop` is still there as the blocking one-shot form for humans and scripts.

---

## Review all your PRs automatically

`crq autoreview` reviews all your open PRs automatically, rate-coordinated. Run it as a background
watcher:

```bash
crq autoreview                  # auto-review every open PR + re-review on each push (FIFO, rate-aware)
crq autoreview --no-incremental # auto-review each PR ONCE only — no re-review on later pushes
crq autoreview --once           # a single pass (e.g. from cron or a timer)
```

By default `autoreview` covers every open PR in `CRQ_SCOPE`. To limit it to specific repos, set an
allowlist (`CRQ_REPOS=owner/a,owner/b`) — or exclude a few with a denylist (`CRQ_EXCLUDE=owner/c`).
Dependabot PRs are skipped out of the box; tune that with `CRQ_AUTOREVIEW_SKIP_AUTHORS`.
For an intentionally low-risk PR that has already had enough local review, include the hidden
`<!-- crq:skip-autoreview -->` marker in its body before creating it. The fleet daemon skips marked
PRs without spending quota; an explicit `crq loop` still reviews one.

Each pass enqueues any open PR in scope whose latest commit CodeRabbit hasn't reviewed yet (a new PR →
its first review; new commits → an incremental review), then fires them FIFO until **every** PR is
reviewed. The two flags mirror CodeRabbit's own toggles: default = *Automatic + Incremental*;
`--no-incremental` = *Automatic* only. (The gate repo itself is never auto-reviewed.) crq records the
commit it requested a review for, so the same commit is never reviewed twice. One process is the
leader at a time (a lease in the shared state), so running the daemon on several machines is safe.

With `CRQ_TIDY=1`, the daemon also **deletes crq's own spent trigger comments** as each round
progresses — the
`@coderabbitai review` / `@codex review` one-liners it posted, which otherwise bury the conversation a
human came to read. It removes a comment only when crq wrote it (never one it adopted from a person,
never a bot's own comment, and never one edited into something else), only from a round that has moved
on, only after the bot answered it, and only once it is too old to adopt again (older than the head
commit, or than a later force-push); a command superseded by crq's own retry is spent regardless of
its timestamp. Automatic tidying is opt-in so older binaries sharing the state ref never mis-pair a
delayed reply after a newer daemon deletes its command. You can also run a pass by hand with
`crq tidy <repo> <pr>` (`--dry-run` reports what it would remove).

<details>
<summary>Run it persistently (macOS launchd / Linux systemd)</summary>

**macOS — a LaunchAgent** at `~/Library/LaunchAgents/<label>.crq-autoreview.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.crq-autoreview</string>
  <key>ProgramArguments</key>
  <array><string>/Users/YOU/.local/bin/crq</string><string>autoreview</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>/Users/YOU</string>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin:/Users/YOU/.local/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/crq-autoreview.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/crq-autoreview.log</string>
</dict>
</plist>
```

```bash
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.example.crq-autoreview.plist
launchctl print "gui/$(id -u)/com.example.crq-autoreview" | grep -E 'state|pid'   # check it's running
# stop with: launchctl bootout "gui/$(id -u)/com.example.crq-autoreview"
```

The `PATH` must include where `gh` and `crq` live; `crq` reads its config from `~/.config/crq/env`
and `gh`'s auth from your login keychain.

**Linux — a systemd user service** (`~/.config/systemd/user/crq-autoreview.service`):

```ini
[Unit]
Description=crq autoreview (CodeRabbit review queue)
[Service]
ExecStart=%h/.local/bin/crq autoreview
Restart=always
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now crq-autoreview
journalctl --user -u crq-autoreview -f
```

Or, if you don't want a long-running process, run `crq autoreview --once` from cron / a timer.
</details>

---

## ⭐ The recommended PR-review loop

This is the autonomous review loop crq was built for, and it is one command:

```bash
crq next "$REPO" "$PR"
```

Read `.action`, do exactly that, call it again.

| `.action` | what to do |
|---|---|
| `fix` | fix `.findings[]`, validate, then `crq resolve` (or `crq decline`) each thread; `crq dismiss` one with no thread |
| `hold` | **do not commit or push** — a required reviewer hasn't answered for this head; call again at `.recheck_after` |
| `push` | the head is released — commit and push your fixes once |
| `wait` | nothing to do until `.recheck_after` |
| `done` | converged |
| `blocked` | needs a human; `.reason` says why |

Every rule this used to spell out is now a value crq computes. Fix-before-review is `fix` coming
before everything else. Hold-the-head is the `hold` action, including the exception where a
CodeRabbit rate-limit degrade releases the head because the queued review will fire on whatever head
exists when the window opens. The wait is `.recheck_after`, derived from the account-quota window,
the round's retry cooldown and the poll interval — never a delay you invent. And because `crq next`
returns immediately and is idempotent, an interrupted run loses nothing: call it again.

Thread-less review-body summaries from an older commit are informational after a fix is pushed:
they cannot be resolved on GitHub, so they do not block a current-head review. That review either
re-reports a still-valid finding or supersedes the stale summary.

crq *is* the review-round primitive — it enqueues, fires when unblocked, tracks both bots on the
current head, and emits normalized JSON — so you never hand-poll the GitHub API (which would burn the
shared REST quota). Run it on as many PRs and machines as you like; they all share the queue without
competing.

If the fleet `crq autoreview` daemon is already running, leave it running and do not create another
watcher. A manual `crq next` and autoreview coordinate through the same idempotent queue entry. After
a push, whichever path sees the new head first enqueues it; the other re-attaches without spending a
second review or posting another CodeRabbit trigger.

```bash
#!/usr/bin/env bash
# review-loop.sh — autonomously address CodeRabbit + Codex feedback until a PR converges.
#   REPO=owner/name PR=123 ./review-loop.sh
#
# The whole loop is: ask crq what to do, do it, ask again. No exit codes to
# interpret, no sleep to guess, no rule about when the head may move.
set -uo pipefail
REPO="${REPO:?set REPO=owner/name}"; PR="${PR:?set PR=<number>}"

# Keep the report OUT of the checkout: crq decides push-vs-done by asking whether
# the working tree holds changes the PR head lacks, so a report written next to
# your code is itself uncommitted work and the loop can never reach "done".
OUT="$(mktemp -t crq-next.XXXXXX.json)"; trap 'rm -f "$OUT"' EXIT

while :; do
  crq next "$REPO" "$PR" > "$OUT" || exit 1
  action=$(jq -r .action "$OUT")

  case "$action" in
    done)    echo "✅ $REPO#$PR converged."; break ;;
    blocked) echo "⛔ $(jq -r .reason "$OUT")"; exit 1 ;;

    fix)
      # Read findings, fix the real ones, run your tests/linters.
      jq -r '.findings[] | "\(.severity) \(.path // "-"):\(.line // 0) — \(.title)"' "$OUT"
      #   ... apply fixes and validate ...

      # Resolve the threads you addressed; record why for any you decline.
      threads=$(jq -r '.findings[] | select(.thread_id != null) | .thread_id' "$OUT")
      # shellcheck disable=SC2086 -- thread ids are opaque tokens with no spaces
      [ -n "$threads" ] && crq resolve $threads
      # crq decline <id> --reason "why this one is declined"
      ;;

    push)    git add -A && git commit -m "address review feedback" && git push ;;

    hold|wait)
      # Don't compute a delay — hand the wait over. crq wait blocks until there
      # is something to do and exits, so there is no timestamp to parse and no
      # sleep to guess. (Parsing .recheck_after yourself needs GNU date; this
      # doesn't.)
      echo "$action: $(jq -r .reason "$OUT")"
      crq wait "$REPO" "$PR" > "$OUT" || exit 1
      ;;
  esac
done
```

[`examples/review-loop.sh`](examples/review-loop.sh) is a minimal **one-shot wrapper** around the
same contract — it runs `crq next` once and prints what to do for the action it returns. It is the
building block, not the full autonomous loop above: drop it inside your own `while` cycle (or let
your agent drive the loop).

> 💡 **Watching the line:** run `crq status` any time to see the queue, what's in flight, and the
> next slot. Or open the gate **issue** on GitHub — it **is** the live dashboard.

**Agent progress etiquette:** long `crq loop` waits can last many minutes. Agents should not relay
every stderr progress line or send repeated "still waiting" messages. Tell the user once when a long
wait begins, then update only on a real state change (review fired, feedback wait, findings,
convergence, timeout, rate-limit/window change, network outage/recovery), on request, or after at
least 10 minutes of silence.

---

## Commands

```bash
crq next <repo> <pr>      # ⭐ the agent loop: emit the single next action as JSON (--wait blocks)
crq unclaim [<repo> <pr>] # abandon an interactive loop early and let autofix take over
crq loop <repo> <pr>      # blocking one-shot round: fire + wait + emit JSON findings
crq feedback <repo> <pr>  # current normalized findings as JSON, WITHOUT triggering a review
crq threads <repo> <pr>                                     # every unresolved thread, outdated included
crq resolve <thread-id> [<thread-id>...]                    # resolve addressed review threads
crq decline <thread-id> [...] --reason "<why>" [--resolve]  # record why a finding is declined
crq autofix install       # ⭐ unattended: watch every PR and fix what needs fixing
crq watch                 #    what autofix runs: drive open PRs through crq next, one JSON
                          #    line each. Fixing is ON by default (--no-dispatch observes only)
crq autofix               # which repositories crq may fix
crq autofix off <repo> --reason "<why>"   # stop fixing there; watching and reviewing continue
crq autofix on <repo> | crq autofix default <repo>
crq hold <repo> <pr> --reason "<why>"                       # persistently stop reviews for a PR
crq unhold <repo> <pr>                                      # resume reviews for a held PR
crq hold                                                    # list held PRs

crq tidy <repo> <pr>      # delete crq's own spent review-trigger comments (--dry-run previews)

crq serve                 # ⭐ GitHub control plane + live web dashboard
crq serve install         #    enable the reboot-persistent service

crq cost <repo> <pr>      # what one more review round there would cost, before firing it
                          # (CRQ_WEEKLY_LIMIT sets the fair-use threshold the dashboard forecasts)

crq fleet                 # the defaults every repository inherits (env → fleet record → repo override)
crq fleet adopt           # ⭐ record THIS host's settings for the fleet (--dry-run shows the plan)
crq fleet env <KEY> [<value>|--clear]   # any single setting, by its environment-variable name
crq fleet set [--bots <a,b>] [--required <a,b>] [--min-interval <dur>] [--weekly-limit <n>]
              [--autofix-default on|off] [--dry-run]   # --dry-run reports the impact, writes nothing

crq solver <repo>         # models, scope, clarification policy, attempts, forks and prompt
crq solver set <repo> [--models <first,next,...>] [--severities minor,potential] [--ask uncertain]
crq solver set <repo> [--effort <e>] [--attempts <n>] [--forks on|off] [--prompt <text>]
crq solver set <repo> [--one-pass on|off] [--merge off|merge|squash|rebase]
crq solver set <repo> --inherit models,effort,severities,ask,forks,skip-authors,one-pass,merge
crq solver set --fleet [...]                          # the default every repository inherits
crq solver clear <repo> | crq solver clear --fleet

crq repos                 # which projects crq reviews, and where each answer comes from
crq repos add <repo> | crq repos remove <repo> --reason "<why>" | crq repos default <repo>

crq reviewers <repo>      # which bots review this project, and what each costs
crq reviewers set <repo> [--bots <a,b>] [--required <a,b>] # choose them (either flag alone)
crq reviewers set <repo> --no-primary | --primary           # CodeRabbit off/on for this project
crq reviewers clear <repo>                                 # back to the fleet default

crq dismiss <repo> <pr> <finding-id> [...] --reason "<why>"  # account for a finding with no thread
crq autoreview            # ⭐ review ALL open PRs automatically, rate-coordinated
                          #    (--no-incremental = first review only; --once = single pass for cron)
crq status                # show the dashboard: queue, in-flight, quota, next slot
crq doctor                # JSON readiness report (gh/auth/config/CLI) — never writes to GitHub
crq preflight [...]       # run the local CodeRabbit CLI pre-push and normalize its JSON
crq cancel <repo> <pr>    # abandon the current round; autoreview may enqueue it again
crq init                  # first-time setup of the gate repo
crq debug <enqueue|forget-host|merge-ready|pump|refresh|retire-merged|state>
                                        # diagnosis only — review loops should use crq next
crq version               # print the version
crq help [command]        # help, optionally for one command
```

`crq preflight` avoids a doomed local review when shared state already records a live CodeRabbit
account block. It exits 0 with `status: "skipped"`, `.skip_reason`, and `.blocked_until`. If shared
state cannot be read, it falls back to running the local CLI normally. Set
`CRQ_PREFLIGHT_SKIP_BLOCKED=0` to force the CLI request instead.

For a temporary bulk campaign, `--one-pass on` caps each PR at the first review recorded anywhere
on the PR, including reviews recorded before one-pass was enabled, and then dispatches one
fixer/finalizer even when that review was clean. `--merge squash` (or `merge` /
`rebase`) makes the autofix watcher merge only the exact head that session released, as soon as
GitHub reports no merge conflict. Review and check status are deliberately ignored: the fixer push
makes the one allowed review stale, and waiting for a new one would turn this mode back into a
review loop. A later push, a failed fixer, a draft, or a conflict is never
silently merged and never starts a second fixer. Keep this repository-scoped and restore ordinary
incremental behavior afterwards with `crq solver set <repo> --inherit one-pass,merge`, which
preserves unrelated repository solver overrides. `crq solver clear <repo>` instead discards every
solver override for that repository.

`<repo>` is `owner/name`; `<pr>` is the number. **`crq next` always exits 0** — read `.action`, not
the exit code. **`crq loop` exit codes:** `0` converged, no
actionable findings, or the PR is held (`.status` says which), `10` actionable findings returned in
`.findings[]`, `2` timed out waiting for feedback. A hold is never `2`: that code means the wait
elapsed and is worth retrying. A hold ends when somebody lifts it or GitHub reports the pull request
as merged. crq keys resolution off GitHub's own thread state, so a finding keeps reappearing in
`feedback`/`loop` until its thread is resolved (or declined-and-resolved) on GitHub.

Use `crq hold` for an administrative pause: the hold survives autoreview passes, posts the reason on
the pull request, and prevents both primary and co-reviewer triggers until `crq unhold`. Merging the
pull request removes the hold automatically. `crq cancel` only abandons the current round, so fleet
autoreview may discover the still-open PR and enqueue it again. Creating a hold requires a live
autoreview daemon that advertises hold support; this keeps an older standby from acquiring the fleet
lease while the active daemon maintains it.

Interactive `next`, `wait`, and `loop` calls also take a short-lived PR work claim. Autofix dispatch
checks it in the same compare-and-swap update that grants its own session claim, so whichever path
starts first owns the fix. Plain `crq next` returns a conflict or wait action to the losing caller;
commands that explicitly wait block until the claim is available. Interactive claims renew on each
call, expire after two hours if the caller disappears, and release automatically at `done` or `blocked`. Run `crq
unclaim [<repo> <pr>]` when intentionally abandoning the loop sooner. Claim creation fails closed if
a recently active autofix host is too old to honour this guarantee; upgrade that daemon first.

<details>
<summary>Feedback JSON shape</summary>

```json
{
  "status": "feedback",
  "repo": "owner/repo",
  "pr": 123,
  "head": "abcdef123",
  "converged": false,
  "reviewed_by": { "coderabbitai[bot]": true, "chatgpt-codex-connector[bot]": false },
  "findings": [
    {
      "id": "…",
      "bot": "coderabbitai[bot]",
      "severity": "major",
      "path": "src/file.ts",
      "line": 42,
      "title": "Short finding title",
      "body": "Full normalized finding text",
      "thread_id": "PRRT_…",
      "source": "review_thread",
      "url": "https://github.com/owner/repo/pull/123#discussion_r…"
    }
  ],
  "checked_at": "2026-06-29T00:00:00Z"
}
```

`findings` is always an array. It includes inline comments, GraphQL review-thread IDs, collapsed
"Outside diff range" / `<details>` review-body findings, prompt blocks, and Codex issue comments — and
surfaces still-open findings from earlier commits, so nothing is silently dropped between passes. A
finding without `thread_id` came from a review body or comment GitHub can't expose as a resolvable
thread; CodeRabbit clears those on its next review.

`source: "review_reply"` is special: when you `crq decline --resolve` a finding and the bot replies
**contesting** the decline ("I'm retaining the finding: …") rather than conceding ("I'm withdrawing
this finding"), crq re-surfaces that rebuttal as a finding so the loop won't converge over a rebuttal
you haven't answered. Fix it, or `crq decline` again with a stronger reason; a bot that then withdraws
clears it. Ambiguous replies surface too — crq never buries a possible rebuttal on a false concession.
</details>

## Configuration

Set these in `~/.config/crq/env` (sourced automatically) or as environment variables:

| Variable | Default | What it does |
|----------|---------|--------------|
| `CRQ_SERVER_URL` | `http://127.0.0.1:7777` | `crq serve` endpoint that owns every GitHub API request; non-loopback endpoints require HTTPS, and GitHub-backed commands fail closed when it is unavailable |
| `CRQ_SERVER_TOKEN` | _(none)_ | shared bearer token required for non-loopback clients; configure the same value on the server and clients |
| `CRQ_REPO` | *(required)* | the gate repo (`owner/name`) holding the state ref, dashboard, calibration PR |
| `CRQ_ISSUE` | from `init` | dashboard issue number |
| `CRQ_CAL_PR` | from `init` | calibration PR number |
| `CRQ_SCOPE` | owner of `CRQ_REPO` | which owners/orgs share this quota (comma-separated) |
| `CRQ_STATE_REF` | `crq-state-v3` | git ref that stores the typed CAS state. The name is fixed; the schema inside it is v6, and a binary that predates a schema **refuses** the payload rather than erasing it — so upgrade every host together |
| `CRQ_STATE_GIT_AUTHOR_NAME` | `kristofferR` | optional author name for commits made by the Git state fallback |
| `CRQ_STATE_GIT_AUTHOR_EMAIL` | public GitHub noreply address | optional author email for commits made by the Git state fallback; use a non-private address |
| `CRQ_REPOS` | _(all in scope)_ | `autoreview` allowlist — only these `owner/name` repos (comma-separated) |
| `CRQ_EXCLUDE` | _(none)_ | denylist — crq never reviews, watches or fixes these `owner/name` repos (comma-separated) |
| `CRQ_AUTOREVIEW_SKIP_AUTHORS` | `dependabot[bot]` | PR authors `autoreview` never enqueues, and the autofix watcher never touches (comma-separated; case and `[bot]` suffix don't matter) — set to empty to auto-review bot PRs too; manual `crq review` is unaffected |
| `CRQ_AUTOREVIEW_SKIP_MARKER` | `<!-- crq:skip-autoreview -->` | exact PR-body marker that suppresses fleet auto-review; set empty to disable; manual `crq loop` is unaffected |
| `CRQ_TIDY` | `0` | set to `1` to delete crq's own spent review-trigger comments as rounds progress (`crq tidy` by hand is unaffected) |
| `CRQ_REQUIRED_BOTS` | `coderabbitai[bot]` | bots that must review the head for convergence (crq waits for all of them) |
| `CRQ_COBOTS` | `codex,bugbot,macroscope` | co-reviewers crq surfaces and (optionally) triggers; set empty to disable all |
| `CRQ_COBOT_<NAME>_REQUIRED` | `0` | make that co-reviewer gate convergence (folds it into `CRQ_REQUIRED_BOTS`); `<NAME>` ∈ `CODEX`, `BUGBOT`, `MACROSCOPE` |
| `CRQ_COBOT_<NAME>_TRIGGER` | codex: `always` iff required, else `never`; bugbot/macroscope: `selfheal` | when crq posts that bot's command — `never`, `selfheal` (only nudge an active bot that missed the head past its grace), or `always` (post in the fire step) |
| `CRQ_COBOT_<NAME>_CMD` | `@codex review` / `bugbot run` / `@macroscope-app review` | that bot's trigger comment; empty forces `never` |
| `CRQ_COBOT_<NAME>_GRACE` | `10m` | how long a `selfheal` trigger waits for the bot to show up on its own before nudging |
| `CRQ_RL_CO_DEGRADE` | on | while CodeRabbit is rate-limited, run co-reviewer-only rounds instead of waiting the window out; set `0` to disable (legacy alias: `CRQ_RL_CODEX_DEGRADE`) |
| `CRQ_PREFLIGHT_SKIP_BLOCKED` | on | skip local CodeRabbit preflight successfully while shared quota state is blocked; set `0` to force the CLI request |
| `CRQ_FEEDBACK_BOTS` | required bots + enabled co-reviewers | bots whose findings are surfaced — a superset of required bots, so co-reviewer findings show up without gating convergence on repos where those bots aren't installed |
| `CRQ_TZ` | `UTC` | dashboard display timezone (IANA name, e.g. `Europe/Oslo`) |
| `CRQ_MIN_INTERVAL` | `90s` | minimum time between fired reviews |
| `CRQ_POLL` | `15s` | how often `crq loop` checks its place in line |
| `CRQ_WAIT_TIMEOUT` | `0` | give up waiting for a slot after this long (`0` = never) |
| `CRQ_FEEDBACK_WAIT_TIMEOUT` | `20m` | how long `crq loop` waits for feedback after firing |
| `CRQ_SETTLE` | `90s` | after convergence the loop keeps polling this long before exiting 0, so a trailing review wave (e.g. a Codex auto-review of the pushed head) is caught by crq, not by a human re-checking the PR |
| `CRQ_CALIBRATE_TTL` | `2m` | how long to trust a quota reading before re-asking CodeRabbit |
| `CRQ_AUTOREVIEW_POLL` | `1m` | how often the `autoreview` daemon scans for PRs to enqueue |
| `CRQ_INFLIGHT_TIMEOUT` | `15m` | backstop to release a stuck in-flight review |
| `CRQ_LEADER_TTL` | `3m` | when a crashed `autoreview` leader is considered gone |
| `CRQ_GITHUB_MAX_WAIT` / `CRQ_GITHUB_RETRIES` | `120s` / `6` | server-owned GitHub rate-limit / 5xx backoff budget per request |
| `CRQ_NETWORK_MAX_WAIT` | `0` (no cap) | server-side cap on riding out an internet/GitHub outage (retrying ~every 30s); `0` = keep trying until connectivity returns |
| `CRQ_WORK_OWNER` | session ID, then checkout | optional stable identity for an interactive work claim; normally inferred automatically |

The pre-co-reviewer Codex variables are still read as legacy aliases, so existing configs keep
working: `CRQ_CODEX_CMD` is an alias of `CRQ_COBOT_CODEX_CMD` (the per-bot key wins when both are
set) and `CRQ_RL_CODEX_DEGRADE` an alias of `CRQ_RL_CO_DEGRADE`. Prefer the `CRQ_COBOT_*` names in
new configuration.

**Other review bots:** crq isn't CodeRabbit-specific. Point `CRQ_BOT`, `CRQ_REVIEW_CMD`,
`CRQ_RATELIMIT_CMD`, and `CRQ_RL_MARKER` at any bot with a similar command surface.

Codex clean reviews are posted as top-level issue comments with `Didn't find any major issues. Keep
them coming!`. crq recognizes that text as a successful, non-actionable review. If Codex is listed in
`CRQ_REQUIRED_BOTS`, the current-head wait must be active and the comment must be newer than that wait
before it satisfies the Codex gate; otherwise the clean summary is simply ignored rather than emitted
as a false finding.

Adding a fourth co-reviewer is a contained change: one entry in `dialect.KnownCoReviewers()`, one
corpus file per message shape under `internal/dialect/testdata/`, and one golden row pinning how it
classifies. The engine and the orchestration are bot-agnostic — they key on the login and never on
any bot's wording.

**Co-reviewers (Codex, Cursor Bugbot, Macroscope):** a co-reviewer is a review bot that isn't the
configured primary and spends no CodeRabbit quota — so its rounds never take the fire slot. All three
are enabled by default (`CRQ_COBOTS`), which means crq *surfaces their findings* and waits for a bot
that is demonstrably participating; it does not make them required. Add one to `CRQ_REQUIRED_BOTS`
(or set `CRQ_COBOT_<NAME>_REQUIRED=1`) to gate convergence on it unconditionally.

Whether crq ever *posts* a bot's trigger is the separate `CRQ_COBOT_<NAME>_TRIGGER` knob. Codex keeps
its historical behavior — `always` when it is required, so the `@codex review` command goes out in
the same fire step as the CodeRabbit one. Bugbot and Macroscope default to `selfheal`, because they
already auto-review every push: crq stays silent unless a bot it has seen working misses the current
head for longer than `CRQ_COBOT_<NAME>_GRACE`. In every mode crq suppresses the trigger when the bot
auto-reviews, has already reviewed the head, has a check run in flight, or has a live command on the
PR — so no bot is ever double-asked. A mode you set explicitly wins over requiredness, per-repo
requiredness included: `crq reviewers set` gives a required bot the trigger its registry default
would have had, but never overrides a `never` you configured yourself.

A co-reviewer that joins a round on its own (an actionable comment, a review, a check run) gates that
round dynamically: convergence waits for it even though it isn't required. An exhaustion notice — for
Codex, its usage-limit message — releases that dynamic gate so a bot that cannot finish can't stall a
round it volunteered for. An explicitly required bot is bounded only by the normal feedback deadline.

**Check runs.** Bugbot and Macroscope publish verdicts as commit check runs, not just comments, and a
clean Bugbot round posts *nothing* on the timeline — its `Cursor Bugbot` check run is the only
evidence it ran. crq therefore reads check runs for the head (one extra ETag'd REST call, so repeat
polls are free 304s) and counts a completed one as that bot's review. `crq feedback --json` reports
this per bot under `co_reviewers`:

```json
"co_reviewers": {
  "cursor": { "reviewed": true, "check_state": "clean" },
  "macroscopeapp": { "reviewed": true, "check_state": "issues", "verdict": "needs_human_review" }
}
```

`check_state` is `clean`, `issues`, `in_progress`, `failed` (the run crashed — crq re-triggers the
bot), `unable` (the bot reported it cannot review this commit at all, e.g. Macroscope's billing-issue
skip — crq stops waiting for it and does **not** re-trigger, since no trigger can fix billing) or
`unknown`. Neither `failed` nor `unable` ever counts as a review. Macroscope's approvability
`verdict` is **informational only** — it never gates convergence and never changes an exit code.

**Skipped reviews.** CodeRabbit sometimes refuses a head outright — too many files for the plan's
limit, no usage credits, an unsupported diff — with a `Review skipped` callout. That notice ships
with CodeRabbit's *rate-limit marker embedded*, so a naive reading files it as a rate limit: crq
would invent a retry window that never clears, re-fire the same PR forever, and park the
**account-wide** quota so every other PR in the fleet stalls behind one oversized diff. crq
classifies it as its own state instead — no account block, no retry loop. The round resolves on the
co-reviewers, and the skip is surfaced as a `major` finding (`source: "review_skipped"` — the one
finding with no thread to resolve, since only changing the PR addresses it) so the PR actually gets
narrowed. The refusal binds to the SHA the notice names, so splitting the PR yields a head crq fires
normally again.

Both cases set `primary_review_unavailable` in `crq feedback --json`, with a
`primary_review_unavailable_reason` naming which. That flag is load-bearing for agents: without it
they see a pending reviewer, reason about the account-quota window, and "hold the head until
CodeRabbit lands" — on a repo where it never lands, so the PR never pushes and never converges. crq
also stops consulting the account block for such a round: no deadline extension, no poll slowdown to
the block window, and no log line about a limit that cannot apply to it.

**Bot-specific quirks crq handles for you.** Macroscope never resolves a thread; it *edits* its own
comment to append `✅ Resolved in <sha>` (or `No longer relevant as of <sha>`), so crq treats that
edit as the resolution and drops the finding. Bugbot re-reports the same bug in a fresh thread after
each push, so crq dedupes on its stable `BUGBOT_BUG_ID` instead of the thread id — one finding, not
one per push.

**Summary-only plans (CodeRabbit Free on private repos):** a Free plan reviews public repos in full
but only *summarizes* private ones — CodeRabbit posts its walkthrough, tags it `🎁 Summarized by
CodeRabbit Free`, and never submits a review, however often it is asked. crq reads that notice and
runs the co-reviewers alone on those PRs: it posts no `@coderabbitai review`, takes no fire slot,
spends no account quota or pacing interval, and converges the round on whichever enabled
co-reviewers actually respond — the completion gate keys on login, not on Codex. Every enabled co-reviewer gates such a round — they are its
only reviewers — while a configured-but-absent bot still can't wedge it. There is nothing to
configure and nothing to reset: upgrade to Pro and crq starts firing CodeRabbit again on the next
observation.

**Multiple orgs:** CodeRabbit's quota is per-org, so PRs in different orgs draw from *different*
buckets. Run a separate gate (its own `CRQ_REPO`) per org rather than mixing them — otherwise you'd
serialize reviews that don't actually compete.

---

## 🤖 For AI agents (LLM-friendly cheat sheet)

If you're an autonomous agent running a PR-review loop, here's everything you need:

- **The loop is one command.** Call `crq next "<owner/repo>" "<pr>"`, do exactly what `.action`
  says, call it again. Don't design a loop of your own, and don't read the exit code — `next`
  always exits 0 on success and the answer is `.action`:
  `fix` · `hold` · `push` · `wait` · `done` · `blocked`.
- **Never post `@coderabbitai review` yourself.** crq is the only trigger, because the review limit
  is account-wide and direct posts stampede it.
- **On `wait` or `hold`, hand the wait over.** Run `crq wait "<owner/repo>" "<pr>"` as your
  harness's background task and end your turn: it blocks until there is something to do and its
  **exit is your wake event**. It holds no round, so killing it costs only the process. Never
  invent a delay, never poll in-chat, and never loop `gh api .../pulls/N/reviews|comments` — that
  drains the shared REST quota the daemon and every other agent are also spending.
- **Never choose when to push.** `hold` vs `push` is crq's answer, and it already accounts for the
  rate-limit degrade and the quiet period after a review lands.
- **Resolve / decline:** after fixing a finding, `crq resolve <thread-id>...` (pass them all at
  once). If you're declining one, `crq decline <thread-id> --reason "…"` — that resolves it too,
  because a thread left open keeps its finding actionable; `--keep-open` overrides.
- **Dismiss what has no thread:** a finding with no `thread_id` cannot be resolved or declined, and
  blocks every future round until it is accounted for: `crq dismiss <repo> <pr> <finding-id>
  --reason "…"`. It covers the current head only. A `source: "review_comment"`
  finding is refused: it lost its thread ID to crq's REST fallback and still has
  an open thread, so it needs `resolve`/`decline` once crq can read threads again. For a skipped review, narrowing the PR fixes the
  cause; dismissing only records that you chose to proceed.
- **Don't narrate the wait.** Report real state changes — findings, a push, convergence, a block —
  not elapsed time.
- **Setup check:** run `crq doctor`; if config is missing, do the Quick Start (install + `crq init`).

The installer puts the **[Codex skill](skills/coderabbit-queue/SKILL.md)** on the local skill path
by default, and a compact machine contract lives in [`llms.txt`](llms.txt).

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `crq doctor` not ready | Finish `crq init`, start `crq serve`, and inspect its health error. The server host needs `GITHUB_TOKEN`/`GH_TOKEN` or `gh auth login`. |
| `crq serve unreachable` | Start or repair the configured `CRQ_SERVER_URL`; ordinary commands deliberately do not fall back to GitHub. Use `crq --direct doctor` only for operator recovery. |
| A PR is stuck "in flight" forever | `crq cancel <repo> <pr>`; it also auto-clears after `CRQ_INFLIGHT_TIMEOUT`. |
| Reviews fire slower than expected | That's the point — you're rate-limited. `crq status` shows the real countdown from CodeRabbit. |
| `GitHub … rate limit hit … resets …` | The server backs off once for the whole fleet (up to `CRQ_GITHUB_MAX_WAIT`); past that it surfaces a clear reset time instead of a raw 403. |
| Internet drops for a while | `crq serve` rides it out, retrying about every 30s with **no timeout by default** and recording recovery in its log. Set `CRQ_NETWORK_MAX_WAIT` to cap it. |
| Calibration PR rejects comments | crq prunes its own probe comments to stay under GitHub's 2500-comment cap and self-heals if it ever hits it. |

## How concurrency works (for the curious)

Every queue change is a **compare-and-swap** on the state ref: crq reads the current commit + tree,
applies the mutation in memory, writes a new blob/tree/commit, and moves the ref **only if it still
points where it did** — GitHub rejects a stale update, and crq retries. That gives a real
cross-machine guarantee with no separate lock to wedge or break. FIFO order uses a monotonic sequence
counter (not timestamps), so it's immune to clock differences between machines, and the `autoreview`
daemon coordinates via a short-lived leader lease stored in the same state.

## License

MIT © Kristoffer Risanger. See [LICENSE](LICENSE). Contributions welcome.
