"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

/**
 * The top bar with the app name. It is hidden on the home page, which shows
 * the name as its own heading.
 */
export function SiteHeader() {
  if (usePathname() === "/") {
    return null;
  }
  return (
    <header className="border-b border-border">
      <div className="mx-auto flex h-12 max-w-5xl items-center px-4">
        <Link href="/" className="text-sm font-semibold tracking-tight">
          Web Scanner
        </Link>
      </div>
    </header>
  );
}
