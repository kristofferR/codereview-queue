import { Link } from "@tanstack/react-router";
import type { BotCard } from "../api";
import { ago, useNow } from "../time";
import { BotIcon, Pill, PRLink } from "../ui";

/* ------------------------------------------------------------------- Bots */

const STATUS: Record<
  string,
  { tone: "ok" | "warn" | "bad" | "mut" | "acc"; label: string; note: string }
> = {
  working: { tone: "ok", label: "Working", note: "crq saw it answer here in the last week" },
  quiet: { tone: "warn", label: "Quiet", note: "crq saw it answer once, but not lately" },
  unable: {
    tone: "bad",
    label: "Can't review",
    note:
      "it said it cannot review at all and has not answered since — crq stops waiting for it " +
      "and does not ask again, since no trigger can fix that",
  },
  silent: {
    tone: "bad",
    label: "Never answered",
    note:
      "crq has asked it and never seen an answer — most likely it is not set up on this account, " +
      "and every trigger crq posts for it is a comment nobody reads",
  },
  unverified: {
    tone: "mut",
    label: "Not verified",
    note:
      "enabled, but crq has no evidence either way yet — it records a bot's answers only as it " +
      "observes rounds, so this is normal for a while after an upgrade",
  },
  off: { tone: "mut", label: "Not enabled", note: "crq does not ask for its review on this fleet" },
};

/**
 * The review-bot guide.
 *
 * Deliberately NOT a control surface. Which bots run is a property of a
 * repository (its Reviewers card) or of the fleet default (Settings), and
 * offering the same switch a third time here would mean three places to look
 * for one answer. This page exists for the question those pages cannot answer:
 * what IS this bot, what does it cost, and is it actually set up.
 *
 * "Set up" is the honest part. crq cannot ask any vendor whether you have an
 * account — it can only report what it has seen the bot do here. A bot enabled
 * by a default nobody chose, on an account nobody has, is precisely the case
 * that looks configured and reviews nothing, so it gets its own status rather
 * than being folded into "enabled".
 */
export function BotsPage({ bots }: { bots: BotCard[] }) {
  const now = useNow(5000);
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Review bots</h1>
      <p className="mt-1 max-w-[760px] text-[13.5px] text-mut">
        Every reviewer crq knows how to drive. Status is what crq itself recorded — a trigger it
        posted, a claim it observed — never a status read from the vendor, which none of them
        offers. To change which bots run, use a repository's <b>Reviewers</b> card, or the fleet
        default under <b>Settings</b>.
      </p>

      <div className="mt-4 grid grid-cols-2 gap-4 max-[940px]:grid-cols-1">
        {bots.map((b) => {
          const st = STATUS[b.status] ?? STATUS.off;
          return (
            <section
              key={b.login}
              className={`flex flex-col rounded-[10px] border bg-card shadow-card ${
                b.enabled ? "border-edge" : "border-dashed border-edge"
              }`}
            >
              <header className="flex items-start gap-3 px-5 pt-4">
                <BotIcon login={b.login} name={b.name} size={38} />
                <div className="min-w-0">
                  <h2 className="text-base font-[650]">{b.name}</h2>
                  <div className="text-xs text-faint">
                    {b.primary ? "primary reviewer" : "co-reviewer"} ·{" "}
                    {b.metered ? "spends the shared quota" : "free, no crq quota"}
                  </div>
                </div>
                <span className="ml-auto shrink-0" title={st.note}>
                  <Pill tone={st.tone}>{st.label}</Pill>
                </span>
              </header>

              <div className="flex-1 px-5 pt-3 text-[13px] text-mut">
                {b.pitch && <p>{b.pitch}</p>}
                {b.cost && (
                  <p className="mt-2">
                    <span className="text-faint">Cost — </span>
                    {b.cost}
                    {b.prices_checked_at && (
                      <span className="text-faint"> (checked {b.prices_checked_at})</span>
                    )}
                  </p>
                )}
                {b.suggested && b.because && (
                  <p className="mt-2 rounded-lg border border-ok-edge bg-ok-bg px-2.5 py-1.5 text-[12.5px] text-ok">
                    <b>Suggested here</b> — {b.because}.
                  </p>
                )}
                {b.suited_to && (
                  <p className="mt-2 rounded-lg border border-acc-edge bg-acc-bg px-2.5 py-1.5 text-[12.5px] text-acc">
                    Worth it for {b.suited_to}.
                  </p>
                )}

                <p className="mt-2.5 text-[12.5px] text-faint">
                  {st.note}
                  {b.last_seen &&
                    b.seen_on &&
                    (() => {
                      const [repo = "", number = ""] = b.seen_on.split("#");
                      const pr = Number(number);
                      if (!repo || !Number.isInteger(pr) || pr <= 0) return null;
                      return (
                        <>
                          {" — last on "}
                          <PRLink repo={repo} pr={pr} /> {ago(b.last_seen, now)}
                        </>
                      );
                    })()}
                  {b.repo_count > 0 && ` · ${b.repo_count} repo override(s) name it`}
                </p>

                {b.status === "unable" &&
                  b.unable_at &&
                  (() => {
                    const [repo = "", number = ""] = (b.unable_on ?? "").split("#");
                    const pr = Number(number);
                    const where =
                      repo && Number.isInteger(pr) && pr > 0 ? (
                        <>
                          {" on "}
                          <PRLink repo={repo} pr={pr} />
                        </>
                      ) : null;
                    return (
                      <p className="mt-2 rounded-lg border border-bad-edge bg-bad-bg px-2.5 py-1.5 text-[12.5px] text-bad">
                        {b.unable_reason
                          ? `${b.unable_reason[0]?.toUpperCase()}${b.unable_reason.slice(1)}`
                          : "Cannot review"}{" "}
                        since {ago(b.unable_at, now)}
                        {where}. It reviews again once that is fixed with the vendor; crq notices
                        its next answer and clears this.
                      </p>
                    );
                  })()}

                {b.status === "silent" && b.last_asked && (
                  <p className="mt-2 rounded-lg border border-bad-edge bg-bad-bg px-2.5 py-1.5 text-[12.5px] text-bad">
                    Last asked {ago(b.last_asked, now)} and it has never answered. Turn it off on
                    the repositories that use it, or finish setting it up — until then crq posts a
                    trigger comment on every round and waits out the grace period for nothing.
                  </p>
                )}

                {b.status !== "working" && (b.setup?.length ?? 0) > 0 && (
                  <details className="mt-2.5 rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5">
                    <summary className="cursor-pointer text-[12.5px] font-[550]">
                      Setting it up
                    </summary>
                    <ol className="mt-1.5 list-decimal pl-4 text-[12.5px]">
                      {b.setup?.map((step) => (
                        <li key={step} className="py-0.5">
                          {step}
                        </li>
                      ))}
                    </ol>
                  </details>
                )}
              </div>

              <footer className="mt-3 flex flex-wrap items-center gap-3 border-t border-[#EEF0F3] px-5 py-2.5 text-[12.5px]">
                {b.site && (
                  <a
                    href={b.site}
                    target="_blank"
                    rel="noreferrer"
                    className="text-acc hover:underline"
                  >
                    {b.status === "off" || b.status === "unverified" ? "Sign up ↗" : "Vendor ↗"}
                  </a>
                )}
                {b.docs && (
                  <a
                    href={b.docs}
                    target="_blank"
                    rel="noreferrer"
                    className="text-acc hover:underline"
                  >
                    Docs ↗
                  </a>
                )}
                <Link to="/repos" className="ml-auto text-mut hover:underline">
                  {b.enabled ? "Choose where it runs →" : "Turn it on →"}
                </Link>
              </footer>
            </section>
          );
        })}
      </div>

      <p className="mt-4 max-w-[760px] text-[12px] text-faint">
        Links go straight to each vendor. There are no referral links here: if any are added they
        will say so on the link itself, and suggestions will stay based on what fits your setup
        rather than on what pays.
      </p>
    </main>
  );
}
