// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const posthog = vi.hoisted(() => ({ init: vi.fn(), capture: vi.fn() }));
vi.mock("posthog-js", () => ({ default: posthog }));

beforeEach(() => {
  vi.resetModules(); // each test starts with analytics off
  posthog.init.mockReset();
  posthog.capture.mockReset();
});
afterEach(() => vi.unstubAllEnvs());

describe("analytics", () => {
  it("does nothing without a project key", async () => {
    vi.stubEnv("NEXT_PUBLIC_POSTHOG_KEY", "");
    const { initAnalytics, track } = await import("./analytics");
    initAnalytics();
    track("scan_started", { mode: "light", outcome: "new" });
    expect(posthog.init).not.toHaveBeenCalled();
    expect(posthog.capture).not.toHaveBeenCalled();
  });

  it("starts without cookies, click capture or recording", async () => {
    vi.stubEnv("NEXT_PUBLIC_POSTHOG_KEY", "phc_test");
    vi.stubEnv("NEXT_PUBLIC_POSTHOG_HOST", "https://eu.i.posthog.com");
    const { initAnalytics } = await import("./analytics");
    initAnalytics();
    initAnalytics(); // a second call changes nothing
    expect(posthog.init).toHaveBeenCalledTimes(1);
    expect(posthog.init).toHaveBeenCalledWith(
      "phc_test",
      expect.objectContaining({
        api_host: "https://eu.i.posthog.com",
        persistence: "memory",
        autocapture: false,
        disable_session_recording: true,
      }),
    );
  });

  it("sends only the event's own properties", async () => {
    vi.stubEnv("NEXT_PUBLIC_POSTHOG_KEY", "phc_test");
    const { initAnalytics, track } = await import("./analytics");
    initAnalytics();
    track("scan_finished", {
      mode: "light",
      status: "completed",
      stop_reason: "",
      duration_s: 19,
      hosts: 87,
      urls: 7484,
    });
    expect(posthog.capture).toHaveBeenCalledWith("scan_finished", {
      mode: "light",
      status: "completed",
      stop_reason: "",
      duration_s: 19,
      hosts: 87,
      urls: 7484,
    });
  });

  it("never lets a failing analytics call break the page", async () => {
    vi.stubEnv("NEXT_PUBLIC_POSTHOG_KEY", "phc_test");
    posthog.capture.mockImplementation(() => {
      throw new Error("blocked");
    });
    const { initAnalytics, track } = await import("./analytics");
    initAnalytics();
    expect(() => track("scan_again", { mode: "full" })).not.toThrow();
  });
});
