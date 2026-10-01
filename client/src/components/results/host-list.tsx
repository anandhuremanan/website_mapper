"use client";

import { useState } from "react";
import { FoundThrough, SourceTags } from "@/components/source-list";
import { getHosts } from "@/lib/api";
import { hostStatus, plural, statusTone } from "@/lib/format";
import type { Host, UrlType } from "@/lib/types";
import { useDebounced, usePages } from "@/lib/use-pages";
import { ListFooter, UrlList } from "./url-list";

interface Props {
  scanId: string;
  apex: string;
  /** How many hosts the result has. */
  total: number;
  /** Hostname to show expanded initially (when arriving from the overview). */
  initialOpen?: string | null;
}

/** A searchable list of hosts, read from the server a page at a time. */
export function HostList({ scanId, apex, total, initialOpen = null }: Props) {
  // Arriving for one host: search for it, so it is listed wherever it sorts.
  const [query, setQuery] = useState(initialOpen ?? "");
  const search = useDebounced(query.trim());

  if (total === 0) {
    return <p className="text-sm text-muted">No hosts were discovered.</p>;
  }

  return (
    <div className="space-y-3">
      <input
        type="search"
        placeholder={`Search ${total.toLocaleString()} hosts`}
        aria-label="Search hosts"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        className="h-9 w-full rounded-md border border-border bg-surface px-3 text-sm outline-none focus:border-accent sm:w-80"
      />
      <HostRows key={search} scanId={scanId} apex={apex} search={search} initialOpen={initialOpen} />
    </div>
  );
}

interface RowsProps {
  scanId: string;
  apex: string;
  search: string;
  initialOpen: string | null;
}

function HostRows({ scanId, apex, search, initialOpen }: RowsProps) {
  const [open, setOpen] = useState<string | null>(initialOpen);
  const { pages, loading, error, hasMore, loadMore } = usePages((after) =>
    getHosts(scanId, { q: search, after }),
  );
  const hosts = pages.flatMap((p) => p.hosts);

  return (
    <>
      <ul className="divide-y divide-border rounded-md border border-border bg-surface">
        {hosts.map((h) => {
          const isOpen = open === h.hostname;
          const st = hostStatus(h);
          return (
            <li key={h.hostname}>
              <button
                type="button"
                onClick={() => setOpen(isOpen ? null : h.hostname)}
                aria-expanded={isOpen}
                className="grid w-full grid-cols-[1.25rem_minmax(0,1fr)] items-center gap-x-2 gap-y-1 px-3 py-2 text-left text-sm hover:bg-subtle sm:grid-cols-[1.25rem_minmax(0,1fr)_9rem_auto]"
              >
                <span aria-hidden className={`text-center ${st.tone}`}>
                  {st.icon}
                </span>
                <span className="min-w-0 truncate">
                  <span className="font-mono">{h.hostname}</span>
                  {h.hostname === apex && <span className="ml-2 text-xs text-muted">apex</span>}
                  {h.http?.title && <span className="ml-3 text-muted">{h.http.title}</span>}
                </span>
                <span className={`col-start-2 font-mono text-xs sm:col-start-auto ${st.tone}`}>
                  {st.label}
                </span>
                <span className="col-start-2 flex flex-wrap items-center gap-2 sm:col-start-auto sm:justify-end">
                  {h.counts.urls > 0 && (
                    <span className="text-xs text-muted">{plural(h.counts.urls, "URL")}</span>
                  )}
                  <SourceTags sources={h.sources} />
                </span>
              </button>
              {isOpen && (
                <div className="border-t border-border bg-subtle/60 px-3 py-4">
                  <HostDetail scanId={scanId} host={h} />
                </div>
              )}
            </li>
          );
        })}
        {hosts.length === 0 && !loading && !error && (
          <li className="px-3 py-4 text-sm text-muted">
            {search ? `No hosts match “${search}”.` : "No hosts to show."}
          </li>
        )}
        {hosts.length === 0 && loading && <li className="px-3 py-4 text-sm text-muted">Loading…</li>}
      </ul>
      <ListFooter
        shown={hosts.length}
        loading={loading}
        error={error}
        hasMore={hasMore}
        onMore={loadMore}
      />
    </>
  );
}

type Section = "routes" | "apis" | "assets";

const sections: Record<Section, { label: string; types: UrlType[] }> = {
  // Routes include URLs of unknown type (for example form targets).
  routes: { label: "Routes", types: ["page", "unknown"] },
  apis: { label: "APIs", types: ["api"] },
  assets: { label: "Assets", types: ["asset"] },
};

export function HostDetail({ scanId, host }: { scanId: string; host: Host }) {
  const st = hostStatus(host);
  const { dns, http, crawl, sitemap, counts } = host;
  const [section, setSection] = useState<Section>("routes");

  const totals: Record<Section, number> = {
    routes: counts.urls - counts.apis - counts.assets,
    apis: counts.apis,
    assets: counts.assets,
  };

  return (
    <div className="space-y-6">
      <p className="text-sm">
        <span className={st.tone}>{st.icon}</span> {st.detail}
      </p>

      <dl className="grid grid-cols-[6.5rem_minmax(0,1fr)] gap-x-3 gap-y-2 text-sm sm:grid-cols-[8rem_minmax(0,1fr)] sm:gap-x-4">
        <Row label="Discovered through">
          <FoundThrough sources={host.sources} />
        </Row>

        <Row label="DNS">
          {!dns ? (
            <span className="text-muted">Not checked</span>
          ) : dns.skipped ? (
            <span className="text-muted">Not resolved: {dns.skipped}</span>
          ) : dns.resolved ? (
            <span className="space-y-0.5">
              <span className="block break-all font-mono">{dns.addresses?.join(", ")}</span>
              {dns.cname && (
                <span className="block text-muted">
                  CNAME <span className="font-mono">{dns.cname}</span>
                </span>
              )}
              {dns.nonPublic && (
                <span className="block text-warn">Private or reserved addresses; not contacted.</span>
              )}
            </span>
          ) : (
            <span className="text-muted">Does not resolve ({dns.error ?? "no records"})</span>
          )}
        </Row>

        <Row label="HTTP">
          {!http ? (
            <span className="text-muted">Not probed</span>
          ) : http.skipped ? (
            <span className="text-muted">Not probed: {http.skipped}</span>
          ) : !http.reachable ? (
            <span className="text-warn">No response ({http.error ?? "unknown error"})</span>
          ) : (
            <span className="space-y-0.5">
              <span className="block break-all font-mono">
                <span className={statusTone(http.status)}>{http.status}</span> {http.url}
              </span>
              {http.redirect && (
                <span className="block break-all text-muted">
                  Redirects to <span className="font-mono">{http.redirect}</span>
                  {http.finalUrl && http.finalUrl !== http.redirect && (
                    <>
                      {" "}
                      → ends at <span className="font-mono">{http.finalUrl}</span>
                    </>
                  )}
                  {http.finalStatus ? ` (${http.finalStatus})` : ""}
                </span>
              )}
              {http.title && <span className="block">{http.title}</span>}
              {http.server && (
                <span className="block text-muted">
                  Server: <span className="font-mono">{http.server}</span>
                </span>
              )}
            </span>
          )}
        </Row>

        {sitemap && (
          <Row label="Sitemaps">
            {sitemap.skipped ? (
              <span className="text-muted">Not read: {sitemap.skipped}</span>
            ) : (
              <span>
                {plural(sitemap.urls, "URL")} from {plural(sitemap.files, "sitemap")}
                <span className="text-muted">
                  {" "}
                  · robots.txt {sitemap.robotsStatus === 200 ? "found" : "not found"}
                  {sitemap.limitReached && " · per-host sitemap limit reached"}
                </span>
              </span>
            )}
          </Row>
        )}
        {crawl && (
          <Row label="Crawl">
            {crawl.skipped ? (
              <span className="text-muted">Not crawled: {crawl.skipped}</span>
            ) : (
              <span>
                {plural(crawl.requests, "request")}
                {crawl.limitReached && (
                  <span className="text-muted"> — per-host request limit reached</span>
                )}
              </span>
            )}
          </Row>
        )}
      </dl>

      {counts.urls > 0 && (
        <div className="space-y-3">
          <div className="flex gap-1" role="tablist">
            {(Object.keys(sections) as Section[]).map((s) => (
              <button
                key={s}
                type="button"
                role="tab"
                aria-selected={section === s}
                onClick={() => setSection(s)}
                className={`rounded px-2.5 py-1 text-sm ${
                  section === s ? "bg-surface font-medium ring-1 ring-border" : "text-muted hover:text-fg"
                }`}
              >
                {sections[s].label}{" "}
                <span className="font-mono text-xs text-muted">{totals[s].toLocaleString()}</span>
              </button>
            ))}
          </div>
          <UrlList
            key={section}
            scanId={scanId}
            host={host.hostname}
            types={sections[section].types}
            total={totals[section]}
            showKind={section !== "apis"}
            empty={`No ${sections[section].label.toLowerCase()} were discovered on this host.`}
          />
          {host.urlsOmitted ? (
            <p className="text-xs text-muted">
              {plural(host.urlsOmitted, "more URL")} found on this host{" "}
              {host.urlsOmitted === 1 ? "was" : "were"} not recorded: the scan reached its limit of
              recorded URLs.
            </p>
          ) : null}
        </div>
      )}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}
