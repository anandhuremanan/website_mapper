// @vitest-environment jsdom
import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useVerification } from "./verification";

/** A stand-in for Cloudflare's widget, driven by the test. */
function fakeTurnstile() {
  const state = {
    options: undefined as undefined | { callback: (t: string) => void; "error-callback"?: () => void },
    resets: 0,
    removed: 0,
  };
  window.turnstile = {
    render: (_el, options) => {
      state.options = options;
      return "widget-1";
    },
    reset: () => {
      state.resets++;
    },
    remove: () => {
      state.removed++;
    },
  };
  return state;
}

/** Renders the hook with its container attached, as a component would. */
async function mount() {
  const el = document.createElement("div");
  const hook = renderHook(() => {
    const v = useVerification();
    v.attach(el);
    return v;
  });
  await Promise.resolve(); // let the script "load"
  await Promise.resolve();
  return hook;
}

beforeEach(() => vi.stubEnv("NEXT_PUBLIC_TURNSTILE_SITE_KEY", "site-key"));
afterEach(() => {
  cleanup();
  vi.unstubAllEnvs();
  vi.useRealTimers();
  delete window.turnstile;
});

describe("useVerification", () => {
  it("is switched off without a site key", async () => {
    vi.stubEnv("NEXT_PUBLIC_TURNSTILE_SITE_KEY", "");
    const widget = fakeTurnstile();
    const { result } = await mount();
    expect(await result.current.getToken()).toBeUndefined();
    expect(widget.options).toBeUndefined();
  });

  it("hands each token out once and asks for the next", async () => {
    const widget = fakeTurnstile();
    const { result } = await mount();
    widget.options!.callback("token-1");

    expect(await result.current.getToken()).toBe("token-1");
    expect(widget.resets).toBe(1);

    // No token is ready now: the next request waits for the widget.
    const second = result.current.getToken();
    widget.options!.callback("token-2");
    expect(await second).toBe("token-2");
  });

  it("waits for a check that has not finished yet", async () => {
    const widget = fakeTurnstile();
    const { result } = await mount();
    const pending = result.current.getToken();
    widget.options!.callback("late-token");
    expect(await pending).toBe("late-token");
  });

  it("fails with a clear message when the check errors or never answers", async () => {
    const widget = fakeTurnstile();
    const { result } = await mount();
    const pending = result.current.getToken();
    widget.options!["error-callback"]!();
    await expect(pending).rejects.toThrow(/could not verify your browser/);

    vi.useFakeTimers();
    const stuck = result.current.getToken();
    const outcome = expect(stuck).rejects.toThrow(/could not verify your browser/);
    await vi.advanceTimersByTimeAsync(30_000);
    await outcome;
  });

  it("removes the widget with the component", async () => {
    const widget = fakeTurnstile();
    const { unmount } = await mount();
    unmount();
    expect(widget.removed).toBe(1);
  });
});
