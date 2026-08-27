import { Link } from "@tanstack/react-router";
import { ExternalLink } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { AutofixLog } from "./AutofixLog";
import { act } from "./actions";
import type { Cost as CostView, Finding, PRView, Snapshot } from "./api";
import { pullRequest } from "./api";
import { Confirm } from "./Confirm";
import { useDashboard } from "./DashboardState";
import { HOLD_ACTIONS_ENABLED } from "./features";
import { ago, clock, countdown, elapsed, useNow } from "./time";
import { BotIcon, BotMarks, Card, CommitLink, Empty, Pill, PRLink, RepoIcon } from "./ui";
import { useOperation } from "./useOperation";

const SEV_ORDER = ["critical", "major", "potential", "minor", "unknown"];
const SEV_TONE: Record<string, "bad" | "warn" | "mut"> = {
  critical: "bad",
  major: "warn",
  potential: "warn",
  minor: "mut",
  unknown: "mut",
};

export function mergeLivePRState(current: PRView | null, next: PRView): PRView {
  if (!current) return next;
  if (next.rev < current.rev) return current;
  const sameHead = !next.round?.head || next.round.head === current.observed?.head;
  return {
    ...next,
    observed: sameHead ? current.observed : undefined,
    observe_error: sameHead ? current.observe_error : undefined,
    cost: sameHead ? current.cost : undefined,
    cost_error: sameHead ? current.cost_error : undefined,
  };
}

function sameHead(a: string | undefined, b: string | undefined): boolean {
  const left = a?.trim().toLowerCase();
  const right = b?.trim().toLowerCase();
  return Boolean(left && right && (left.startsWith(right) || right.startsWith(left)));
}

/** Attach a slow GitHub observation without rolling back newer persisted state. */
export function mergePRDetails(current: PRView | null, next: PRView): PRView {
  if (!current || next.rev >= current.rev) return next;

  const currentHead = current.round?.head;
  const detailsHead = next.observed?.head ?? next.round?.head;
  if (currentHead && detailsHead && !sameHead(currentHead, detailsHead)) return current;

  return {
    ...current,
    title: current.title || next.title,
    observed: next.observed,
    observe_error: next.observe_error,
    cost: next.cost,
    cost_error: next.cost_error,
  };
}

export function isNewLiveSnapshot(previous: Snapshot | null, next: Snapshot | null): boolean {
  return next !== null && next !== previous;
}

export function PRDetailPage({ repo, pr }: { repo: string; pr: number }) {
  const { snapshot } = useDashboard();
  const now = useNow();
  const [view, setView] = useState<PRView | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [action, setAction] = useState<{
    finding: Finding;
    kind: "resolve" | "decline" | "dismiss";
  } | null>(null);
  const { run: runLoad, error, clearError: clearLoadError } = useOperation();
  const { run: runInitialState } = useOperation();
  const { run: runLiveState } = useOperation();
  const {
    run: runFinding,
    running: busy,
    error: actErr,
    clearError: clearActError,
  } = useOperation();
  // Round-level actions, kept apart from the finding-level ones above: they
  // act on different things and one being in flight should not disable the
  // other.
  const [pending, setPending] = useState<"hold" | "cancel" | null>(null);
  const { run: runRoundOperation, running: acting, error: roundErr } = useOperation();
  const [roundWarning, setRoundWarning] = useState<string | null>(null);
  const liveCursor = useRef({ key: `${repo}#${pr}`, snapshot });

  const runRound = (kind: "hold" | "unhold" | "cancel", reason = "") => {
    setRoundWarning(null);
    runRoundOperation(act(kind, { repo, pr, reason }), {
      onSuccess: ({ warning }) => {
        setRoundWarning(warning ?? null);
        setPending(null);
        load(true);
      },
    });
  };

  const runAction = (reason: string) => {
    if (!action) return;
    const f = action.finding;
    const threadId = f.thread_id;
    if (action.kind !== "dismiss" && !threadId) return;
    const threadIds = threadId ? [threadId] : [];
    const program =
      action.kind === "resolve"
        ? act("resolve", { repo, pr, thread_ids: threadIds })
        : action.kind === "decline"
          ? act("decline", { repo, pr, thread_ids: threadIds, reason })
          : act("dismiss", { repo, pr, finding_ids: [f.id], reason });
    runFinding(program, {
      onSuccess: () => {
        setAction(null);
        load(true); // the finding list is GitHub's answer, so re-observe
      },
    });
  };

  const load = useCallback(
    (refresh = false) => {
      setRefreshing(refresh);
      runLoad(pullRequest(repo, pr, refresh), {
        onSuccess: (next) => setView((current) => mergePRDetails(current, next)),
        onFinally: () => setRefreshing(false),
      });
    },
    [pr, repo, runLoad],
  );

  // Paint persisted state immediately, then attach the slower GitHub-backed
  // findings and pricing. The two requests deliberately use separate
  // operations so the fast one cannot cancel the detailed one.
  useEffect(() => {
    setView(null);
    runInitialState(pullRequest(repo, pr, false, true), {
      onSuccess: (next) => setView((current) => mergeLivePRState(current, next)),
    });
    load();
  }, [load, pr, repo, runInitialState]);

  // SSE snapshots update only the cheap persisted-state layer. Findings and
  // pricing stay attached to their observed head until a refresh or head move.
  // A frame can legitimately change without advancing the state revision:
  // time-derived state such as an expired dispatch claim is part of the
  // snapshot digest too, so identity — not rev — is the live cursor here.
  useEffect(() => {
    const key = `${repo}#${pr}`;
    if (liveCursor.current.key !== key) {
      liveCursor.current = { key, snapshot };
      setRoundWarning(null);
      return;
    }
    if (!isNewLiveSnapshot(liveCursor.current.snapshot, snapshot)) return;
    liveCursor.current.snapshot = snapshot;
    runLiveState(pullRequest(repo, pr, false, true), {
      onSuccess: (next) => setView((current) => mergeLivePRState(current, next)),
    });
  }, [pr, repo, runLiveState, snapshot]);

  if (error && !view) {
    return (
      <main className="mx-auto max-w-[1180px] px-6 py-16 text-mut">
        Could not load {repo}#{pr}: {error}
      </main>
    );
  }
  if (!view) {
    return <main className="mx-auto max-w-[1180px] px-6 py-16 text-mut">Reading state…</main>;
  }

  const findings = view.observed?.findings ?? [];
  const dismissed = view.round?.dismissed ?? [];
  const knownSeverities = new Set(SEV_ORDER);
  const grouped = [
    ...SEV_ORDER.map((sev) => ({
      sev,
      items: findings.filter((f) => (f.severity || "unknown") === sev),
    })),
    {
      sev: "other",
      items: findings.filter((f) => !knownSeverities.has(f.severity || "unknown")),
    },
  ].filter((group) => group.items.length > 0);

  return (
    <main className="mx-auto max-w-[1180px] px-6 pt-4.5 pb-16">
      <div className="mb-2 text-[12.5px] text-faint">
        <Link to="/" className="text-acc hover:underline">
          Overview
        </Link>{" "}
        / {repo} / #{pr}
      </div>

      <div className="mb-3.5 rounded-[10px] border border-edge bg-card px-5 py-3.5 shadow-card">
        <div className="flex flex-wrap items-center gap-3">
          <RepoIcon repo={repo} size={24} />
          <h1 className="text-[18px] font-[650] tracking-tight">
            <PRLink repo={repo} pr={pr} />
          </h1>
          {view.title && (
            <span className="max-w-[46ch] truncate text-[13.5px] text-mut" title={view.title}>
              {view.title}
            </span>
          )}
          {view.round ? (
            <Pill tone={view.round.phase === "reviewing" ? "ok" : "acc"}>{view.round.phase}</Pill>
          ) : (
            <Pill tone="mut">no active round</Pill>
          )}
          {view.round?.fixing && <Pill tone="ok">fixing</Pill>}
          {view.hold && <Pill tone="bad">held</Pill>}
          {view.observed && (
            <Pill tone={view.observed.converged ? "ok" : "warn"}>
              {view.observed.converged ? "converged" : `${findings.length} open`}
            </Pill>
          )}
          <span className="ml-auto flex flex-wrap items-center gap-2">
            {/* A pull request opened by link was read-only: the two actions
                that matter existed only as hover buttons on an Overview row,
                which is not where you are when you have just read its
                findings. */}
            {view.hold ? (
              <button
                type="button"
                disabled={acting}
                onClick={() => void runRound("unhold")}
                className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
              >
                Unhold
              </button>
            ) : HOLD_ACTIONS_ENABLED ? (
              <button
                type="button"
                disabled={acting}
                onClick={() => setPending("hold")}
                className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
              >
                Hold…
              </button>
            ) : null}
            {view.round && (
              <button
                type="button"
                disabled={acting}
                onClick={() => setPending("cancel")}
                className="rounded-lg border border-bad-edge px-3 py-1.5 text-[13px] font-semibold text-bad disabled:opacity-45"
              >
                Cancel round…
              </button>
            )}
            <button
              type="button"
              onClick={() => load(true)}
              disabled={refreshing}
              className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
            >
              {refreshing ? "Refreshing…" : "Refresh"}
            </button>
          </span>
        </div>
        {error && (
          <div className="mt-3 flex items-start gap-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            <span className="min-w-0 flex-1">Refresh failed: {error}</span>
            <button
              type="button"
              onClick={clearLoadError}
              className="shrink-0 font-semibold hover:underline"
            >
              Dismiss
            </button>
          </div>
        )}
        {roundWarning && (
          <div
            role="status"
            className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn"
          >
            {roundWarning}
          </div>
        )}
        {view.round && (
          <div className="mt-2 flex flex-wrap gap-4 text-[12.5px] text-mut">
            <span>
              head <CommitLink repo={repo} sha={view.round.head} />
            </span>
            <span>enqueued {clock(view.round.enqueued_at)}</span>
            {view.round.fired_at && (
              <span>
                fired {clock(view.round.fired_at)} · attempt {view.round.attempts || 1}
              </span>
            )}
            {view.round.host && <span className="font-mono">{view.round.host}</span>}
          </div>
        )}
        {view.round?.next && <div className="mt-1.5 text-[13px] text-mut">{view.round.next}</div>}
      </div>

      {view.hold && (
        <div className="mb-3.5 rounded-[10px] border border-bad-edge border-l-4 border-l-bad bg-bad-bg px-4 py-2.5 text-[13.5px]">
          <b>Held</b> by {view.hold.by} {ago(view.hold.at, now)}
          {view.hold.reason && <span className="ml-2 text-mut">“{view.hold.reason}”</span>}
          <span className="ml-2 text-faint">— crq will not review it until this is lifted.</span>
        </div>
      )}

      {pending && (
        <Confirm
          title={pending === "hold" ? `Hold ${repo}#${pr}?` : `Cancel the round on ${repo}#${pr}?`}
          danger={pending === "cancel"}
          confirmLabel={pending === "hold" ? "Hold it" : "Cancel the round"}
          needsReason={pending === "hold"}
          reasonLabel="Why is it held"
          busy={acting}
          error={roundErr}
          body={
            pending === "hold" ? (
              "No round is enqueued or fired here until the hold is lifted. Reviews already in flight finish."
            ) : (
              <>
                The current round is abandoned. Auto-review may enqueue this pull request again on
                its next pass, at whatever head it then has.
                {view.round?.phase === "fired" && (
                  <p className="mt-2 text-warn">
                    This round holds the fire slot; cancelling releases it for the next pull
                    request.
                  </p>
                )}
              </>
            )
          }
          onConfirm={(reason) => void runRound(pending, reason)}
          onCancel={() => setPending(null)}
        />
      )}

      {action && (
        <Confirm
          title={
            action.kind === "resolve"
              ? "Resolve this thread?"
              : action.kind === "decline"
                ? "Decline this finding?"
                : "Dismiss this finding?"
          }
          body={
            action.kind === "resolve" ? (
              <>
                Marks the review thread resolved on GitHub, where it can be reopened. Use this when
                the finding has actually been handled.
              </>
            ) : action.kind === "decline" ? (
              <>
                Posts your reasoning as a reply on the thread and resolves it. crq reads the bot's
                answer back, so a withdrawal or a stand-by-it becomes part of the record.
              </>
            ) : (
              <>
                For findings GitHub gives no way to close. It is recorded against{" "}
                <b>this head only</b> — a new head may report it again.
              </>
            )
          }
          confirmLabel={
            action.kind === "resolve"
              ? "Resolve"
              : action.kind === "decline"
                ? "Decline"
                : "Dismiss"
          }
          needsReason={action.kind !== "resolve"}
          reasonLabel={
            action.kind === "decline"
              ? "Why you disagree (posted to the PR)"
              : "Why (kept in state)"
          }
          busy={busy}
          error={actErr}
          onConfirm={runAction}
          onCancel={() => {
            setAction(null);
            clearActError();
          }}
        />
      )}

      <div className="grid grid-cols-[minmax(0,1fr)_360px] items-start gap-4 max-[1150px]:grid-cols-[minmax(0,1fr)]">
        <div>
          <div className="mb-3.5 flex flex-wrap items-center gap-2.5 rounded-lg border border-acc-edge bg-acc-bg px-3.5 py-2 text-[12.5px] text-mut">
            {view.observed ? (
              <>
                <b className="text-acc">Observed {ago(view.observed.checked_at, now)}</b>
                <span>
                  at <CommitLink repo={repo} sha={view.observed.head} /> · reviewed by{" "}
                  {Object.entries(view.observed.reviewed_by ?? {})
                    .filter(([, v]) => v)
                    .map(([k]) => k)
                    .join(", ") || "nobody yet"}
                </span>
              </>
            ) : (
              <span>
                {view.observe_error
                  ? `Could not reach GitHub — ${view.observe_error}`
                  : "Reading findings from GitHub…"}
              </span>
            )}
          </div>

          <Card
            title="Findings"
            count={
              view.observed
                ? `${findings.length} open${view.observed.dismissed ? ` · ${view.observed.dismissed} dismissed` : ""}`
                : "—"
            }
          >
            {!view.observed ? (
              <Empty>Findings need a GitHub read; the round above came from state.</Empty>
            ) : findings.length === 0 ? (
              <div className="px-[18px] py-6 text-center">
                <div className="text-[15px] font-semibold">No open findings</div>
                <p className="mt-1 text-[13px] text-mut">
                  {view.observed.converged
                    ? "Every required reviewer finished and nothing is blocking."
                    : "Nothing actionable is outstanding at this head."}
                </p>
              </div>
            ) : (
              <div className="px-[18px] pb-3">
                {grouped.map((g) => (
                  <div key={g.sev}>
                    <div className="pt-2.5 pb-1 text-[11px] font-semibold tracking-[0.05em] text-faint uppercase">
                      {g.sev} · {g.items.length}
                    </div>
                    {g.items.map((f) => (
                      <FindingRow
                        key={f.id}
                        f={f}
                        onAct={(a) => setAction({ finding: f, kind: a })}
                      />
                    ))}
                  </div>
                ))}
              </div>
            )}
          </Card>
        </div>

        <aside>
          {view.round?.fixing && (
            <Card title="Fix session" count="live">
              <div className="px-[18px] pb-3.5 text-[13px]">
                <Pill tone="ok">Running · {elapsed(view.round.fixing.since, now)}</Pill>
                <div className="mt-1.5 text-mut">
                  {view.round.fixing.host}
                  {view.round.fixing.model ? ` · ${view.round.fixing.model}` : ""} · attempt{" "}
                  {view.round.fixing.attempt}
                  {view.round.fixing.max_attempts ? ` of ${view.round.fixing.max_attempts}` : ""}
                  {view.round.fixing.findings
                    ? ` · working through ${view.round.fixing.findings} finding(s)`
                    : ""}
                  {view.round.fixing.heartbeat &&
                    ` · heartbeat ${clock(view.round.fixing.heartbeat)}`}
                </div>
                <div className="mt-2">
                  <AutofixLog repo={repo} pr={pr} />
                </div>
                <p className="mt-1.5 text-[11.5px] text-faint">
                  While a session holds this round the queue leaves it alone; the claim is released
                  when the session pushes or exits.
                </p>
              </div>
            </Card>
          )}

          {view.round && (
            <Card title="Round" count={view.round.head}>
              <div className="px-[18px] pb-3 text-[13px]">
                <KV k="Phase" v={view.round.phase} />
                <KV k="Enqueued" v={clock(view.round.enqueued_at)} />
                {view.round.fired_at && <KV k="Fired" v={clock(view.round.fired_at)} />}
                {view.round.deadline && (
                  <KV
                    k="Deadline"
                    v={`${countdown(view.round.deadline, now)} · ${clock(view.round.deadline)}`}
                  />
                )}
                {view.round.retry_at && <KV k="Retries after" v={clock(view.round.retry_at)} />}
                {view.round.co_only && <KV k="Scope" v="co-reviewers only — spends no quota" />}
                {view.round.note && <KV k="Note" v={`“${view.round.note}”`} />}
              </div>
              <div className="border-t border-[#EEF0F3] px-[18px] py-2.5">
                <div className="mb-1.5 text-[11px] font-medium tracking-[0.06em] text-faint uppercase">
                  Reviewers
                </div>
                <BotMarks bots={view.round.bots} />
              </div>
            </Card>
          )}

          {view.observed?.converged && (
            <Card title="Verdict">
              <div className="px-[18px] pb-3.5 pt-1">
                <p className="text-[13.5px]">
                  <b className="text-ok">Nothing left to do</b> —{" "}
                  {view.observed.reason || "every required reviewer finished"}.
                </p>
                <div className="mt-2 text-[12.5px] text-mut">
                  <div className="mb-1 text-[11px] font-medium tracking-[0.06em] text-faint uppercase">
                    Reviewed by
                  </div>
                  <span className="flex flex-wrap items-center gap-2.5">
                    {Object.entries(view.observed.reviewed_by ?? {}).map(([bot, done]) => (
                      <span key={bot} className="flex items-center gap-1.5">
                        <BotIcon login={bot} name={bot} size={18} />
                        <span className={done ? "text-ok" : "text-faint"}>
                          {done ? "✓" : "pending"}
                        </span>
                      </span>
                    ))}
                  </span>
                </div>
                <p className="mt-2.5 text-[12.5px] text-faint">
                  What happens next: nothing. Merge when you are ready. Push another commit and a
                  fresh round is enqueued for the new head — a converged round is the record that
                  THIS head was reviewed, not that the pull request is finished.
                </p>
              </div>
            </Card>
          )}

          {(view.cost || view.cost_error) && <CostCard cost={view.cost} error={view.cost_error} />}

          {dismissed.length > 0 && (
            <Card title="Dismissed" count={dismissed.length}>
              <div className="px-[18px] pb-3 text-[12.5px] text-mut">
                {dismissed.map((d) => (
                  <div key={d.id} className="border-b border-[#EEF0F3] py-1.5 last:border-none">
                    “{d.reason}”
                    <div className="font-mono text-[11px] text-faint">{d.id.slice(0, 12)}</div>
                  </div>
                ))}
                <p className="pt-2 text-faint">Dismissals apply to this head only.</p>
              </div>
            </Card>
          )}

          <Card title="Round history" count={`${view.history.length} head(s)`}>
            <div className="px-[18px] pb-3">
              {view.history.length === 0 && <Empty>No round has run for this PR.</Empty>}
              {view.history.map((h) => (
                <div
                  key={`${h.head}-${h.outcome}`}
                  className="border-b border-[#EEF0F3] py-2 text-[13px] last:border-none"
                >
                  <div className="flex items-center gap-2">
                    <CommitLink repo={repo} sha={h.head} />
                    {h.current && <Pill tone="acc">current</Pill>}
                  </div>
                  <div className="text-[12px] text-faint">
                    {h.outcome}
                    {h.note && ` — ${h.note}`}
                    {h.at && ` · ${clock(h.at)}`}
                  </div>
                </div>
              ))}
            </div>
          </Card>
        </aside>
      </div>
    </main>
  );
}

const DISMISSIBLE = new Set(["review_body", "review_prompt", "review_skipped", "issue_comment"]);

function FindingRow({
  f,
  onAct,
}: {
  f: Finding;
  onAct: (kind: "resolve" | "decline" | "dismiss") => void;
}) {
  const [open, setOpen] = useState(false);
  const sev = f.severity || "unknown";
  const threaded = Boolean(f.thread_id);
  // Only threadless findings can be dismissed — a threaded one is closed by
  // resolving or declining it, which is visible on the pull request.
  const dismissible = !threaded && DISMISSIBLE.has(f.source ?? "");
  return (
    <div className="mb-2 overflow-hidden rounded-lg border border-edge">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        className="flex w-full items-baseline gap-2.5 px-3.5 py-2.5 text-left hover:bg-[#F7F8FA]"
      >
        <span className="flex-1">
          <span className="text-[13.5px] font-[550]">{f.title || "(untitled finding)"}</span>
          <span className="mt-0.5 block font-mono text-[12px] text-faint">
            {f.bot}
            {f.path ? ` · ${f.path}${f.line ? `:${f.line}` : ""}` : ""}
            {f.category ? ` · ${f.category}` : ""}
            {f.effort ? ` · ${f.effort}` : ""}
            {f.thread_id ? " · thread open" : ""}
          </span>
        </span>
        <Pill tone={SEV_TONE[sev] ?? "mut"}>{f.scale || sev}</Pill>
      </button>
      {open && (
        <div className="border-t border-[#EEF0F3] px-4 py-2.5 text-[13px] text-mut">
          <p className="whitespace-pre-wrap">
            {f.body || "No detail was captured for this finding."}
          </p>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            {threaded && (
              <>
                <button
                  type="button"
                  onClick={() => onAct("resolve")}
                  className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
                >
                  Resolve thread
                </button>
                <button
                  type="button"
                  onClick={() => onAct("decline")}
                  className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
                >
                  Decline…
                </button>
              </>
            )}
            {dismissible && (
              <button
                type="button"
                onClick={() => onAct("dismiss")}
                className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
              >
                Dismiss…
              </button>
            )}
            {!threaded && !dismissible && (
              <span className="text-[12px] text-faint">
                No thread to resolve, and this source cannot be dismissed — it clears when the
                finding stops being reported.
              </span>
            )}
            {f.url && (
              <a
                href={f.url}
                target="_blank"
                rel="noreferrer"
                className="ml-auto inline-flex items-center gap-1 text-[12.5px] text-acc hover:underline"
              >
                View on GitHub <ExternalLink aria-hidden className="size-3" />
              </a>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex justify-between gap-3 border-b border-[#EEF0F3] py-1.5 last:border-none">
      <span className="text-mut">{k}</span>
      <span className="text-right font-medium">{v}</span>
    </div>
  );
}

/**
 * What the next round would cost.
 *
 * Every figure here is an estimate and the card says so — including which
 * reviewers crq could not price at all, because a total that quietly omits one
 * reads as complete. The per-reviewer basis is shown rather than tucked into a
 * tooltip: a number whose reasoning you cannot see is a number you cannot check.
 */
function CostCard({ cost, error }: { cost?: CostView; error?: string }) {
  if (!cost) {
    return (
      <Card title="Estimated cost">
        <div className="px-[18px] pb-3.5 pt-1 text-[12.5px] text-faint">
          {error ? `Could not work out a price — ${error}` : "No price could be worked out."}
        </div>
      </Card>
    );
  }
  const money = (n: number) => `$${n.toFixed(2)}`;
  return (
    <Card title="Estimated cost" count={cost.summary}>
      <div className="px-[18px] pb-3 pt-1">
        <p className="text-[12.5px] text-faint">
          {cost.diff.additions + cost.diff.deletions} changed lines across {cost.diff.changed_files}{" "}
          file(s), for one more round at this head.
        </p>
        <table className="mt-2 w-full border-collapse">
          <tbody>
            {cost.reviewers.map((r) => (
              <tr key={r.bot} className="border-b border-[#EEF0F3] last:border-none">
                <td className="py-1.5 pr-2 align-top">
                  <BotIcon login={r.bot} name={r.bot} size={18} />
                </td>
                <td className="py-1.5 pr-2 align-top text-[12.5px] text-mut">{r.basis}</td>
                <td className="py-1.5 text-right align-top font-mono text-[12.5px] whitespace-nowrap">
                  {r.unknown ? (
                    <span className="text-warn">unknown</span>
                  ) : r.high === 0 ? (
                    <span className="text-faint">included</span>
                  ) : r.low === r.high ? (
                    money(r.high)
                  ) : (
                    `${money(r.low)}–${money(r.high)}`
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {(cost.unpriced?.length ?? 0) > 0 && (
          <p className="mt-2 rounded-lg border border-warn-edge bg-warn-bg px-2.5 py-1.5 text-[12px] text-warn">
            The total is a floor: {cost.unpriced?.join(", ")} could not be priced.
          </p>
        )}
        <p className="mt-2 text-[11.5px] text-faint">
          Estimate. Published prices last checked {cost.prices_checked_at}. {cost.pricing_note}
        </p>
      </div>
    </Card>
  );
}
