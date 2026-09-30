// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const pathname = vi.hoisted(() => ({ current: "/" }));
vi.mock("next/navigation", () => ({ usePathname: () => pathname.current }));

import { SiteFooter } from "./site-footer";
import { SiteHeader } from "./site-header";

afterEach(cleanup);

describe("SiteHeader", () => {
  it("is hidden on the home page", () => {
    pathname.current = "/";
    const { container } = render(<SiteHeader />);
    expect(container.innerHTML).toBe("");
  });

  it("links home from other pages", () => {
    pathname.current = "/scans/abc123";
    render(<SiteHeader />);
    expect(screen.getByRole("link", { name: "Web Scanner" }).getAttribute("href")).toBe("/");
  });
});

describe("SiteFooter", () => {
  it("credits the author and links the source and license", () => {
    render(<SiteFooter />);
    const href = (name: string) => screen.getByRole("link", { name }).getAttribute("href");
    expect(href("Anandhu Remanan")).toBe("https://imanandhu.in");
    expect(href("Source on GitHub")).toBe("https://github.com/anandhuremanan/website_mapper");
    expect(href("MIT License")).toBe("https://github.com/anandhuremanan/website_mapper/blob/main/LICENSE");
    for (const link of screen.getAllByRole("link")) {
      expect(link.getAttribute("rel")).toContain("noopener");
    }
  });
});
