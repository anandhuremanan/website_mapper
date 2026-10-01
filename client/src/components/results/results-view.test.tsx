// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Counts, Scan, ScanSummary } from "@/lib/types";

const api = vi.hoisted(() => ({
  createScan: vi.fn(),
  getHosts: vi.fn(),
  getUrls: vi.fn(),
  getTree: vi.fn(),
  exportUrl: (id: string) => `/api/scans/${id}/export`,
}));
vi.mock("@/lib/api", () => api);
const push = vi.hoisted(() => vi.fn());
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

import { liveSummary, ResultsView } from "./results-view";

afterEach(cleanup);

/** A tab of the result sections, by the start of its label. */
function tab(name: RegExp) {
  return within(screen.getByRole("navigation")).queryByRole("button", { name });
}

const counts: Counts = {
  hosts: 2,
  hostsResolved: 2,
  hostsReachable: 1,
  hostsCrawled: 0,
  hostsUnresolved: 0,
  hostsResolvePending: 0,
  hostsProbed: 1,
  hostsUnreachable: 0,
  hostsProbePending: 0,
  hostsCrawlPending: 0,
  urls: 10,
  pages: 6,
  apis: 1,
  assets: 2,
  javascript: 1,
  urlsFetched: 0,
  urlsFailed: 0,
  cache: { hosts: 0, dns: 0, probes: 0, pages: 0 },
  limits: {
    hostsOmitted: 0,
    resolveSkipped: 0,
    probeSkipped: 0,
    crawlSkipped: 0,
    crawlLimited: 0,
    urlsOmitted: 0,
  },
};

function scan(status: Scan["status"]): Scan {
  return {
    id: "s1",
    target: "example.com",
    domain: "example.com",
    startUrl: "https://example.com/",
    mode: "light",
    status,
    createdAt: "2026-10-01T10:00:00Z",
    startedAt: "2026-10-01T10:00:01Z",
    phase: status === "running" ? "probing_hosts" : "done",
    limits: [],
    resources: { requests: 0 },
    steps: [],
    counts,
    errors: [],
  };
}

const finished: ScanSummary = {
  ...liveSummary(scan("completed")),
  technologies: [{ name: "nginx", evidence: ["Server: nginx"] }],
};

beforeEach(() => {
  push.mockReset();
  api.createScan.mockReset().mockResolvedValue({ id: "s2" });
  api.getHosts.mockReset().mockResolvedValue({
    hosts: [
      {
        hostname: "example.com",
        state: "reachable",
        sources: ["target"],
        counts: { urls: 10, pages: 6, apis: 1, assets: 2 },
      },
    ],
  });
});

describe("ResultsView for a finished scan", () => {
  it("counts the tabs from the summary, including unknown URLs as pages", async () => {
    render(<ResultsView scan={scan("completed")} result={finished} />);
    await screen.findByText("example.com", { selector: "span" });
    // 10 URLs - 1 API - 2 assets = 7 in the Pages tab.
    expect(tab(/^Pages/)?.textContent).toBe("Pages7");
    expect(tab(/^Technologies/)?.textContent).toBe("Technologies1");
    expect(screen.getByRole("link", { name: "Download URLs (CSV)" }).getAttribute("href")).toBe(
      "/api/scans/s1/export",
    );
  });

  it("starts a fresh scan of the same target and mode on request", async () => {
    render(<ResultsView scan={scan("completed")} result={finished} />);
    fireEvent.click(screen.getByRole("button", { name: "Scan again" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/scans/s2"));
    expect(api.createScan).toHaveBeenCalledWith("example.com", "light", true);
  });
});

describe("ResultsView for a running scan", () => {
  it("shows what was found so far, without what only a finished scan has", async () => {
    const running = scan("running");
    render(<ResultsView scan={running} result={liveSummary(running)} live />);
    await screen.findByText("example.com", { selector: "span" });

    expect(screen.getByRole("heading", { name: "Found so far" })).toBeTruthy();
    expect(tab(/^Technologies/)).toBeNull();
    // Not in the overview table either.
    expect(screen.queryByRole("button", { name: "Technologies" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Download URLs (CSV)" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Scan again" })).toBeNull();
  });

  it("reloads the lists on request", async () => {
    const running = scan("running");
    render(<ResultsView scan={running} result={liveSummary(running)} live />);
    await screen.findByText("example.com", { selector: "span" });
    expect(api.getHosts).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Refresh lists" }));
    await waitFor(() => expect(api.getHosts).toHaveBeenCalledTimes(2));
  });

  it("keeps the open tab and reloads its list when the scan finishes", async () => {
    api.getUrls.mockReset().mockResolvedValue({ urls: [] });
    const running = scan("running");
    const { rerender } = render(<ResultsView scan={running} result={liveSummary(running)} live />);
    fireEvent.click(tab(/^APIs/)!);
    await waitFor(() => expect(api.getUrls).toHaveBeenCalledTimes(1));

    rerender(<ResultsView scan={scan("completed")} result={finished} />);
    await waitFor(() => expect(api.getUrls).toHaveBeenCalledTimes(2));
    expect(tab(/^APIs/)?.getAttribute("aria-current")).toBe("page");
  });
});
