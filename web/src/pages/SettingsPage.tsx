import type { BotCard, SettingsView, Snapshot } from "../api";
import { EnvEditor } from "../EnvEditor";
import { FleetEditor } from "../FleetEditor";
import { clock } from "../time";
import { Card, Pill, Td, Th } from "../ui";

/* --------------------------------------------------------------- Settings */

export function SettingsPage({
  settings,
  bots,
  onSnapshot,
}: {
  settings: SettingsView;
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const c = settings.config;
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Fleet settings</h1>
      <p className="mt-1 max-w-[840px] text-[13.5px] text-mut">
        The defaults every repository inherits. The editable ones live in shared state, so one
        host's env file is no longer the fleet's source of truth; everything below the first card is
        this server's own environment, shown for reference.
      </p>

      {settings.fleet && <FleetEditor fleet={settings.fleet} bots={bots} onSnapshot={onSnapshot} />}

      {settings.env && <EnvEditor env={settings.env} onSnapshot={onSnapshot} />}

      <Card title="CodeRabbit account" end="the metered lane's shared quota">
        <div className="flex flex-wrap gap-4 px-[18px] py-3">
          <Box k="Scope" v={settings.quota.scope || "—"} />
          <Box
            k="Remaining"
            v={
              settings.quota.remaining === null || settings.quota.remaining === undefined
                ? "unknown"
                : String(settings.quota.remaining)
            }
          />
          <Box
            k="Blocked until"
            v={settings.quota.blocked_until ? clock(settings.quota.blocked_until) : "not blocked"}
          />
          <Box k="Source" v={settings.quota.source || "—"} />
          <Box k="Checked" v={clock(settings.quota.checked_at)} />
        </div>
      </Card>

      <Card title="Reviewers" count={c.reviewers.length}>
        <table className="mt-1.5 w-full border-collapse">
          <thead>
            <tr>
              <Th>Reviewer</Th>
              <Th>Role</Th>
              <Th>Required</Th>
              <Th>Trigger</Th>
              <Th className="c-host">Command</Th>
            </tr>
          </thead>
          <tbody>
            {c.reviewers.map((r) => (
              <tr key={r.login} className="hover:bg-[#F7F8FA]">
                <Td className="font-[550]">{r.name}</Td>
                <Td>
                  {r.primary ? (
                    <Pill tone="warn">primary · metered</Pill>
                  ) : (
                    <Pill tone="ok">co-reviewer · free</Pill>
                  )}
                </Td>
                <Td>{r.required ? "waits for it" : <span className="text-faint">no</span>}</Td>
                <Td>
                  {r.trigger || "—"}
                  {r.trigger === "selfheal" && r.grace ? ` · ${r.grace}` : ""}
                </Td>
                <Td className="c-host font-mono text-[12.5px] text-mut">{r.command || "—"}</Td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <Card title="Pacing &amp; limits" end="protects the shared quota and GitHub's REST budget">
        <div className="px-[18px] pb-3">
          <Row
            k="Minimum interval between fires"
            v={c.min_interval}
            d="the queue's main throttle"
          />
          <Row
            k="In-flight timeout"
            v={c.inflight_timeout}
            d="release a round whose bot never answered"
          />
          <Row k="Watch interval" v={c.watch_interval} d="how often open PRs are driven forward" />
        </div>
      </Card>

      <Card title="Autofix defaults">
        <div className="px-[18px] pb-3">
          <Row
            k="Agent command"
            v={c.autofix_command?.join(" ") || "not configured"}
            d="one argv for the fleet"
          />
          <Row k="Max attempts per head" v={String(c.autofix_max_attempts ?? "—")} />
          <Row k="Review rounds per PR" v={String(c.max_review_rounds ?? "—")} />
          <Row
            k="Concurrency"
            v={c.autofix_concurrency ? String(c.autofix_concurrency) : "uncapped"}
          />
          <Row k="Fix fork PRs" v={c.autofix_forks ? "yes" : "no"} />
          <Row k="Workspace" v={c.workspace_root || "—"} />
        </div>
      </Card>

      <Card title="Automatic review">
        <div className="px-[18px] pb-3">
          <Row k="Scope" v={c.scope?.join(", ") || "—"} d="owners searched for open PRs" />
          <Row k="Allowlist" v={c.allow_repos?.join(", ") || "everything in scope"} d="CRQ_REPOS" />
          <Row k="Excluded" v={c.exclude_repos?.join(", ") || "none"} d="CRQ_EXCLUDE" />
          <Row k="Skip authors" v={c.skip_authors?.join(", ") || "none"} />
          <Row
            k="Skip marker"
            v={c.skip_marker || "—"}
            d="put this in a PR body to keep the fleet off it"
          />
        </div>
      </Card>

      <Card title="Plumbing" end="read-only — crq init owns these">
        <div className="px-[18px] pb-3">
          {settings.plumbing.map((p) => (
            <Row key={p.key} k={p.key} v={p.value} d={p.detail} />
          ))}
        </div>
      </Card>
    </main>
  );
}

function Row({ k, v, d }: { k: string; v: string; d?: string }) {
  return (
    <div className="grid grid-cols-[260px_1fr] gap-3 border-b border-[#EEF0F3] py-2 text-[13.5px] last:border-none max-[1150px]:grid-cols-[minmax(0,1fr)] max-[1150px]:gap-1">
      <span className="font-medium">
        {k}
        {d && <span className="block text-xs font-normal text-faint">{d}</span>}
      </span>
      <span className="font-mono text-[12.5px] break-words text-mut">{v}</span>
    </div>
  );
}

function Box({ k, v }: { k: string; v: string }) {
  return (
    <div className="min-w-[120px] rounded-lg border border-edge px-3.5 py-2">
      <div className="text-[11px] font-medium tracking-[0.06em] text-faint uppercase">{k}</div>
      <div className="mt-0.5 text-[15px] font-[650]">{v}</div>
    </div>
  );
}
