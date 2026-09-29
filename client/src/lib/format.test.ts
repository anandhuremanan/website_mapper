import { describe, expect, it } from "vitest";
import { allUrls, hostStatus } from "./format";
import type { Host } from "./types";

function host(partial: Partial<Host>): Host {
  return {
    hostname: "api.example.com",
    state: "discovered",
    sources: ["certificate-transparency"],
    counts: { urls: 0, pages: 0, apis: 0, assets: 0 },
    urls: [],
    ...partial,
  };
}

describe("hostStatus", () => {
  it("describes a reachable host with status, scheme and redirect", () => {
    const st = hostStatus(
      host({
        state: "reachable",
        dns: { resolved: true },
        http: { reachable: true, scheme: "https", status: 301, redirect: "https://example.com/api" },
      }),
    );
    expect(st.icon).toBe("✓");
    expect(st.label).toBe("301 · HTTPS");
    expect(st.detail).toContain("example.com/api");
  });

  it("distinguishes resolving hosts without HTTP from private ones", () => {
    expect(
      hostStatus(host({ state: "resolved", dns: { resolved: true }, http: { reachable: false, error: "connection refused" } }))
        .label,
    ).toBe("no HTTP response");
    expect(hostStatus(host({ state: "resolved", dns: { resolved: true, nonPublic: true } })).label).toBe(
      "private address",
    );
  });

  it("keeps hosts that no longer resolve visible", () => {
    const st = hostStatus(host({ dns: { resolved: false, error: "no such host" } }));
    expect(st.label).toBe("does not resolve");
    expect(st.detail).toContain("no such host");
  });

  it("reports unchecked hosts honestly", () => {
    expect(hostStatus(host({})).label).toBe("not checked");
    expect(hostStatus(host({ dns: { resolved: false, skipped: "host limit reached" } })).detail).toContain(
      "host limit reached",
    );
  });
});

describe("allUrls", () => {
  it("flattens URLs across hosts", () => {
    const u = (url: string) => ({
      url,
      hostname: new URL(url).hostname,
      path: new URL(url).pathname,
      type: "page" as const,
      typeEvidence: "",
      sources: [],
      fetched: false,
    });
    const hosts = [
      host({ hostname: "a.example.com", urls: [u("https://a.example.com/1")] }),
      host({ hostname: "b.example.com", urls: [u("https://b.example.com/1"), u("https://b.example.com/2")] }),
    ];
    expect(allUrls(hosts).map((x) => x.url)).toEqual([
      "https://a.example.com/1",
      "https://b.example.com/1",
      "https://b.example.com/2",
    ]);
  });
});
