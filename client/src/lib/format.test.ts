import { describe, expect, it } from "vitest";
import { hostStatus } from "./format";
import type { Host } from "./types";

function host(partial: Partial<Host>): Host {
  return {
    hostname: "api.example.com",
    state: "discovered",
    sources: ["certificate-transparency"],
    counts: { urls: 0, pages: 0, apis: 0, assets: 0 },
    ...partial,
  };
}

describe("hostStatus in passive scans", () => {
  it("does not claim a host failed HTTP when it was never contacted", () => {
    const st = hostStatus(
      host({ state: "resolved", dns: { resolved: true, addresses: ["93.184.216.34"] } }),
    );
    expect(st.label).toBe("resolves");
    expect(st.detail).toContain("Not contacted");
  });
});

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
