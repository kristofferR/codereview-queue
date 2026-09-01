import { Schema } from "effect";

// Server payloads are mutable view models in the browser. Effect Schema arrays
// are readonly by default, so make that mutability choice once at the boundary.
const MutableArray = <S extends Schema.Constraint>(item: S) => Schema.mutable(Schema.Array(item));
const Optional = Schema.optionalKey;
const StringRecord = Schema.Record(Schema.String, Schema.String);

export const HeadlineSchema = Schema.Struct({
  kind: Schema.Literals(["stranded", "blocked", "reviewing", "awaiting", "queued", "held", "idle"]),
  text: Schema.String,
  detail: Optional(Schema.String),
  subject: Optional(Schema.String),
});
export type Headline = Schema.Schema.Type<typeof HeadlineSchema>;

export const FairUseSchema = Schema.Struct({
  fires: Schema.Number,
  limit: Optional(Schema.Number),
  complete: Schema.Boolean,
  since: Optional(Schema.String),
  level: Schema.Literals(["ok", "warn", "over"]),
  note: Schema.String,
});

export const QuotaSchema = Schema.Struct({
  scope: Optional(Schema.String),
  remaining: Optional(Schema.NullOr(Schema.Number)),
  blocked_until: Optional(Schema.String),
  source: Optional(Schema.String),
  checked_at: Optional(Schema.String),
  last_fired: Optional(Schema.String),
  fair_use: FairUseSchema,
});
export type Quota = Schema.Schema.Type<typeof QuotaSchema>;

export const SlotSchema = Schema.Struct({
  held: Schema.Boolean,
  key: Optional(Schema.String),
  since: Optional(Schema.String),
  hold_until: Optional(Schema.String),
});
export type Slot = Schema.Schema.Type<typeof SlotSchema>;

export const LeaderSchema = Schema.Struct({
  owner: Schema.String,
  host: Schema.String,
  expires_at: Schema.String,
  expired: Schema.Boolean,
});
export type Leader = Schema.Schema.Type<typeof LeaderSchema>;

export const CountsSchema = Schema.Struct({
  in_flight: Schema.Number,
  queued: Schema.Number,
  held: Schema.Number,
  fixing: Schema.Number,
});
export type Counts = Schema.Schema.Type<typeof CountsSchema>;

export const AttentionSchema = Schema.Struct({
  kind: Schema.String,
  level: Schema.Literals(["bad", "warn"]),
  subject: Optional(Schema.String),
  text: Schema.String,
  detail: Optional(Schema.String),
  link: Optional(Schema.String),
  link_text: Optional(Schema.String),
});
export type Attention = Schema.Schema.Type<typeof AttentionSchema>;

export const BotSchema = Schema.Struct({
  login: Schema.String,
  name: Schema.String,
  mark: Schema.Literals(["commanded", "claimed", "pending"]),
  required: Optional(Schema.Boolean),
  at: Optional(Schema.String),
  primary: Optional(Schema.Boolean),
});
export type Bot = Schema.Schema.Type<typeof BotSchema>;

export const RoundRowSchema = Schema.Struct({
  title: Optional(Schema.String),
  key: Schema.String,
  repo: Schema.String,
  pr: Schema.Number,
  head: Schema.String,
  phase: Schema.String,
  fired_at: Optional(Schema.String),
  deadline: Optional(Schema.String),
  bots: MutableArray(BotSchema),
  host: Optional(Schema.String),
  note: Optional(Schema.String),
  next: Optional(Schema.String),
  fixing: Optional(Schema.Boolean),
});
export type RoundRow = Schema.Schema.Type<typeof RoundRowSchema>;

export const QueueRowSchema = Schema.Struct({
  title: Optional(Schema.String),
  key: Schema.String,
  repo: Schema.String,
  pr: Schema.Number,
  head: Schema.String,
  position: Optional(Schema.Number),
  ready_at: Optional(Schema.String),
  why: Optional(Schema.String),
  attempts: Optional(Schema.Number),
  host: Optional(Schema.String),
  co_only: Optional(Schema.Boolean),
  next: Optional(Schema.String),
});
export type QueueRow = Schema.Schema.Type<typeof QueueRowSchema>;

export const HeldRowSchema = Schema.Struct({
  title: Optional(Schema.String),
  key: Schema.String,
  repo: Schema.String,
  pr: Schema.Number,
  head: Optional(Schema.String),
  reason: Optional(Schema.String),
  by: Optional(Schema.String),
  at: Schema.String,
});
export type HeldRow = Schema.Schema.Type<typeof HeldRowSchema>;

export const SessionSchema = Schema.Struct({
  key: Schema.String,
  repo: Schema.String,
  pr: Schema.Number,
  head: Optional(Schema.String),
  host: Optional(Schema.String),
  model: Optional(Schema.String),
  attempt: Optional(Schema.Number),
  max_attempts: Optional(Schema.Number),
  findings: Optional(Schema.Number),
  log: Optional(Schema.String),
  since: Schema.String,
  heartbeat: Optional(Schema.String),
});
export type Session = Schema.Schema.Type<typeof SessionSchema>;

export const HostSchema = Schema.Struct({
  name: Schema.String,
  health: Schema.Literals(["healthy", "unhealthy", "unknown"]),
  failures: Optional(Schema.Number),
  last_error: Optional(Schema.String),
  last_failure: Optional(Schema.String),
  last_success: Optional(Schema.String),
});
export type Host = Schema.Schema.Type<typeof HostSchema>;

export const DoneRowSchema = Schema.Struct({
  title: Optional(Schema.String),
  key: Schema.String,
  repo: Schema.String,
  pr: Schema.Number,
  head: Schema.String,
  outcome: Schema.String,
  note: Optional(Schema.String),
  at: Optional(Schema.String),
});
export type DoneRow = Schema.Schema.Type<typeof DoneRowSchema>;

export const OverviewSchema = Schema.Struct({
  now: Schema.String,
  rev: Schema.Number,
  wrote_at: Optional(Schema.String),
  headline: HeadlineSchema,
  quota: QuotaSchema,
  slot: SlotSchema,
  leader: Optional(LeaderSchema),
  counts: CountsSchema,
  attention: MutableArray(AttentionSchema),
  in_flight: MutableArray(RoundRowSchema),
  queue: MutableArray(QueueRowSchema),
  held: MutableArray(HeldRowSchema),
  autofix: Schema.Struct({
    sessions: MutableArray(SessionSchema),
    hosts: MutableArray(HostSchema),
  }),
  finished: MutableArray(DoneRowSchema),
});
export type Overview = Schema.Schema.Type<typeof OverviewSchema>;

export const RepoSolverSchema = Schema.Struct({
  overridden: Schema.Boolean,
  agent: Optional(Schema.String),
  models: MutableArray(Schema.String),
  model_choices: MutableArray(Schema.String),
  model: Optional(Schema.String),
  effort: Optional(Schema.String),
  prompt: Optional(Schema.String),
  max_attempts: Schema.Number,
  max_review_rounds: Optional(Schema.Number),
  severities: MutableArray(Schema.String),
  ask_mode: Schema.String,
  forks: Schema.Boolean,
  skip_authors: MutableArray(Schema.String),
  one_pass: Schema.Boolean,
  merge_method: Optional(Schema.String),
  sources: StringRecord,
  by: Optional(Schema.String),
  lagging_hosts: Optional(MutableArray(Schema.String)),
  one_pass_lagging_hosts: Optional(MutableArray(Schema.String)),
  review_budget_lagging_hosts: Optional(MutableArray(Schema.String)),
  agent_on: Optional(
    MutableArray(
      Schema.Struct({
        host: Schema.String,
        has: Optional(Schema.Boolean),
        path: Optional(Schema.String),
        stale: Optional(Schema.Boolean),
      }),
    ),
  ),
});
export type RepoSolver = Schema.Schema.Type<typeof RepoSolverSchema>;

export const RepoRowSchema = Schema.Struct({
  repo: Schema.String,
  enrollment: Schema.Literals(["state", "env", "excluded", "scope", "off"]),
  reviewed: Schema.Boolean,
  env_conflict: Optional(Schema.Boolean),
  clear_enables: Optional(Schema.Boolean),
  enroll_reason: Optional(Schema.String),
  enroll_by: Optional(Schema.String),
  enroll_at: Optional(Schema.String),
  env_host: Optional(Schema.String),
  reviewers: MutableArray(Schema.String),
  required: MutableArray(Schema.String),
  primary_off: Optional(Schema.Boolean),
  solver: Optional(RepoSolverSchema),
  override: Schema.Boolean,
  override_by: Optional(Schema.String),
  override_at: Optional(Schema.String),
  autofix: Schema.Literals(["default", "on", "off"]),
  autofix_reason: Optional(Schema.String),
  autofix_by: Optional(Schema.String),
  autofix_at: Optional(Schema.String),
  active_rounds: Schema.Number,
  queued_rounds: Schema.Number,
  held_prs: Schema.Number,
  fixing: Schema.Number,
});
export type RepoRow = Schema.Schema.Type<typeof RepoRowSchema>;

export const BotCardSchema = Schema.Struct({
  login: Schema.String,
  name: Schema.String,
  primary: Schema.Boolean,
  metered: Schema.Boolean,
  enabled: Schema.Boolean,
  required: Schema.Boolean,
  configurable: Schema.Boolean,
  command: Optional(Schema.String),
  trigger: Optional(Schema.String),
  grace: Optional(Schema.String),
  last_seen: Optional(Schema.String),
  seen_on: Optional(Schema.String),
  repo_count: Schema.Number,
  status: Schema.Literals(["working", "quiet", "silent", "unverified", "off"]),
  last_asked: Optional(Schema.String),
  site: Optional(Schema.String),
  docs: Optional(Schema.String),
  pitch: Optional(Schema.String),
  cost: Optional(Schema.String),
  setup: Optional(MutableArray(Schema.String)),
  suited_to: Optional(Schema.String),
  prices_checked_at: Optional(Schema.String),
  suggested: Optional(Schema.Boolean),
  because: Optional(Schema.String),
});
export type BotCard = Schema.Schema.Type<typeof BotCardSchema>;

export const CheckSchema = Schema.Struct({
  key: Schema.String,
  label: Schema.String,
  status: Schema.Literals(["ok", "warn", "bad", "unknown"]),
  detail: Optional(Schema.String),
});
export type Check = Schema.Schema.Type<typeof CheckSchema>;

export const ToolSchema = Schema.Struct({
  fix: Optional(MutableArray(Schema.String)),
  name: Schema.String,
  purpose: Schema.String,
  required: Schema.Boolean,
  found: Schema.Boolean,
  path: Optional(Schema.String),
});
export type Tool = Schema.Schema.Type<typeof ToolSchema>;

export const HostInfoSchema = Schema.Struct({
  name: Schema.String,
  roles: Optional(MutableArray(Schema.String)),
  health: Optional(Schema.Literals(["healthy", "unhealthy", "unknown"])),
  last_seen: Optional(Schema.String),
  failures: Optional(Schema.Number),
  last_error: Optional(Schema.String),
  caps: Optional(Schema.Number),
});
export type HostInfo = Schema.Schema.Type<typeof HostInfoSchema>;

export const HostToolsSchema = Schema.Struct({
  host: Schema.String,
  agent: Optional(Schema.String),
  version: Optional(Schema.String),
  caps: Optional(Schema.Number),
  roles: Optional(MutableArray(Schema.String)),
  tools: MutableArray(
    Schema.Struct({
      name: Schema.String,
      path: Optional(Schema.String),
      version: Optional(Schema.String),
    }),
  ),
  at: Optional(Schema.String),
  stale: Optional(Schema.Boolean),
  behind: Optional(Schema.Boolean),
});
export type HostTools = Schema.Schema.Type<typeof HostToolsSchema>;

export const SetupViewSchema = Schema.Struct({
  checks: MutableArray(CheckSchema),
  tools: MutableArray(ToolSchema),
  hosts: MutableArray(HostInfoSchema),
  tools_host: Schema.String,
  fleet: Optional(MutableArray(HostToolsSchema)),
  ready: Schema.Number,
  attention: Schema.Number,
  optional: Schema.Number,
});
export type SetupView = Schema.Schema.Type<typeof SetupViewSchema>;

export const ReviewerCfgSchema = Schema.Struct({
  login: Schema.String,
  name: Schema.String,
  primary: Schema.Boolean,
  required: Schema.Boolean,
  metered: Schema.Boolean,
  command: Optional(Schema.String),
  trigger: Optional(Schema.String),
  grace: Optional(Schema.String),
});
export type ReviewerCfg = Schema.Schema.Type<typeof ReviewerCfgSchema>;

export const FleetConfigSchema = Schema.Struct({
  gate_repo: Schema.String,
  state_ref: Schema.String,
  dashboard_issue: Optional(Schema.Number),
  calibration_pr: Optional(Schema.Number),
  scope: Optional(MutableArray(Schema.String)),
  allow_repos: Optional(MutableArray(Schema.String)),
  exclude_repos: Optional(MutableArray(Schema.String)),
  skip_authors: Optional(MutableArray(Schema.String)),
  skip_marker: Optional(Schema.String),
  min_interval: Schema.String,
  inflight_timeout: Schema.String,
  watch_interval: Schema.String,
  reviewers: MutableArray(ReviewerCfgSchema),
  autofix_command: Optional(MutableArray(Schema.String)),
  autofix_max_attempts: Optional(Schema.Number),
  max_review_rounds: Optional(Schema.Number),
  autofix_concurrency: Optional(Schema.Number),
  autofix_forks: Optional(Schema.Boolean),
  workspace_root: Optional(Schema.String),
});
export type FleetConfig = Schema.Schema.Type<typeof FleetConfigSchema>;

export const KVSchema = Schema.Struct({
  key: Schema.String,
  value: Schema.String,
  detail: Optional(Schema.String),
});
export type KV = Schema.Schema.Type<typeof KVSchema>;

export const FleetSettingsSchema = Schema.Struct({
  recorded: Schema.Boolean,
  reviewers: MutableArray(
    Schema.Struct({
      login: Schema.String,
      budget: Schema.String,
      required: Schema.Boolean,
      trigger: Optional(Schema.String),
    }),
  ),
  min_interval: Schema.String,
  weekly_limit: Schema.Number,
  autofix_default: Schema.Boolean,
  sources: StringRecord,
  overriding: Optional(MutableArray(Schema.String)),
  by: Optional(Schema.String),
  updated_at: Optional(Schema.String),
  lagging_hosts: Optional(MutableArray(Schema.String)),
});
export type FleetSettings = Schema.Schema.Type<typeof FleetSettingsSchema>;

export const EnvSettingSchema = Schema.Struct({
  key: Schema.String,
  kind: Schema.String,
  group: Schema.String,
  label: Schema.String,
  help: Schema.String,
  per_host: Optional(Schema.Boolean),
  identity: Optional(Schema.Boolean),
  review_impact: Optional(Schema.Boolean),
  value: Schema.String,
  source: Schema.Literals(["fleet", "env", "default"]),
  host_value: Optional(Schema.String),
});
export type EnvSetting = Schema.Schema.Type<typeof EnvSettingSchema>;

export const SettingsViewSchema = Schema.Struct({
  config: FleetConfigSchema,
  quota: QuotaSchema,
  plumbing: MutableArray(KVSchema),
  fleet: Optional(FleetSettingsSchema),
  env: Optional(MutableArray(EnvSettingSchema)),
});
export type SettingsView = Schema.Schema.Type<typeof SettingsViewSchema>;

export const FindingSchema = Schema.Struct({
  id: Schema.String,
  bot: Schema.String,
  severity: Schema.String,
  scale: Optional(Schema.String),
  category: Optional(Schema.String),
  effort: Optional(Schema.String),
  path: Optional(Schema.String),
  line: Optional(Schema.Number),
  title: Schema.String,
  body: Optional(Schema.String),
  thread_id: Optional(Schema.String),
  url: Optional(Schema.String),
  source: Optional(Schema.String),
  commit: Optional(Schema.String),
  created_at: Optional(Schema.String),
});
export type Finding = Schema.Schema.Type<typeof FindingSchema>;

export const ObservationSchema = Schema.Struct({
  head: Schema.String,
  converged: Schema.Boolean,
  status: Optional(Schema.String),
  reason: Optional(Schema.String),
  reviewed_by: Optional(Schema.Record(Schema.String, Schema.Boolean)),
  findings: MutableArray(FindingSchema),
  dismissed: Optional(Schema.Number),
  checked_at: Schema.String,
});
export type Observation = Schema.Schema.Type<typeof ObservationSchema>;

export const RoundViewSchema = Schema.Struct({
  head: Schema.String,
  phase: Schema.String,
  attempts: Optional(Schema.Number),
  enqueued_at: Schema.String,
  fired_at: Optional(Schema.String),
  deadline: Optional(Schema.String),
  retry_at: Optional(Schema.String),
  note: Optional(Schema.String),
  host: Optional(Schema.String),
  co_only: Optional(Schema.Boolean),
  bots: MutableArray(BotSchema),
  fixing: Optional(SessionSchema),
  dismissed: Optional(
    MutableArray(
      Schema.Struct({
        id: Schema.String,
        reason: Schema.String,
      }),
    ),
  ),
  next: Optional(Schema.String),
});
export type RoundView = Schema.Schema.Type<typeof RoundViewSchema>;

export const HistoryEntrySchema = Schema.Struct({
  head: Schema.String,
  outcome: Schema.String,
  note: Optional(Schema.String),
  at: Optional(Schema.String),
  current: Optional(Schema.Boolean),
});
export type HistoryEntry = Schema.Schema.Type<typeof HistoryEntrySchema>;

export const CostSchema = Schema.Struct({
  low: Schema.Number,
  high: Schema.Number,
  exact: Optional(Schema.Boolean),
  unpriced: Optional(MutableArray(Schema.String)),
  summary: Schema.String,
  prices_checked_at: Schema.String,
  pricing_note: Schema.String,
  reviewers: MutableArray(
    Schema.Struct({
      bot: Schema.String,
      low: Schema.Number,
      high: Schema.Number,
      exact: Optional(Schema.Boolean),
      unknown: Optional(Schema.Boolean),
      basis: Schema.String,
    }),
  ),
  diff: Schema.Struct({
    additions: Schema.Number,
    deletions: Schema.Number,
    changed_files: Schema.Number,
  }),
});
export type Cost = Schema.Schema.Type<typeof CostSchema>;

export const PRViewSchema = Schema.Struct({
  repo: Schema.String,
  pr: Schema.Number,
  rev: Schema.Number,
  round: Optional(RoundViewSchema),
  hold: Optional(HeldRowSchema),
  title: Optional(Schema.String),
  observed: Optional(ObservationSchema),
  observe_error: Optional(Schema.String),
  cost: Optional(CostSchema),
  cost_error: Optional(Schema.String),
  history: MutableArray(HistoryEntrySchema),
});
export type PRView = Schema.Schema.Type<typeof PRViewSchema>;

export const EventSchema = Schema.Struct({
  at: Schema.String,
  kind: Schema.String,
  level: Schema.Literals(["ok", "warn", "bad", "info"]),
  repo: Optional(Schema.String),
  pr: Optional(Schema.Number),
  head: Optional(Schema.String),
  text: Schema.String,
  detail: Optional(Schema.String),
});
export type Event = Schema.Schema.Type<typeof EventSchema>;

export const SnapshotSchema = Schema.Struct({
  overview: OverviewSchema,
  repos: MutableArray(RepoRowSchema),
  bots: MutableArray(BotCardSchema),
  setup: SetupViewSchema,
  settings: SettingsViewSchema,
  events: MutableArray(EventSchema),
  stale: Optional(
    Schema.Struct({
      error: Schema.String,
      since: Schema.String,
    }),
  ),
});
export type Snapshot = Schema.Schema.Type<typeof SnapshotSchema>;

export const CandidateSchema = Schema.Struct({
  repo: Schema.String,
  private: Schema.Boolean,
  archived: Schema.Boolean,
  fork: Schema.Boolean,
  issues: Schema.Number,
  pushed_at: Optional(Schema.String),
  language: Optional(Schema.String),
  enrollment: Optional(
    Schema.Struct({
      source: Schema.String,
      enabled: Schema.Boolean,
      env_conflict: Optional(Schema.Boolean),
      clear_enables: Optional(Schema.Boolean),
      reason: Optional(Schema.String),
      by: Optional(Schema.String),
    }),
  ),
});
export type Candidate = Schema.Schema.Type<typeof CandidateSchema>;

export const DiscoverResponseSchema = Schema.Struct({
  repos: MutableArray(CandidateSchema),
  truncated: Optional(MutableArray(Schema.String)),
});

export const EnrollImpactSchema = Schema.Struct({
  rev: Schema.Number,
  repo: Schema.String,
  open: Schema.Number,
  eligible: Schema.Number,
  skipped: Optional(Schema.Record(Schema.String, Schema.Number)),
  low: Schema.Number,
  high: Schema.Number,
  summary: Schema.String,
  prices_checked_at: Schema.String,
});
export type EnrollImpact = Schema.Schema.Type<typeof EnrollImpactSchema>;

export const FleetImpactSchema = Schema.Struct({
  rev: Schema.Number,
  summary: Schema.String,
  changes: MutableArray(Schema.String),
  reopened: Schema.Number,
});
export type FleetImpact = Schema.Schema.Type<typeof FleetImpactSchema>;

export const FleetImpactResponseSchema = Schema.Struct({
  impact: FleetImpactSchema,
});

export const ActionResultSchema = Schema.Struct({
  snapshot: SnapshotSchema,
  warning: Optional(Schema.String),
});
export type ActionResult = Schema.Schema.Type<typeof ActionResultSchema>;

export const ActionResponseSchema = Schema.Union([ActionResultSchema, SnapshotSchema]);
