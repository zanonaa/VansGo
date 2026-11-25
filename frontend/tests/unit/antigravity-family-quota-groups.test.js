// Antigravity family quota groups (agy-style): the usage endpoint returns
// family windows keyed `<family>_<window>` (session = 5h, weekly) alongside
// per-model rows; the dashboard must render them as grouped bars, not as flat
// table rows mixed with the ~37 models.
import { describe, it, expect } from "vitest";
import {
  parseQuotaData,
  splitAntigravityFamilyQuotas,
} from "@/app/(dashboard)/dashboard/usage/components/ProviderLimits/utils.js";

const FAMILY_WINDOWS = {
  gemini_session: {
    used: 22,
    total: 1000,
    resetAt: "2026-11-25T21:41:41.000Z",
    remainingPercentage: 97.787046,
    unlimited: false,
    displayName: "Gemini (5h)",
  },
  gemini_weekly: {
    used: 14,
    total: 1000,
    resetAt: "2026-12-02T11:41:41.000Z",
    remainingPercentage: 98.56726,
    unlimited: false,
    displayName: "Gemini (Weekly)",
  },
  claude_gpt_session: {
    used: 0,
    total: 0,
    resetAt: null,
    remainingPercentage: 100,
    unlimited: true,
    displayName: "Claude & GPT (5h)",
  },
  claude_gpt_weekly: {
    used: 0,
    total: 1000,
    resetAt: "2026-12-02T14:24:23.000Z",
    remainingPercentage: 99.99257,
    unlimited: false,
    displayName: "Claude & GPT (Weekly)",
  },
};

const PER_MODEL = {
  "gemini-2.5-flash": {
    used: 22,
    total: 1000,
    resetAt: "2026-11-25T21:41:41.000Z",
    remainingPercentage: 97.787046,
    unlimited: false,
  },
  "claude-opus-5-5-high": {
    used: 3,
    total: 1000,
    resetAt: "2026-12-02T14:24:23.000Z",
    remainingPercentage: 99.7,
    unlimited: false,
  },
};

describe("antigravity family quota groups", () => {
  it("parseQuotaData keeps family windows and forwards the unlimited flag", () => {
    const parsed = parseQuotaData("antigravity", {
      plan: "Free",
      quotas: { ...FAMILY_WINDOWS, ...PER_MODEL },
    });

    const geminiSession = parsed.find((q) => q.modelKey === "gemini_session");
    expect(geminiSession).toBeDefined();
    expect(geminiSession.name).toBe("Gemini (5h)");
    expect(geminiSession.unlimited).toBe(false);

    const claudeGptSession = parsed.find(
      (q) => q.modelKey === "claude_gpt_session",
    );
    expect(claudeGptSession.unlimited).toBe(true);

    // Per-model rows keep the catalog ordering intact
    expect(parsed.find((q) => q.modelKey === "gemini-2.5-flash")).toBeDefined();
  });

  it("splitAntigravityFamilyQuotas groups windows per family and keeps models apart", () => {
    const parsed = parseQuotaData("antigravity", {
      plan: "Free",
      quotas: { ...FAMILY_WINDOWS, ...PER_MODEL },
    });
    const { groups, models } = splitAntigravityFamilyQuotas(parsed);

    expect(groups.map((g) => g.id)).toEqual(["gemini", "claude_gpt"]);
    expect(groups[0].label).toBe("Gemini");
    expect(groups[1].label).toBe("Claude & GPT");

    expect(groups[0].session.modelKey).toBe("gemini_session");
    expect(groups[0].weekly.modelKey).toBe("gemini_weekly");
    expect(groups[1].session.unlimited).toBe(true);
    expect(groups[1].weekly.modelKey).toBe("claude_gpt_weekly");

    // Per-model rows are untouched by the split
    expect(models.every((q) => !/_(session|weekly)$/.test(q.modelKey))).toBe(
      true,
    );
    expect(
      models.find((q) => q.modelKey === "gemini-2.5-flash"),
    ).toBeDefined();
  });

  it("handles Ultra's gemini_4_argon family and unknown future families", () => {
    const parsed = parseQuotaData("antigravity", {
      plan: "Ultra",
      quotas: {
        gemini_4_argon_session: {
          used: 5,
          total: 1000,
          remainingPercentage: 99.5,
        },
        gemini_4_argon_weekly: {
          used: 9,
          total: 1000,
          remainingPercentage: 99.1,
        },
      },
    });
    const { groups, models } = splitAntigravityFamilyQuotas(parsed);

    expect(groups).toHaveLength(1);
    expect(groups[0].id).toBe("gemini_4_argon");
    expect(groups[0].label).toBe("Gemini 4 Argon");
    expect(groups[0].session.used).toBe(5);
    expect(groups[0].weekly.used).toBe(9);
    expect(models).toHaveLength(0);
  });

  it("returns empty groups for empty or family-less quota lists", () => {
    expect(splitAntigravityFamilyQuotas([])).toEqual({
      groups: [],
      models: [],
    });

    const { groups, models } =
      splitAntigravityFamilyQuotas(PER_MODEL_ONLY());
    expect(groups).toEqual([]);
    expect(models).toHaveLength(2);
  });

  it("tolerates null/undefined input", () => {
    expect(splitAntigravityFamilyQuotas(null)).toEqual({
      groups: [],
      models: [],
    });
    expect(splitAntigravityFamilyQuotas(undefined)).toEqual({
      groups: [],
      models: [],
    });
  });
});

function PER_MODEL_ONLY() {
  return Object.entries(PER_MODEL).map(([modelKey, quota]) => ({
    name: modelKey,
    modelKey,
    used: quota.used,
    total: quota.total,
    resetAt: quota.resetAt || null,
    remainingPercentage: quota.remainingPercentage,
    unlimited: quota.unlimited === true,
  }));
}
