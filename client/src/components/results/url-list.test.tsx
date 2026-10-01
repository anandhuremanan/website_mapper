// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DiscoveredUrl } from "@/lib/types";

const api = vi.hoisted(() => ({ getUrls: vi.fn() }));
vi.mock("@/lib/api", () => api);

import { UrlList } from "./url-list";

afterEach(cleanup);

function url(path: string): DiscoveredUrl {
  return {
    url: `https://example.com${path}`,
    hostname: "example.com",
    path,
    type: "page",
    typeEvidence: "Linked from HTML",
    sources: ["html"],
    fetched: false,
    state: "discovered",
  };
}

beforeEach(() => {
  api.getUrls.mockReset().mockImplementation(async (_id: string, q: { q?: string; after?: string }) => {
    if (q.q === "nothing") return { urls: [] };
    if (q.q) return { urls: [url(`/found-${q.q}`)] };
    if (q.after === "page-2") return { urls: [url("/c")] };
    return { urls: [url("/a"), url("/b")], next: "page-2" };
  });
});

describe("UrlList", () => {
  it("shows the first page and loads the next on request", async () => {
    render(<UrlList scanId="s1" host="example.com" types={["page", "unknown"]} total={3} empty="None." />);
    await screen.findByText("/a");
    expect(api.getUrls).toHaveBeenCalledWith("s1", {
      host: "example.com",
      types: ["page", "unknown"],
      q: "",
      after: undefined,
    });
    expect(screen.queryByText("/c")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    await screen.findByText("/c");
    expect(api.getUrls).toHaveBeenLastCalledWith("s1", expect.objectContaining({ after: "page-2" }));
    // The last page has no "Show more".
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    expect(screen.getByText("/a")).toBeTruthy();
  });

  it("searches on the server and starts the listing again", async () => {
    render(<UrlList scanId="s1" total={3} empty="None." />);
    await screen.findByText("/a");

    fireEvent.change(screen.getByRole("searchbox"), { target: { value: " pricing " } });
    await screen.findByText("/found-pricing");
    expect(api.getUrls).toHaveBeenLastCalledWith("s1", expect.objectContaining({ q: "pricing", after: undefined }));
    expect(screen.queryByText("/a")).toBeNull();

    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "nothing" } });
    await screen.findByText("No URLs match “nothing”.");
  });

  it("asks the server for nothing when the listing is empty", () => {
    render(<UrlList scanId="s1" total={0} empty="No assets were discovered." />);
    expect(screen.getByText("No assets were discovered.")).toBeTruthy();
    expect(api.getUrls).not.toHaveBeenCalled();
  });

  it("offers a retry when a page fails to load", async () => {
    api.getUrls.mockRejectedValueOnce(new Error("Could not reach the server."));
    render(<UrlList scanId="s1" total={3} empty="None." />);
    fireEvent.click(await screen.findByRole("button", { name: "Try again" }));
    await screen.findByText("/a");
  });
});
