import { Pause, Settings2, XCircle } from "lucide-react";
import { useState } from "react";
import { act } from "./actions";
import type { BotCard, Event as EventItem, Overview, RepoRow, Snapshot } from "./api";
import { Confirm } from "./Confirm";
import { HOLD_ACTIONS_ENABLED } from "./features";
import { QuickSettings } from "./QuickSettings";
import { ago, clock, countdown, elapsed, useNow } from "./time";
import { BotMarks, Card, CommitLink, Empty, Pill, PRLink, PRTitle, RepoIcon, Td, Th } from "./ui";
import { useOperation } from "./useOperation";

const WHY_TONE: Record<string, "ok" | "warn" | "mut"> = {
  "": "ok",
  "account blocked": "warn",
  "cooling down": "warn",
  pacing: "warn",
  "slot busy": "mut",
  "behind an earlier round": "mut",
};

type Filter = "all" | "in_flight" | "queued" | "held" | "fixing" | "attention";

// Lanes a row can belong to. "finished" is a lane but never a chip: filtering
// TO finished work is not a thing anyone wants to do on an operations page.
type Lane = Filter | "finished";

const FILTER_LABELS: Record<Filter, string> = {
  all: "All",
  in_flight: "In flight",
  queued: "Queued",
  held: "Held",
  fixing: "Fixing",
  attention: "Needs attention",
};

// A filtered count has to say what it is a count OF, or a narrowed table reads
// as a fleet that suddenly has less work in it.
function countLabel(shown: number, total: number, narrowed: boolean) {
  return narrowed && shown !== total ? `${shown} of ${total}` : total;
}

type Pending =
  | { kind: "hold"; repo: string; pr: number }
  | { kind: "unhold"; repo: string; pr: number }
  | { kind: "cancel"; repo: string; pr: number; phase: string; slot: boolean };

export function OverviewPage({
  ov,
  events,
  repos,
  bots,
  onSnapshot,
}: {
  ov: Overview;
  events: EventItem[];
  repos: RepoRow[];
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const now = useNow();
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [pending, setPending] = useState<Pending | null>(null);
  // Which repository's quick settings are open, if any.
  const [settingsFor, setSettingsFor] = useState<string | null>(null);
  const { run: runOperation, running: busy, error, clearError } = useOperation();
  const [warning, setWarning] = useState<string | null>(null);
  const [prioritizingKey, setPrioritizingKey] = useState<string | null>(null);
  const { run: runPrioritize, running: prioritizing, error: prioritizeError } = useOperation();

  const run = (reason: string) => {
    if (!pending) return;
    setWarning(null);
    runOperation(act(pending.kind, { repo: pending.repo, pr: pending.pr, reason }), {
      onSuccess: ({ snapshot, warning: nextWarning }) => {
        onSnapshot?.(snapshot);
        setWarning(nextWarning ?? null);
        setPending(null);
      },
    });
  };
  const prioritize = (repo: string, pr: number) => {
    if (prioritizing) return;
    setPrioritizingKey(`${repo.toLowerCase()}#${pr}`);
    runPrioritize(act("prioritize", { repo, pr }), {
      onSuccess: ({ snapshot }) => onSnapshot?.(snapshot),
      onFinally: () => setPrioritizingKey(null),
    });
  };
  // Filtering is client-side on purpose: the whole snapshot is already here,
  // so narrowing it costs nothing and cannot go stale against the tables it is
  // narrowing.
  const q = query.trim().toLowerCase();
  const matches = (row: { repo: string; pr: number; title?: string }) =>
    q === "" ||
    `${row.repo}#${row.pr}`.toLowerCase().includes(q) ||
    (row.title ?? "").toLowerCase().includes(q);

  const attention = new Set(
    ov.attention.map((a) => (a.subject ?? "").toLowerCase()).filter(Boolean),
  );
  const keep = <T extends { repo: string; pr: number; title?: string; key?: string }>(
    rows: T[],
    lane: Lane,
  ) =>
    rows.filter(
      (r) =>
        matches(r) &&
        (filter === "all" ||
          filter === lane ||
          (filter === "attention" && attention.has((r.key ?? "").toLowerCase()))),
    );

  const inFlight = keep(ov.in_flight, "in_flight");
  const queued = keep(ov.queue, "queued");
  const held = keep(ov.held, "held");
  const fixing = keep(ov.autofix.sessions, "fixing");
  const finished = keep(ov.finished, "finished");
  const narrowed = q !== "" || filter !== "all";

  return (
    <main className="mx-auto max-w-[1400px] px-6 pt-4.5 pb-16">
      <Banner ov={ov} now={now} />
      {ov.attention.map((a) => (
        <div
          key={`${a.kind}-${a.subject ?? ""}-${a.text}`}
          className={`mb-3.5 flex flex-wrap items-center gap-3 rounded-[10px] border border-l-4 px-4 py-2.5 text-[13.5px] ${
            a.level === "bad"
              ? "border-bad-edge border-l-bad bg-bad-bg"
              : "border-warn-edge border-l-warn-fg bg-warn-bg"
          }`}
        >
          <Pill tone={a.level === "bad" ? "bad" : "warn"}>Needs attention</Pill>
          <span className="min-w-0">
            {a.text}
            {a.detail && <span className="ml-2 text-mut">{a.detail}</span>}
          </span>
          {a.link && (
            <a
              href={a.link}
              className="ml-auto shrink-0 text-[12.5px] font-semibold text-acc hover:underline"
            >
              {a.link_text ?? "Open"} →
            </a>
          )}
        </div>
      ))}
      {prioritizeError && (
        <div
          role="alert"
          className="mb-3.5 rounded-[10px] border border-bad-edge bg-bad-bg px-4 py-2.5 text-[13px] text-bad"
        >
          Could not prioritize the pull request: {prioritizeError}
        </div>
      )}
      {warning && (
        <div
          role="status"
          className="mb-3.5 rounded-[10px] border border-warn-edge bg-warn-bg px-4 py-2.5 text-[13px] text-warn"
        >
          {warning}
        </div>
      )}

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <input
          aria-label="Filter pull requests"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search repo, PR number or title…"
          className="w-[300px] rounded-lg border border-edge bg-card px-2.5 py-1.5 text-[13px]"
        />
        {(["all", "in_flight", "queued", "held", "fixing", "attention"] as Filter[]).map((f) => (
          <button
            key={f}
            type="button"
            onClick={() => setFilter(f)}
            className={`rounded-full border px-3 py-1 text-[12.5px] font-medium ${
              filter === f
                ? "border-ink bg-ink text-white"
                : "border-edge text-mut hover:bg-[#F7F8FA]"
            }`}
          >
            {FILTER_LABELS[f]}
          </button>
        ))}
        {narrowed && (
          <button
            type="button"
            onClick={() => {
              setQuery("");
              setFilter("all");
            }}
            className="text-[12.5px] text-acc hover:underline"
          >
            Clear
          </button>
        )}
      </div>

      <div className="grid grid-cols-[minmax(0,1fr)_340px] items-start gap-3.5 max-[1050px]:grid-cols-[minmax(0,1fr)]">
        <div>
          <Card
            title="In flight"
            count={countLabel(inFlight.length, ov.counts.in_flight, narrowed)}
          >
            {inFlight.length === 0 ? (
              <Empty>Nothing is being reviewed right now.</Empty>
            ) : (
              <table className="mt-1.5 w-full border-collapse">
                <thead>
                  <tr>
                    <Th>Pull request</Th>
                    <Th>Head</Th>
                    <Th>Phase</Th>
                    <Th>Fired</Th>
                    <Th>Deadline</Th>
                    <Th>Reviewers</Th>
                    <Th className="c-host">Host</Th>
                    <Th className="c-actions" />
                  </tr>
                </thead>
                <tbody>
                  {inFlight.map((r) => (
                    <tr key={r.key} className="group hover:bg-[#F7F8FA]">
                      <Td>
                        <div className="flex items-center gap-2 font-[550]">
                          <RepoIcon repo={r.repo} />
                          <PRTitle repo={r.repo} pr={r.pr} title={r.title} />
                          {r.fixing && <Pill tone="ok">fixing</Pill>}
                        </div>
                        {r.next && <div className="mt-1 ml-6 text-[12.5px] text-mut">{r.next}</div>}
                      </Td>
                      <Td className="text-[13px]">
                        <CommitLink repo={r.repo} sha={r.head} />
                      </Td>
                      <Td>
                        <Pill tone={r.phase === "reviewing" ? "ok" : "acc"}>{r.phase}</Pill>
                      </Td>
                      <Td className="tabular-nums">
                        {clock(r.fired_at)}
                        <div className="text-[11.5px] text-faint">{ago(r.fired_at, now)}</div>
                      </Td>
                      <Td className="tabular-nums">
                        <b>{countdown(r.deadline, now)}</b>
                        <div className="text-[11.5px] text-faint">{clock(r.deadline)}</div>
                      </Td>
                      <Td>
                        <BotMarks bots={r.bots} />
                      </Td>
                      <Td className="c-host font-mono text-[13px] text-mut">{r.host ?? "—"}</Td>
                      <Td className="c-actions">
                        <RowActions
                          onSettings={() => setSettingsFor(r.repo)}
                          onHold={
                            HOLD_ACTIONS_ENABLED
                              ? () => setPending({ kind: "hold", repo: r.repo, pr: r.pr })
                              : undefined
                          }
                          onCancel={() =>
                            setPending({
                              kind: "cancel",
                              repo: r.repo,
                              pr: r.pr,
                              phase: r.phase,
                              slot: ov.slot.key === r.key,
                            })
                          }
                        />
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>

          <Card
            title="Queue"
            count={`${countLabel(queued.length, ov.counts.queued, narrowed)} waiting`}
            end="only the front row carries a time — later starts are unknowable"
          >
            {queued.length === 0 ? (
              <Empty>The queue is empty.</Empty>
            ) : (
              <table className="mt-1.5 w-full border-collapse">
                <thead>
                  <tr>
                    <Th className="w-6">#</Th>
                    <Th>Pull request</Th>
                    <Th>Head</Th>
                    <Th>Ready</Th>
                    <Th>Why</Th>
                    <Th className="c-att text-right">Att.</Th>
                    <Th className="c-host">Host</Th>
                    <Th className="c-actions" />
                  </tr>
                </thead>
                <tbody>
                  {queued.map((q) => (
                    <tr key={q.key} className="group hover:bg-[#F7F8FA]">
                      <Td className="tabular-nums">
                        {q.position ? q.position : <span className="text-faint">—</span>}
                      </Td>
                      <Td>
                        <div className="flex items-center gap-2 font-[550]">
                          <RepoIcon repo={q.repo} />
                          <PRTitle repo={q.repo} pr={q.pr} title={q.title} />
                        </div>
                        {q.next && <div className="mt-1 ml-6 text-[12.5px] text-mut">{q.next}</div>}
                      </Td>
                      <Td className="text-[13px]">
                        <CommitLink repo={q.repo} sha={q.head} />
                      </Td>
                      <Td className="tabular-nums">
                        {q.ready_at ? (
                          <b>{countdown(q.ready_at, now)}</b>
                        ) : q.why ? (
                          <span className="text-faint">—</span>
                        ) : (
                          <b>now</b>
                        )}
                      </Td>
                      <Td>
                        <Pill tone={WHY_TONE[q.why ?? ""] ?? "mut"}>
                          {q.why || "next up"}
                          {q.co_only ? " · co-only" : ""}
                        </Pill>
                      </Td>
                      <Td className="c-att text-right tabular-nums">{q.attempts ?? 0}</Td>
                      <Td className="c-host font-mono text-[13px] text-mut">{q.host ?? "—"}</Td>
                      <Td className="c-actions">
                        <RowActions
                          onPrioritize={() => prioritize(q.repo, q.pr)}
                          prioritizing={
                            prioritizing && prioritizingKey === `${q.repo.toLowerCase()}#${q.pr}`
                          }
                          prioritizeDisabled={prioritizing}
                          onSettings={() => setSettingsFor(q.repo)}
                          onHold={
                            HOLD_ACTIONS_ENABLED
                              ? () => setPending({ kind: "hold", repo: q.repo, pr: q.pr })
                              : undefined
                          }
                          onCancel={() =>
                            setPending({
                              kind: "cancel",
                              repo: q.repo,
                              pr: q.pr,
                              phase: "queued",
                              slot: false,
                            })
                          }
                        />
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>

          {ov.held.length > 0 && (
            <Card title="Held" count={countLabel(held.length, ov.counts.held, narrowed)}>
              <table className="mt-1.5 w-full border-collapse">
                <thead>
                  <tr>
                    <Th>Pull request</Th>
                    <Th>Head</Th>
                    <Th className="c-host">Held by</Th>
                    <Th>Since</Th>
                    <Th>Reason</Th>
                    <Th className="c-actions" />
                  </tr>
                </thead>
                <tbody>
                  {held.map((h) => (
                    <tr key={h.key} className="hover:bg-[#F7F8FA]">
                      <Td>
                        <div className="flex items-center gap-2 font-[550]">
                          <RepoIcon repo={h.repo} />
                          <PRTitle repo={h.repo} pr={h.pr} title={h.title} />
                        </div>
                      </Td>
                      <Td className="text-[13px]">
                        <CommitLink repo={h.repo} sha={h.head} />
                      </Td>
                      <Td className="c-host font-mono text-[13px] text-mut">{h.by || "—"}</Td>
                      <Td className="tabular-nums">{clock(h.at)}</Td>
                      <Td>
                        <Pill tone="bad">Held</Pill>
                        {h.reason && <span className="ml-2 text-faint">“{h.reason}”</span>}
                      </Td>
                      <Td className="c-actions">
                        <button
                          type="button"
                          onClick={() => setPending({ kind: "unhold", repo: h.repo, pr: h.pr })}
                          className="text-[12.5px] text-acc hover:underline"
                        >
                          Unhold
                        </button>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
          )}

          <Card
            title="Recently finished"
            count={countLabel(finished.length, ov.finished.length, narrowed)}
          >
            {finished.length === 0 ? (
              <Empty>Nothing has finished yet.</Empty>
            ) : (
              <table className="mt-1.5 w-full border-collapse">
                <thead>
                  <tr>
                    <Th>Pull request</Th>
                    <Th>Head</Th>
                    <Th>Outcome</Th>
                    <Th>When</Th>
                  </tr>
                </thead>
                <tbody>
                  {finished.map((d) => (
                    <tr key={`${d.key}-${d.head}`} className="hover:bg-[#F7F8FA]">
                      <Td>
                        <div className="flex items-center gap-2 font-[550]">
                          <RepoIcon repo={d.repo} />
                          <PRTitle repo={d.repo} pr={d.pr} title={d.title} />
                        </div>
                      </Td>
                      <Td className="text-[13px]">
                        <CommitLink repo={d.repo} sha={d.head} />
                      </Td>
                      <Td>
                        <Pill
                          tone={d.outcome === "completed" || d.outcome === "merged" ? "ok" : "mut"}
                        >
                          {d.outcome === "merged" ? "Merged" : d.outcome}
                        </Pill>
                        {d.note && d.note !== d.outcome && (
                          <span className="ml-2 text-faint">{d.note}</span>
                        )}
                      </Td>
                      <Td className="tabular-nums">{clock(d.at)}</Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>
        </div>
        <aside className="sticky top-16 min-w-0 max-[1050px]:static">
          <AutofixCard autofix={ov.autofix} fixing={fixing} narrowed={narrowed} now={now} />
          <ActivityFeed events={events} />
        </aside>
      </div>

      {settingsFor &&
        (() => {
          const row = repos.find((r) => r.repo.toLowerCase() === settingsFor.toLowerCase());
          return row ? (
            <QuickSettings
              repo={row}
              bots={bots}
              onClose={() => setSettingsFor(null)}
              onSnapshot={onSnapshot}
            />
          ) : null;
        })()}

      {pending && (
        <Confirm
          title={
            pending.kind === "hold"
              ? `Hold ${pending.repo.split("/").pop()}#${pending.pr}?`
              : pending.kind === "unhold"
                ? `Resume reviews on ${pending.repo.split("/").pop()}#${pending.pr}?`
                : `Cancel the round on ${pending.repo.split("/").pop()}#${pending.pr}?`
          }
          body={
            pending.kind === "hold" ? (
              <>
                crq will stop reviewing this pull request until the hold is lifted. Nothing already
                in flight is undone.
              </>
            ) : pending.kind === "unhold" ? (
              <>It rejoins the queue on the daemon's next pass.</>
            ) : (
              <>
                The round is archived and its state discarded — it is currently{" "}
                <b>{pending.phase}</b>. A new push re-enqueues the pull request.
                {pending.slot && (
                  <>
                    {" "}
                    <b className="text-bad">This round holds the fire slot</b>, so cancelling
                    releases it for the next PR.
                  </>
                )}
              </>
            )
          }
          confirmLabel={
            pending.kind === "hold"
              ? "Hold it"
              : pending.kind === "unhold"
                ? "Unhold"
                : "Cancel the round"
          }
          danger={pending.kind === "cancel"}
          needsReason={pending.kind === "hold"}
          reasonLabel="Reason for the hold"
          busy={busy}
          error={error}
          onConfirm={run}
          onCancel={() => {
            setPending(null);
            clearError();
          }}
        />
      )}

      <footer className="flex flex-wrap gap-4 px-0.5 py-2.5 text-xs text-faint">
        <span>✓ commanded · ⏳ claim posted, not yet recorded · — not enabled</span>
        <span className="ml-auto">
          rev {ov.rev} · written {clock(ov.wrote_at)}
        </span>
      </footer>
    </main>
  );
}

function AutofixCard({
  autofix,
  fixing,
  narrowed,
  now,
}: {
  autofix: Overview["autofix"];
  fixing: Overview["autofix"]["sessions"];
  narrowed: boolean;
  now: number;
}) {
  return (
    <Card
      title="Autofix"
      count={`${countLabel(fixing.length, autofix.sessions.length, narrowed)} running`}
      end={`${autofix.hosts.length} hosts`}
    >
      {fixing.length === 0 ? (
        <Empty>{narrowed ? "No fix session matches." : "No fix session is running."}</Empty>
      ) : (
        fixing.map((s) => (
          <div key={s.key} className="border-b border-[#EEF0F3] px-[18px] py-3 text-[13px]">
            <div className="flex flex-wrap items-center gap-2">
              <RepoIcon repo={s.repo} />
              <PRLink repo={s.repo} pr={s.pr} className="font-[550]" />
              <Pill tone="ok">Fixing · {elapsed(s.since, now)}</Pill>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-faint">
              <CommitLink repo={s.repo} sha={s.head} />
              <span>
                {s.host}
                {s.model ? ` · ${s.model}` : ""}
                {s.attempt ? ` · attempt ${s.attempt}` : ""}
                {s.heartbeat ? ` · heartbeat ${clock(s.heartbeat)}` : ""}
              </span>
            </div>
          </div>
        ))
      )}
      <div className="grid gap-2 px-[18px] py-3">
        {autofix.hosts.length === 0 && (
          <span className="text-[13px] text-faint">No host has reported yet.</span>
        )}
        {autofix.hosts.map((h) => (
          <div
            key={h.name}
            className={`rounded-lg border px-3 py-2 text-[12.5px] text-mut ${
              h.health === "unhealthy" ? "border-bad-edge bg-bad-bg" : "border-edge"
            }`}
          >
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-mono font-semibold text-ink">{h.name}</span>
              <Pill tone={h.health === "healthy" ? "ok" : h.health === "unhealthy" ? "bad" : "mut"}>
                {h.health === "unknown" ? "No recent signal" : h.health}
              </Pill>
            </div>
            <div className="mt-1">
              {h.health === "unhealthy"
                ? `${h.failures} consecutive failures`
                : h.last_success
                  ? `last success ${clock(h.last_success)}`
                  : "nothing reported"}
            </div>
            {h.last_error && (
              <div className="mt-1 font-mono text-[11.5px] break-words">{h.last_error}</div>
            )}
          </div>
        ))}
      </div>
    </Card>
  );
}

/**
 * The feed is derived from state revisions, so it is honest about its scope:
 * it starts when the server starts and cannot see a change that appears and
 * reverts between two polls.
 */
function ActivityFeed({ events }: { events: EventItem[] }) {
  const tone = (level: string) =>
    level === "bad" ? "bad" : level === "warn" ? "warn" : level === "ok" ? "ok" : "acc";
  const occurrences = new Map<string, number>();
  const keyedEvents = events.slice(0, 60).map((event) => {
    const base = `${event.at}-${event.kind}-${event.repo ?? ""}-${event.pr ?? ""}-${event.text}`;
    const occurrence = occurrences.get(base) ?? 0;
    occurrences.set(base, occurrence + 1);
    return { event, key: `${base}-${occurrence}` };
  });
  return (
    <Card title="Activity" end="live — since this dashboard started">
      {events.length === 0 ? (
        <Empty>Nothing has changed since this dashboard started.</Empty>
      ) : (
        <ol className="px-[18px] pt-1.5 pb-3">
          {keyedEvents.map(({ event: e, key }) => (
            <li
              key={key}
              className="flex gap-2.5 border-b border-dashed border-[#EEF0F3] py-1.5 text-[12.5px] last:border-none"
            >
              <span className="w-[52px] shrink-0 pt-0.5 font-mono text-[11px] text-faint">
                {clock(e.at)}
              </span>
              <span className="shrink-0">
                <Pill tone={tone(e.level)}>{e.kind}</Pill>
              </span>
              <span className="text-mut">
                {e.repo && e.pr ? (
                  <>
                    <PRLink repo={e.repo} pr={e.pr} /> —{" "}
                  </>
                ) : e.repo ? (
                  <>{e.repo.split("/").pop()} — </>
                ) : null}
                {e.text}
                {e.detail && <span className="text-faint"> · {e.detail}</span>}
              </span>
            </li>
          ))}
        </ol>
      )}
      <p className="border-t border-[#EEF0F3] px-[18px] py-2 text-[11.5px] text-faint">
        Derived by comparing state revisions — not an audit log. {events.length} event(s) held.
      </p>
    </Card>
  );
}

/** Destructive row actions remain discoverable to touch and keyboard users. */
function RowActions({
  onPrioritize,
  onHold,
  onCancel,
  onSettings,
  prioritizing,
  prioritizeDisabled,
}: {
  onPrioritize?: () => void;
  onHold?: () => void;
  onCancel: () => void;
  onSettings?: () => void;
  prioritizing?: boolean;
  prioritizeDisabled?: boolean;
}) {
  return (
    <span className="flex justify-end gap-1 whitespace-nowrap opacity-70 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
      {onPrioritize && (
        <button
          type="button"
          disabled={prioritizeDisabled}
          onClick={onPrioritize}
          title="move to top of review and autofix queues"
          className="inline-flex h-7 items-center rounded-md px-1.5 text-[12px] text-acc hover:bg-acc-bg disabled:opacity-45"
        >
          {prioritizing ? "Moving…" : "↑ Top"}
        </button>
      )}
      {onSettings && (
        <button
          type="button"
          onClick={onSettings}
          aria-label="Reviewers for this repository"
          className="inline-flex size-7 items-center justify-center rounded-md text-acc hover:bg-acc-bg"
        >
          <Settings2 aria-hidden className="size-3.5" />
        </button>
      )}
      {onHold && (
        <button
          type="button"
          onClick={onHold}
          className="inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-[12px] text-acc hover:bg-acc-bg"
        >
          <Pause aria-hidden className="size-3" /> Hold
        </button>
      )}
      <button
        type="button"
        onClick={onCancel}
        className="inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-[12px] text-acc hover:bg-acc-bg"
      >
        <XCircle aria-hidden className="size-3" /> Cancel
      </button>
    </span>
  );
}

function Banner({ ov, now }: { ov: Overview; now: number }) {
  const blocked = ov.headline.kind === "blocked";
  return (
    <div
      className={`mb-2.5 grid grid-cols-[1fr_auto_auto_auto] items-center gap-4 rounded-[10px] border px-5 py-3.5 shadow-card max-[1150px]:flex max-[1150px]:flex-wrap ${
        blocked
          ? "border-warn-edge bg-gradient-to-br from-[#FDF7EA] to-warn-bg"
          : "border-edge bg-card"
      }`}
    >
      <div className="max-[1150px]:basis-full">
        <div className="flex flex-wrap items-baseline gap-3">
          <span className={`text-[17px] font-[650] ${blocked ? "text-warn" : "text-ink"}`}>
            {ov.headline.text}
          </span>
          {blocked && ov.quota.blocked_until && (
            <>
              <span className="text-[27px] font-[650] tracking-tight text-warn-fg tabular-nums">
                {countdown(ov.quota.blocked_until, now)}
              </span>
              <span className="text-faint tabular-nums">
                reopens {clock(ov.quota.blocked_until)}
              </span>
            </>
          )}
        </div>
        {ov.headline.detail && (
          <div className="mt-0.5 text-[13px] text-mut">{ov.headline.detail}</div>
        )}
        <div className="mt-0.5 text-[13px] text-mut">
          {ov.counts.in_flight} in flight · {ov.counts.queued} queued · {ov.counts.held} held ·{" "}
          {ov.counts.fixing} fixing
        </div>
      </div>
      <Tile k="Quota">
        {ov.quota.remaining === null || ov.quota.remaining === undefined ? (
          <span className="text-faint">unknown</span>
        ) : (
          <span className="tabular-nums">{ov.quota.remaining} remaining</span>
        )}
      </Tile>
      <FairUseTile fu={ov.quota.fair_use} />
      <Tile k="Fire slot">
        {ov.slot.held ? <Pill tone="warn">Held</Pill> : <Pill tone="ok">Open</Pill>}
      </Tile>
      <Tile k="Leader">
        {ov.leader && !ov.leader.expired ? (
          <Pill tone="ok">{ov.leader.host}</Pill>
        ) : (
          <Pill tone="warn">none</Pill>
        )}
      </Tile>
    </div>
  );
}

/**
 * The rolling-week count against the vendor's fair-use threshold.
 *
 * This is a different scarcity from the hourly quota beside it: going past it
 * does not stop reviews, it slows every one of them to about an hour apart. The
 * tile is deliberately quiet until the count is close, and it says when the
 * count is a floor rather than an answer — a log younger than a week cannot
 * report a weekly number, and reading a fresh one as a quiet week is the exact
 * mistake worth preventing.
 */
function FairUseTile({ fu }: { fu: Overview["quota"]["fair_use"] }) {
  const tone = fu.level === "over" ? "text-bad" : fu.level === "warn" ? "text-warn" : "";
  return (
    <Tile k="This week">
      <span className={`tabular-nums ${tone}`} title={fu.note}>
        {fu.fires}
        {fu.limit ? ` / ${fu.limit}` : ""}
        {!fu.complete && <span className="text-faint">+</span>}
      </span>
    </Tile>
  );
}

function Tile({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="min-w-[104px] rounded-lg border border-edge bg-card px-3.5 py-2">
      <div className="text-[11px] font-medium tracking-[0.06em] text-faint uppercase">{k}</div>
      <div className="flex items-center gap-2 text-[14.5px] font-semibold">{children}</div>
    </div>
  );
}
