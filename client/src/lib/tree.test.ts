import { describe, expect, it } from "vitest";
import { buildTrees } from "./tree";
import type { DiscoveredUrl } from "./types";

function url(u: string): DiscoveredUrl {
  const parsed = new URL(u);
  return {
    url: u,
    hostname: parsed.hostname,
    path: parsed.pathname,
    type: "page",
    state: "discovered",
    typeEvidence: "",
    sources: ["html"],
    fetched: false,
  };
}

describe("buildTrees", () => {
  it("groups URLs by host and path segment", () => {
    const trees = buildTrees([
      url("https://example.com/"),
      url("https://example.com/blog/post-1"),
      url("https://example.com/blog/post-2"),
      url("https://example.com/about"),
      url("https://api.example.com/v1/users"),
    ]);

    expect([...trees.keys()]).toEqual(["example.com", "api.example.com"]);

    const root = trees.get("example.com")!;
    expect(root.total).toBe(4);
    expect(root.urls.map((u) => u.url)).toEqual(["https://example.com/"]);
    // Sections with children sort before leaves.
    expect(root.children.map((c) => c.name)).toEqual(["blog", "about"]);

    const blog = root.children[0];
    expect(blog.path).toBe("/blog");
    expect(blog.total).toBe(2);
    expect(blog.urls).toEqual([]);
    expect(blog.children.map((c) => c.path)).toEqual(["/blog/post-1", "/blog/post-2"]);
  });

  it("keeps URLs that differ only by query on the same node", () => {
    const trees = buildTrees([
      url("https://example.com/search?q=a"),
      url("https://example.com/search?q=b"),
    ]);
    const search = trees.get("example.com")!.children[0];
    expect(search.urls).toHaveLength(2);
  });
});
