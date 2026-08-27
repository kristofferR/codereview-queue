import { join, normalize } from "node:path";

const source = "http://127.0.0.1:7777/api/snapshot";
const dist = join(import.meta.dir, "..", "..", "internal", "serve", "dist");
const base = await fetch(source).then((response) => response.json());
const started = new Date();

const demoRepos = [
  "acme/payments",
  "acme/mobile",
  "acme/storefront",
  "acme/platform",
  "acme/docs",
  "acme/api",
];

const realRepos = new Set<string>();
const collectRepos = (value: unknown) => {
  if (Array.isArray(value)) {
    value.forEach(collectRepos);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const [key, child] of Object.entries(value)) {
    if (key === "repo" && typeof child === "string") realRepos.add(child.toLowerCase());
    collectRepos(child);
  }
};
collectRepos(base);
const repoMap = new Map(
  [...realRepos].sort().map((repo, index) => [repo, demoRepos[index % demoRepos.length]]),
);

const sanitizeString = (input: string) => {
  let value = input;
  for (const [real, demo] of repoMap) {
    value = value.replaceAll(real, demo).replaceAll(real.toLowerCase(), demo);
  }
  return value
    .replaceAll("kristofferR", "acme")
    .replaceAll("kristofferr", "acme")
    .replaceAll("omarchy", "runner-eu-1")
    .replaceAll("K-Mac.local", "studio-mac")
    .replace(/\/home\/kristoffer\/[^\s\"]*/g, "/opt/crq/bin/tool")
    .replace(/\/Users\/kristoffer\/[^\s\"]*/g, "/opt/crq/bin/tool");
};

const sanitize = (value: unknown): any => {
  if (typeof value === "string") return sanitizeString(value);
  if (Array.isArray(value)) return value.map(sanitize);
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, sanitize(child)]));
};

const at = (minutes: number) => new Date(started.getTime() + minutes * 60_000).toISOString();

const bot = (
  login: string,
  name: string,
  mark: "commanded" | "claimed" | "pending",
  minutes: number,
  required = true,
) => ({ login, name, mark, at: at(minutes), required });

const commonOverview = () => ({
  now: at(0),
  rev: 4821,
  wrote_at: at(-1),
  headline: {
    kind: "reviewing",
    text: "Reviewing acme/payments#1842",
    detail: "Two reviewers are working on the current head. Autofix continues in parallel.",
    subject: "acme/payments#1842",
  },
  quota: {
    scope: "acme",
    remaining: 18,
    checked_at: at(-2),
    last_fired: at(-4),
    fair_use: {
      fires: 42,
      limit: 60,
      complete: true,
      since: at(-6 * 24 * 60),
      level: "ok",
      note: "18 metered reviews remain in the current weekly window",
    },
  },
  slot: {
    held: true,
    key: "acme/payments#1842",
    since: at(-4),
    hold_until: at(16),
  },
  leader: {
    owner: "host=runner-eu-1 pid=4182 run=demo",
    host: "runner-eu-1",
    expires_at: at(2),
    expired: false,
  },
  counts: { in_flight: 2, queued: 3, held: 1, fixing: 1 },
  attention: [],
  in_flight: [
    {
      key: "acme/payments#1842",
      title: "Prevent duplicate captures during checkout retries",
      repo: "acme/payments",
      pr: 1842,
      head: "a81df42c9",
      phase: "reviewing",
      fired_at: at(-4),
      deadline: at(16),
      bots: [
        bot("coderabbitai[bot]", "coderabbitai", "commanded", -4),
        bot("chatgpt-codex-connector[bot]", "codex", "claimed", -3),
      ],
      host: "runner-eu-1",
      next: "Waiting for both required reviewers to finish on this head.",
    },
    {
      key: "acme/platform#905",
      title: "Make deploy locks survive runner restarts",
      repo: "acme/platform",
      pr: 905,
      head: "90eb87f14",
      phase: "fired",
      fired_at: at(-1),
      deadline: at(19),
      bots: [bot("chatgpt-codex-connector[bot]", "codex", "commanded", -1)],
      host: "studio-mac",
      next: "Review requested; waiting for the bot to acknowledge it.",
    },
  ],
  queue: [
    {
      key: "acme/storefront#631",
      title: "Reduce product-grid layout shifts",
      repo: "acme/storefront",
      pr: 631,
      head: "59d8b0a1e",
      position: 1,
      why: "slot busy",
      attempts: 0,
      host: "runner-eu-1",
      next: "Starts when the current metered review releases the fleet slot.",
    },
    {
      key: "acme/mobile#733",
      title: "Keep offline drafts after authentication refresh",
      repo: "acme/mobile",
      pr: 733,
      head: "188ab6fd2",
      position: 2,
      ready_at: at(6),
      why: "cooling down",
      attempts: 1,
      host: "studio-mac",
      next: "Eligible after the minimum interval, then waits for its queue turn.",
    },
    {
      key: "acme/api#412",
      title: "Return stable pagination cursors",
      repo: "acme/api",
      pr: 412,
      head: "72f3ca190",
      position: 3,
      why: "behind an earlier round",
      attempts: 0,
      host: "runner-eu-1",
      next: "Starts after the earlier eligible pull requests finish.",
    },
  ],
  held: [
    {
      key: "acme/docs#219",
      title: "Document multi-region failover",
      repo: "acme/docs",
      pr: 219,
      head: "c6038da21",
      reason: "Waiting for the architecture diagram",
      by: "maya",
      at: at(-37),
    },
  ],
  autofix: {
    sessions: [
      {
        key: "acme/mobile#731",
        repo: "acme/mobile",
        pr: 731,
        head: "21edb7f81",
        host: "studio-mac",
        model: "gpt-5.6-sol",
        attempt: 2,
        max_attempts: 5,
        findings: 3,
        log: "autofix/acme-mobile-731.log",
        since: at(-9),
        heartbeat: at(0),
      },
    ],
    hosts: [
      { name: "runner-eu-1", health: "healthy", last_success: at(-2) },
      { name: "studio-mac", health: "healthy", last_success: at(-1) },
    ],
  },
  finished: [
    {
      key: "acme/api#408",
      title: "Reject expired upload grants",
      repo: "acme/api",
      pr: 408,
      head: "4d197d0bd",
      outcome: "completed",
      note: "all required reviewers completed",
      at: at(-18),
    },
    {
      key: "acme/storefront#628",
      title: "Cache locale dictionaries by release",
      repo: "acme/storefront",
      pr: 628,
      head: "8bb21f6c2",
      outcome: "converged",
      note: "clean on the current head",
      at: at(-32),
    },
    {
      key: "acme/platform#899",
      title: "Bound workspace cleanup concurrency",
      repo: "acme/platform",
      pr: 899,
      head: "6b3adf802",
      outcome: "completed",
      at: at(-48),
    },
  ],
});

const quotaOverview = () => {
  const overview = commonOverview();
  overview.rev = 4824;
  overview.headline = {
    kind: "blocked",
    text: "CodeRabbit quota blocked",
    detail: "Only the metered lane is paused. Codex review and autofix continue.",
    subject: "acme/storefront#631",
  };
  overview.quota = {
    scope: "acme",
    remaining: 0,
    blocked_until: at(48),
    checked_at: at(-1),
    last_fired: at(-31),
    fair_use: {
      fires: 64,
      limit: 60,
      complete: true,
      since: at(-6 * 24 * 60),
      level: "over",
      note: "past the weekly fair-use threshold; metered reviews are paced to the next window",
    },
  };
  overview.slot = { held: false };
  overview.counts = { in_flight: 1, queued: 4, held: 1, fixing: 1 };
  overview.attention = [
    {
      kind: "fairuse",
      level: "bad",
      subject: "acme",
      text: "Past the weekly fair-use threshold (64 of 60)",
      detail: "Metered reviews resume automatically when the vendor window opens.",
      link: "#/settings",
      link_text: "Fleet settings",
    },
  ];
  overview.in_flight = [overview.in_flight[1]];
  overview.queue = [
    ...overview.queue.map((row: any) => ({ ...row, why: "account blocked", ready_at: at(48) })),
    {
      key: "acme/payments#1844",
      title: "Make settlement exports idempotent",
      repo: "acme/payments",
      pr: 1844,
      head: "ef92a36b1",
      position: 4,
      ready_at: at(48),
      why: "account blocked",
      attempts: 0,
      host: "runner-eu-1",
      next: "Waits for the metered quota window, then its turn in the queue.",
    },
  ];
  return overview;
};

const demoSnapshot = (scenario: string) => {
  const snapshot = sanitize(structuredClone(base));
  snapshot.overview = scenario === "quota" ? quotaOverview() : commonOverview();
  snapshot.events = [
    {
      at: at(-1),
      kind: "reviewing",
      level: "info",
      repo: "acme/payments",
      pr: 1842,
      head: "a81df42c9",
      text: "Codex acknowledged the review command",
      detail: "both required reviewers are now working",
    },
    {
      at: at(-4),
      kind: "fired",
      level: "ok",
      repo: "acme/payments",
      pr: 1842,
      head: "a81df42c9",
      text: "Posted the metered review command",
    },
    {
      at: at(-9),
      kind: "autofix",
      level: "info",
      repo: "acme/mobile",
      pr: 731,
      head: "21edb7f81",
      text: "Autofix started attempt 2 of 5",
      detail: "3 verified findings on the current head",
    },
    {
      at: at(-18),
      kind: "completed",
      level: "ok",
      repo: "acme/api",
      pr: 408,
      head: "4d197d0bd",
      text: "All required reviewers completed",
    },
  ];

  snapshot.repos = demoRepos.map((repo, index) => {
    const template = snapshot.repos[index % Math.max(snapshot.repos.length, 1)] ?? {};
    return {
      ...template,
      repo,
      enrollment: index === 5 ? "state" : "env",
      reviewed: true,
      env_host: index === 1 ? "studio-mac" : "runner-eu-1",
      reviewers: index === 2 ? ["codex", "cursor"] : ["coderabbitai", "codex"],
      required: index === 2 ? ["codex"] : ["coderabbitai", "codex"],
      primary_off: index === 2,
      override: index === 2,
      autofix: index === 4 ? "off" : index === 1 ? "on" : "default",
      autofix_reason: index === 4 ? "documentation-only repository" : undefined,
      active_rounds: index < 2 ? 1 : 0,
      queued_rounds: index === 2 || index === 5 ? 1 : 0,
      held_prs: index === 4 ? 1 : 0,
      fixing: index === 1 ? 1 : 0,
    };
  });

  snapshot.setup.checks = [
    { key: "state", label: "Queue home", status: "ok", detail: "acme/review-state · ref crq-state-v3 · rev 4821" },
    { key: "dashboard", label: "Markdown dashboard", status: "ok", detail: "issue #12" },
    { key: "calibration", label: "Quota calibration", status: "ok", detail: "PR #1" },
    { key: "leader", label: "Review daemon", status: "ok", detail: "leader runner-eu-1" },
    { key: "tools", label: "Required tools", status: "ok", detail: "present on both hosts" },
    { key: "autofix", label: "Autofix", status: "ok", detail: "2 hosts reporting" },
  ];
  snapshot.setup.hosts = [
    { name: "runner-eu-1", roles: ["autofix", "leader", "serve"], health: "healthy", last_seen: at(-1), caps: 15 },
    { name: "studio-mac", roles: ["autofix"], health: "healthy", last_seen: at(-2), caps: 15 },
  ];
  snapshot.setup.tools_host = "runner-eu-1";
  snapshot.settings.config.gate_repo = "acme/review-state";
  snapshot.settings.config.scope = ["acme/*"];
  snapshot.settings.config.workspace_root = "/srv/crq/workspaces";
  snapshot.settings.quota = snapshot.overview.quota;
  if (snapshot.settings.fleet) {
    snapshot.settings.fleet.by = "release-ops";
    snapshot.settings.fleet.updated_at = at(-120);
  }
  return snapshot;
};

const prView = (scenario: string) => ({
  repo: "acme/payments",
  pr: 1842,
  rev: scenario === "quota" ? 4824 : 4821,
  title: "Prevent duplicate captures during checkout retries",
  round: {
    head: "a81df42c9",
    phase: "reviewing",
    attempts: 1,
    enqueued_at: at(-7),
    fired_at: at(-4),
    deadline: at(16),
    host: "runner-eu-1",
    bots: [
      bot("coderabbitai[bot]", "coderabbitai", "commanded", -4),
      bot("chatgpt-codex-connector[bot]", "codex", "claimed", -3),
    ],
    next: "Resolve the verified findings or push a new head for another review round.",
  },
  observed: {
    head: "a81df42c9",
    converged: false,
    status: "reviewing",
    reason: "3 verified findings remain",
    reviewed_by: { "coderabbitai[bot]": true, "chatgpt-codex-connector[bot]": true },
    checked_at: at(-1),
    dismissed: 1,
    findings: [
      {
        id: "demo-1",
        bot: "coderabbitai[bot]",
        severity: "major",
        category: "correctness",
        effort: "medium",
        path: "internal/checkout/capture.go",
        line: 184,
        title: "Retry path can submit the same capture twice",
        body: "The retry branch does not carry the idempotency key from the original attempt, so a network timeout can create a second capture.",
        thread_id: "demo-thread-1",
        commit: "a81df42c9",
        created_at: at(-2),
      },
      {
        id: "demo-2",
        bot: "chatgpt-codex-connector[bot]",
        severity: "potential",
        category: "reliability",
        effort: "small",
        path: "internal/checkout/reconcile.go",
        line: 77,
        title: "Reconciliation stops after the first transient error",
        body: "A single provider timeout ends the batch and leaves later settlements unprocessed until the next scheduled run.",
        thread_id: "demo-thread-2",
        commit: "a81df42c9",
        created_at: at(-2),
      },
      {
        id: "demo-3",
        bot: "coderabbitai[bot]",
        severity: "minor",
        category: "tests",
        effort: "small",
        path: "internal/checkout/capture_test.go",
        line: 246,
        title: "Missing assertion for the preserved idempotency key",
        body: "The retry test checks the response but not the provider request, so this regression would still pass.",
        thread_id: "demo-thread-3",
        commit: "a81df42c9",
        created_at: at(-2),
      },
    ],
  },
  cost: {
    low: 0.32,
    high: 0.48,
    exact: false,
    summary: "$0.32–$0.48 estimated for one more full round",
    prices_checked_at: "2026-08-01",
    pricing_note: "Estimate based on the current diff and published reviewer pricing.",
    reviewers: [
      { bot: "coderabbitai[bot]", low: 0.24, high: 0.36, basis: "418 changed lines" },
      { bot: "chatgpt-codex-connector[bot]", low: 0.08, high: 0.12, basis: "418 changed lines" },
    ],
    diff: { additions: 336, deletions: 82, changed_files: 11 },
  },
  history: [
    { head: "a81df42c9", outcome: "reviewing", at: at(-4), current: true },
    { head: "4af71c290", outcome: "superseded", note: "new commits pushed", at: at(-22) },
  ],
});

const scenarioFor = (request: Request) => {
  const referer = request.headers.get("referer");
  if (!referer) return "busy";
  try {
    return new URL(referer).searchParams.get("scenario") ?? "busy";
  } catch {
    return "busy";
  }
};

const server = Bun.serve({
  port: 7788,
  async fetch(request) {
    const url = new URL(request.url);
    const scenario = scenarioFor(request);
    if (url.pathname === "/api/snapshot") {
      return Response.json(demoSnapshot(scenario));
    }
    if (url.pathname === "/api/events") {
      const payload = `data: ${JSON.stringify(demoSnapshot(scenario))}\n\n`;
      return new Response(payload, {
        headers: {
          "cache-control": "no-cache",
          "content-type": "text/event-stream",
        },
      });
    }
    if (url.pathname.startsWith("/api/pr/")) {
      return Response.json(prView(scenario));
    }
    if (url.pathname.startsWith("/api/icon/")) {
      return new Response(null, { status: 404 });
    }
    if (url.pathname.startsWith("/api/")) {
      return new Response("demo endpoint is read-only", { status: 405 });
    }

    const relative = url.pathname === "/" ? "index.html" : url.pathname.slice(1);
    const filePath = normalize(join(dist, relative));
    if (!filePath.startsWith(dist)) return new Response("not found", { status: 404 });
    const file = Bun.file(filePath);
    if (await file.exists()) return new Response(file);
    return new Response(Bun.file(join(dist, "index.html")));
  },
});

console.log(`demo dashboard ready on ${server.url}`);
