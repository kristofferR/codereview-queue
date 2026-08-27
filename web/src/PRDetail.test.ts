import { describe, expect, it } from "vitest";
import type { PRView, Snapshot } from "./api";
import { isNewLiveSnapshot, mergeLivePRState, mergePRDetails } from "./PRDetail";

const state = (head: string, phase: string): PRView => ({
  repo: "owner/repo",
  pr: 7,
  rev: 3,
  round: { head, phase, enqueued_at: "2026-07-29T11:00:00Z", bots: [] },
  history: [],
});

describe("live PR state", () => {
  it("updates persisted state without refetching same-head findings", () => {
    const current: PRView = {
      ...state("abcdef123", "reviewing"),
      observed: {
        head: "abcdef123",
        converged: false,
        findings: [],
        checked_at: "2026-07-29T12:00:00Z",
      },
    };

    const merged = mergeLivePRState(current, state("abcdef123", "completed"));
    expect(merged.round?.phase).toBe("completed");
    expect(merged.observed).toBe(current.observed);
  });

  it("drops observations when the persisted head moves", () => {
    const current: PRView = {
      ...state("abcdef123", "completed"),
      observed: {
        head: "abcdef123",
        converged: true,
        findings: [],
        checked_at: "2026-07-29T12:00:00Z",
      },
    };

    expect(mergeLivePRState(current, state("fedcba987", "queued")).observed).toBeUndefined();
  });

  it("does not let an older request overwrite a newer live revision", () => {
    const current = { ...state("abcdef123", "completed"), rev: 5 };
    const stale = { ...state("abcdef123", "reviewing"), rev: 4 };
    expect(mergeLivePRState(current, stale)).toBe(current);
  });

  it("accepts a distinct live frame even when its state revision is unchanged", () => {
    const previous = { overview: { rev: 5 } } as Snapshot;
    const timeDerivedUpdate = { overview: { rev: 5 } } as Snapshot;

    expect(isNewLiveSnapshot(previous, previous)).toBe(false);
    expect(isNewLiveSnapshot(previous, timeDerivedUpdate)).toBe(true);
  });

  it("attaches delayed GitHub details without rolling back newer state", () => {
    const current = { ...state("abcdef123", "completed"), rev: 5 };
    const observed: PRView = {
      ...state("abcdef123", "reviewing"),
      rev: 4,
      observed: {
        head: "abcdef1234",
        converged: true,
        findings: [],
        checked_at: "2026-07-29T12:00:00Z",
      },
    };

    const merged = mergePRDetails(current, observed);
    expect(merged.rev).toBe(5);
    expect(merged.round?.phase).toBe("completed");
    expect(merged.observed).toBe(observed.observed);
  });

  it("preserves newer time-derived state when delayed details have the same revision", () => {
    const current = { ...state("abcdef123", "completed"), rev: 5 };
    const observed: PRView = {
      ...state("abcdef123", "reviewing"),
      rev: 5,
      round: {
        head: "abcdef123",
        phase: "reviewing",
        enqueued_at: "2026-07-29T11:00:00Z",
        bots: [],
        fixing: {
          key: "owner/repo#7",
          repo: "owner/repo",
          pr: 7,
          since: "2026-07-29T11:30:00Z",
        },
      },
      observed: {
        head: "abcdef1234",
        converged: true,
        findings: [],
        checked_at: "2026-07-29T12:00:00Z",
      },
    };

    const merged = mergePRDetails(current, observed);
    expect(merged.round?.phase).toBe("completed");
    expect(merged.round?.fixing).toBeUndefined();
    expect(merged.observed).toBe(observed.observed);
  });

  it("rejects delayed details for a superseded head", () => {
    const current = { ...state("fedcba987", "queued"), rev: 5 };
    const observed: PRView = {
      ...state("abcdef123", "completed"),
      rev: 4,
      observed: {
        head: "abcdef123",
        converged: true,
        findings: [],
        checked_at: "2026-07-29T12:00:00Z",
      },
    };

    expect(mergePRDetails(current, observed)).toBe(current);
  });
});
