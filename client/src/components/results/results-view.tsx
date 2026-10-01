"use client";

import Link from "next/link";
import { useMemo, useState } from "react";
import { allUrls, formatDuration, hostStatus, modeLabels, plural, sourceLabel } from "@/lib/format";
import type { Scan, ScanResult } from "@/lib/types";
import { HostList } from "./host-list";
import { SiteMap } from "./site-map";
import { UrlList } from "./url-list";

type Tab = "overview" | "hosts" | "map" | "pages" | "apis" | "assets" | "technologies";

export function ResultsView({ scan, result }: { scan: Scan; result: ScanResult }) {
  const [tab, setTab] = useState<Tab>("overview");
  const [openHost, setOpenHost] = useState<string | null>(null);

  const urls = useMemo(() => allUrls(result.hosts), [result.hosts]);
  const groups = useMemo(
    () => ({
      pages: urls.filter((u) => u.type === "page" || u.type === "unknown"),
      apis: urls.filter((u) => u.type === "api"),
      assets: urls.filter((u) => u.type === "asset"),
    }),
    [urls],
  );

  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: "overview", label: "Overview" },
    { id: "hosts", label: "Hosts", count: result.hosts.length },
    { id: "map", label: "Map" },
    { id: "pages", label: "Pages", count: groups.pages.length },
    { id: "apis", label: "APIs", count: groups.apis.length },
    { id: "assets", label: "Assets", count: groups.assets.length },
    { id: "technologies", label: "Technologies", count: result.technologies.length },
  ];

  const { counts, domain } = result;
  const showHost = (hostname: string) => {
    setOpenHost(hostname);
    setTab("hosts");
  };

  return (
    <div className="space-y-6">
      <header className="space-y-2">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h1 className="font-mono text-2xl font-semibold tracking-tight">{domain.canonical}</h1>
          <Link href="/" className="text-sm text-accent hover:underline">
            New scan
          </Link>
        </div>
        <p className="text-sm">
          {plural(counts.hosts, "host")} ({counts.hostsReachable} reachable) ·{" "}
          {plural(counts.urls, "URL")} · {plural(counts.apis, "API-like endpoint")} ·{" "}
          {plural(counts.assets, "asset")}
        </p>
        <p className="text-xs text-muted">
          {modeLabels[domain.mode] ?? domain.mode} scan from{" "}
          <span className="font-mono">{domain.startUrl}</span> ·{" "}
          {new Date(domain.scannedAt).toLocaleString()} · took {formatDuration(domain.durationMs)}
        </p>
      </header>

      {scan.status === "failed" && (
        <p className="rounded-md border border-border bg-surface px-3 py-2 text-sm">
          <span className="font-medium text-danger">The scan could not complete.</span>{" "}
          {scan.error} Any partial results are shown below.
        </p>
      )}

      {(scan.status === "cancelled" || result.limits.length > 0) && (
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
                <span className="ml-1.5 font-mono text-xs text-muted">{t.count}</span>
              )}
            </button>
          ))}
        </div>
      </nav>

      <section>
        {tab === "overview" && <Overview result={result} goTo={setTab} showHost={showHost} />}
        {tab === "hosts" && (
          <HostList
            key={openHost ?? ""}
            hosts={result.hosts}
            apex={domain.canonical}
            initialOpen={openHost}
          />
        )}
        {tab === "map" && <SiteMap hosts={result.hosts} />}
        {tab === "pages" && (
          <UrlList
            urls={groups.pages}
            showKind
            empty="No pages were discovered."
            note="Pages are URLs that returned HTML or were linked from HTML, on every host. Unknown URLs (for example form targets) are listed here too."
          />
        )}
        {tab === "apis" && (
          <UrlList
            urls={groups.apis}
            empty="No API-like endpoints were discovered."
            note="API-like means the URL returned JSON, or its path looks like an API route. Open an entry to see which evidence applies — a matching path alone is not proof."
          />
        )}
        {tab === "assets" && (
          <UrlList urls={groups.assets} showKind empty="No assets were discovered." />
        )}
        {tab === "technologies" && <Technologies result={result} />}
      </section>
    </div>
  );
}

interface OverviewProps {
  result: ScanResult;
  goTo: (t: Tab) => void;
  showHost: (hostname: string) => void;
}

function Overview({ result, goTo, showHost }: OverviewProps) {
  const { counts } = result;
  const rows: { label: string; value: number; tab: Tab }[] = [
    { label: "Hosts discovered", value: counts.hosts, tab: "hosts" },
    { label: "Hosts that resolve", value: counts.hostsResolved, tab: "hosts" },
    { label: "Hosts answering HTTP(S)", value: counts.hostsReachable, tab: "hosts" },
    { label: "Hosts crawled", value: counts.hostsCrawled, tab: "hosts" },
    { label: "Pages", value: counts.pages, tab: "pages" },
    { label: "API-like endpoints", value: counts.apis, tab: "apis" },
    { label: "Assets", value: counts.assets, tab: "assets" },
    { label: "Technologies", value: result.technologies.length, tab: "technologies" },
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

      <div className="text-sm">
        <h2 className="mb-2 font-medium">Hosts</h2>
        <ul className="divide-y divide-border rounded-md border border-border bg-surface">
          {result.hosts.map((h) => {
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
        </ul>
      </div>
    </div>
  );
}

function Technologies({ result }: { result: ScanResult }) {
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
