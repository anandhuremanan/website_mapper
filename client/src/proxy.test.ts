import { NextRequest } from "next/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { proxy } from "./proxy";

afterEach(() => vi.unstubAllEnvs());

/** The request headers the proxy sends on, as Next.js records them. */
function forwarded(response: Response, name: string): string | null {
  return response.headers.get(`x-middleware-request-${name}`);
}

function request(path: string, headers: Record<string, string> = {}) {
  return new NextRequest(`https://webscanner.example${path}`, { headers });
}

describe("proxy", () => {
  it("forwards /api requests to the server, path and query intact", () => {
    vi.stubEnv("API_URL", "https://api.internal.example");
    const res = proxy(request("/api/scans/abc/urls?type=page,unknown&q=a%20b"));
    expect(res.headers.get("x-middleware-rewrite")).toBe(
      "https://api.internal.example/api/scans/abc/urls?type=page,unknown&q=a%20b",
    );
  });

  it("uses the local server when no address is configured", () => {
    vi.stubEnv("API_URL", undefined);
    const res = proxy(request("/api/health"));
    expect(res.headers.get("x-middleware-rewrite")).toBe("http://localhost:8080/api/health");
  });

  it("adds the secret and the visitor's address when a secret is set", () => {
    vi.stubEnv("API_PROXY_SECRET", "s3cret");
    const res = proxy(
      request("/api/scans", { "x-real-ip": "203.0.113.7", "x-forwarded-for": "198.51.100.1" }),
    );
    expect(forwarded(res, "x-scanner-secret")).toBe("s3cret");
    expect(forwarded(res, "x-scanner-client")).toBe("203.0.113.7");
  });

  it("falls back to the first forwarded address", () => {
    vi.stubEnv("API_PROXY_SECRET", "s3cret");
    const res = proxy(request("/api/scans", { "x-forwarded-for": "203.0.113.7, 10.0.0.1" }));
    expect(forwarded(res, "x-scanner-client")).toBe("203.0.113.7");
  });

  it("never passes on what a visitor put in its own headers", () => {
    // With a secret: the visitor's values are replaced.
    vi.stubEnv("API_PROXY_SECRET", "s3cret");
    let res = proxy(
      request("/api/scans", {
        "x-scanner-secret": "guess",
        "x-scanner-client": "192.0.2.1",
        "x-real-ip": "203.0.113.7",
      }),
    );
    expect(forwarded(res, "x-scanner-secret")).toBe("s3cret");
    expect(forwarded(res, "x-scanner-client")).toBe("203.0.113.7");

    // Without one: they are dropped.
    vi.stubEnv("API_PROXY_SECRET", "");
    res = proxy(request("/api/scans", { "x-scanner-secret": "guess", "x-scanner-client": "192.0.2.1" }));
    expect(forwarded(res, "x-scanner-secret")).toBeNull();
    expect(forwarded(res, "x-scanner-client")).toBeNull();
    const overridden = res.headers.get("x-middleware-override-headers") ?? "";
    expect(overridden).not.toContain("x-scanner-secret");
    expect(overridden).not.toContain("x-scanner-client");
  });
});
