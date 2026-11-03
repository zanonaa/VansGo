/**
 * Tests for the Providers page display filter ("Active" / "All").
 *
 * Requirement: page loads with the "Active" filter as default, shows only
 * providers with live connections, and uses the same definition of "active"
 * the page already had (getProviderStats / isActive / testStatus), not a new
 * one.
 *
 * Because the page is a JSX-heavy client component we cannot import it in
 * Vitest without a full browser setup. We verify the source text instead.
 */

import { describe, it, expect } from "vitest";
import fs from "fs";
import path from "path";

const src = fs.readFileSync(
  path.resolve(__dirname, "../../src/app/(dashboard)/dashboard/providers/page.js"),
  "utf-8"
);

describe("ProvidersPage — Active/All display filter", () => {
  it("defaults the display filter to active", () => {
    expect(src).toMatch(/useState\(\s*"active"\s*\)/);
    expect(src).toContain('value: "active", label: "Active"');
    expect(src).toContain('value: "all", label: "All"');
  });

  it("renders the two options through the shared SegmentedControl", () => {
    expect(src).toContain("SegmentedControl");
    expect(src).toContain("PROVIDER_FILTER_OPTIONS");
  });

  it("reuses the page's own active definition (getProviderStats) instead of inventing one", () => {
    expect(src).toMatch(/getProviderStats\(providerId,\s*authType\)/);
    expect(src).toMatch(/isActiveProvider/);
    // connected comes from the status === "active" || status === "success" check
    expect(src).toContain('status === "active" || status === "success"');
    // allDisabled comes from the isActive === false check
    expect(src).toContain("c.isActive === false");
  });

  it("applies the filter to every provider list on the page", () => {
    const filters = src.match(/displayFilter === "all" \|\| isActiveProvider\(/g) || [];
    // oauth, free, freeTier, apikey, webCookie, openai-compatible, anthropic-compatible
    expect(filters.length).toBe(7);
  });

  it("does not persist the choice (fresh default on every load)", () => {
    expect(src).not.toContain("localStorage");
  });
});
