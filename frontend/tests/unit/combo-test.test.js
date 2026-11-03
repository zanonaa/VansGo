import { describe, expect, it } from "vitest";
import {
  buildComboTestReport,
  describeAttempt,
  normalizeComboTestResponse,
} from "@/shared/comboTest.js";

const PASSED = {
  ok: true,
  strategy: "fallback",
  combo: "zn-glm",
  models: ["glm/glm-5.3", "cx/gpt-6.1-sol", "gcli/grok-4.7"],
  attempts: [
    { model: "glm/glm-5.3", ok: false, status: 429, latencyMs: 340, error: "HTTP 429: 1113 insufficient balance" },
    { model: "cx/gpt-6.1-sol", ok: true, status: 200, latencyMs: 812, error: "" },
  ],
  stoppedBy: false,
  error: "",
  durationMs: 1250,
  testedAt: "2026-11-03T00:00:00Z",
};

describe("normalizeComboTestResponse", () => {
  it("passes through a well-formed payload", () => {
    const result = normalizeComboTestResponse(PASSED);
    expect(result.ok).toBe(true);
    expect(result.strategy).toBe("fallback");
    expect(result.models).toHaveLength(3);
    expect(result.attempts).toHaveLength(2);
    expect(result.attempts[0]).toMatchObject({ model: "glm/glm-5.3", status: 429, ok: false });
  });

  it("tolerates missing and malformed fields", () => {
    const result = normalizeComboTestResponse(null);
    expect(result.ok).toBe(false);
    expect(result.attempts).toEqual([]);
    expect(result.error).toBeTruthy();
    const partial = normalizeComboTestResponse({ ok: 1, attempts: [{ ok: true }] });
    expect(partial.attempts[0].model).toBe("");
    expect(partial.attempts[0].status).toBe(0);
    expect(partial.ok).toBe(false);
  });

  it("coerces bools strictly and clips long errors", () => {
    const result = normalizeComboTestResponse({
      ok: "yes",
      stoppedBy: 1,
      error: "x".repeat(1200),
      attempts: [{ model: "a/b", ok: "yes", status: "429", latencyMs: "33.7" }],
    });
    expect(result.ok).toBe(false);
    expect(result.stoppedBy).toBe(false);
    expect(result.attempts[0].ok).toBe(false);
    expect(result.attempts[0].status).toBe(429);
    expect(result.attempts[0].latencyMs).toBe(33);
    expect(result.error.length).toBeLessThanOrEqual(402);
  });
});

describe("describeAttempt / buildComboTestReport", () => {
  it("describes a failed attempt with status and error", () => {
    expect(describeAttempt(normalizeComboTestResponse(PASSED).attempts[0])).toContain("HTTP 429");
    expect(describeAttempt(normalizeComboTestResponse(PASSED).attempts[1])).toContain("OK");
  });

  it("builds a copyable report with chain, attempts and verdict", () => {
    const report = buildComboTestReport({ combo: { name: "zn-glm" }, result: normalizeComboTestResponse(PASSED) });
    expect(report).toContain("zn-glm");
    expect(report).toContain("fallback");
    expect(report).toContain("glm/glm-5.3 → cx/gpt-6.1-sol → gcli/grok-4.7");
    expect(report).toContain("HTTP 429");
  });

  it("notes upstream content-block stops in the report", () => {
    const stopped = normalizeComboTestResponse({ ...PASSED, ok: false, stoppedBy: true, error: "HTTP 400: prompt too long" });
    const report = buildComboTestReport({ combo: { name: "zn-glm" }, result: stopped });
    expect(report).toContain("Stopped");
    expect(report).toContain("HTTP 400");
  });
});
