"use client";

import { useState } from "react";
import { SourceTags } from "@/components/source-list";
import { getHosts, getTree } from "@/lib/api";
import { plural, statusTone, typeLabels } from "@/lib/format";
import type { DiscoveredUrl, Host } from "@/lib/types";
import { usePages } from "@/lib/use-pages";
import { UrlDetail } from "./url-detail";
import { ListFooter } from "./url-list";

/** What every part of the map needs to show and change the selected URL. */
interface Selection {
  selected: DiscoveredUrl | null;
  onSelect: (u: DiscoveredUrl) => void;
  onClose: () => void;
}

/**
 * A browsable map of the site: hosts, with their discovered paths as a tree.
 *
 * Nothing is loaded until it is looked at: hosts come in pages, a host's
 * tree when the host is opened, and each folder's children when the folder
 * is expanded. The first host starts open.
 *
 * Selecting a path shows its details in a side panel on wide screens. On
 * narrow screens the layout is a single column, where that panel would end
 * up below every host's tree, far from the tapped row, so details open
 * inline under the selected path instead.
 */
export function SiteMap({ scanId }: { scanId: string }) {
  const [includeAssets, setIncludeAssets] = useState(false);
  const [selected, setSelected] = useState<DiscoveredUrl | null>(null);
  const selection: Selection = {
    selected,
    // Selecting the open entry again closes it.
    onSelect: (u) => setSelected((cur) => (cur?.url === u.url ? null : u)),
    onClose: () => setSelected(null),
  };

  const { pages, loading, error, hasMore, loadMore } = usePages((after) =>
    getHosts(scanId, { after }),
  );
  const hosts = pages.flatMap((p) => p.hosts);
  const mapped = hosts.filter((h) => mappedUrls(h, includeAssets) > 0);

  return (
    <div className="space-y-4">
      <label className="flex items-center gap-2 text-sm text-muted">
        <input
          type="checkbox"
          checked={includeAssets}
          onChange={(e) => setIncludeAssets(e.target.checked)}
        />
        Include assets (scripts, styles, images, fonts)
      </label>

      {/* grid-cols-1 gives the column a real width (minmax(0, 1fr)); an
          implicit column would grow to its widest row and overflow the page. */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,1fr)_24rem]">
        <div className="min-w-0 space-y-4">
          {mapped.map((h, i) => (
            // Assets change every count in a tree, so it is loaded again.
            <HostTree
              key={`${h.hostname}|${includeAssets}`}
              scanId={scanId}
              host={h}
              assets={includeAssets}
              defaultOpen={i === 0}
              {...selection}
            />
          ))}
          {hosts.length === 0 && loading && <p className="text-sm text-muted">Loading…</p>}
          {hosts.length > 0 && (
            <p className="text-sm text-muted">
              {mapped.length === 0 ? "None of these hosts has" : "Hosts are listed only if they have"}{" "}
              discovered URLs{includeAssets ? "" : " besides assets"}. See the Hosts tab for every
              host&apos;s DNS and HTTP status.
            </p>
          )}
          <ListFooter
            shown={hosts.length}
            loading={loading}
            error={error}
            hasMore={hasMore}
            onMore={loadMore}
          />
        </div>

        <aside className="hidden lg:sticky lg:top-4 lg:block lg:self-start">
          <div className="rounded-md border border-border bg-surface p-4">
            {selected ? (
              <UrlDetail url={selected} />
            ) : (
              <p className="text-sm text-muted">Select a path to see how it was discovered.</p>
            )}
          </div>
        </aside>
      </div>
    </div>
  );
}

/** The URLs of a host that the map shows. */
function mappedUrls(h: Host, includeAssets: boolean): number {
  return h.counts.urls - (includeAssets ? 0 : h.counts.assets);
}

interface HostTreeProps extends Selection {
  scanId: string;
  host: Host;
  assets: boolean;
  defaultOpen: boolean;
}

function HostTree({ scanId, host, assets, defaultOpen, ...selection }: HostTreeProps) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <section className="overflow-x-auto rounded-md border border-border bg-surface p-3">
      <h3 className="flex flex-wrap items-baseline gap-x-2 text-sm">
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          className="flex min-w-0 items-baseline gap-1 text-left"
        >
          <span aria-hidden className="w-4 shrink-0 text-muted">
            {open ? "▾" : "▸"}
          </span>
          <span className="break-all font-mono font-medium">{host.hostname}</span>
        </button>
        <span className="text-xs text-muted">{plural(mappedUrls(host, assets), "URL")}</span>
      </h3>
      {open && (
        <div className="mt-2">
          <Level scanId={scanId} host={host.hostname} assets={assets} root {...selection} />
        </div>
      )}
    </section>
  );
}

interface LevelProps extends Selection {
  scanId: string;
  host: string;
  /** The parent path; the host's root when omitted. */
  path?: string;
  assets: boolean;
  /** Show the parent itself ("/") above its children. */
  root?: boolean;
}

/** One level of a host's tree: the children of a path, loaded when shown. */
function Level({ scanId, host, path, assets, root, ...selection }: LevelProps) {
  const { pages, loading, error, hasMore, loadMore } = usePages((after) =>
    getTree(scanId, { host, path, assets, after }),
  );
  const children = pages.flatMap((p) => p.children);
  const first = pages[0];

  const list = (
    <>
      {children.map((c) => (
        <Branch key={c.path} scanId={scanId} host={host} assets={assets} node={c} {...selection} />
      ))}
      {!first && loading && <p className="ml-5 text-xs text-muted">Loading…</p>}
      <div className="ml-5 text-xs">
        <ListFooter
          shown={children.length}
          loading={loading}
          error={error}
          hasMore={hasMore}
          onMore={loadMore}
        />
      </div>
    </>
  );
  if (!root) return <div className="text-sm">{list}</div>;
  return (
    <div className="text-sm">
      {first && <NodeRow label="/" urls={first.urls} total={first.total} {...selection} />}
      <div className="ml-2 border-l border-border pl-3">{list}</div>
    </div>
  );
}

interface BranchProps extends Selection {
  scanId: string;
  host: string;
  assets: boolean;
  node: { name: string; path: string; total: number; hasChildren: boolean; urls: DiscoveredUrl[] };
}

function Branch({ scanId, host, assets, node, ...selection }: BranchProps) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="text-sm">
      <NodeRow
        label={node.name}
        path={node.path}
        urls={node.urls}
        total={node.hasChildren ? node.total : undefined}
        expanded={node.hasChildren ? expanded : undefined}
        onToggle={() => setExpanded(!expanded)}
        {...selection}
      />
      {expanded && node.hasChildren && (
        <div className="ml-2 border-l border-border pl-3">
          <Level scanId={scanId} host={host} path={node.path} assets={assets} {...selection} />
        </div>
      )}
    </div>
  );
}

interface NodeRowProps extends Selection {
  label: string;
  /** Full path, for the expand button's label. */
  path?: string;
  /** URLs that end exactly at this node. */
  urls: DiscoveredUrl[];
  /** URLs at or below the node; shown for nodes that have children. */
  total?: number;
  /** Whether the node is expanded; undefined for nodes without children. */
  expanded?: boolean;
  onToggle?: () => void;
}

function NodeRow({ label, path, urls, total, expanded, onToggle, selected, onSelect, onClose }: NodeRowProps) {
  const selectedHere = urls.find((u) => u.url === selected?.url);
  return (
    <>
      {/* The arrow keeps its place; the name and badges wrap beside it. */}
      <div className="flex items-start gap-1" data-row>
        {expanded !== undefined ? (
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={expanded ? `Collapse ${path}` : `Expand ${path}`}
            className="w-4 shrink-0 text-muted hover:text-fg"
          >
            {expanded ? "▾" : "▸"}
          </button>
        ) : (
          <span className="w-4 shrink-0" />
        )}
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-1 gap-y-0.5">
          {urls.length > 0 ? (
            // The path itself is the obvious thing to tap, especially on phones.
            <button
              type="button"
              onClick={() => onSelect(urls[0])}
              aria-expanded={selectedHere !== undefined}
              className="min-w-0 break-all text-left font-mono hover:underline"
            >
              {label}
            </button>
          ) : (
            <span className="min-w-0 break-all font-mono">{label}</span>
          )}
          {total !== undefined && (
            <span className="text-xs text-muted">({total.toLocaleString()})</span>
          )}
          {urls.map((u) => (
            <button
              key={u.url}
              type="button"
              onClick={() => onSelect(u)}
              aria-expanded={selected?.url === u.url}
              className={`ml-1 flex shrink-0 items-center gap-1.5 rounded px-1.5 py-px text-xs hover:bg-subtle ${
                selected?.url === u.url ? "bg-subtle ring-1 ring-border" : ""
              }`}
              title={u.url}
            >
              <span className={`font-mono ${statusTone(u.status)}`}>{u.status ?? "—"}</span>
              <span className="text-muted">{typeLabels[u.type]}</span>
              <span className="hidden sm:inline">
                <SourceTags sources={u.sources} />
              </span>
            </button>
          ))}
        </div>
      </div>
      {selectedHere && (
        <div className="my-2 rounded-md border border-border bg-surface p-3 lg:hidden">
          <div className="mb-2 flex justify-end">
            <button type="button" onClick={onClose} className="text-xs text-muted hover:text-fg">
              Close
            </button>
          </div>
          <UrlDetail url={selectedHere} />
        </div>
      )}
    </>
  );
}
