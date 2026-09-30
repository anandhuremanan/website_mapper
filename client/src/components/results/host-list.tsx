"use client";

import { useState } from "react";
import { FoundThrough, SourceTags } from "@/components/source-list";
import { hostStatus, plural, statusTone } from "@/lib/format";
import type { Host } from "@/lib/types";
import { UrlList } from "./url-list";

interface Props {
  hosts: Host[];
  apex: string;
  /** Hostname expanded initially (e.g. when arriving from the overview). */
  initialOpen?: string | null;
}

export function HostList({ hosts, apex, initialOpen = null }: Props) {
  const [open, setOpen] = useState<string | null>(initialOpen);

  if (hosts.length === 0) {
    return <p className="text-sm text-muted">No hosts were discovered.</p>;
  }

  return (
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
              className="grid w-full grid-cols-[1.25rem_1fr] items-center gap-x-2 gap-y-1 px-3 py-2 text-left text-sm hover:bg-subtle sm:grid-cols-[1.25rem_1fr_9rem_auto]"
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
                <HostDetail host={h} />
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}

type Section = "routes" | "apis" | "assets";

export function HostDetail({ host }: { host: Host }) {
  const st = hostStatus(host);
  const { dns, http, crawl, sitemap } = host;
  const [section, setSection] = useState<Section>("routes");

  const groups: Record<Section, typeof host.urls> = {
    routes: host.urls.filter((u) => u.type === "page" || u.type === "unknown"),
    apis: host.urls.filter((u) => u.type === "api"),
    assets: host.urls.filter((u) => u.type === "asset"),
  };
  const labels: Record<Section, string> = { routes: "Routes", apis: "APIs", assets: "Assets" };

  return (
    <div className="space-y-6">
      <p className="text-sm">
        <span className={st.tone}>{st.icon}</span> {st.detail}
      </p>

      <dl className="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-2 text-sm">
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

      {host.urls.length > 0 && (
        <div className="space-y-3">
          <div className="flex gap-1" role="tablist">
            {(Object.keys(groups) as Section[]).map((s) => (
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
                {labels[s]} <span className="font-mono text-xs text-muted">{groups[s].length}</span>
              </button>
            ))}
          </div>
          <UrlList
            key={section}
            urls={groups[section]}
            showKind={section !== "apis"}
            empty={`No ${labels[section].toLowerCase()} were discovered on this host.`}
          />
          {host.urlsOmitted ? (
            <p className="text-xs text-muted">
              {plural(host.urlsOmitted, "more URL")} referenced on this host{" "}
              {host.urlsOmitted === 1 ? "was" : "were"} not recorded (per-host limit).
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
