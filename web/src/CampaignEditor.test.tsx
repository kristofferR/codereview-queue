import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { RepoSolver } from "./api";
import { CampaignEditor } from "./CampaignEditor";

afterEach(cleanup);

describe("CampaignEditor", () => {
  it("preserves agent-default effort for an active campaign", () => {
    const solver: RepoSolver = {
      overridden: true,
      models: ["gpt-5.6-sol"],
      model_choices: ["gpt-5.6-sol"],
      model: "gpt-5.6-sol",
      effort: "",
      max_attempts: 1,
      severities: ["critical", "major"],
      ask_mode: "blocked",
      forks: false,
      skip_authors: [],
      one_pass: true,
      merge_method: "squash",
      sources: {},
    };

    render(<CampaignEditor repo="owner/repo" solver={solver} />);

    const effort = screen.getByRole("combobox", {
      name: "Campaign reasoning effort",
    }) as HTMLSelectElement;
    expect(effort.value).toBe("");
    expect(effort.selectedOptions[0]?.textContent).toBe("agent default");
    const update = screen.getByRole("button", { name: "Update campaign" }) as HTMLButtonElement;
    expect(update.disabled).toBe(true);
  });

  it("blocks campaign activation on one-pass-incompatible hosts", () => {
    const solver: RepoSolver = {
      overridden: false,
      models: ["gpt-5.6-sol"],
      model_choices: ["gpt-5.6-sol"],
      max_attempts: 3,
      severities: ["critical", "major"],
      ask_mode: "blocked",
      forks: false,
      skip_authors: [],
      one_pass: false,
      sources: {},
      one_pass_lagging_hosts: ["old-review"],
    };

    render(<CampaignEditor repo="owner/repo" solver={solver} />);

    screen.getByText(/old-review/);
    const start = screen.getByRole("button", { name: "Start campaign" }) as HTMLButtonElement;
    expect(start.disabled).toBe(true);
  });
});
