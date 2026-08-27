import { Link } from "@tanstack/react-router";
import { ExternalLink } from "lucide-react";
import { type ReactNode, useState } from "react";
import type { Bot } from "./api";

/** Status pill: always a dot *and* a word, never colour alone. */
export function Pill({
  tone = "mut",
  children,
}: {
  tone?: "ok" | "warn" | "bad" | "mut" | "acc";
  children: ReactNode;
}) {
  const tones = {
    ok: "text-ok bg-ok-bg border-ok-edge",
    warn: "text-warn bg-warn-bg border-warn-edge",
    bad: "text-bad bg-bad-bg border-bad-edge",
    mut: "text-mut bg-[#EEF0F3] border-edge",
    acc: "text-acc bg-acc-bg border-acc-edge",
  }[tone];
  return (
    <span
      className={`inline-flex items-center gap-1.5 whitespace-nowrap rounded-full border px-2.5 py-px text-xs font-medium ${tones}`}
    >
      <span className="size-[7px] shrink-0 rounded-full bg-current" />
      {children}
    </span>
  );
}

export function Card({
  title,
  count,
  end,
  children,
}: {
  title: string;
  count?: ReactNode;
  end?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="data-card mb-3.5 overflow-x-auto rounded-[10px] border border-edge bg-card shadow-card">
      <header className="sticky left-0 flex min-w-fit flex-wrap items-baseline gap-2.5 border-b border-edge/70 px-[18px] py-2.5">
        <h2 className="text-[14px] font-[650] tracking-[-0.01em]">{title}</h2>
        {count !== undefined && <span className="text-[12.5px] text-faint">{count}</span>}
        {end && <span className="ml-auto text-[12.5px] text-faint">{end}</span>}
      </header>
      {children}
    </section>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-[18px] py-4 text-[13px] text-faint">{children}</p>;
}

/**
 * A repository's icon: the favicon out of the repo itself, falling back to a
 * letter tile. The server fetches and caches it — the browser must not hold a
 * GitHub token, and private repos would 404 without one.
 */
export function RepoIcon({ repo, size = 16 }: { repo: string; size?: number }) {
  const [failed, setFailed] = useState(false);
  const name = repo.split("/").pop() ?? repo;
  const style = { width: size, height: size, borderRadius: size > 20 ? 7 : 4 };
  if (failed) {
    return (
      <span
        title={repo}
        style={{ ...style, fontSize: Math.max(9, size * 0.5) }}
        className="inline-flex shrink-0 items-center justify-center border border-edge bg-[#7A8496] align-[-3px] font-bold text-white"
      >
        {name.slice(0, 1).toUpperCase()}
      </span>
    );
  }
  return (
    <img
      src={`/api/icon/repo/${repo}`}
      alt=""
      title={repo}
      style={style}
      onError={() => setFailed(true)}
      className="shrink-0 border border-edge bg-white object-cover align-[-3px]"
    />
  );
}

/** A reviewer's GitHub avatar, with the same server-side fetch and fallback. */
export function BotIcon({
  login,
  name,
  size = 20,
}: {
  login: string;
  name: string;
  size?: number;
}) {
  const [failed, setFailed] = useState(false);
  const style = { width: size, height: size, borderRadius: size > 24 ? 10 : 5 };
  if (failed) {
    return (
      <span
        title={name}
        style={{ ...style, fontSize: Math.max(9, size * 0.42) }}
        className="inline-flex shrink-0 items-center justify-center border border-edge bg-bg font-mono font-semibold text-mut"
      >
        {name.slice(0, 2).toUpperCase()}
      </span>
    );
  }
  return (
    <img
      src={`/api/icon/bot/${encodeURIComponent(login)}`}
      alt=""
      title={name}
      style={style}
      onError={() => setFailed(true)}
      className="shrink-0 border border-edge bg-white object-cover align-[-3px]"
    />
  );
}

/**
 * Links a repo#pr to its detail page, with a small ↗ for GitHub itself. The
 * name goes to crq's own view because that is where the round lives; the arrow
 * is for when you actually want the pull request.
 */
export function PRLink({
  repo,
  pr,
  className = "",
}: {
  repo: string;
  pr: number;
  className?: string;
}) {
  const [owner = "", name = repo] = repo.split("/");
  return (
    <span className="inline-flex items-baseline gap-1">
      <Link
        to="/pr/$owner/$name/$pr"
        params={{ owner, name, pr }}
        className={`text-acc hover:underline ${className}`}
      >
        {name}#{pr}
      </Link>
      <a
        href={`https://github.com/${repo}/pull/${pr}`}
        target="_blank"
        rel="noreferrer"
        title="Open the pull request on GitHub"
        className="text-faint hover:text-acc"
      >
        <ExternalLink aria-hidden className="inline size-3" />
        <span className="sr-only">Open on GitHub</span>
      </a>
    </span>
  );
}

/**
 * Links a head to its commit on GitHub. crq stores 9-char short SHAs, which
 * GitHub resolves fine — no need to carry the full one just to build a URL.
 */
export function CommitLink({
  repo,
  sha,
  className = "",
}: {
  repo: string;
  sha?: string;
  className?: string;
}) {
  if (!sha) return <span className="text-faint">—</span>;
  return (
    <a
      href={`https://github.com/${repo}/commit/${sha}`}
      target="_blank"
      rel="noreferrer"
      title={`commit ${sha}`}
      className={`font-mono text-acc hover:underline ${className}`}
    >
      {sha}
    </a>
  );
}

/**
 * Reviewer marks: commanded, claim posted but not yet recorded, or not enabled.
 * The bot's identity travels as data, so this never names a bot itself.
 */
export function BotMarks({ bots }: { bots: Bot[] }) {
  return (
    <span className="inline-flex items-center gap-2">
      {bots.map((b) => {
        const glyph = b.mark === "commanded" ? "✓" : b.mark === "claimed" ? "⏳" : "—";
        const badge =
          b.mark === "commanded" ? "bg-ok" : b.mark === "claimed" ? "bg-warn-fg" : "bg-faint";
        return (
          <span
            key={b.login}
            title={`${b.name}${b.required ? " (required)" : ""} — ${
              b.mark === "commanded"
                ? "trigger commanded"
                : b.mark === "claimed"
                  ? "claim posted, not yet recorded"
                  : "runs here — not asked for this head yet"
            }`}
            className={`relative inline-block ${b.mark === "pending" ? "opacity-45" : ""}`}
          >
            <BotIcon login={b.login} name={b.name} size={22} />
            <i
              className={`absolute -right-1 -bottom-1 flex size-[13px] items-center justify-center rounded-full border-[1.5px] border-card text-[8px] leading-none text-white not-italic ${badge}`}
            >
              {glyph}
            </i>
          </span>
        );
      })}
    </span>
  );
}

export function Th({ children, className = "" }: { children?: ReactNode; className?: string }) {
  return (
    <th
      className={`border-b border-edge px-[18px] py-1.5 text-left text-[11px] font-medium tracking-[0.06em] text-faint uppercase ${className}`}
    >
      {children}
    </th>
  );
}

export function Td({ children, className = "" }: { children?: ReactNode; className?: string }) {
  return (
    <td className={`border-b border-[#EEF0F3] px-[18px] py-2.5 align-top ${className}`}>
      {children}
    </td>
  );
}

/**
 * The one on/off control. Everything that turns a thing on or off uses this,
 * so the gesture never changes between a repository's autofix switch and a
 * bot's — a checkbox in one place and a labelled button in another reads as
 * two different kinds of decision when it is the same one.
 *
 * locked disables it. Callers pass their own title, because "why can I not
 * change this" is specific to what is locked.
 */
export function Toggle({
  on,
  label,
  locked,
  title,
  onClick,
}: {
  on: boolean;
  label: string;
  locked?: boolean;
  title?: string;
  onClick?: () => void;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      disabled={locked}
      onClick={onClick}
      title={title}
      className={`relative inline-block h-[19px] w-[34px] shrink-0 rounded-full transition-colors ${
        on ? "bg-ok" : "bg-[#D6DAE0]"
      } ${locked ? "opacity-55" : ""}`}
    >
      <span
        className={`absolute top-0.5 size-[15px] rounded-full bg-white shadow transition-all ${
          on ? "left-[17px]" : "left-0.5"
        }`}
      />
    </button>
  );
}

/**
 * A pull request's number and what it is about.
 *
 * Every list showed "repo#141" and nothing else, so the queue read as a set of
 * ticket numbers: you had to open each one to find out what it was. The title
 * is recorded on the round when it is enqueued, which is why showing it costs
 * no request — and why it can be a commit or two stale after a rename, which is
 * a fair price for a list that says what it is listing.
 */
export function PRTitle({
  repo,
  pr,
  title,
  className = "",
}: {
  repo: string;
  pr: number;
  title?: string;
  className?: string;
}) {
  return (
    <span className={`grid min-w-0 gap-0.5 ${className}`}>
      <PRLink repo={repo} pr={pr} className="font-[650] leading-[1.15]" />
      {title && (
        <span
          className="block min-w-0 truncate text-[12.5px] leading-[1.25] font-normal text-mut"
          title={title}
        >
          {title}
        </span>
      )}
    </span>
  );
}
