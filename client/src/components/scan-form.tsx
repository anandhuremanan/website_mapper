"use client";

import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { createScan } from "@/lib/api";

export function ScanForm() {
  const router = useRouter();
  const [target, setTarget] = useState("");
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
      const scan = await createScan(target.trim());
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
          className="h-11 flex-1 rounded-md border border-border bg-surface px-3 font-mono text-sm outline-none placeholder:text-muted/70 focus:border-accent focus:ring-2 focus:ring-accent/20"
        />
        <button
          type="submit"
          disabled={submitting}
          className="h-11 rounded-md bg-fg px-5 text-sm font-medium text-bg transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          {submitting ? "Starting…" : "Map website"}
        </button>
      </div>
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
    </form>
  );
}
