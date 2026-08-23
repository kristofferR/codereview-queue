import { useEffect, useState } from "react";
import { type ActionBody, act } from "./actions";
import type { RepoSolver, Snapshot } from "./api";
import { Confirm } from "./Confirm";
import { Card, Pill } from "./ui";
import { useOperation } from "./useOperation";

type MergeMethod = "" | "merge" | "squash" | "rebase";
type Confirmation = "save" | "end" | "reset" | null;

/**
 * The temporary bulk-review workflow, kept separate from ordinary fix tuning.
 * A campaign is a repository policy boundary: one review round, one isolated
 * fixer/finalizer per PR, then an optional exact-revision merge.
 */
export function CampaignEditor({
  repo,
  solver,
  onSnapshot,
}: {
  repo: string;
  solver: RepoSolver;
  onSnapshot?: (snapshot: Snapshot) => void;
}) {
  const active = solver.one_pass;
  const currentMerge = (solver.merge_method ?? "") as MergeMethod;
  const currentModel = solver.model ?? "";
  const currentEffort = solver.effort ?? "";
  const [model, setModel] = useState(currentModel);
  const [effort, setEffort] = useState(active ? currentEffort : currentEffort || "medium");
  const [mergeMethod, setMergeMethod] = useState<MergeMethod>(active ? currentMerge : "squash");
  const [confirmation, setConfirmation] = useState<Confirmation>(null);
  const [warning, setWarning] = useState<string | null>(null);
  const { run: runOperation, running: busy, error } = useOperation();

  useEffect(() => {
    setModel(currentModel);
    setEffort(active ? currentEffort : currentEffort || "medium");
    setMergeMethod(active ? currentMerge : "squash");
    setConfirmation(null);
  }, [active, currentEffort, currentMerge, currentModel]);

  const dirty =
    !active ||
    model !== currentModel ||
    effort !== currentEffort ||
    mergeMethod !== currentMerge ||
    solver.max_attempts !== 1;

  const run = (solverChange: NonNullable<ActionBody["solver"]>) => {
    setWarning(null);
    runOperation(act("solver", { repo, solver: solverChange }), {
      onSuccess: ({ snapshot, warning: nextWarning }) => {
        onSnapshot?.(snapshot);
        setWarning(nextWarning ?? null);
        setConfirmation(null);
      },
    });
  };

  const save = () => run(campaignChange(solver, { model, effort, mergeMethod }));
  const end = () => run({ unset_one_pass: true, unset_merge: true });
  const reset = () => run({ clear: true });

  return (
    <Card
      title="One-pass campaign"
      end={
        <span className="inline-flex items-center gap-2">
          <Pill tone={active ? "ok" : "mut"}>{active ? "Active" : "Off"}</Pill>
          {active && currentMerge && <span>auto-merge: {currentMerge}</span>}
        </span>
      }
    >
      <div className="px-[18px] pt-3 pb-4">
        <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(310px,0.72fr)]">
          <div>
            <p className="max-w-[760px] text-[13px] leading-5 text-mut">
              Each pull request gets at most one configured review round, followed by exactly one
              isolated fixer/finalizer. A review already recorded on the PR consumes that round. The
              fixed commit is not sent back through another review loop.
            </p>
            <ol className="mt-3 grid gap-1.5 text-[12.5px] text-mut sm:grid-cols-3">
              <CampaignStep n="01" title="Review once" detail="Collect the configured reviewers." />
              <CampaignStep
                n="02"
                title="Fix once"
                detail="One agent integrates base and validates."
              />
              <CampaignStep
                n="03"
                title={mergeMethod ? "Merge fixed head" : "Stop after fixing"}
                detail={
                  mergeMethod
                    ? "Exact head and base, open, non-draft, unheld, conflict-free."
                    : "Leave the finalized PR for a person."
                }
              />
            </ol>
          </div>

          <div className="border-edge lg:border-l lg:pl-4">
            <label className="block">
              <span className="text-[12px] font-semibold text-mut">Fix model</span>
              <select
                aria-label="Campaign fix model"
                value={model}
                onChange={(event) => setModel(event.target.value)}
                className="mt-1 w-full rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 font-mono text-[12.5px]"
              >
                <option value="">agent default</option>
                {solver.model_choices.map((choice) => (
                  <option key={choice} value={choice}>
                    {choice}
                  </option>
                ))}
              </select>
            </label>
            <div className="mt-2.5 grid grid-cols-2 gap-2.5">
              <label>
                <span className="text-[12px] font-semibold text-mut">Effort</span>
                <select
                  aria-label="Campaign reasoning effort"
                  value={effort}
                  onChange={(event) => setEffort(event.target.value)}
                  className="mt-1 w-full rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[12.5px]"
                >
                  <option value="">agent default</option>
                  {["low", "medium", "high", "xhigh", "max"].map((choice) => (
                    <option key={choice} value={choice}>
                      {choice}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                <span className="text-[12px] font-semibold text-mut">After fixing</span>
                <select
                  aria-label="Campaign merge method"
                  value={mergeMethod}
                  onChange={(event) => setMergeMethod(event.target.value as MergeMethod)}
                  className="mt-1 w-full rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[12.5px]"
                >
                  <option value="">do not merge</option>
                  <option value="squash">squash merge</option>
                  <option value="merge">merge commit</option>
                  <option value="rebase">rebase merge</option>
                </select>
              </label>
            </div>
          </div>
        </div>

        {mergeMethod && (
          <div className="mt-3 border-l-[3px] border-warn-fg bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            Auto-merge does not wait for a review bot to approve the fixer commit. It binds the
            merge to the exact pushed head and the base revision the finalizer integrated, and
            refuses drafts, holds, closed PRs, moved revisions, and merge conflicts.
          </div>
        )}
        {solver.lagging_hosts && solver.lagging_hosts.length > 0 && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            Do not start this campaign yet. These review/autofix hosts cannot honor one-pass mode:{" "}
            {solver.lagging_hosts.join(", ")}.
          </div>
        )}
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
            disabled={busy || (active && !dirty) || (solver.lagging_hosts?.length ?? 0) > 0}
            onClick={() => setConfirmation("save")}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            {active ? "Update campaign" : "Start campaign"}
          </button>
          {active && (
            <>
              <button
                type="button"
                disabled={busy}
                onClick={() => setConfirmation("end")}
                className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
              >
                End campaign
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => setConfirmation("reset")}
                className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
              >
                End and restore fleet fix settings
              </button>
            </>
          )}
        </div>
        {active && (
          <p className="mt-2 text-[11.5px] text-faint">
            End campaign removes only one-pass and auto-merge, preserving this repository&apos;s
            model and fix tuning. Restore fleet fix settings clears every repository-level solver
            override.
          </p>
        )}
      </div>

      {confirmation === "save" && (
        <Confirm
          title={active ? "Update this one-pass campaign?" : "Start a one-pass campaign?"}
          body={
            <p>
              Open PRs in <b className="font-mono">{repo}</b> will get no more than one review round
              and one fixer session each.
              {mergeMethod
                ? ` After that session succeeds, crq may ${mergeMethod}-merge without another review round.`
                : " The finalized PRs will not be merged automatically."}
            </p>
          }
          confirmLabel={active ? "Update campaign" : "Start campaign"}
          danger={Boolean(mergeMethod)}
          busy={busy}
          error={error}
          onConfirm={() => save()}
          onCancel={() => setConfirmation(null)}
        />
      )}
      {confirmation === "end" && (
        <Confirm
          title="End this campaign?"
          body="crq will return to ordinary incremental review. Existing model and fixer settings stay in place; pending campaign merge hand-offs are removed."
          confirmLabel="End campaign"
          busy={busy}
          error={error}
          onConfirm={() => end()}
          onCancel={() => setConfirmation(null)}
        />
      )}
      {confirmation === "reset" && (
        <Confirm
          title="End campaign and restore fleet settings?"
          body="This removes every repository-level fix setting, including the campaign, model, effort, prompt, attempt limit, and severity overrides."
          confirmLabel="Restore fleet settings"
          danger
          busy={busy}
          error={error}
          onConfirm={() => reset()}
          onCancel={() => setConfirmation(null)}
        />
      )}
    </Card>
  );
}

export function campaignChange(
  solver: RepoSolver,
  edited: { model: string; effort: string; mergeMethod: MergeMethod },
): NonNullable<ActionBody["solver"]> {
  const change: NonNullable<ActionBody["solver"]> = {
    one_pass: true,
    merge_method: edited.mergeMethod,
    max_attempts: 1,
  };
  if (edited.model !== (solver.model ?? "")) {
    change.models =
      edited.model === ""
        ? []
        : [edited.model, ...solver.models.filter((model) => model !== edited.model)];
  }
  if (edited.effort !== (solver.effort ?? "")) {
    change.effort = edited.effort;
  }
  return change;
}

function CampaignStep({ n, title, detail }: { n: string; title: string; detail: string }) {
  return (
    <li className="border-t border-edge pt-2">
      <div className="flex items-baseline gap-2">
        <span className="font-mono text-[10px] font-semibold tracking-[0.08em] text-acc">{n}</span>
        <b className="text-[12.5px] text-ink">{title}</b>
      </div>
      <p className="mt-0.5 pl-6 text-[11.5px] leading-4 text-faint">{detail}</p>
    </li>
  );
}
