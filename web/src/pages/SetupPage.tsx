import { Link } from "@tanstack/react-router";
import type { BotCard, HostTools, RepoRow, SetupView } from "../api";
import { ago, useNow } from "../time";
import { BotIcon, Card, Empty, Pill, RepoIcon, Td, Th } from "../ui";

/* ------------------------------------------------------------------ Setup */

const STATUS_TONE: Record<string, "ok" | "warn" | "bad" | "mut"> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "mut",
};

export function SetupPage({
  setup,
  bots,
  repos,
}: {
  setup: SetupView;
  bots: BotCard[];
  repos: RepoRow[];
}) {
  const now = useNow(5000);
  const problems = setup.checks.filter((c) => c.status === "bad" || c.status === "warn").length;
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Setup</h1>
      <p className="mt-1 max-w-[820px] text-[13.5px] text-mut">
        What crq needs, and whether this fleet has it. Useful after the first run too — it is where
        to look when a host stops fixing or a lease goes missing.
      </p>

      <div className="my-3.5 flex flex-wrap items-center gap-3 rounded-[10px] border border-edge bg-card px-5 py-4 shadow-card">
        <h2 className="text-[15px] font-[650]">
          {problems === 0
            ? "Everything crq checks is in place"
            : `${problems} thing(s) need attention`}
        </h2>
        <span className="flex flex-wrap items-center gap-2 text-[12.5px]">
          <Pill tone="ok">{setup.ready} ready</Pill>
          {setup.attention > 0 && <Pill tone="warn">{setup.attention} need attention</Pill>}
          {setup.optional > 0 && <Pill tone="mut">{setup.optional} optional missing</Pill>}
        </span>
        <span className="ml-auto text-[12.5px] text-faint">
          {setup.fleet?.length ?? 0} host(s) reporting · updates as they do
        </span>
      </div>

      {(setup.fleet?.length ?? 0) > 0 && <FleetTools fleet={setup.fleet ?? []} now={now} />}

      <div className="grid grid-cols-2 gap-4 max-[860px]:grid-cols-1">
        <Card title="Review bots" count={bots.filter((b) => b.enabled).length}>
          <div className="px-[18px] pb-3.5 pt-1 text-[13px]">
            {bots.filter((b) => b.enabled).length === 0 ? (
              <Empty>No reviewer is enabled.</Empty>
            ) : (
              <ul>
                {bots
                  .filter((b) => b.enabled)
                  .map((b) => (
                    <li key={b.login} className="flex items-center gap-2 py-1">
                      <BotIcon login={b.login} name={b.name} size={18} />
                      <span className="font-[550]">{b.name}</span>
                      <span className="ml-auto">
                        <Pill
                          tone={
                            b.status === "working"
                              ? "ok"
                              : b.status === "silent" || b.status === "unable"
                                ? "bad"
                                : "mut"
                          }
                        >
                          {b.status === "working"
                            ? "working"
                            : b.status === "unable"
                              ? (b.unable_reason ?? "can't review")
                              : b.status === "silent"
                                ? "never answered"
                                : "not verified"}
                        </Pill>
                      </span>
                    </li>
                  ))}
              </ul>
            )}
            <Link to="/bots" className="mt-1.5 inline-block text-[12.5px] text-acc hover:underline">
              Compare and set them up →
            </Link>
          </div>
        </Card>

        <Card title="Repositories" count={repos.filter((r) => r.reviewed).length}>
          <div className="px-[18px] pb-3.5 pt-1 text-[13px]">
            {repos.filter((r) => r.reviewed).length === 0 ? (
              <Empty>Nothing is enrolled.</Empty>
            ) : (
              <ul>
                {repos
                  .filter((r) => r.reviewed)
                  .slice(0, 8)
                  .map((r) => (
                    <li key={r.repo} className="flex items-center gap-2 py-1">
                      <RepoIcon repo={r.repo} />
                      <span>{short(r.repo)}</span>
                      <span className="ml-auto text-[12px] text-faint">{r.enrollment}</span>
                    </li>
                  ))}
              </ul>
            )}
            {/* An env-enrolled repository is one the other hosts may not agree
                about, which is the whole reason enrollment records exist. */}
            {repos.filter((r) => r.reviewed && r.enrollment === "env").length > 0 && (
              <p className="mt-1.5 rounded-lg border border-warn-edge bg-warn-bg px-2.5 py-1.5 text-[12px] text-warn">
                {repos.filter((r) => r.reviewed && r.enrollment === "env").length} still enrolled by
                this host's env file — the other hosts decide for themselves.{" "}
                <code>crq fleet adopt</code> records them for everyone.
              </p>
            )}
            <Link
              to="/repos"
              className="mt-1.5 inline-block text-[12.5px] text-acc hover:underline"
            >
              Manage repositories →
            </Link>
          </div>
        </Card>
      </div>

      <Card title="Checks" count={setup.checks.length}>
        <div className="px-[18px] pb-3">
          {setup.checks.map((c) => (
            <div
              key={c.key}
              className="flex items-baseline gap-3 border-b border-[#EEF0F3] py-2 text-[13.5px] last:border-none"
            >
              <Pill tone={STATUS_TONE[c.status] ?? "mut"}>{c.status}</Pill>
              <span className="font-[550]">{c.label}</span>
              <span className="text-faint">{c.detail}</span>
            </div>
          ))}
        </div>
      </Card>

      <Card title="Command-line tools" end={`on ${setup.tools_host}`}>
        <p className="px-[18px] pt-1 text-[12.5px] text-faint">
          This machine, probed from this server's own PATH. The per-host table above is the reported
          one — a tool has to be visible to the <i>service</i>, not just to your shell, and only the
          host itself can say whether it is.
        </p>
        <table className="mt-1.5 w-full border-collapse">
          <thead>
            <tr>
              <Th>Tool</Th>
              <Th>Purpose</Th>
              <Th>Status</Th>
              <Th className="c-host">Path</Th>
            </tr>
          </thead>
          <tbody>
            {setup.tools.map((t) => (
              <tr key={t.name} className="hover:bg-[#F7F8FA]">
                <Td className="font-mono text-[13px] font-semibold">{t.name}</Td>
                <Td className="text-[12.5px] text-mut">
                  {t.purpose}
                  {!t.found && (t.fix?.length ?? 0) > 0 && (
                    <details className="mt-1 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1">
                      <summary className="cursor-pointer text-[12px] font-[550]">
                        How to fix it
                      </summary>
                      <pre className="mt-1 overflow-x-auto text-[11.5px] whitespace-pre-wrap">
                        {t.fix?.join("\n")}
                      </pre>
                      <p className="mt-1 text-[11.5px] text-faint">
                        Installing it for your shell is not enough: the service gets its own PATH.
                        Add the directory with <code>systemctl --user edit crq-autofix</code> and an{" "}
                        <code>Environment=PATH=…</code> line, or reinstall so crq writes it for you.
                      </p>
                    </details>
                  )}
                </Td>
                <Td>
                  {t.found ? (
                    <Pill tone="ok">found</Pill>
                  ) : t.required ? (
                    <Pill tone="bad">missing</Pill>
                  ) : (
                    <Pill tone="mut">not installed</Pill>
                  )}
                  {t.required && <span className="ml-2 text-[11.5px] text-faint">required</span>}
                </Td>
                <Td className="c-host font-mono text-[12px] text-faint">{t.path ?? "—"}</Td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <Card title="Hosts" count={setup.hosts.length}>
        <div className="flex flex-wrap gap-3 px-[18px] py-3">
          {setup.hosts.length === 0 && <Empty>No host has written state in the last day.</Empty>}
          {setup.hosts.map((h) => (
            <div
              key={h.name}
              className={`min-w-[220px] flex-1 rounded-lg border px-3.5 py-2.5 text-[12.5px] text-mut ${
                h.health === "unhealthy" ? "border-bad-edge bg-bad-bg" : "border-edge"
              }`}
            >
              <div className="flex items-center gap-2">
                <span className="font-mono text-[13.5px] font-[650] text-ink">{h.name}</span>
                {h.health && (
                  <Pill
                    tone={h.health === "healthy" ? "ok" : h.health === "unhealthy" ? "bad" : "mut"}
                  >
                    {h.health === "unknown" ? "no recent signal" : h.health}
                  </Pill>
                )}
              </div>
              <div className="mt-1">{h.roles?.length ? h.roles.join(" · ") : "writes state"}</div>
              {h.last_seen && <div className="mt-0.5">last write {ago(h.last_seen, now)}</div>}
              {h.last_error && (
                <div className="mt-1 font-mono text-[11.5px] break-words">{h.last_error}</div>
              )}
            </div>
          ))}
        </div>
      </Card>
    </main>
  );
}

/**
 * What each host can actually reach.
 *
 * The single most useful table in the product, because every question about a
 * fleet turns into a per-host question and crq could previously only answer for
 * whichever machine you happened to be asking from. The failure it exists to
 * show is the quiet one: a tool installed for your shell and invisible to the
 * service, which looks fine everywhere except in the fix session that needs it.
 *
 * A host reports its OWN PATH, and a daemon reports the service's — so a tick
 * here means the thing that runs fix sessions can find it, not that you can.
 */
function FleetTools({ fleet, now }: { fleet: HostTools[]; now: number }) {
  const names = [...new Set(fleet.flatMap((h) => h.tools.map((t) => t.name)))];
  return (
    <Card title="Tools, per host" count={`${fleet.length} host(s)`}>
      <div className="overflow-x-auto px-[18px] pb-3">
        <table className="w-full border-collapse text-[12.5px]">
          <thead>
            <tr>
              <Th>Host</Th>
              {names.map((n) => (
                <Th key={n} className="text-center">
                  {n}
                </Th>
              ))}
            </tr>
          </thead>
          <tbody>
            {fleet.map((h) => (
              <tr key={h.host} className="border-t border-[#EEF0F3]">
                <Td>
                  <div className="font-[550]">{h.host}</div>
                  <div className="text-[11.5px] text-faint">
                    {h.version && (
                      <span className={h.behind ? "text-warn" : ""}>
                        crq {h.version}
                        {h.behind ? " · behind" : ""}
                      </span>
                    )}
                    {(h.roles?.length ?? 0) > 0 && ` · ${h.roles?.join(", ")}`}
                    {h.stale && h.at && (
                      <span className="text-warn"> · last heard {ago(h.at, now)}</span>
                    )}
                  </div>
                </Td>
                {names.map((n) => {
                  const t = h.tools.find((x) => x.name === n);
                  return (
                    <Td key={n} className="text-center">
                      {t?.path ? (
                        <span
                          title={`${t.path}${t.version ? ` · ${t.version}` : ""}`}
                          className="text-ok"
                        >
                          ✓
                        </span>
                      ) : (
                        <span
                          className="text-faint"
                          title="not on the PATH this host's service runs with"
                        >
                          —
                        </span>
                      )}
                    </Td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
        <p className="pt-2 text-[11.5px] text-faint">
          <b>These are command-line tools, not the review bots.</b> The Codex and CodeRabbit
          reviewers are GitHub apps and need nothing installed here — a host missing the{" "}
          <code>codex</code> CLI still gets Codex reviews. Whether a REVIEWER works is on the{" "}
          <Link to="/bots" className="text-acc hover:underline">
            Bots
          </Link>{" "}
          page. What these decide is whether this host can run a fix session or a local preflight.
          <br />
          Each host probes the PATH its own crq process runs with. A daemon reports the SERVICE's
          PATH, which is the one that decides whether a fix session can start — a tool installed for
          your shell and missing here is exactly the failure that looks fine until it is needed. Fix
          it by adding the directory to the unit: <code>systemctl --user edit crq-autofix</code>,
          then <code>Environment=PATH=…</code>, then reinstall with <code>crq autofix install</code>
          .
        </p>
      </div>
    </Card>
  );
}

function short(repo: string): string {
  return repo.split("/").pop() ?? repo;
}
