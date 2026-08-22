import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { AddRepo, EnrollmentEditor } from "../AddRepo";
import { act } from "../actions";
import type { BotCard, FleetImpact, HeldRow, RepoRow, Snapshot } from "../api";
import { Confirm } from "../Confirm";
import { HOLD_ACTIONS_ENABLED } from "../features";
import { SolverEditor } from "../SolverEditor";
import { ago, useNow } from "../time";
import { BotIcon, Card, Empty, Pill, PRLink, RepoIcon, Td, Th, Toggle } from "../ui";
import { sameMembers } from "../ui/utils";
import { useOperation } from "../useOperation";

/* ------------------------------------------------------------------ Repos */

// The label says the ANSWER; the note says where it came from. Both matter: a
// repository excluded by a host's env cannot be turned on from here, and one
// enrolled by a record can.
const ENROLLMENT: Record<
  string,
  { tone: "ok" | "acc" | "mut" | "bad"; label: string; note: string }
> = {
  state: { tone: "ok", label: "Reviewed", note: "recorded here, so every host agrees" },
  env: { tone: "acc", label: "Reviewed", note: "listed in CRQ_REPOS on this host" },
  scope: {
    tone: "ok",
    label: "Reviewed",
    note: "its owner is in CRQ_SCOPE and there is no allow-list",
  },
  excluded: {
    tone: "mut",
    label: "Excluded",
    note: "CRQ_EXCLUDE on this host, or the gate repo — a kill switch state cannot override",
  },
  off: {
    tone: "mut",
    label: "Not reviewed",
    note: "no record, and this host's allow-list omits it",
  },
};

export function ReposPage({
  repos,
  bots,
  held,
  onSnapshot,
}: {
  repos: RepoRow[];
  bots: BotCard[];
  held: HeldRow[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const now = useNow(5000);
  const [picked, setPicked] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const selected = repos.find((r) => r.repo === picked) ?? repos[0];
  return (
    <main className="mx-auto grid max-w-[1400px] grid-cols-[320px_minmax(0,1fr)] items-start gap-4.5 px-6 pt-4.5 pb-16 max-[1400px]:grid-cols-[minmax(0,1fr)]">
      <div>
        <Card
          title="Repositories"
          count={repos.length}
          end={
            <button
              type="button"
              onClick={() => setAdding(true)}
              className="rounded-lg bg-ink px-2.5 py-1 text-[12px] font-semibold text-white"
            >
              Add repository
            </button>
          }
        >
          {repos.length === 0 ? (
            <Empty>No repository has been seen yet.</Empty>
          ) : (
            <ul>
              {repos.map((r) => {
                const e = ENROLLMENT[r.enrollment] ?? ENROLLMENT.off;
                const label = r.enrollment === "state" && !r.reviewed ? "Turned off" : e.label;
                const tone = r.enrollment === "state" && !r.reviewed ? "mut" : e.tone;
                const on = selected?.repo === r.repo;
                return (
                  <li key={r.repo} className="border-t border-[#EEF0F3]">
                    <button
                      type="button"
                      onClick={() => setPicked(r.repo)}
                      title={e.note}
                      className={`w-full px-4 py-2.5 text-left ${on ? "border-l-[3px] border-acc bg-acc-bg pl-[13px]" : "hover:bg-[#F7F8FA]"}`}
                    >
                      <div className="flex items-center gap-2 text-[13.5px] font-[550]">
                        <RepoIcon repo={r.repo} />
                        {short(r.repo)}
                        <span className="ml-auto">
                          <Pill tone={tone}>{label}</Pill>
                        </span>
                      </div>
                      <div className="mt-0.5 ml-6 text-xs text-faint">
                        {r.override ? "override" : "fleet default"}
                        {r.active_rounds > 0 && ` · ${r.active_rounds} active`}
                        {r.held_prs > 0 && ` · ${r.held_prs} held`}
                        {r.fixing > 0 && ` · ${r.fixing} fixing`}
                      </div>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
          <p className="border-t border-[#EEF0F3] px-4 py-2.5 text-xs text-faint">
            A record in shared state wins over a host's CRQ_REPOS in both directions; CRQ_EXCLUDE
            wins over everything, because it is a per-host kill switch.
          </p>
        </Card>
      </div>

      {selected && (
        <div>
          <div className="mb-3.5 flex flex-wrap items-center gap-3.5 rounded-[10px] border border-edge bg-card px-5 py-4 shadow-card">
            <RepoIcon repo={selected.repo} size={26} />
            <h1 className="font-mono text-[18px] font-[650] tracking-tight">{selected.repo}</h1>
            <Pill
              tone={
                selected.reviewed ? (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).tone : "mut"
              }
            >
              {selected.reviewed
                ? (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).label
                : selected.enrollment === "state"
                  ? "Turned off"
                  : (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).label}
            </Pill>
            <span className="text-[12px] text-faint">
              {(ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).note}
            </span>
            {selected.override && <Pill tone="warn">Override</Pill>}
            {selected.primary_off && <Pill tone="bad">Primary off</Pill>}
            {selected.solver?.overridden && <Pill tone="warn">Fix settings</Pill>}
            <span className="text-[12.5px] text-faint">
              {selected.active_rounds} active · {selected.queued_rounds} queued
              {selected.override_by && ` · set by ${selected.override_by}`}
              {selected.override_at && ` ${ago(selected.override_at, now)}`}
            </span>
            <a
              href={`https://github.com/${selected.repo}`}
              target="_blank"
              rel="noreferrer"
              className="ml-auto text-[12.5px] text-acc hover:underline"
            >
              Open on GitHub ↗
            </a>
          </div>

          <EnrollmentEditor
            key={`${selected.repo}-enroll`}
            repo={selected.repo}
            source={selected.enrollment}
            reviewed={selected.reviewed}
            envConflict={selected.env_conflict}
            clearEnables={selected.clear_enables}
            reason={selected.enroll_reason}
            by={selected.enroll_by}
            active={selected.active_rounds}
            onSnapshot={onSnapshot}
          />
          <HeldHere
            key={`${selected.repo}-held`}
            repo={selected.repo}
            held={held.filter((h) => h.repo.toLowerCase() === selected.repo.toLowerCase())}
            elsewhere={
              held.filter((h) => h.repo.toLowerCase() !== selected.repo.toLowerCase()).length
            }
            now={now}
            onSnapshot={onSnapshot}
          />
          <ReviewerEditor key={selected.repo} repo={selected} bots={bots} onSnapshot={onSnapshot} />
          <AutofixEditor
            key={`${selected.repo}-autofix`}
            repo={selected}
            now={now}
            onSnapshot={onSnapshot}
          />
          {selected.solver && (
            <SolverEditor
              key={`${selected.repo}-solver`}
              repo={selected.repo}
              solver={selected.solver}
              onSnapshot={onSnapshot}
            />
          )}
        </div>
      )}
      <AddRepo open={adding} onClose={() => setAdding(false)} onSnapshot={onSnapshot} />
    </main>
  );
}

/**
 * Holds on this repository, and a way to place one.
 *
 * A hold could only be placed from an Overview row, which means from the one
 * page that shows every repository at once — so the answer to "stop reviewing
 * this PR while I rework it" was to go and find it in a list. It belongs where
 * the repository's other decisions are.
 */
function HeldHere({
  repo,
  held,
  elsewhere,
  now,
  onSnapshot,
}: {
  repo: string;
  held: HeldRow[];
  elsewhere: number;
  now: number;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [pr, setPr] = useState("");
  const [reason, setReason] = useState("");
  const { run: runOperation, running: busy, error } = useOperation();
  const [warning, setWarning] = useState<string | null>(null);

  const run = (kind: "hold" | "unhold", num: number, why = "") => {
    setWarning(null);
    runOperation(act(kind, { repo, pr: num, reason: why }), {
      onSuccess: ({ snapshot, warning: nextWarning }) => {
        onSnapshot?.(snapshot);
        setWarning(nextWarning ?? null);
        setAdding(false);
        setPr("");
        setReason("");
      },
    });
  };

  return (
    <Card
      title="Held pull requests"
      count={held.length}
      end={elsewhere > 0 ? `${elsewhere} held elsewhere` : undefined}
    >
      <div className="px-[18px] pb-3.5 pt-1">
        {held.length === 0 ? (
          <p className="text-[12.5px] text-faint">
            Nothing is held here. A hold stops crq enqueuing or firing for one pull request until
            somebody lifts it; reviews already in flight finish.
          </p>
        ) : (
          <ul className="text-[13px]">
            {held.map((h) => (
              <li
                key={h.key}
                className="flex items-start gap-2 border-b border-[#EEF0F3] py-1.5 last:border-none"
              >
                <span className="min-w-0">
                  <PRLink repo={h.repo} pr={h.pr} />
                  {h.title && <span className="ml-2 text-[12.5px] text-mut">{h.title}</span>}
                  <div className="text-[12.5px] text-faint">
                    “{h.reason}”{h.by && ` — ${h.by}`} {h.at && ago(h.at, now)}
                  </div>
                </span>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void run("unhold", h.pr)}
                  className="ml-auto shrink-0 rounded-lg border border-edge px-2.5 py-0.5 text-[12px] font-semibold text-mut disabled:opacity-45"
                >
                  Unhold
                </button>
              </li>
            ))}
          </ul>
        )}

        {error && (
          <div className="mt-2.5 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}
        {warning && (
          <div
            role="status"
            className="mt-2.5 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn"
          >
            {warning}
          </div>
        )}

        {HOLD_ACTIONS_ENABLED &&
          (adding ? (
            <div className="mt-2.5 flex flex-wrap items-center gap-2">
              <input
                aria-label="Pull request number to hold"
                value={pr}
                inputMode="numeric"
                placeholder="PR #"
                onChange={(e) => setPr(e.target.value.replace(/[^0-9]/g, ""))}
                className="w-20 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <input
                aria-label="Reason for holding the pull request"
                value={reason}
                placeholder="why — every screen that shows the hold shows this"
                onChange={(e) => setReason(e.target.value)}
                className="min-w-[260px] flex-1 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              />
              <button
                type="button"
                disabled={busy || !pr || reason.trim() === ""}
                onClick={() => void run("hold", Number(pr), reason.trim())}
                className="rounded-lg bg-ink px-3 py-1 text-[12.5px] font-semibold text-white disabled:opacity-45"
              >
                Hold it
              </button>
              <button
                type="button"
                onClick={() => setAdding(false)}
                className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut"
              >
                Cancel
              </button>
            </div>
          ) : (
            <button
              type="button"
              onClick={() => setAdding(true)}
              className="mt-2.5 rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut"
            >
              Hold a pull request…
            </button>
          ))}
      </div>
    </Card>
  );
}

// Set-up bots first, then unproven, then ones crq has asked and never heard
// from — worst last, because that is the order in which they are worth
// considering.
function rank(b: BotCard) {
  return b.status === "working" ? 0 : b.status === "silent" ? 2 : 1;
}

/** Runs and Required are separate ideas, so they get separate toggles. */
function ReviewerEditor({
  repo,
  bots,
  onSnapshot,
}: {
  repo: RepoRow;
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [runs, setRuns] = useState<string[]>(repo.reviewers);
  const [required, setRequired] = useState<string[]>(repo.required);
  const [saveImpact, setSaveImpact] = useState<FleetImpact | null>(null);
  const [resetImpact, setResetImpact] = useState<FleetImpact | null>(null);
  const { run: runOperation, running: busy, error, clearError } = useOperation();
  const [warning, setWarning] = useState<string | null>(null);
  const serverRuns = [...repo.reviewers].sort().join("\0");
  const serverRequired = [...repo.required].sort().join("\0");

  useEffect(() => {
    setRuns(serverRuns ? serverRuns.split("\0") : []);
    setRequired(serverRequired ? serverRequired.split("\0") : []);
    setSaveImpact(null);
    setResetImpact(null);
  }, [serverRuns, serverRequired]);

  // The primary is the one metered reviewer, so its Runs toggle is a budget
  // decision (a private repo on a free plan gets nothing from it) and travels
  // in its own field — the co-reviewer list only accepts registry bots.
  const primaryBot = bots.find((b) => b.primary);
  const primaryOn = primaryBot ? runs.includes(primaryBot.name) : false;
  const primaryWas = primaryBot ? repo.reviewers.includes(primaryBot.name) : false;

  const dirty = !sameMembers(runs, repo.reviewers) || !sameMembers(required, repo.required);
  const newlyOn = runs.filter((b) => !repo.reviewers.includes(b) && b !== primaryBot?.name);
  const configurableNames = new Set(bots.filter((bot) => bot.configurable).map((bot) => bot.name));
  const retiredOn = [
    ...new Set(
      runs.filter(
        (name) =>
          name !== primaryBot?.name && !configurableNames.has(name) && !required.includes(name),
      ),
    ),
  ];

  const toggleRuns = (name: string) => {
    setRuns((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));
    // Dropping a reviewer must drop its requirement too, or convergence would
    // wait for a bot that never runs.
    setRequired((cur) => (runs.includes(name) ? cur.filter((n) => n !== name) : cur));
  };
  const toggleRequired = (name: string) => {
    setRequired((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));
    // Requiring a reviewer implies running it.
    setRuns((cur) => (cur.includes(name) ? cur : [...cur, name]));
  };

  const reviewerBody = () => ({
    repo: repo.repo,
    cobots: runs.filter((name) => name !== primaryBot?.name && configurableNames.has(name)),
    required,
    primary: primaryBot ? primaryOn : undefined,
  });

  const save = (clear = false, expectedRev?: number) => {
    runOperation(
      act("reviewers", {
        ...reviewerBody(),
        clear,
        expected_rev: expectedRev,
      }),
      {
        onSuccess: ({ snapshot, warning: nextWarning }) => {
          onSnapshot?.(snapshot);
          setWarning(nextWarning ?? null);
          setSaveImpact(null);
          setResetImpact(null);
        },
      },
    );
  };

  const previewReset = () =>
    runOperation(act("reviewers", { repo: repo.repo, clear: true, preview: true }), {
      onSuccess: (impact) => setResetImpact(impact),
    });

  const previewSave = () =>
    runOperation(
      act("reviewers", {
        ...reviewerBody(),
        preview: true,
      }),
      { onSuccess: (impact) => setSaveImpact(impact) },
    );

  return (
    <Card title="Reviewers" end={repo.override ? "override" : "inherited from the fleet"}>
      <div className="px-[18px] pb-4">
        <p className="mb-2.5 text-[12.5px] text-faint">
          <b>Runs</b> — the bot reviews this repo. <b>Required</b> — convergence waits for it.
          Requiring a bot turns Runs on; the required set cannot be empty. Turning the primary off
          means this repo never spends the shared review allowance. A bot crq has never seen work
          here is marked — enabling one is allowed, since a bot cannot prove itself until it is
          asked, but an unset-up one just collects trigger comments.
        </p>
        <table className="w-full border-collapse">
          <thead>
            <tr>
              <Th>Reviewer</Th>
              <Th className="w-20 text-center">Runs</Th>
              <Th className="w-24 text-center">Required</Th>
              <Th className="c-host">Role</Th>
            </tr>
          </thead>
          <tbody>
            {/* Bots crq has actually seen work here come first and are the
                real options. One it has never seen is offered too — a fresh
                bot cannot produce evidence until it is enabled, so hiding it
                would make the first one impossible to turn on — but it is
                marked, because enabling a bot nobody has an account for means
                crq posts a trigger on every round and waits for an answer that
                never comes. */}
            {[...bots]
              .sort((a, b) => rank(a) - rank(b))
              .map((b) => (
                <tr key={b.login} className={b.status === "working" ? "" : "opacity-75"}>
                  <Td>
                    <span className="flex items-center gap-2.5">
                      <BotIcon login={b.login} name={b.name} size={20} />
                      <span className="font-[550]">{b.name}</span>
                      {b.status !== "working" && (
                        <Link
                          to="/bots"
                          title={
                            b.status === "silent"
                              ? "crq has asked it and never seen an answer — most likely not set up"
                              : "crq has never seen this bot work here"
                          }
                        >
                          <Pill tone={b.status === "silent" ? "bad" : "mut"}>
                            {b.status === "silent" ? "never answered" : "not set up?"}
                          </Pill>
                        </Link>
                      )}
                    </span>
                  </Td>
                  <Td className="text-center">
                    <Toggle
                      on={runs.includes(b.name)}
                      label={`${b.name} runs for ${repo.repo}`}
                      locked={!b.primary && !b.configurable}
                      title={
                        b.primary
                          ? "the metered reviewer — turning it off spends no quota here"
                          : b.configurable
                            ? undefined
                            : "this retired reviewer is shown for history and cannot be configured"
                      }
                      onClick={() => toggleRuns(b.name)}
                    />
                  </Td>
                  <Td className="text-center">
                    <Toggle
                      on={required.includes(b.name)}
                      label={`${b.name} required for ${repo.repo}`}
                      locked={!b.primary && !b.configurable}
                      title={
                        !b.primary && !b.configurable
                          ? "this retired reviewer is shown for history and cannot be configured"
                          : undefined
                      }
                      onClick={() => toggleRequired(b.name)}
                    />
                  </Td>
                  <Td className="c-host text-[12.5px] text-faint">
                    {b.primary
                      ? primaryOn
                        ? "primary · metered against the shared allowance"
                        : "primary · turned off here"
                      : "co-reviewer"}
                  </Td>
                </tr>
              ))}
          </tbody>
        </table>

        {warning && (
          <div className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            {warning}
          </div>
        )}
        {error && !saveImpact && !resetImpact && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        <div className="mt-3.5 flex flex-wrap items-center gap-2.5">
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={previewSave}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            Save reviewers
          </button>
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => {
              setRuns(repo.reviewers);
              setRequired(repo.required);
              setSaveImpact(null);
            }}
            className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
          >
            Discard
          </button>
          {dirty && <span className="text-[12.5px] text-warn">unsaved changes</span>}
          {repo.override && !dirty && (
            <button
              type="button"
              disabled={busy}
              onClick={previewReset}
              className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
            >
              Reset to fleet default
            </button>
          )}
        </div>
      </div>

      {saveImpact && (
        <Confirm
          title={`Save reviewers for ${repo.repo.split("/").pop()}?`}
          danger={saveImpact.reopened > 0}
          body={
            <>
              <ul className="mb-2 list-disc pl-4">
                {saveImpact.changes.map((change) => (
                  <li key={change}>{change}</li>
                ))}
              </ul>
              {saveImpact.summary}. This repository will stop following the fleet default and keep
              its own list.
              {primaryBot && primaryOn !== primaryWas && (
                <>
                  {" "}
                  <b>
                    {primaryBot.name} will {primaryOn ? "review" : "no longer review"} this
                    repository.
                  </b>{" "}
                  {primaryOn
                    ? "Its rounds go back to waiting for the fire slot and the account quota."
                    : "Its rounds stop taking the fire slot and stop waiting on the account quota — the co-reviewers resolve them alone."}
                </>
              )}
              {newlyOn.length > 0 && (
                <>
                  {" "}
                  <b>{newlyOn.join(", ")}</b> {newlyOn.length === 1 ? "is" : "are"} newly enabled
                  and may be triggered on current open heads as soon as this is saved.
                </>
              )}
              {retiredOn.length > 0 && (
                <>
                  {" "}
                  <b>{retiredOn.join(", ")}</b> {retiredOn.length === 1 ? "is" : "are"} retired and
                  will be removed from this repository&apos;s saved reviewer and required sets.
                </>
              )}
              {saveImpact.reopened > 0 && (
                <p className="mt-2 text-warn">
                  Reopened rounds are reviewed again, and metered reviews spend the shared
                  allowance.
                </p>
              )}
            </>
          }
          confirmLabel={saveImpact.reopened > 0 ? `Save and reopen ${saveImpact.reopened}` : "Save"}
          busy={busy}
          error={error}
          onConfirm={() => save(false, saveImpact.rev)}
          onCancel={() => {
            setSaveImpact(null);
            clearError();
          }}
        />
      )}
      {resetImpact && (
        <Confirm
          title={`Reset reviewers for ${repo.repo.split("/").pop()}?`}
          danger={resetImpact.reopened > 0}
          confirmLabel={
            resetImpact.reopened > 0
              ? `Reset and reopen ${resetImpact.reopened}`
              : "Reset to fleet default"
          }
          busy={busy}
          error={error}
          body={
            <>
              <ul className="mb-2 list-disc pl-4">
                {resetImpact.changes.map((change) => (
                  <li key={change}>{change}</li>
                ))}
              </ul>
              {resetImpact.summary}
              {resetImpact.reopened > 0 && (
                <p className="mt-2 text-warn">
                  Reopened rounds are reviewed again, and metered reviews spend the shared
                  allowance.
                </p>
              )}
            </>
          }
          onConfirm={() => save(true, resetImpact.rev)}
          onCancel={() => {
            setResetImpact(null);
            clearError();
          }}
        />
      )}
    </Card>
  );
}

/** Autofix is a tri-state: the fleet default is a real choice, not just "on". */
function AutofixEditor({
  repo,
  now,
  onSnapshot,
}: {
  repo: RepoRow;
  now: number;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const { run: runOperation, running: busy, error, clearError } = useOperation();
  const [offReason, setOffReason] = useState<boolean>(false);

  const apply = (enabled?: boolean, reason = "") =>
    runOperation(act("autofix", { repo: repo.repo, enabled, reason }), {
      onSuccess: ({ snapshot }) => {
        onSnapshot?.(snapshot);
        setOffReason(false);
      },
    });

  const choice = repo.autofix;
  return (
    <Card title="Autofix" end={choice === "default" ? "fleet default" : "override"}>
      <div className="px-[18px] pb-4">
        <p className="mb-2.5 text-[12.5px] text-faint">
          Permission only — whether an agent exists is a per-host question, on the Setup page.
        </p>
        <div className="flex flex-wrap items-center gap-3">
          <span className="inline-flex overflow-hidden rounded-lg border border-edge">
            {(["off", "on", "default"] as const).map((v) => (
              <button
                key={v}
                type="button"
                disabled={busy}
                onClick={() =>
                  v === "off" ? setOffReason(true) : apply(v === "on" ? true : undefined)
                }
                className={`border-r border-edge px-3 py-1 text-[12.5px] last:border-r-0 ${
                  choice === v ? "bg-ok-bg font-medium text-ok" : "text-mut hover:bg-bg"
                }`}
              >
                {v === "default" ? "Fleet default" : v}
              </button>
            ))}
          </span>
          {repo.autofix_reason && (
            <span className="text-[13px] text-faint">“{repo.autofix_reason}”</span>
          )}
          {repo.autofix_by && (
            <span className="text-[12.5px] text-faint">
              set by {repo.autofix_by} {ago(repo.autofix_at, now)}
            </span>
          )}
        </div>
        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}
      </div>

      {offReason && (
        <Confirm
          title={`Turn autofix off for ${repo.repo.split("/").pop()}?`}
          body={
            <>
              crq will keep reviewing this repository but stop writing fixes to it. Any session
              already running finishes.
            </>
          }
          confirmLabel="Turn it off"
          needsReason
          reasonLabel="Why (shown wherever this switch appears)"
          busy={busy}
          error={error}
          onConfirm={(reason) => apply(false, reason)}
          onCancel={() => {
            setOffReason(false);
            clearError();
          }}
        />
      )}
    </Card>
  );
}

/** A switch that can be locked — the primary always runs, and says so. */

function short(repo: string): string {
  return repo.split("/").pop() ?? repo;
}
