// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { DiscoveredUrl, Host } from "@/lib/types";
import { SiteMap } from "./site-map";

afterEach(cleanup);

function url(path: string, extra: Partial<DiscoveredUrl> = {}): DiscoveredUrl {
  return {
    url: `https://example.com${path}`,
    hostname: "example.com",
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

function hosts(): Host[] {
  const urls = [url("/"), url("/about", { title: "About us" }), url("/team")];
  // A second host whose tree would sit between the tapped row and a panel
  // placed after all trees.
  const other = Array.from({ length: 30 }, (_, i) => ({
    ...url(`/post-${i}`),
    url: `https://blog.example.com/post-${i}`,
    hostname: "blog.example.com",
  }));
  return [
    { hostname: "example.com", state: "reachable", sources: ["target"], counts: { urls: 3, pages: 3, apis: 0, assets: 0 }, urls },
    { hostname: "blog.example.com", state: "reachable", sources: ["html"], counts: { urls: 30, pages: 30, apis: 0, assets: 0 }, urls: other },
  ];
}

/** Details panels are identified by the "Found through" row UrlDetail renders. */
function inlinePanels(container: HTMLElement) {
  return Array.from(container.querySelectorAll("section div.lg\\:hidden")).filter((el) =>
    el.textContent?.includes("Found through"),
  );
}

describe("SiteMap details on narrow screens", () => {
  it("opens details directly under the tapped path, inside its host's tree", () => {
    const { container } = render(<SiteMap hosts={hosts()} />);
    fireEvent.click(screen.getByRole("button", { name: "about" }));

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

  it("closes with the Close button or by tapping the path again", () => {
    const { container } = render(<SiteMap hosts={hosts()} />);
    const about = screen.getByRole("button", { name: "about" });

    fireEvent.click(about);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(inlinePanels(container)).toHaveLength(0);

    fireEvent.click(about);
    fireEvent.click(about);
    expect(inlinePanels(container)).toHaveLength(0);
  });

  it("keeps one open entry at a time", () => {
    const { container } = render(<SiteMap hosts={hosts()} />);
    fireEvent.click(screen.getByRole("button", { name: "about" }));
    fireEvent.click(screen.getByRole("button", { name: "team" }));
    const panels = inlinePanels(container);
    expect(panels).toHaveLength(1);
    expect(panels[0].textContent).toContain("https://example.com/team");
  });

  it("uses the side panel only on wide screens", () => {
    const { container } = render(<SiteMap hosts={hosts()} />);
    const aside = container.querySelector("aside");
    // Hidden by default (narrow), shown from the lg breakpoint.
    expect(aside?.className).toContain("hidden");
    expect(aside?.className).toContain("lg:block");
    fireEvent.click(screen.getByRole("button", { name: "about" }));
    expect(aside?.textContent).toContain("Found through");
  });
});
