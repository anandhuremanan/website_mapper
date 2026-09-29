"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { createScan } from "@/lib/api";

export function ScanForm() {
  const router = useRouter();
  const [target, setTarget] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!target.trim()) {
      setError("Enter a domain such as example.com");
      return;
    }
    if (!confirmed) {
      setError("Confirm that you own this domain or have permission to scan it.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const scan = await createScan(target.trim(), confirmed);
      router.push(`/scans/${scan.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Something went wrong");
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="space-y-4">
      <label htmlFor="target" className="sr-only">
        Domain or URL
      </label>
      <div className="flex flex-col gap-2 sm:flex-row">
        {/* flex-1 only in the row layout: in a column it would override the height. */}
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
          className="h-12 w-full min-w-0 rounded-md border border-border bg-surface px-3 font-mono text-base outline-none placeholder:text-muted/70 focus:border-accent focus:ring-2 focus:ring-accent/20 sm:h-11 sm:flex-1 sm:text-sm"
        />
        <button
          type="submit"
          disabled={submitting || !confirmed}
          className="h-12 w-full rounded-md bg-fg px-5 text-sm font-medium text-bg transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-50 sm:h-11 sm:w-auto"
        >
          {submitting ? "Starting…" : "Map website"}
        </button>
      </div>

      <div className="space-y-2 rounded-md border border-border bg-surface px-3 py-3 text-sm">
        <p className="font-medium">Only scan domains you own or have permission to assess.</p>
        <label className="flex items-start gap-2.5">
          <input
            type="checkbox"
            name="authorizationConfirmed"
            required
            checked={confirmed}
            onChange={(e) => setConfirmed(e.target.checked)}
            className="mt-0.5 size-4 shrink-0 accent-fg"
          />
          <span>I confirm I own this domain or have permission to scan it.</span>
        </label>
        <p className="text-xs text-muted">
          The scanner identifies itself and respects robots.txt.{" "}
          <Link href="/bot" className="text-accent hover:underline">
            About the crawler
          </Link>
        </p>
      </div>

      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
    </form>
  );
}
