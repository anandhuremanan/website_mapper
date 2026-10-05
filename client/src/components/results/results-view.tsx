"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { track } from "@/lib/analytics";
import { createScan, exportUrl, getHosts } from "@/lib/api";
import { formatDuration, hostStatus, modeLabels, plural, sourceLabel } from "@/lib/format";
import type { Scan, ScanSummary } from "@/lib/types";
import { usePages } from "@/lib/use-pages";
import { useVerification } from "@/lib/verification";
import { HostList } from "./host-list";
import { SiteMap } from "./site-map";
import { ListFooter, UrlList } from "./url-list";

type Tab = "overview" | "hosts" | "map" | "pages" | "apis" | "assets" | "technologies";

/** What a running scan has found so far, in the shape of a result summary. */
export function liveSummary(scan: Scan): ScanSummary {
  return {
    scanId: scan.id,
    status: scan.status,
    limits: scan.limits,
    domain: {
      mode: scan.mode,
      target: scan.target,
      canonical: scan.domain,
      startUrl: scan.startUrl,
      scannedAt: scan.startedAt ?? scan.createdAt,
      durationMs: 0,
    },
    errors: scan.errors,
    counts: scan.counts,
    technologies: [],
  };
}

interface Props {
  scan: Scan;
  result: ScanSummary;
  /**
   * The scan is still running: `result` is what it has found so far (see
   * liveSummary). Counts follow the scan; lists show what had been found
   * when they were opened, until they are refreshed.
   */
  live?: boolean;
}

/**
 * A scan's result. Only the summary is loaded up front; each tab reads its
 * hosts and URLs from the server in pages as they are shown.
 */
export function ResultsView({ scan, result, live = false }: Props) {
  const [tab, setTabState] = useState<Tab>("overview");
  const [openHost, setOpenHost] = useState<string | null>(null);
  // Changing this starts every list again from its first page.
  const [refresh, setRefresh] = useState(0);
  const setTab = (next: Tab) => {
    if (next !== tab) track("result_tab_opened", { tab: next, live });
    setTabState(next);
  };

  const { counts, domain } = result;
  // The Pages tab also lists URLs of unknown type (for example form targets).
  const pages = counts.urls - counts.apis - counts.assets;

  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: "overview", label: "Overview" },
    { id: "hosts", label: "Hosts", count: counts.hosts },
    { id: "map", label: "Map" },
    { id: "pages", label: "Pages", count: pages },
    { id: "apis", label: "APIs", count: counts.apis },
    { id: "assets", label: "Assets", count: counts.assets },
    // Technologies are detected from the whole result, once it is complete.
    ...(live
      ? []
      : [{ id: "technologies" as const, label: "Technologies", count: result.technologies.length }]),
  ];

  const showHost = (hostname: string) => {
    setOpenHost(hostname);
    setTab("hosts");
  };

  return (
    <div className="space-y-6">
      {live ? (
        <header className="space-y-1">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h2 className="text-lg font-semibold">Found so far</h2>
            <button
              type="button"
              onClick={() => setRefresh((n) => n + 1)}
              className="text-sm text-accent hover:underline"
            >
              Refresh lists
            </button>
          </div>
          <p className="text-xs text-muted">
            The counts follow the scan. Lists show what had been found when they were opened.
          </p>
        </header>
      ) : (
        <header className="space-y-2">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h1 className="font-mono text-2xl font-semibold tracking-tight">{domain.canonical}</h1>
            <span className="flex flex-wrap items-baseline gap-x-4 text-sm">
              {counts.urls > 0 && (
                <a
                  href={exportUrl(scan.id)}
                  download
                  onClick={() => track("urls_exported", { urls: counts.urls })}
                  className="text-accent hover:underline"
                >
                  Download URLs (CSV)
                </a>
              )}
              <ScanAgain scan={scan} />
              <Link href="/" className="text-accent hover:underline">
                New scan
              </Link>
            </span>
          </div>
          <p className="text-sm">
            {plural(counts.hosts, "host")} ({counts.hostsReachable} reachable) ·{" "}
            {plural(counts.urls, "URL")} · {plural(counts.apis, "API-like endpoint")} ·{" "}
            {plural(counts.assets, "asset")}
          </p>
          <p className="text-xs text-muted">
            {modeLabels[domain.mode] ?? domain.mode} scan from{" "}
            <span className="font-mono">{domain.startUrl}</span> ·{" "}
            {new Date(domain.scannedAt).toLocaleString()} · took{" "}
            {formatDuration(domain.durationMs)}
          </p>
        </header>
      )}

      {scan.status === "failed" && (
        <p className="rounded-md border border-border bg-surface px-3 py-2 text-sm">
          <span className="font-medium text-danger">The scan could not complete.</span>{" "}
          {scan.error} Any partial results are shown below.
        </p>
      )}

      {!live && (scan.status === "cancelled" || result.limits.length > 0) && (
        <div className="space-y-1 rounded-md border border-border bg-surface px-3 py-2 text-sm">
          {scan.status === "cancelled" && (
            <p className="font-medium">
              {scan.stopReason === "server_shutdown"
                ? "The scan was stopped because the server shut down."
                : "The scan was cancelled."}{" "}
              Results collected before it stopped are shown below.
            </p>
          )}
          {result.limits.map((l, i) => (
            <p key={i} className="text-muted">
              {l.message}
            </p>
          ))}
        </div>
      )}

      <nav className="-mx-4 overflow-x-auto px-4" aria-label="Result sections">
        <div className="flex gap-1 border-b border-border">
          {tabs.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setTab(t.id)}
              aria-current={tab === t.id ? "page" : undefined}
              className={`-mb-px whitespace-nowrap border-b-2 px-3 py-2 text-sm ${
                tab === t.id
                  ? "border-fg font-medium"
                  : "border-transparent text-muted hover:text-fg"
              }`}
            >
              {t.label}
              {t.count !== undefined && (
                <span className="ml-1.5 font-mono text-xs text-muted">
                  {t.count.toLocaleString()}
                </span>
              )}
            </button>
          ))}
        </div>
      </nav>

      {/* Lists start again when refreshed, and when the scan finishes. */}
      <section key={`${refresh}|${live}`}>
        {tab === "overview" && (
          <Overview
            scanId={scan.id}
            result={result}
            live={live}
            goTo={setTab}
            showHost={showHost}
          />
        )}
        {tab === "hosts" && (
          <HostList
            key={openHost ?? ""}
            scanId={scan.id}
            apex={domain.canonical}
            total={counts.hosts}
            initialOpen={openHost}
          />
        )}
        {tab === "map" && <SiteMap scanId={scan.id} />}
        {tab === "pages" && (
          <UrlList
            scanId={scan.id}
            types={["page", "unknown"]}
            total={pages}
            showKind
            empty="No pages were discovered."
            note="Pages are URLs that returned HTML or were linked from HTML, on every host. Unknown URLs (for example form targets) are listed here too."
          />
        )}
        {tab === "apis" && (
          <UrlList
            scanId={scan.id}
            types={["api"]}
            total={counts.apis}
            empty="No API-like endpoints were discovered."
            note="API-like means the URL returned JSON, or its path looks like an API route. Open an entry to see which evidence applies — a matching path alone is not proof."
          />
        )}
        {tab === "assets" && (
          <UrlList
            scanId={scan.id}
            types={["asset"]}
            total={counts.assets}
            showKind
            empty="No assets were discovered."
          />
        )}
        {tab === "technologies" && <Technologies result={result} />}
      </section>
    </div>
  );
}

/** Starts a new scan of the same target and mode, ignoring recent results. */
function ScanAgain({ scan }: { scan: Scan }) {
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const { attach: attachVerification, getToken } = useVerification();

  async function onClick() {
    setBusy(true);
    setError(null);
    try {
      const verificationToken = await getToken();
      const next = await createScan(scan.target, scan.mode, { fresh: true, verificationToken });
      track("scan_again", { mode: scan.mode });
      router.push(`/scans/${next.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={onClick}
        disabled={busy}
        className="text-accent hover:underline disabled:opacity-50"
      >
        {busy ? "Starting…" : "Scan again"}
      </button>
      <div ref={attachVerification} />
      {error && (
        <span role="alert" className="text-warn">
          {error}
        </span>
      )}
    </>
  );
}

interface OverviewProps {
  scanId: string;
  result: ScanSummary;
  live: boolean;
  goTo: (t: Tab) => void;
  showHost: (hostname: string) => void;
}

function Overview({ scanId, result, live, goTo, showHost }: OverviewProps) {
  const { counts } = result;
  const rows: { label: string; value: number; tab: Tab }[] = [
    { label: "Hosts discovered", value: counts.hosts, tab: "hosts" },
    { label: "Hosts that resolve", value: counts.hostsResolved, tab: "hosts" },
    { label: "Hosts answering HTTP(S)", value: counts.hostsReachable, tab: "hosts" },
    { label: "Hosts crawled", value: counts.hostsCrawled, tab: "hosts" },
    { label: "Pages", value: counts.pages, tab: "pages" },
    { label: "API-like endpoints", value: counts.apis, tab: "apis" },
    { label: "Assets", value: counts.assets, tab: "assets" },
    // Technologies are detected once the scan is complete.
    ...(live
      ? []
      : [{ label: "Technologies", value: result.technologies.length, tab: "technologies" as const }]),
  ];

  return (
    <div className="grid grid-cols-1 gap-8 md:grid-cols-[18rem_minmax(0,1fr)]">
      <div className="space-y-6">
        <table className="w-full text-sm">
          <tbody className="divide-y divide-border">
            {rows.map((r) => (
              <tr key={r.label}>
                <td className="py-2">
                  <button type="button" onClick={() => goTo(r.tab)} className="hover:underline">
                    {r.label}
                  </button>
                </td>
                <td className="py-2 text-right font-mono tabular-nums">
                  {r.value.toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        <p className="text-sm leading-6 text-muted">
          This map shows what the scanner could discover publicly: hostnames from certificate
          logs and links, and routes linked from pages it crawled. It is not a complete
          inventory of DNS records or server-side routes.
        </p>

        {result.errors.length > 0 && (
          <div className="text-sm">
            <h2 className="mb-2 font-medium">Problems during the scan</h2>
            <ul className="space-y-1">
              {result.errors.map((e, i) => (
                <li key={i}>
                  <span className="font-mono">{sourceLabel(e.engine)}</span>
                  {e.partial && <span className="text-muted"> (partly succeeded)</span>}
                  <span className="text-muted"> — {e.message}</span>
                </li>
              ))}
            </ul>
            <p className="mt-2 text-muted">Results from the other steps are still shown.</p>
          </div>
        )}
      </div>

      <OverviewHosts scanId={scanId} showHost={showHost} />
    </div>
  );
}

/** The result's hosts with their status, a page at a time. */
function OverviewHosts({ scanId, showHost }: Pick<OverviewProps, "scanId" | "showHost">) {
  const { pages, loading, error, hasMore, loadMore } = usePages((after) =>
    getHosts(scanId, { after }),
  );
  const hosts = pages.flatMap((p) => p.hosts);

  return (
    <div className="space-y-3 text-sm">
      <h2 className="font-medium">Hosts</h2>
      <ul className="divide-y divide-border rounded-md border border-border bg-surface">
        {hosts.map((h) => {
          const st = hostStatus(h);
          return (
            <li key={h.hostname}>
              <button
                type="button"
                onClick={() => showHost(h.hostname)}
                className="grid w-full grid-cols-[1.25rem_minmax(0,1fr)_auto] items-center gap-x-2 px-3 py-1.5 text-left hover:bg-subtle"
              >
                <span aria-hidden className={`text-center ${st.tone}`}>
                  {st.icon}
                </span>
                <span className="truncate font-mono">{h.hostname}</span>
                <span className={`text-right font-mono text-xs ${st.tone}`}>
                  {st.label}
                  {h.counts.urls > 0 && (
                    <span className="ml-3 text-muted">{plural(h.counts.urls, "URL")}</span>
                  )}
                </span>
              </button>
            </li>
          );
        })}
        {hosts.length === 0 && (
          <li className="px-3 py-3 text-muted">
            {loading ? "Loading…" : error ? "" : "No hosts were discovered."}
          </li>
        )}
      </ul>
      <ListFooter
        shown={hosts.length}
        loading={loading}
        error={error}
        hasMore={hasMore}
        onMore={loadMore}
      />
    </div>
  );
}

function Technologies({ result }: { result: ScanSummary }) {
  if (result.technologies.length === 0) {
    return (
      <p className="text-sm text-muted">
        No technologies were identified from the evidence collected.
      </p>
    );
  }
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted">
        Detected from simple, visible evidence such as response headers and URL patterns.
      </p>
      <ul className="divide-y divide-border rounded-md border border-border bg-surface">
        {result.technologies.map((t) => (
          <li key={t.name} className="px-3 py-3 text-sm">
            <div className="font-medium">{t.name}</div>
            <ul className="mt-1 space-y-0.5">
              {t.evidence.map((ev) => (
                <li key={ev} className="break-all font-mono text-xs text-muted">
                  {ev}
                </li>
              ))}
            </ul>
          </li>
        ))}
      </ul>
    </div>
  );
}
