"use client";

import Link from "next/link";
import { useState } from "react";
import { cancelScan } from "@/lib/api";
import type { Phase, Scan, StepStatus } from "@/lib/types";

const stepIcon: Record<StepStatus, { icon: string; className: string }> = {
  pending: { icon: "○", className: "text-muted" },
  running: { icon: "●", className: "text-accent animate-pulse" },
  done: { icon: "✓", className: "text-ok" },
  failed: { icon: "✕", className: "text-warn" },
  skipped: { icon: "–", className: "text-muted" },
  stopped: { icon: "■", className: "text-muted" },
};

const phaseLabel: Partial<Record<Phase, string>> = {
  discovering_subdomains: "Discovering subdomains",
  searching_archives: "Searching web archives",
  reading_sitemaps: "Reading robots.txt and sitemaps",
  resolving_hosts: "Resolving hosts",
  probing_hosts: "Probing hosts",
  crawling_hosts: "Crawling hosts",
  finalizing: "Finalizing results",
};

function heading(scan: Scan): string {
  switch (scan.status) {
    case "queued":
      return scan.queuePosition
        ? `Queued — position ${scan.queuePosition}. Other scans are using the server's capacity; this one starts automatically.`
        : "Queued — starting…";
    case "failed":
      return "Scan failed";
    case "cancelled":
      return scan.stopReason === "server_shutdown" ? "Stopped: the server shut down" : "Cancelled";
    case "completed":
      // Shown only while the finished scan's results are downloading.
      return "Loading results…";
    default: {
      const label = phaseLabel[scan.phase] ?? "Scanning";
      const p = scan.progress;
      return p && p.total > 0
        ? `${label} ${p.completed.toLocaleString()} / ${p.total.toLocaleString()}`
        : `${label}…`;
    }
  }
}

export function ScanProgress({ scan }: { scan: Scan }) {
  const { counts } = scan;
  const active = scan.status === "queued" || scan.status === "running";
  const [cancelling, setCancelling] = useState(false);
  const [cancelError, setCancelError] = useState<string | null>(null);
  const [detached, setDetached] = useState(false);

  async function onCancel() {
    setCancelling(true);
    setCancelError(null);
    try {
      const res = await cancelScan(scan.id);
      if (res.detached) setDetached(true);
    } catch (err) {
      setCancelError(err instanceof Error ? err.message : String(err));
      setCancelling(false);
    }
  }

  return (
    <div className="max-w-xl space-y-8">
      <div>
        <div className="flex items-baseline justify-between gap-4">
          <h1 className="font-mono text-2xl font-semibold tracking-tight">{scan.domain}</h1>
          {active && !detached && (
            <button
              type="button"
              onClick={onCancel}
              disabled={cancelling}
              className="text-sm text-muted hover:text-danger disabled:opacity-50"
            >
              {cancelling ? "Cancelling…" : "Cancel scan"}
            </button>
          )}
        </div>
        <p className="mt-1 text-sm text-muted" aria-live="polite">
          {heading(scan)}
        </p>
        {scan.progress && scan.progress.total > 0 && scan.status === "running" && (
          <div className="mt-3 h-1 overflow-hidden rounded bg-subtle" aria-hidden>
            <div
              className="h-full bg-accent transition-[width]"
              style={{ width: `${(100 * scan.progress.completed) / scan.progress.total}%` }}
            />
          </div>
        )}
        {scan.error && <p className="mt-2 text-sm text-danger">{scan.error}</p>}
        {active && !detached && (scan.subscribers ?? 1) > 1 && (
          <p className="mt-2 text-sm text-muted">
            Someone else requested the same scan, so you are sharing one scan instead of starting
            another.
          </p>
        )}
        {detached && (
          <p className="mt-2 text-sm text-muted">
            You left this scan. It keeps running because others are waiting for it.{" "}
            <Link href="/" className="text-accent hover:underline">
              Start a new scan
            </Link>
          </p>
        )}
        {cancelError && <p className="mt-2 text-sm text-danger">{cancelError}</p>}
      </div>

      <dl className="grid grid-cols-2 gap-x-8 gap-y-3 text-sm sm:grid-cols-3">
        <Count label="Hosts discovered" value={counts.hosts} />
        <Count label="Hosts resolved" value={counts.hostsResolved} />
        <Count label="Hosts reachable" value={counts.hostsReachable} />
        <Count label="Hosts crawled" value={counts.hostsCrawled} />
        <Count label="URLs discovered" value={counts.urls} />
        <Count label="URLs fetched" value={counts.urlsFetched} />
        <Count label="API-like endpoints" value={counts.apis} />
        <Count label="JavaScript files" value={counts.javascript} />
      </dl>

      <ol className="space-y-2 text-sm">
        {scan.steps.map((step) => {
          const { icon, className } = stepIcon[step.status];
          return (
            <li key={step.id} className="flex items-center gap-3">
              <span aria-hidden className={`w-4 text-center ${className}`}>
                {icon}
              </span>
              <span className={step.status === "pending" ? "text-muted" : ""}>{step.label}</span>
              <span className="sr-only">({step.status})</span>
            </li>
          );
        })}
      </ol>

      {scan.limits.length > 0 && (
        <ul className="space-y-1 text-sm text-muted">
          {scan.limits.map((l, i) => (
            <li key={i}>{l.message}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Count({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <dt className="text-muted">{label}</dt>
      <dd className="font-mono text-lg tabular-nums">{value.toLocaleString()}</dd>
    </div>
  );
}
