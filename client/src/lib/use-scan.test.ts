import { describe, expect, it } from "vitest";
import { pollDelay } from "./use-scan";

describe("pollDelay", () => {
  it("polls quickly at first and slows down for long scans", () => {
    expect(pollDelay(0)).toBe(1500);
    expect(pollDelay(19)).toBe(1500);
    expect(pollDelay(20)).toBe(3000);
    expect(pollDelay(60)).toBe(5000);
    expect(pollDelay(10_000)).toBe(5000);
  });

  it("never speeds up again", () => {
    for (let n = 1; n < 200; n++) {
      expect(pollDelay(n)).toBeGreaterThanOrEqual(pollDelay(n - 1));
    }
  });
});
