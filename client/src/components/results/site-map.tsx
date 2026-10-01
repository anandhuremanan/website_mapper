"use client";

import { useMemo, useState } from "react";
import { SourceTags } from "@/components/source-list";
import { allUrls, plural, statusTone, typeLabels } from "@/lib/format";
import { buildTrees, type TreeNode } from "@/lib/tree";
import type { DiscoveredUrl, Host } from "@/lib/types";
import { UrlDetail } from "./url-detail";

const MAX_CHILDREN = 100;

/**
 * A browsable map of the site: hosts, with their discovered paths as a tree.
 *
 * Selecting a path shows its details in a side panel on wide screens. On
 * narrow screens the layout is a single column, where that panel would end
 * up below every host's tree, far from the tapped row, so details open
 * inline under the selected path instead.
 */
export function SiteMap({ hosts }: { hosts: Host[] }) {
  const [includeAssets, setIncludeAssets] = useState(false);
  const [selected, setSelected] = useState<DiscoveredUrl | null>(null);
  // Selecting the open entry again closes it.
  const toggle = (u: DiscoveredUrl) =>
    setSelected((cur) => (cur?.url === u.url ? null : u));
  const close = () => setSelected(null);

  const trees = useMemo(() => {
    const urls = allUrls(hosts);
    return buildTrees(
      includeAssets ? urls : urls.filter((u) => u.type !== "asset"),
    );
  }, [hosts, includeAssets]);
  const mapped = hosts.filter((h) => trees.has(h.hostname));
  const empty = hosts.length - mapped.length;

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
          {mapped.map((h) => {
            const tree = trees.get(h.hostname);
            return (
              <section
                key={h.hostname}
                className="overflow-x-auto rounded-md border border-border bg-surface p-3"
              >
                <h3 className="mb-2 flex flex-wrap items-baseline gap-x-2 text-sm">
                  <span className="break-all font-mono font-medium">
                    {h.hostname}
                  </span>
                  <span className="text-xs text-muted">
                    {tree
                      ? plural(tree.total, "URL")
                      : "no URLs discovered on this host"}
                  </span>
                </h3>
                {tree && (
                  <Branch
                    node={tree}
                    depth={0}
                    selected={selected}
                    onSelect={toggle}
                    onClose={close}
                  />
                )}
              </section>
            );
          })}
          {empty > 0 && (
            <p className="text-sm text-muted">
              {plural(empty, "other host")} {empty === 1 ? "has" : "have"} no
              discovered URLs
              {includeAssets ? "" : " (besides assets)"}. See the Hosts tab for
              their DNS and HTTP status.
            </p>
          )}
        </div>

        <aside className="hidden lg:sticky lg:top-4 lg:block lg:self-start">
          <div className="rounded-md border border-border bg-surface p-4">
            {selected ? (
              <UrlDetail url={selected} />
            ) : (
              <p className="text-sm text-muted">
                Select a path to see how it was discovered.
              </p>
            )}
          </div>
        </aside>
      </div>
    </div>
  );
}

interface BranchProps {
  node: TreeNode;
  depth: number;
  selected: DiscoveredUrl | null;
  onSelect: (u: DiscoveredUrl) => void;
  onClose: () => void;
}

function Branch({ node, depth, selected, onSelect, onClose }: BranchProps) {
  const [expanded, setExpanded] = useState(depth < 1);
  const [showAll, setShowAll] = useState(false);
  const hasChildren = node.children.length > 0;
  const children = showAll
    ? node.children
    : node.children.slice(0, MAX_CHILDREN);
  const selectedHere = node.urls.find((u) => u.url === selected?.url);
  const label = depth === 0 ? "/" : node.name;

  return (
    <div className="text-sm">
      {/* The arrow keeps its place; the name and badges wrap beside it. */}
      <div className="flex items-start gap-1" data-row>
        {hasChildren ? (
          <button
            type="button"
            onClick={() => setExpanded(!expanded)}
            aria-expanded={expanded}
            aria-label={
              expanded ? `Collapse ${node.path}` : `Expand ${node.path}`
            }
            className="w-4 shrink-0 text-muted hover:text-fg"
          >
            {expanded ? "▾" : "▸"}
          </button>
        ) : (
          <span className="w-4 shrink-0" />
        )}
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-1 gap-y-0.5">
          {node.urls.length > 0 ? (
            // The path itself is the obvious thing to tap, especially on phones.
            <button
              type="button"
              onClick={() => onSelect(node.urls[0])}
              aria-expanded={selectedHere !== undefined}
              className="min-w-0 break-all text-left font-mono hover:underline"
            >
              {label}
            </button>
          ) : (
            <span className="min-w-0 break-all font-mono">{label}</span>
          )}
          {hasChildren && (
            <span className="text-xs text-muted">({node.total})</span>
          )}
          {node.urls.map((u) => (
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
              <span className={`font-mono ${statusTone(u.status)}`}>
                {u.status ?? "—"}
              </span>
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
            <button
              type="button"
              onClick={onClose}
              className="text-xs text-muted hover:text-fg"
            >
              Close
            </button>
          </div>
          <UrlDetail url={selectedHere} />
        </div>
      )}
      {expanded && hasChildren && (
        <div className="ml-2 border-l border-border pl-3">
          {children.map((c) => (
            <Branch
              key={c.path}
              node={c}
              depth={depth + 1}
              selected={selected}
              onSelect={onSelect}
              onClose={onClose}
            />
          ))}
          {node.children.length > children.length && (
            <button
              type="button"
              onClick={() => setShowAll(true)}
              className="ml-5 text-xs text-accent hover:underline"
            >
              Show {node.children.length - children.length} more
            </button>
          )}
        </div>
      )}
    </div>
  );
}
