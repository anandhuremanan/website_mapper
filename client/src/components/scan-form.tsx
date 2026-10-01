"use client";

import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { track } from "@/lib/analytics";
import { createScan } from "@/lib/api";
import type { ScanMode } from "@/lib/types";

const modes: { id: ScanMode; label: string; detail: string }[] = [
  {
    id: "passive",
    label: "Passive",
    detail:
      "Certificate logs, web archives and DNS. Never contacts the site. Archived routes may be out of date.",
  },
  {
    id: "light",
    label: "Standard",
    detail:
      "Adds a check of each live host and its robots.txt and sitemaps. A few requests per host.",
  },
  {
    id: "full",
    label: "Full crawl",
    detail:
      "Follows links on live hosts to discover routes that aren't listed in sitemaps. Makes significantly more requests and takes longer.",
  },
];

export function ScanForm() {
  const router = useRouter();
  const [target, setTarget] = useState("");
  const [mode, setMode] = useState<ScanMode>("light");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!target.trim()) {
      setError("Enter a domain such as example.com");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const scan = await createScan(target.trim(), mode);
      track("scan_started", {
        mode,
        outcome: scan.reused ? "reused" : scan.coalesced ? "joined" : "new",
      });
      router.push(`/scans/${scan.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Something went wrong");
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="space-y-3">
      <label htmlFor="target" className="sr-only">
        Domain or URL
      </label>
      <div className="flex flex-col gap-2 sm:flex-row">
        <input
          id="target"
          name="target"
          type="text"
          inputMode="url"
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          autoFocus
          placeholder="https://example.com"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          // flex-1 only applies side by side: in the stacked phone layout it
          // would collapse the input's height. 16px text on phones stops iOS
          // from zooming the page when the field is focused.
          className="h-11 w-full shrink-0 rounded-md border border-border bg-surface px-3 font-mono text-base outline-none placeholder:text-muted/70 focus:border-accent focus:ring-2 focus:ring-accent/20 sm:w-auto sm:flex-1 sm:shrink sm:text-sm"
        />
        <button
          type="submit"
          disabled={submitting}
          className="h-11 shrink-0 rounded-md bg-fg px-5 text-sm font-medium text-bg transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          {submitting ? "Starting…" : "Map website"}
        </button>
      </div>
      <fieldset className="space-y-2 pt-1">
        <legend className="mb-2 text-sm font-medium">Scan Mode</legend>
        {modes.map((m) => (
          <label
            key={m.id}
            className="flex cursor-pointer items-start gap-2 text-sm"
          >
            <input
              type="radio"
              name="mode"
              value={m.id}
              checked={mode === m.id}
              onChange={() => setMode(m.id)}
              className="mt-1"
            />
            <span>
              <span className="font-medium">{m.label}</span>
              <span className="block text-muted">{m.detail}</span>
            </span>
          </label>
        ))}
      </fieldset>
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
    </form>
  );
}
