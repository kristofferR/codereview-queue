import { Effect } from "effect";
import type { DashboardError } from "./client";
import { requestJson } from "./client";
import type { ActionResult, FleetImpact } from "./data/contracts";
import { ActionResponseSchema, FleetImpactResponseSchema } from "./data/contracts";

export type { ActionResult } from "./data/contracts";

export type ActionName =
  | "hold"
  | "unhold"
  | "prioritize"
  | "cancel"
  | "autofix"
  | "enroll"
  | "fleet"
  | "solver"
  | "env"
  | "reviewers"
  | "resolve"
  | "decline"
  | "dismiss";

export type ActionBody = {
  /** Empty for fleet settings, which are the answer for every repository. */
  repo?: string;
  pr?: number;
  reason?: string;
  /** Omitted entirely means "back to the fleet default" for autofix. */
  enabled?: boolean;
  expected_rev?: number;
  /** Whole intended sets, not a delta — a delta could not express "none". */
  cobots?: string[];
  required?: string[];
  /** Whether the metered primary runs on this repo; omitted leaves it alone. */
  primary?: boolean;
  clear?: boolean;
  thread_ids?: string[];
  finding_ids?: string[];
  /** Decline without resolving, when the disagreement is worth leaving visible. */
  keep_open?: boolean;
  /** A fleet-defaults change; every field is optional and absent means "leave it". */
  fleet?: {
    cobots?: string[];
    required?: string[];
    min_interval?: string;
    weekly_limit?: number;
    autofix_default?: boolean;
    expected_rev?: number;
    clear?: boolean;
  };
  /** Ask what a change would do without making it. */
  preview?: boolean;
  /** One setting, addressed by its environment-variable name. */
  key?: string;
  value?: string;
  /** A fix-session change; an empty repo means the fleet default. */
  solver?: {
    models?: string[];
    model?: string;
    effort?: string;
    prompt?: string;
    max_attempts?: number;
    max_review_rounds?: number;
    severities?: string[];
    ask_mode?: string;
    forks?: boolean;
    skip_authors?: string[];
    one_pass?: boolean;
    merge_method?: "" | "merge" | "squash" | "rebase";
    unset_models?: boolean;
    unset_effort?: boolean;
    unset_prompt?: boolean;
    unset_max_review_rounds?: boolean;
    unset_severities?: boolean;
    unset_ask_mode?: boolean;
    unset_forks?: boolean;
    unset_skip_authors?: boolean;
    unset_one_pass?: boolean;
    unset_merge?: boolean;
    clear?: boolean;
  };
};

/**
 * Runs one action and returns the refreshed snapshot. The server re-reads state
 * before answering, so a successful call already reflects the change — the UI
 * never has to poll to find out whether the click worked.
 *
 * The X-CRQ-Dashboard header is what stops another site posting here: the
 * server is unauthenticated on the tailnet, and a browser cannot set a custom
 * header cross-origin without a preflight the server never approves.
 */
export function act(
  action: "fleet" | "env" | "reviewers",
  body: ActionBody & { preview: true },
): Effect.Effect<FleetImpact, DashboardError>;
export function act(
  action: "fleet" | "env" | "reviewers",
  body: ActionBody & { preview?: false | undefined },
): Effect.Effect<ActionResult, DashboardError>;
export function act(
  action: "fleet" | "env" | "reviewers",
  body: ActionBody,
): Effect.Effect<ActionResult | FleetImpact, DashboardError>;
export function act(
  action: Exclude<ActionName, "fleet" | "env" | "reviewers">,
  body: ActionBody,
): Effect.Effect<ActionResult, DashboardError>;
export function act(
  action: ActionName,
  body: ActionBody,
): Effect.Effect<ActionResult | FleetImpact, DashboardError> {
  if ((action === "fleet" || action === "env" || action === "reviewers") && body.preview === true) {
    return requestJson(FleetImpactResponseSchema, `/api/action/${action}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CRQ-Dashboard": "1" },
      body: JSON.stringify(body),
    }).pipe(Effect.map(({ impact }) => impact));
  }
  return requestJson(ActionResponseSchema, `/api/action/${action}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CRQ-Dashboard": "1" },
    body: JSON.stringify(body),
  }).pipe(
    // Older servers returned a bare snapshot. Keep that compatibility at this
    // one boundary instead of teaching every action caller about two envelopes.
    Effect.map((parsed) => ("snapshot" in parsed ? parsed : { snapshot: parsed })),
  );
}
