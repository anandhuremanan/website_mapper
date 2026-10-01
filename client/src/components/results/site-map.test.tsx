// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DiscoveredUrl, Host, TreeChild, TreeLevel } from "@/lib/types";

const api = vi.hoisted(() => ({ getHosts: vi.fn(), getTree: vi.fn() }));
vi.mock("@/lib/api", () => api);

import { SiteMap } from "./site-map";

afterEach(cleanup);

function url(host: string, path: string, extra: Partial<DiscoveredUrl> = {}): DiscoveredUrl {
  return {
    url: `https://${host}${path}`,
    hostname: host,
    path,
    status: 200,
    contentType: "text/html",
    type: "page",
    typeEvidence: "Responded with text/html",
    sources: ["html"],
    fetched: true,
    state: "verified",
    ...extra,
  };
}

function host(hostname: string, urls: number, assets = 0): Host {
  return {
    hostname,
    state: "reachable",
    sources: ["target"],
    counts: { urls, pages: urls - assets, apis: 0, assets },
  };
}

function leaf(hostname: string, name: string, extra: Partial<DiscoveredUrl> = {}): TreeChild {
  return { name, path: `/${name}`, total: 1, hasChildren: false, urls: [url(hostname, `/${name}`, extra)] };
}

// What the server would answer, by host and parent path.
const levels: Record<string, TreeLevel> = {
  "example.com|": {
    path: "/",
    total: 5,
    urls: [url("example.com", "/")],
    children: [
      leaf("example.com", "about", { title: "About us" }),
      { name: "docs", path: "/docs", total: 2, hasChildren: true, urls: [] },
      leaf("example.com", "team"),
    ],
  },
  "example.com|/docs": {
    path: "/docs",
    total: 2,
    urls: [],
    children: [
      { name: "intro", path: "/docs/intro", total: 1, hasChildren: false, urls: [url("example.com", "/docs/intro")] },
    ],
    next: "more-docs",
  },
  "blog.example.com|": {
    path: "/",
    total: 30,
    urls: [],
    children: [leaf("blog.example.com", "post-1")],
  },
};

beforeEach(() => {
  api.getHosts.mockReset().mockResolvedValue({
    // A second host whose tree would sit between the tapped row and a panel
    // placed after all trees; a third that has only assets.
    hosts: [host("example.com", 5), host("blog.example.com", 30), host("cdn.example.com", 4, 4)],
  });
  api.getTree.mockReset().mockImplementation(
    async (_id: string, q: { host: string; path?: string; after?: string }) => {
      if (q.after) return { path: q.path ?? "/", total: 0, urls: [], children: [leaf(q.host, "docs/second")] };
      return levels[`${q.host}|${q.path ?? ""}`];
    },
  );
});

/** Details panels are identified by the "Found through" row UrlDetail renders. */
function inlinePanels(container: HTMLElement) {
  return Array.from(container.querySelectorAll("section div.lg\\:hidden")).filter((el) =>
    el.textContent?.includes("Found through"),
  );
}

describe("SiteMap loads only what is shown", () => {
  it("opens the first host's tree and leaves the others closed", async () => {
    render(<SiteMap scanId="s1" />);
    await screen.findByRole("button", { name: "about" });

    expect(api.getTree).toHaveBeenCalledTimes(1);
    expect(api.getTree).toHaveBeenCalledWith("s1", { host: "example.com", path: undefined, assets: false, after: undefined });
    // Hosts with nothing but assets are not listed until assets are included.
    expect(screen.queryByText("cdn.example.com")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /blog\.example\.com/ }));
    await screen.findByRole("button", { name: "post-1" });
    expect(api.getTree).toHaveBeenCalledTimes(2);
  });

  it("loads a folder's children when it is expanded, and more on request", async () => {
    render(<SiteMap scanId="s1" />);
    fireEvent.click(await screen.findByRole("button", { name: "Expand /docs" }));
    await screen.findByRole("button", { name: "intro" });
    expect(api.getTree).toHaveBeenLastCalledWith("s1", { host: "example.com", path: "/docs", assets: false, after: undefined });

    fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    await screen.findByRole("button", { name: "docs/second" });
    expect(api.getTree).toHaveBeenLastCalledWith("s1", { host: "example.com", path: "/docs", assets: false, after: "more-docs" });
  });

  it("reloads the trees with assets when they are included", async () => {
    render(<SiteMap scanId="s1" />);
    await screen.findByRole("button", { name: "about" });
    fireEvent.click(screen.getByRole("checkbox"));
    await screen.findByText("cdn.example.com");
    expect(api.getTree).toHaveBeenLastCalledWith("s1", { host: "example.com", path: undefined, assets: true, after: undefined });
  });
});

describe("SiteMap details on narrow screens", () => {
  it("opens details directly under the tapped path, inside its host's tree", async () => {
    const { container } = render(<SiteMap scanId="s1" />);
    fireEvent.click(await screen.findByRole("button", { name: "about" }));

    const panels = inlinePanels(container);
    expect(panels).toHaveLength(1);
    const panel = panels[0];
    // Inside example.com's section, not after every host's tree.
    expect(panel.closest("section")?.textContent).toContain("example.com");
    expect(within(panel as HTMLElement).getByText("About us")).toBeTruthy();
    // It immediately follows the tapped row.
    const row = screen.getByRole("button", { name: "about" }).closest("[data-row]");
    expect(row?.nextElementSibling).toBe(panel);
  });

  it("closes with the Close button or by tapping the path again", async () => {
    const { container } = render(<SiteMap scanId="s1" />);
    const about = await screen.findByRole("button", { name: "about" });

    fireEvent.click(about);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(inlinePanels(container)).toHaveLength(0);

    fireEvent.click(about);
    fireEvent.click(about);
    expect(inlinePanels(container)).toHaveLength(0);
  });

  it("keeps one open entry at a time", async () => {
    const { container } = render(<SiteMap scanId="s1" />);
    fireEvent.click(await screen.findByRole("button", { name: "about" }));
    fireEvent.click(screen.getByRole("button", { name: "team" }));
    const panels = inlinePanels(container);
    expect(panels).toHaveLength(1);
    expect(panels[0].textContent).toContain("https://example.com/team");
  });

  it("uses the side panel only on wide screens", async () => {
    const { container } = render(<SiteMap scanId="s1" />);
    const aside = container.querySelector("aside");
    // Hidden by default (narrow), shown from the lg breakpoint.
    expect(aside?.className).toContain("hidden");
    expect(aside?.className).toContain("lg:block");
    fireEvent.click(await screen.findByRole("button", { name: "about" }));
    expect(aside?.textContent).toContain("Found through");
  });
});
