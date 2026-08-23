import { describe, expect, it } from "vitest";
import type { FleetSettings, RepoSolver, Snapshot } from "./api";
import { campaignChange } from "./CampaignEditor";
import { isFirstRun } from "./FirstRun";
import { fleetChange } from "./FleetEditor";
import { solverChange } from "./SolverEditor";

const fleet: FleetSettings = {
  recorded: false,
  reviewers: [],
  min_interval: "90s",
  weekly_limit: 60,
  autofix_default: true,
  sources: {},
};

describe("settings deltas", () => {
  it("sends only edited fleet fields", () => {
    expect(
      fleetChange(fleet, ["codex"], ["coderabbitai"], {
        runs: ["codex"],
        required: ["coderabbitai"],
        minInterval: "2m",
        weekly: "60",
        autofix: true,
      }),
    ).toEqual({ min_interval: "2m" });
  });

  it("does not save an empty weekly limit as zero", () => {
    expect(
      fleetChange(fleet, [], [], {
        runs: [],
        required: [],
        minInterval: "90s",
        weekly: "",
        autofix: true,
      }),
    ).toEqual({});
  });

  it("keeps solver model fallbacks and omits inherited fields", () => {
    const solver: RepoSolver = {
      overridden: false,
      models: ["gpt-5.6-sol", "gpt-5.6-terra", "codex-auto-review"],
      model_choices: [],
      model: "gpt-5.6-sol",
      max_attempts: 3,
      severities: ["critical", "major"],
      ask_mode: "blocked",
      forks: false,
      skip_authors: ["dependabot[bot]"],
      one_pass: false,
      sources: {},
    };

    expect(
      solverChange(solver, {
        model: "gpt-5.6-terra",
        effort: "",
        prompt: "",
        attempts: "4",
        severities: ["critical"],
        askMode: "uncertain",
        forks: false,
        authors: "dependabot[bot]",
      }),
    ).toEqual({
      models: ["gpt-5.6-terra", "codex-auto-review"],
      max_attempts: 4,
      severities: ["critical"],
      ask_mode: "uncertain",
    });
  });

  it("selects the agent default with an explicit empty ranking and ignores severity ordering", () => {
    const solver: RepoSolver = {
      overridden: true,
      models: ["gpt-5.6-sol", "gpt-5.6-terra"],
      model_choices: [],
      model: "gpt-5.6-sol",
      max_attempts: 3,
      severities: ["critical", "major"],
      ask_mode: "blocked",
      forks: false,
      skip_authors: [],
      one_pass: false,
      sources: {},
    };

    expect(
      solverChange(solver, {
        model: "",
        effort: "",
        prompt: "",
        attempts: "3",
        severities: ["major", "critical"],
        askMode: "blocked",
        forks: false,
        authors: "",
      }),
    ).toEqual({ models: [] });
  });

  it("starts a one-pass auto-merge campaign as one atomic solver change", () => {
    const solver: RepoSolver = {
      overridden: false,
      models: ["gpt-5.6-sol", "gpt-5.6-terra"],
      model_choices: ["gpt-5.6-sol", "gpt-5.6-terra"],
      model: "gpt-5.6-terra",
      effort: "high",
      max_attempts: 5,
      severities: ["critical", "major"],
      ask_mode: "blocked",
      forks: false,
      skip_authors: [],
      one_pass: false,
      sources: {},
    };

    expect(
      campaignChange(solver, {
        model: "gpt-5.6-sol",
        effort: "medium",
        mergeMethod: "squash",
      }),
    ).toEqual({
      models: ["gpt-5.6-sol", "gpt-5.6-terra"],
      effort: "medium",
      max_attempts: 1,
      one_pass: true,
      merge_method: "squash",
    });

    expect(
      campaignChange(solver, {
        model: "",
        effort: "high",
        mergeMethod: "",
      }),
    ).toEqual({
      models: [],
      max_attempts: 1,
      one_pass: true,
      merge_method: "",
    });
  });
});

describe("first-run enrollment", () => {
  it("does not call an empty scope-wide fleet unenrolled", () => {
    const snapshot = {
      repos: [],
      settings: { config: { scope: ["owner"], allow_repos: [] } },
      overview: {
        counts: { in_flight: 0, queued: 0, held: 0, fixing: 0 },
        finished: [],
      },
    } as unknown as Snapshot;

    expect(isFirstRun(snapshot)).toBe(false);
  });
});
