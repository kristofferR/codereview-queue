import { useEffect, useState } from "react";
import { type ActionBody, act } from "./actions";
import type { RepoSolver, Snapshot } from "./api";
import { Card, Pill } from "./ui";
import { sameMembers } from "./ui/utils";
import { useOperation } from "./useOperation";

/**
 * How this repository's fix sessions run.
 *
 * The agent is deliberately not editable here. It is chosen by `crq autofix
 * install` and baked into the session script, because switching between claude
 * and codex is a different command line rather than a different flag — so the
 * card names it and stops there, instead of offering a control that would
 * quietly do nothing.
 *
 * Everything else resolves through three layers and each row says which one
 * answered: a value reading `env` is this host's file, `fleet` is the shared
 * default, `repo` is this repository's own.
 */
export function SolverEditor({
  repo,
  solver,
  onSnapshot,
}: {
  repo: string;
  solver: RepoSolver;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [model, setModel] = useState(solver.model ?? "");
  const [effort, setEffort] = useState(solver.effort ?? "");
  const [prompt, setPrompt] = useState(solver.prompt ?? "");
  const [attempts, setAttempts] = useState(String(solver.max_attempts));
  const [rounds, setRounds] = useState(String(solver.max_review_rounds ?? 5));
  const [severities, setSeverities] = useState(solver.severities);
  const [askMode, setAskMode] = useState(solver.ask_mode);
  const [forks, setForks] = useState(solver.forks);
  const [authors, setAuthors] = useState((solver.skip_authors ?? []).join(", "));
  const { run: runOperation, running: busy, error } = useOperation();
  const [warning, setWarning] = useState<string | null>(null);
  const solverModel = solver.model ?? "";
  const solverEffort = solver.effort ?? "";
  const solverPrompt = solver.prompt ?? "";
  const solverAttempts = String(solver.max_attempts);
  const solverRounds = String(solver.max_review_rounds ?? 5);
  const solverSeverities = [...solver.severities].sort().join("\0");
  const solverAskMode = solver.ask_mode;
  const solverAuthors = (solver.skip_authors ?? []).join(", ");

  useEffect(() => {
    setModel(solverModel);
    setEffort(solverEffort);
    setPrompt(solverPrompt);
    setAttempts(solverAttempts);
    setRounds(solverRounds);
    setSeverities(solverSeverities ? solverSeverities.split("\0") : []);
    setAskMode(solverAskMode);
    setForks(solver.forks);
    setAuthors(solverAuthors);
  }, [
    solverModel,
    solverEffort,
    solverPrompt,
    solverAttempts,
    solverRounds,
    solverSeverities,
    solverAskMode,
    solver.forks,
    solverAuthors,
  ]);

  const dirty =
    model !== solverModel ||
    effort !== solverEffort ||
    prompt !== solverPrompt ||
    attempts !== solverAttempts ||
    rounds !== solverRounds ||
    !sameMembers(severities, solver.severities) ||
    askMode !== solverAskMode ||
    forks !== solver.forks ||
    authors !== solverAuthors;
  const roundsValid = rounds !== "";

  const save = (clear = false) => {
    setWarning(null);
    runOperation(
      act("solver", {
        repo,
        solver: clear
          ? { clear: true }
          : solverChange(solver, {
              model,
              effort,
              prompt,
              attempts,
              rounds,
              severities,
              askMode,
              forks,
              authors,
            }),
      }),
      {
        onSuccess: ({ snapshot, warning: next }) => {
          onSnapshot?.(snapshot);
          setWarning(next ?? null);
        },
      },
    );
  };

  const inherit = (
    field:
      | "models"
      | "effort"
      | "prompt"
      | "max_review_rounds"
      | "severities"
      | "ask_mode"
      | "forks"
      | "skip_authors",
  ) => {
    setWarning(null);
    const change: NonNullable<ActionBody["solver"]> =
      field === "models"
        ? { unset_models: true }
        : field === "effort"
          ? { unset_effort: true }
          : field === "prompt"
            ? { unset_prompt: true }
            : field === "max_review_rounds"
              ? { unset_max_review_rounds: true }
              : field === "severities"
                ? { unset_severities: true }
                : field === "ask_mode"
                  ? { unset_ask_mode: true }
                  : field === "forks"
                    ? { unset_forks: true }
                    : { unset_skip_authors: true };
    runOperation(
      act("solver", {
        repo,
        solver: change,
      }),
      {
        onSuccess: ({ snapshot, warning: next }) => {
          onSnapshot?.(snapshot);
          setWarning(next ?? null);
        },
      },
    );
  };

  const src = (key: string) => solver.sources?.[key] ?? "env";

  return (
    <Card
      title="Fix sessions"
      end={
        solver.overridden ? `override${solver.by ? ` by ${solver.by}` : ""}` : "following the fleet"
      }
    >
      <div className="px-[18px] pb-4 pt-1">
        <p className="text-[12.5px] text-faint">
          {solver.agent ? (
            <>
              Agent: <b className="font-mono">{solver.agent.split("/").pop()}</b> — chosen at
              install time and the same for every repository.{" "}
            </>
          ) : (
            <>
              The agent is baked into the installed session script and this server does not read it
              — CRQ_DISPATCH_CMD is set for the autofix service, not for this one.{" "}
            </>
          )}
          Each row below says which layer its value came from.
        </p>

        {(solver.agent_on?.length ?? 0) > 0 && (
          <p className="mt-2 flex flex-wrap items-center gap-2 text-[12px]">
            <span className="text-faint">Agent available on:</span>
            {solver.agent_on?.map((h) => (
              <span
                key={h.host}
                title={
                  h.stale
                    ? "this host's last report is stale"
                    : h.has
                      ? h.path
                      : "not on the PATH that host's service runs with"
                }
                className={`rounded-full border px-2 py-0.5 ${
                  h.stale
                    ? "border-warn-edge bg-warn-bg text-warn"
                    : h.has === undefined
                      ? "border-edge text-faint"
                      : h.has
                        ? "border-ok-edge bg-ok-bg text-ok"
                        : "border-bad-edge bg-bad-bg text-bad"
                }`}
              >
                {h.host}{" "}
                {h.stale ? "· stale" : h.has === undefined ? "· unknown" : h.has ? "✓" : "missing"}
              </span>
            ))}
          </p>
        )}

        {solver.lagging_hosts && solver.lagging_hosts.length > 0 && (
          <div className="mt-2.5 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            These hosts run a binary that predates per-repository fix settings and will use their
            own install-time values: {solver.lagging_hosts.join(", ")}
          </div>
        )}
        {solver.review_budget_lagging_hosts && solver.review_budget_lagging_hosts.length > 0 && (
          <div className="mt-2.5 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            The review-round limit is paused until these review/fix services are upgraded:{" "}
            {solver.review_budget_lagging_hosts.join(", ")}
          </div>
        )}

        <table className="mt-2.5 w-full border-collapse">
          <tbody>
            <Row label="Model" source={src("model")}>
              <input
                aria-label="Autofix model"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="the agent's own default"
                className="w-56 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <Inherit
                shown={src("model") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("models")}
              />
            </Row>
            <Row label="Effort" source={src("effort")}>
              <select
                aria-label="Autofix reasoning effort"
                value={effort}
                onChange={(e) => setEffort(e.target.value)}
                className="rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              >
                <option value="">the agent's own default</option>
                {["low", "medium", "high", "xhigh", "max"].map((e) => (
                  <option key={e} value={e}>
                    {e}
                  </option>
                ))}
              </select>
              <Inherit
                shown={src("effort") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("effort")}
              />
            </Row>
            <Row label="Attempts" source={src("max_attempts")}>
              <input
                aria-label="Maximum autofix attempts per head"
                value={attempts}
                inputMode="numeric"
                onChange={(e) => setAttempts(e.target.value.replace(/[^0-9]/g, ""))}
                className="w-16 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <span className="ml-2 text-[12px] text-faint">
                fix sessions per head before crq stops trying; 0 inherits
              </span>
            </Row>
            <Row label="Review rounds" source={src("max_review_rounds")}>
              <input
                aria-label="Maximum reviewed heads per pull request"
                aria-invalid={!roundsValid}
                value={rounds}
                inputMode="numeric"
                onChange={(e) => setRounds(e.target.value.replace(/[^0-9]/g, ""))}
                className="w-16 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <span className="ml-2 text-[12px] text-faint">
                reviewed heads before a scope hold; 0 is unlimited
              </span>
              <Inherit
                shown={src("max_review_rounds") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("max_review_rounds")}
              />
            </Row>
            <Row label="Fix findings" source={src("severities")}>
              <div className="flex flex-wrap gap-2">
                {[
                  ["critical", "Critical"],
                  ["major", "Major"],
                  ["potential", "Potential"],
                  ["minor", "Minor"],
                  ["unknown", "Unclassified"],
                ].map(([value, label]) => {
                  const active = severities.includes(value);
                  return (
                    <button
                      key={value}
                      type="button"
                      aria-pressed={active}
                      onClick={() =>
                        setSeverities((current) =>
                          active
                            ? current.length > 1
                              ? current.filter((severity) => severity !== value)
                              : current
                            : [...current, value],
                        )
                      }
                      className={`rounded-full border px-3 py-1 text-[12.5px] font-semibold ${
                        active
                          ? "border-ink bg-ink text-white"
                          : "border-edge bg-white text-mut hover:border-edge2"
                      }`}
                    >
                      {label}
                    </button>
                  );
                })}
              </div>
              <Inherit
                shown={src("severities") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("severities")}
              />
            </Row>
            <Row label="Ask me" source={src("ask_mode")}>
              <select
                aria-label="When autofix asks for clarification"
                value={askMode}
                onChange={(event) => setAskMode(event.target.value)}
                className="rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              >
                <option value="blocked">only when blocked</option>
                <option value="uncertain">when confidence is low</option>
                <option value="ambiguous">at first ambiguity</option>
              </select>
              <Inherit
                shown={src("ask_mode") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("ask_mode")}
              />
            </Row>
            <Row label="Fork PRs" source={src("forks")}>
              <button
                type="button"
                onClick={() => setForks((v) => !v)}
                className={`rounded-lg border px-3 py-1 text-[12.5px] font-semibold ${
                  forks ? "border-warn-edge bg-warn-bg text-warn" : "border-edge text-mut"
                }`}
              >
                {forks ? "Allowed" : "Blocked"}
              </button>
              <span className="ml-2 text-[12px] text-faint">
                a session runs an agent over that branch's code with approvals bypassed
              </span>
              <Inherit
                shown={src("forks") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("forks")}
              />
            </Row>
            <Row label="Skip authors" source={src("skip_authors")}>
              <input
                aria-label="Authors skipped by autofix"
                value={authors}
                onChange={(e) => setAuthors(e.target.value)}
                placeholder="dependabot[bot], …"
                className="w-full rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <Inherit
                shown={src("skip_authors") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("skip_authors")}
              />
            </Row>
            <Row label="Extra prompt" source={src("prompt")}>
              <textarea
                aria-label="Extra autofix prompt"
                value={prompt}
                rows={2}
                onChange={(e) => setPrompt(e.target.value)}
                placeholder="standing instruction appended to every fix session here"
                className="w-full rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              />
              <Inherit
                shown={src("prompt") === "repo"}
                busy={busy || dirty}
                onClick={() => inherit("prompt")}
              />
            </Row>
          </tbody>
        </table>

        {warning && (
          <div className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            {warning}
          </div>
        )}
        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        <div className="mt-3.5 flex flex-wrap items-center gap-2.5">
          <button
            type="button"
            disabled={!dirty || busy || !roundsValid}
            onClick={() => void save()}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            {busy ? "Saving…" : "Save fix settings"}
          </button>
          {dirty && (
            <span className="text-[12.5px] text-warn">
              {roundsValid ? "unsaved changes" : "review rounds cannot be blank"}
            </span>
          )}
          {solver.overridden && !dirty && (
            <button
              type="button"
              disabled={busy}
              onClick={() => void save(true)}
              className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
            >
              Reset to the fleet default
            </button>
          )}
        </div>

        <p className="mt-2.5 text-[11.5px] text-faint">
          These reach the session through its environment, not its arguments — the watcher's argv is
          fixed when it starts, and one watcher handles every repository. A session script from an
          install that predates this ignores them, so reinstalling autofix is what turns them on.
        </p>
      </div>
    </Card>
  );
}

export function solverChange(
  solver: RepoSolver,
  edited: {
    model: string;
    effort: string;
    prompt: string;
    attempts: string;
    rounds?: string;
    severities: string[];
    askMode: string;
    forks: boolean;
    authors: string;
  },
): NonNullable<ActionBody["solver"]> {
  const change: NonNullable<ActionBody["solver"]> = {};
  const currentModel = solver.model ?? "";
  if (edited.model !== currentModel) {
    if (edited.model === "") {
      change.models = [];
    } else {
      const ranking = [edited.model, ...solver.models.slice(1)].filter(Boolean);
      change.models = ranking.filter((model, index) => ranking.indexOf(model) === index);
    }
  }
  if (edited.effort !== (solver.effort ?? "")) change.effort = edited.effort;
  if (edited.prompt !== (solver.prompt ?? "")) change.prompt = edited.prompt;
  if (edited.attempts !== String(solver.max_attempts)) {
    change.max_attempts = Number(edited.attempts) || 0;
  }
  if (
    edited.rounds !== undefined &&
    edited.rounds !== "" &&
    edited.rounds !== String(solver.max_review_rounds ?? 5)
  ) {
    change.max_review_rounds = Number(edited.rounds);
  }
  if (!sameMembers(edited.severities, solver.severities)) {
    change.severities = edited.severities;
  }
  if (edited.askMode !== solver.ask_mode) change.ask_mode = edited.askMode;
  if (edited.forks !== solver.forks) change.forks = edited.forks;
  const currentAuthors = (solver.skip_authors ?? []).join(", ");
  if (edited.authors !== currentAuthors) {
    change.skip_authors = edited.authors
      .split(",")
      .map((author) => author.trim())
      .filter(Boolean);
  }
  return change;
}

function Inherit({ shown, busy, onClick }: { shown: boolean; busy: boolean; onClick: () => void }) {
  if (!shown) return null;
  return (
    <button
      type="button"
      disabled={busy}
      onClick={onClick}
      className="ml-2 text-[11.5px] text-acc hover:underline disabled:opacity-45"
    >
      Use fleet default
    </button>
  );
}

function Row({
  label,
  source,
  children,
}: {
  label: string;
  source: string;
  children: React.ReactNode;
}) {
  return (
    <tr className="border-b border-[#EEF0F3] last:border-none">
      <td className="w-32 py-2 pr-3 align-top">
        <div className="text-[13px] font-[550]">{label}</div>
        <Pill tone={source === "repo" ? "ok" : source === "fleet" ? "acc" : "mut"}>{source}</Pill>
      </td>
      <td className="py-2 align-middle">{children}</td>
    </tr>
  );
}
