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
 */
export function SiteMap({ hosts }: { hosts: Host[] }) {
  const [includeAssets, setIncludeAssets] = useState(false);
  const [selected, setSelected] = useState<DiscoveredUrl | null>(null);

  const trees = useMemo(() => {
    const urls = allUrls(hosts);
    return buildTrees(includeAssets ? urls : urls.filter((u) => u.type !== "asset"));
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

      <div className="grid gap-4 lg:grid-cols-[1fr_24rem]">
        <div className="space-y-4">
          {mapped.map((h) => {
            const tree = trees.get(h.hostname);
            return (
              <section key={h.hostname} className="rounded-md border border-border bg-surface p-3">
                <h3 className="mb-2 flex items-baseline gap-2 text-sm">
                  <span className="font-mono font-medium">{h.hostname}</span>
                  <span className="text-xs text-muted">
                    {tree ? plural(tree.total, "URL") : "no URLs discovered on this host"}
                  </span>
                </h3>
                {tree && (
                  <Branch node={tree} depth={0} selected={selected} onSelect={setSelected} />
                )}
              </section>
            );
          })}
          {empty > 0 && (
            <p className="text-sm text-muted">
              {plural(empty, "other host")} {empty === 1 ? "has" : "have"} no discovered URLs
              {includeAssets ? "" : " (besides assets)"}. See the Hosts tab for their DNS and
              HTTP status.
            </p>
          )}
        </div>

        <aside className="lg:sticky lg:top-4 lg:self-start">
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

interface BranchProps {
  node: TreeNode;
  depth: number;
  selected: DiscoveredUrl | null;
  onSelect: (u: DiscoveredUrl) => void;
}

function Branch({ node, depth, selected, onSelect }: BranchProps) {
  const [expanded, setExpanded] = useState(depth < 1);
  const [showAll, setShowAll] = useState(false);
  const hasChildren = node.children.length > 0;
  const children = showAll ? node.children : node.children.slice(0, MAX_CHILDREN);

  return (
    <div className="text-sm">
      <div className="flex items-center gap-1">
        {hasChildren ? (
          <button
            type="button"
            onClick={() => setExpanded(!expanded)}
            aria-expanded={expanded}
            aria-label={expanded ? `Collapse ${node.path}` : `Expand ${node.path}`}
            className="w-4 text-muted hover:text-fg"
          >
            {expanded ? "▾" : "▸"}
          </button>
        ) : (
          <span className="w-4" />
        )}
        <span className="font-mono">{depth === 0 ? "/" : node.name}</span>
        {hasChildren && <span className="text-xs text-muted">({node.total})</span>}
        {node.urls.map((u) => (
          <button
            key={u.url}
            type="button"
            onClick={() => onSelect(u)}
            className={`ml-2 flex items-center gap-1.5 rounded px-1.5 py-px text-xs hover:bg-subtle ${
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
      {expanded && hasChildren && (
        <div className="ml-2 border-l border-border pl-3">
          {children.map((c) => (
            <Branch key={c.path} node={c} depth={depth + 1} selected={selected} onSelect={onSelect} />
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
